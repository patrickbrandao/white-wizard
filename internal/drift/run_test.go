package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// TestSpec017Defaults pins the provider table of spec 017 (ADR 0063):
// opencode-google in the default list, azure out of it, the key names, the
// Gemini alias and the Gemini probe model.
func TestSpec017Defaults(t *testing.T) {
	want := []string{"openai", "anthropic", "openrouter", "opencode", "gemini", "opencode-google"}
	if !slices.Equal(DefaultProviders, want) {
		t.Errorf("DefaultProviders = %v", DefaultProviders)
	}
	if providers[ProviderOpenCodeGoogle].EnvKey != "OPENCODE_API_KEY" || providers[ProviderOpenCodeGoogle].DriverName() != ProviderOpenCode {
		t.Errorf("opencode-google: %+v", providers[ProviderOpenCodeGoogle])
	}
	if providers[ProviderAzure].EnvKey != "AZURE_OPENAI_API_KEY" || providers[ProviderGemini].DefaultModel != "gemini-3.5-flash-lite" {
		t.Errorf("azure/gemini: %+v %+v", providers[ProviderAzure], providers[ProviderGemini])
	}
	env := map[string]string{"GEMENI_API_KEY": "g-alias", "AZURE_OPENAI_API_KEY": "az", "OPENCODE_API_KEY": "oc"}
	keys, err := LoadKeys("", false, func(k string) string { return env[k] })
	if err != nil || keys[ProviderGemini] != "g-alias" || keys[ProviderAzure] != "az" || keys[ProviderOpenCodeGoogle] != "oc" {
		t.Errorf("keys = %v %v", keys, err)
	}
	// GEMINI_API_KEY wins over the alias; the keys file accepts the alias.
	env["GEMINI_API_KEY"] = "g-main"
	if keys, _ := LoadKeys("", false, func(k string) string { return env[k] }); keys[ProviderGemini] != "g-main" {
		t.Errorf("gemini main = %v", keys)
	}
	path := filepath.Join(t.TempDir(), "keys.env")
	if err := os.WriteFile(path, []byte("GEMENI_API_KEY=g-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if keys, _ := LoadKeys(path, true, func(string) string { return "" }); keys[ProviderGemini] != "g-file" {
		t.Errorf("file alias = %v", keys)
	}
}

// sentinel is the "real" key of the tests: it must never leave the process
// except in the Authorization/x-api-key header sent to the fake provider.
const sentinel = "sk-SENTINEL-7d1f0c9e4b"

// mutation edits the answer the fake provider forwards from wwz.
type mutation func(probe string, status *int, h http.Header, body []byte) []byte

// fakeReal is a fake provider: it forwards every request to an embedded
// wwz (swapping the sentinel key for the wwz key) and applies a mutation,
// so that without mutation its answers have exactly the wwz shape.
type fakeReal struct {
	t      *testing.T
	target string // wwz base + driver prefix
	mutate mutation

	mu     sync.Mutex
	seen   []http.Header
	bodies [][]byte
	paths  []string // path and query of each request
}

func probeOf(r *http.Request, body []byte) string {
	switch {
	case r.Method == http.MethodGet:
		return ProbeModels
	case bytes.Contains(body, []byte(MissingModel)), strings.Contains(r.URL.Path, MissingModel):
		return ProbeErrorModel
	case strings.Contains(r.Header.Get("Authorization")+r.Header.Get("x-api-key")+r.Header.Get("x-goog-api-key")+r.Header.Get("api-key"), InvalidKey):
		return ProbeErrorAuth
	case strings.HasSuffix(r.URL.Path, "/responses") && bytes.Contains(body, []byte(`"stream":true`)):
		return ProbeResponsesStream
	case strings.HasSuffix(r.URL.Path, "/responses"):
		return ProbeResponses
	case bytes.Contains(body, []byte(`"stream":true`)), strings.Contains(r.URL.Path, ":streamGenerateContent"):
		return ProbeChatStream
	}
	return ProbeChat
}

func (f *fakeReal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Header.Clone())
	f.bodies = append(f.bodies, body)
	f.paths = append(f.paths, r.URL.RequestURI())
	f.mu.Unlock()
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, f.target+r.URL.RequestURI(), bytes.NewReader(body))
	req.Header = r.Header.Clone()
	if req.Header.Get("Authorization") == "Bearer "+sentinel {
		req.Header.Set("Authorization", "Bearer "+DefaultWWZKey)
	}
	if req.Header.Get("x-api-key") == sentinel {
		req.Header.Set("x-api-key", DefaultWWZKey)
	}
	if req.Header.Get("x-goog-api-key") == sentinel {
		req.Header.Set("x-goog-api-key", DefaultWWZKey)
	}
	if req.Header.Get("api-key") == sentinel {
		req.Header.Set("api-key", DefaultWWZKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Errorf("fake: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	status := resp.StatusCode
	h := resp.Header.Clone()
	h.Del("Content-Length")
	if f.mutate != nil {
		out = f.mutate(probeOf(r, body), &status, h, out)
	}
	for k, v := range h {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func startEmbedded(t *testing.T) *EmbeddedWWZ {
	t.Helper()
	w, err := StartWWZ(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// supportedProbes is how many probes a provider answers: every probe but
// the responses ones on the providers without a Responses route.
func supportedProbes(name string) int {
	if providers[name].ResponsesPath == "" {
		return len(ProbeNames) - 2
	}
	return len(ProbeNames)
}

// fakes starts one fake provider per name, all forwarding to wwz.
func fakes(t *testing.T, wwz *EmbeddedWWZ, muts map[string]mutation) (map[string]string, map[string]*fakeReal) {
	t.Helper()
	urls, fs := map[string]string{}, map[string]*fakeReal{}
	for _, name := range ProviderNames {
		f := &fakeReal{t: t, target: wwz.URL + providers[name].WWZPrefix, mutate: muts[name]}
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		urls[name], fs[name] = srv.URL, f
	}
	return urls, fs
}

func allKeys(k string) map[string]string {
	m := map[string]string{}
	for _, n := range ProviderNames {
		m[n] = k
	}
	return m
}

func render(t *testing.T, rep *Report) (string, string) {
	t.Helper()
	var js, txt bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteText(&txt); err != nil {
		t.Fatal(err)
	}
	return js.String(), txt.String()
}

func providerReport(t *testing.T, rep *Report, name string) ProviderReport {
	t.Helper()
	for _, p := range rep.Providers {
		if p.Provider == name {
			return p
		}
	}
	t.Fatalf("provider %s missing", name)
	return ProviderReport{}
}

func TestRunNoDivergence(t *testing.T) {
	t.Chdir(t.TempDir())
	upstream := startEmbedded(t)
	urls, fs := fakes(t, upstream, map[string]mutation{
		// The real provider echoes the key in values and headers: shape
		// unchanged, but the key must never reach any output.
		ProviderOpenAI: func(probe string, _ *int, h http.Header, body []byte) []byte {
			h.Set("X-Echo", sentinel)
			if probe == ProbeErrorAuth {
				return bytes.Replace(body, []byte("Incorrect API key provided: sk-inval********rift."), []byte("Incorrect API key provided: "+sentinel+"."), 1)
			}
			return body
		},
	})
	t0 := time.Date(2026, 10, 31, 6, 0, 0, 500, time.UTC)
	rep, err := Run(context.Background(), Options{
		Providers: ProviderNames, Keys: allKeys(sentinel), BaseURLs: urls, SaveRaw: "tmp/raw", ToolVersion: "v-test",
		Now: func() time.Time { return t0 }, Session: "ses_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	js, txt := render(t, rep)
	if code := ExitCode(rep, err); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, txt)
	}
	if rep.Version != 1 || rep.Kind != "drift" || rep.ToolVersion != "v-test" || rep.WWZ != (WWZInfo{URL: InProcessURL, Version: "v-test"}) ||
		!rep.Started.Equal(t0.Truncate(time.Second)) || rep.Summary != (Summary{OK: 7}) {
		t.Fatalf("report header: %+v", rep)
	}
	for _, p := range rep.Providers {
		if p.Status != StatusOK || len(p.Probes) != len(ProbeNames) {
			t.Errorf("%s: %+v", p.Provider, p)
		}
	}
	for _, name := range ProviderNames {
		// Fixed and priced model, the same on both sides.
		p := providerReport(t, rep, name)
		if p.Model != providers[name].DefaultModel || p.SpendUSD <= 0 || p.SpendUSD >= UnknownCallUSD {
			t.Errorf("%s: model %s spend %v", name, p.Model, p.SpendUSD)
		}
		for i, b := range fs[name].bodies {
			path := fs[name].paths[i]
			if len(b) > 0 && !bytes.Contains(b, []byte(`"model":"`+p.Model+`"`)) && !bytes.Contains(b, []byte(MissingModel)) &&
				!strings.Contains(path, "/models/"+p.Model+":") && !strings.Contains(path, MissingModel) {
				t.Errorf("%s: real request with another model: %s %s", name, path, b)
			}
		}
	}
	// Keys and session reached the fake providers only in headers.
	if h := fs[ProviderAnthropic].seen[0]; h.Get("x-api-key") != sentinel || h.Get("anthropic-version") != AnthropicVersion {
		t.Errorf("anthropic headers: %v", h)
	}
	if h := fs[ProviderOpenCode].seen[0]; h.Get("Authorization") != "Bearer "+sentinel || h.Get("User-Agent") != OpenCodeUserAgent || h.Get("x-opencode-session") != "ses_test" {
		t.Errorf("opencode headers: %v", h)
	}
	if h := fs[ProviderOpenAI].seen[0]; h.Get("User-Agent") != "wwz-drift/v-test" {
		t.Errorf("openai user agent: %v", h)
	}
	if h := fs[ProviderGemini].seen[0]; h.Get("x-goog-api-key") != sentinel || h.Get("Authorization") != "" {
		t.Errorf("gemini headers: %v", h)
	}
	if !slices.Contains(fs[ProviderGemini].paths, "/v1beta/models/gemini-3.5-flash-lite:streamGenerateContent?alt=sse") ||
		!slices.Contains(fs[ProviderGemini].paths, "/v1beta/models?pageSize=1000") {
		t.Errorf("gemini paths: %v", fs[ProviderGemini].paths)
	}
	if h := fs[ProviderAzure].seen[0]; h.Get("api-key") != sentinel || h.Get("Authorization") != "" {
		t.Errorf("azure headers: %v", h)
	}
	if !slices.Contains(fs[ProviderAzure].paths, "/openai/deployments/gpt-4o-mini/chat/completions?api-version="+AzureAPIVersion) ||
		!slices.Contains(fs[ProviderAzure].paths, "/openai/models?api-version="+AzureAPIVersion) {
		t.Errorf("azure paths: %v", fs[ProviderAzure].paths)
	}
	if h := fs[ProviderOpenCodeGoogle].seen[1]; h.Get("x-goog-api-key") != sentinel || h.Get("x-opencode-session") != "ses_test" ||
		!slices.Contains(fs[ProviderOpenCodeGoogle].paths, "/v1beta/models/deepseek-v4.1-flash:generateContent") {
		t.Errorf("opencode-google: %v %v", h, fs[ProviderOpenCodeGoogle].paths)
	}
	// Spend was recorded for the paid probes.
	b, err := ReadBudget(DefaultBudgetFile)
	// chat, chat_stream, responses and responses_stream on openai and
	// opencode; only the two chat probes on anthropic (no Responses route).
	if err != nil || b["openai"].Calls != 4 || b["opencode"].Calls != 4 || b["anthropic"].Calls != 2 || b["openai"].USD <= 0 {
		t.Errorf("budget = %+v, %v", b, err)
	}
	// The responses probes go to each provider's Responses route and are
	// not_supported where there is none (task 239).
	for name, want := range map[string]string{
		ProviderOpenAI: "/v1/responses", ProviderOpenRouter: "/v1/responses",
		ProviderOpenCode: "/openai/v1/responses", ProviderAzure: "/openai/v1/responses",
	} {
		if !slices.Contains(fs[name].paths, want) {
			t.Errorf("%s: no %s in %v", name, want, fs[name].paths)
		}
	}
	for _, name := range []string{ProviderAnthropic, ProviderGemini, ProviderOpenCodeGoogle} {
		for _, pr := range providerReport(t, rep, name).Probes {
			if (pr.Probe == ProbeResponses || pr.Probe == ProbeResponsesStream) && (pr.Status != StatusSkipped || pr.Reason != ReasonNotSupported) {
				t.Errorf("%s/%s: %s/%s, want skipped/not_supported", name, pr.Probe, pr.Status, pr.Reason)
			}
		}
	}
	// The sentinel never appears in the report, the text or the raw files.
	outputs := js + txt
	raws, _ := filepath.Glob("tmp/raw/*.txt")
	want := 0
	for _, name := range ProviderNames {
		want += 2 * supportedProbes(name)
	}
	if len(raws) != want {
		t.Errorf("raw files: %d", len(raws))
	}
	for _, f := range raws {
		data, _ := os.ReadFile(f)
		outputs += string(data)
	}
	if strings.Contains(outputs, sentinel) {
		t.Fatal("sentinel key leaked")
	}
	raw, _ := os.ReadFile("tmp/raw/openai-error_auth-real.txt")
	if !strings.Contains(string(raw), "Incorrect API key provided: ***") || !strings.Contains(string(raw), "X-Echo: ***") || strings.Contains(string(raw), "\nAuthorization:") {
		t.Errorf("raw answer: %s", raw)
	}
}

var createdRe = regexp.MustCompile(`"created": ?(\d+)`)

// reverseTop rewrites a JSON object with its top-level keys in reverse
// alphabetical order (wwz writes them sorted).
func reverseTop(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		b.Write(kj)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func TestRunDetectsEveryKind(t *testing.T) {
	upstream := startEmbedded(t)
	urls, _ := fakes(t, upstream, map[string]mutation{
		ProviderOpenAI: func(probe string, status *int, h http.Header, body []byte) []byte {
			switch probe {
			case ProbeModels:
				h.Set("X-Openai-Proxy-Wasm", "v0.1") // infrastructure: never compared
				h.Set("Openai-Organization", "org")  // ignored by --ignore below
				return bytes.Replace(body, []byte(`"object": "list"`), []byte(`"object": "list", "has_more": false`), 1)
			case ProbeChat:
				h.Set("Openai-Model", "gpt-4o-mini") // a header wwz does not send
				body = createdRe.ReplaceAll(body, []byte(`"created":"$1"`))
				return bytes.Replace(body, []byte(`"service_tier": "default",`), nil, 1)
			case ProbeChatStream:
				return bytes.Replace(body, []byte("data: [DONE]"), []byte("event: ping\ndata: {}\n\ndata: [DONE]"), 1)
			case ProbeErrorAuth:
				*status = http.StatusForbidden
			}
			return body
		},
		ProviderAnthropic: func(probe string, _ *int, h http.Header, body []byte) []byte {
			switch probe {
			case ProbeModels:
				return reverseTop(body)
			case ProbeChat:
				h.Del("Request-Id")
			}
			return body
		},
		ProviderOpenCode: func(probe string, _ *int, _ http.Header, body []byte) []byte {
			if probe == ProbeChat {
				return bytes.Replace(body, []byte(`{"id"`), []byte(`{"extra_oc":1,"id"`), 1)
			}
			return body
		},
	})
	budget := filepath.Join(t.TempDir(), "spend.json")
	rep, err := Run(context.Background(), Options{
		Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: budget, Timeout: 10 * time.Second,
		Ignores: []Ignore{
			{Provider: ProviderOpenCode, Probe: ProbeChat, Kind: KindMissingField, Path: "extra_oc", Reason: "campo aceito"},
			{Provider: ProviderOpenAI, Kind: KindMissingHeader, Path: "openai-organization", Reason: "header aceito"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	js, txt := render(t, rep)
	if code := ExitCode(rep, err); code != ExitDrift {
		t.Fatalf("exit %d\n%s", code, txt)
	}
	found := map[string]bool{}
	ignored := map[string]bool{}
	for _, p := range rep.Providers {
		for _, pr := range p.Probes {
			for _, d := range pr.Divergences {
				if d.Ignored {
					ignored[d.Path] = true
				} else {
					found[d.Kind] = true
				}
			}
		}
	}
	for _, k := range Kinds {
		if !found[k] {
			t.Errorf("kind %s not detected\n%s", k, txt)
		}
	}
	if !ignored["openai-organization"] || !ignored["extra_oc"] {
		t.Errorf("ignored: %v", ignored)
	}
	if rep.Summary != (Summary{OK: 4, Drift: 2, Skipped: 1}) || providerReport(t, rep, ProviderOpenCode).Status != StatusOK {
		t.Errorf("summary %+v\n%s", rep.Summary, txt)
	}
	for _, want := range []string{`"kind": "missing_field"`, `"path": "has_more"`, `"ignored": true`, `"real_status": 403`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %s", want)
		}
	}
	if !strings.Contains(txt, "[ignorada]") || !strings.Contains(txt, "resumo: ok=4 drift=2 skipped=1 error=0") {
		t.Errorf("text:\n%s", txt)
	}
}

func TestRunSkips(t *testing.T) {
	t.Chdir(t.TempDir())
	upstream := startEmbedded(t)
	urls, _ := fakes(t, upstream, nil)
	if err := os.MkdirAll("tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DefaultBudgetFile, []byte(`{"anthropic":{"usd":20,"calls":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Options{
		Providers: []string{ProviderOpenAI, ProviderAnthropic, ProviderOpenRouter},
		Probes:    []string{ProbeModels, ProbeErrorAuth},
		Keys:      map[string]string{ProviderOpenAI: sentinel, ProviderAnthropic: sentinel, ProviderOpenCode: sentinel},
		BaseURLs:  urls, WWZURL: upstream.URL + "/", WWZVersion: "v9",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, txt := render(t, rep)
	if ExitCode(rep, err) != ExitOK || rep.WWZ != (WWZInfo{URL: upstream.URL, Version: "v9"}) {
		t.Fatalf("exit %d %+v\n%s", ExitCode(rep, err), rep.WWZ, txt)
	}
	want := map[string]string{
		ProviderOpenAI:         StatusOK + "/",
		ProviderAnthropic:      StatusSkipped + "/" + ReasonBudgetExhausted,
		ProviderOpenRouter:     StatusSkipped + "/" + ReasonNoKey,
		ProviderOpenCode:       StatusSkipped + "/" + ReasonNotSelected,
		ProviderGemini:         StatusSkipped + "/" + ReasonNotSelected,
		ProviderOpenCodeGoogle: StatusSkipped + "/" + ReasonNotSelected,
		ProviderAzure:          StatusSkipped + "/" + ReasonNotSelected,
	}
	for _, p := range rep.Providers {
		if got := p.Status + "/" + p.Reason; got != want[p.Provider] {
			t.Errorf("%s = %s, want %s", p.Provider, got, want[p.Provider])
		}
	}
	oa := providerReport(t, rep, ProviderOpenAI)
	var skippedProbes []string
	for _, pr := range oa.Probes {
		if pr.Status == StatusSkipped && pr.Reason == ReasonNotSelected {
			skippedProbes = append(skippedProbes, pr.Probe)
		}
	}
	if !slices.Equal(skippedProbes, []string{ProbeChat, ProbeChatStream, ProbeErrorModel, ProbeResponses, ProbeResponsesStream}) || oa.SpendUSD != 0 {
		t.Errorf("openai probes: %+v", oa)
	}
	if rep.Summary != (Summary{OK: 1, Skipped: 6}) || !strings.Contains(txt, "(budget_exhausted)") || !strings.Contains(txt, "(not_selected)") {
		t.Errorf("summary %+v\n%s", rep.Summary, txt)
	}
}

func TestRunBudgetFailures(t *testing.T) {
	upstream := startEmbedded(t)
	urls, _ := fakes(t, upstream, nil)
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	rep, err := Run(context.Background(), Options{Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: bad, WWZURL: upstream.URL})
	if err == nil || ExitCode(rep, err) != ExitError || rep.Providers[0].Status != StatusError || len(rep.Providers) != 1 {
		t.Fatalf("unreadable budget: %v %+v", err, rep)
	}

	// The budget is readable but the rewrite fails: the probe result is
	// kept and the run stops with an execution failure.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked+"."+strconv.Itoa(os.Getpid())+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	rep, err = Run(context.Background(), Options{Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: blocked, WWZURL: upstream.URL})
	if err == nil || ExitCode(rep, err) != ExitError {
		t.Fatalf("unwritable budget: %v", err)
	}
	oa := rep.Providers[0]
	if len(oa.Probes) != 2 || oa.Probes[1].Probe != ProbeChat || oa.Status != StatusOK || oa.SpendUSD <= 0 {
		t.Errorf("openai: %+v", oa)
	}
}

// closedURL returns the URL of a server that is no longer listening.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestRunWWZFailures(t *testing.T) {
	ctx := context.Background()
	rep, err := Run(ctx, Options{WWZURL: closedURL(t) + "/" + sentinel, Keys: allKeys(sentinel)})
	if err == nil || ExitCode(rep, err) != ExitError || !strings.Contains(err.Error(), "wwz inalcançável") || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("unreachable wwz: %v", err)
	}
	if !strings.Contains(rep.WWZ.URL, Mask) {
		t.Errorf("wwz url not masked: %s", rep.WWZ.URL)
	}

	orig := startWWZ
	t.Cleanup(func() { startWWZ = orig })
	startWWZ = func(map[string]string) (*EmbeddedWWZ, error) { return nil, errors.New("boom") }
	rep, err = Run(ctx, Options{})
	if err == nil || !strings.Contains(err.Error(), "wwz embutido: boom") || ExitCode(rep, err) != ExitError {
		t.Fatalf("embedded failure: %v", err)
	}
}

func TestStartWWZErrors(t *testing.T) {
	if _, err := StartWWZ([]byte("server: [")); err == nil {
		t.Error("invalid YAML accepted")
	}
	if _, err := StartWWZ([]byte("drivers: [{name: nope}]\n")); err == nil {
		t.Error("unknown driver accepted")
	}
	orig := listenTCP
	t.Cleanup(func() { listenTCP = orig })
	listenTCP = func(string, string) (net.Listener, error) { return nil, errors.New("no port") }
	if _, err := StartWWZ(nil); err == nil {
		t.Error("listen failure ignored")
	}
}

// hijackClose answers "/" normally and drops the connection otherwise, or
// after a truncated body when truncate is set.
func hijackClose(truncate bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		conn, buf, _ := w.(http.Hijacker).Hijack()
		if truncate {
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"a\"")
			_ = buf.Flush()
		}
		_ = conn.Close()
	}
}

func TestRunProbeErrors(t *testing.T) {
	upstream := startEmbedded(t)
	ctx := context.Background()
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	}))
	t.Cleanup(html.Close)
	trunc := httptest.NewServer(hijackClose(true))
	t.Cleanup(trunc.Close)
	drop := httptest.NewServer(hijackClose(false))
	t.Cleanup(drop.Close)
	urls, _ := fakes(t, upstream, nil)

	cases := []struct {
		name   string
		base   string
		wwz    string
		reason string
	}{
		{"real unreachable", closedURL(t) + "/" + sentinel, upstream.URL, "real: "},
		{"real truncated", trunc.URL, upstream.URL, "real: unexpected EOF"},
		{"real not JSON", html.URL, upstream.URL, "real: resposta ilegível"},
		{"wwz drops", urls[ProviderOpenAI], drop.URL, "wwz: "},
		{"wwz not JSON", urls[ProviderOpenAI], html.URL, "wwz: resposta ilegível"},
	}
	for _, c := range cases {
		rep, err := Run(ctx, Options{
			Providers: []string{ProviderOpenAI}, Probes: []string{ProbeModels},
			Keys: map[string]string{ProviderOpenAI: sentinel}, BaseURLs: map[string]string{ProviderOpenAI: c.base},
			WWZURL: c.wwz, BudgetFile: filepath.Join(t.TempDir(), "b.json"),
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		js, txt := render(t, rep)
		pr := rep.Providers[0].Probes[0]
		if ExitCode(rep, err) != ExitError || pr.Status != StatusError || !strings.HasPrefix(pr.Reason, c.reason) || rep.Summary.Error != 1 {
			t.Errorf("%s: exit %d %+v", c.name, ExitCode(rep, err), pr)
		}
		if strings.Contains(js+txt, sentinel) {
			t.Errorf("%s: sentinel leaked", c.name)
		}
	}
}

func TestRunSaveRawErrors(t *testing.T) {
	upstream := startEmbedded(t)
	urls, _ := fakes(t, upstream, nil)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "tmp"), nil, 0o600) // tmp is a file
	blocked := filepath.Join(dir, "x", "tmp")
	_ = os.MkdirAll(filepath.Join(blocked, "openai-models-real.txt"), 0o750)
	_ = os.MkdirAll(filepath.Join(blocked, "openai-models-wwz.txt"), 0o750)
	for _, raw := range []string{filepath.Join(dir, "tmp", "raw"), blocked} {
		rep, err := Run(context.Background(), Options{
			Providers: []string{ProviderOpenAI}, Probes: []string{ProbeModels},
			Keys: map[string]string{ProviderOpenAI: sentinel}, BaseURLs: urls, WWZURL: upstream.URL,
			BudgetFile: filepath.Join(dir, "b.json"), SaveRaw: raw,
		})
		if err != nil || !strings.HasPrefix(rep.Providers[0].Probes[0].Reason, "save-raw: ") {
			t.Errorf("%s: %v %+v", raw, err, rep.Providers[0])
		}
	}
}

// TestRunModelOverride: --model changes the model on both sides, and the
// embedded wwz serves it (a new id copies the provider's model; a known id
// gains the protocol).
func TestRunModelOverride(t *testing.T) {
	models := map[string]string{ProviderOpenCode: "kimi-k2", ProviderOpenAI: "openai/gpt-4.1-nano"}
	upstream, err := startWith(nil, models)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	urls, fs := fakes(t, upstream, nil)
	rep, err := Run(context.Background(), Options{
		Providers: []string{ProviderOpenAI, ProviderOpenCode}, Probes: []string{ProbeModels, ProbeChat},
		Keys: allKeys(sentinel), BaseURLs: urls, Models: models, BudgetFile: filepath.Join(t.TempDir(), "b.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, txt := render(t, rep)
	for p, m := range models {
		pr := providerReport(t, rep, p)
		if pr.Status != StatusOK || pr.Model != m || !bytes.Contains(fs[p].bodies[1], []byte(`"model":"`+m+`"`)) {
			t.Errorf("%s: %+v\n%s", p, pr, txt)
		}
	}
}

func TestAddModels(t *testing.T) {
	c, err := config.Parse(WWZConfig)
	if err != nil {
		t.Fatal(err)
	}
	n := len(c.Models)
	addModels(c, map[string]string{
		ProviderOpenAI:     "gpt-4o-mini",      // already served
		ProviderAnthropic:  "claude-haiku-4-5", // alias, already served
		ProviderOpenRouter: "gpt-4o-mini",      // known id, gains the protocol
		ProviderOpenCode:   "kimi-k2",          // new id, copy of the opencode model
	})
	if len(c.Models) != n+1 || !c.FindModel("gpt-4o-mini").ModelSupports(ProviderOpenRouter) {
		t.Fatalf("models: %+v", c.Models)
	}
	k := c.FindModel("kimi-k2")
	if k == nil || k.DisplayName != "kimi-k2" || !slices.Equal(k.Drivers, []string{ProviderOpenCode}) || k.Capabilities.Reasoning == nil {
		t.Errorf("kimi-k2: %+v", k)
	}
	// A configuration without a model for the provider is left alone.
	c, err = config.Parse([]byte("models: [{id: m, protocols: [openai], drivers: [openai]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	addModels(c, map[string]string{ProviderOpenCode: "x"})
	if len(c.Models) != 1 {
		t.Errorf("models: %+v", c.Models)
	}
}

func TestOptionsValidate(t *testing.T) {
	cases := map[string]Options{
		"provedor \"bedrock\"":          {Providers: []string{"bedrock"}},
		"sonda \"nope\"":                {Probes: []string{"nope"}},
		"provedor \"vertex\"":           {Models: map[string]string{"vertex": "m"}},
		"provedor \"x\"":                {Keys: map[string]string{"x": "k"}},
		"--base-url openai":             {BaseURLs: map[string]string{"openai": "ftp://x"}},
		"--wwz":                         {WWZURL: "://"},
		"--budget-usd negativo":         {BudgetUSD: -1},
		"--timeout negativo":            {Timeout: -1},
		"precisa estar sob tmp/":        {SaveRaw: "raw"},
		"ignore[0]: reason é obrigatór": {Ignores: []Ignore{{Path: "x"}}},
	}
	for want, o := range cases {
		rep, err := Run(context.Background(), o)
		if rep != nil || !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), want) || ExitCode(rep, err) != ExitUsage {
			t.Errorf("%s: %v", want, err)
		}
	}
	for _, d := range []string{"tmp", "tmp/raw", "./tmp/x/y", "/a/tmp/b"} {
		if !UnderTmp(d) {
			t.Errorf("UnderTmp(%q) = false", d)
		}
	}
	for _, d := range []string{"raw", "/a/tmpx/b", "../tmpdir"} {
		if UnderTmp(d) {
			t.Errorf("UnderTmp(%q) = true", d)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestReportOutput(t *testing.T) {
	var zero Report
	if err := zero.WriteJSON(failWriter{}); err == nil {
		t.Error("WriteJSON ignored the writer error")
	}
	if err := zero.WriteText(failWriter{}); err == nil {
		t.Error("WriteText ignored the writer error")
	}
	rep := &Report{
		ToolVersion: "v1", WWZ: WWZInfo{URL: "http://wwz"},
		Providers: []ProviderReport{
			{Provider: "openai", Status: StatusError, Model: "m", Probes: []ProbeReport{
				{Probe: ProbeChat, Status: StatusError, Reason: "real: boom"},
				{Probe: ProbeModels, Status: StatusOK, RealStatus: 200, WWZStatus: 200},
			}},
		},
	}
	rep.summarize()
	var b bytes.Buffer
	_ = rep.WriteText(&b)
	if s := b.String(); !strings.HasPrefix(s, "wwz-drift v1: wwz http://wwz\n") || !strings.Contains(s, "chat: error (real: boom)\n") ||
		!strings.Contains(s, "models: ok real=200 wwz=200") || rep.Summary.Error != 1 {
		t.Errorf("text:\n%s", s)
	}
	if rollup([]ProbeReport{{Status: StatusSkipped}}) != StatusOK || rollup([]ProbeReport{{Status: StatusError}, {Status: StatusDrift}}) != StatusDrift {
		t.Error("rollup")
	}
	for _, c := range []struct {
		rep  *Report
		err  error
		want int
	}{
		{nil, nil, ExitOK},
		{&Report{Summary: Summary{OK: 1}}, nil, ExitOK},
		{&Report{Summary: Summary{Error: 1}}, nil, ExitError},
		{&Report{Summary: Summary{Drift: 1, Error: 1}}, errors.New("x"), ExitDrift},
		{nil, errors.New("x"), ExitError},
		{nil, usage("x"), ExitUsage},
	} {
		if got := ExitCode(c.rep, c.err); got != c.want {
			t.Errorf("ExitCode(%+v, %v) = %d, want %d", c.rep, c.err, got, c.want)
		}
	}
}
