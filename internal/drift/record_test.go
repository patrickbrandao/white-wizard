package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFixture(t *testing.T, path string) (Fixture, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx Fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return fx, string(raw)
}

// TestRecordFakeProviders records every provider against fake providers
// (forwarding to the embedded wwz, with real-like headers added) and
// checks the files: the layout and format of testdata/real, markers for
// the volatile values and no key anywhere.
func TestRecordFakeProviders(t *testing.T) {
	wwz := startEmbedded(t)
	addHeaders := func(_ string, _ *int, h http.Header, body []byte) []byte {
		h.Set("Date", "Fri, 31 Oct 2026 12:00:00 GMT")
		h.Set("Cf-Ray", "abc-GRU")
		h.Set("Server-Timing", "gfet4t7; dur=123")
		h.Set("X-Ratelimit-Reset-Tokens", "6ms")
		h.Set("Openai-Processing-Ms", "42")
		h.Set("Anthropic-Ratelimit-Tokens-Reset", "2026-10-31T12:00:01Z")
		h.Set("X-Echo", sentinel)
		return body
	}
	muts := map[string]mutation{}
	for _, n := range ProviderNames {
		muts[n] = addHeaders
	}
	urls, fs := fakes(t, wwz, muts)
	dir := filepath.Join(t.TempDir(), "real")
	budget := filepath.Join(t.TempDir(), "spend.json")
	clock := func() time.Time { return time.Date(2026, 10, 31, 23, 30, 0, 0, time.FixedZone("x", -3*3600)) }
	rep, err := Record(context.Background(), Options{
		Providers: ProviderNames, Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: budget,
		Now: clock, Session: "ses_" + strings.Repeat("ab", 32),
	}, dir)
	if err != nil || ExitCode(rep, err) != ExitOK || rep.Kind != RecordKind || rep.Dir != dir {
		t.Fatalf("Record: %v %+v", err, rep)
	}
	for _, name := range ProviderNames {
		pr := providerReport(t, rep, name)
		n := 0
		for _, p := range pr.Probes {
			if p.Status == StatusOK {
				n++
				if !strings.HasPrefix(p.File, dir) || p.RealStatus == 0 {
					t.Errorf("%s/%s: %+v", name, p.Probe, p)
				}
			}
		}
		if pr.Status != StatusOK || n != supportedProbes(name) {
			t.Errorf("%s: %s, %d recorded", name, pr.Status, n)
		}
		if pr.SpendUSD <= 0 {
			t.Errorf("%s: no spend", name)
		}
		if f := fs[name]; len(f.paths) != supportedProbes(name) {
			t.Errorf("%s: %d real calls", name, len(f.paths))
		}
	}
	spend, err := ReadBudget(budget)
	if err != nil || spend[ProviderOpenAI].Calls != 4 || spend[ProviderAnthropic].Calls != 2 {
		t.Errorf("budget: %+v %v", spend, err)
	}

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		if bytes.Contains(raw, []byte(sentinel)) || !bytes.HasSuffix(raw, []byte("}\n")) || bytes.Contains(raw, []byte(`\u003c`)) {
			t.Errorf("%s: key, missing newline or escaped marker", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	fx, raw := readFixture(t, filepath.Join(dir, "anthropic", "shape", "chat.json"))
	if fx.Provider != "anthropic" || fx.Name != "chat" || fx.RecordedAt != "2026-11-01" || fx.Request.Method != "POST" ||
		fx.Request.Path != "/v1/messages" || fx.Request.Headers["x-api-key"] != "<key>" || fx.Request.Headers["anthropic-version"] != AnthropicVersion ||
		fx.Request.Body == nil || !strings.Contains(*fx.Request.Body, ProbePrompt) || fx.Response.Status != 200 {
		t.Errorf("anthropic chat: %s", raw)
	}
	h := fx.Response.Headers
	if h["request-id"] != "<request_id>" || h["anthropic-ratelimit-tokens-reset"] != "<rfc3339>" || h["anthropic-organization-id"] != "<organization_id>" ||
		h["anthropic-workspace-id"] != "<workspace_id>" || h["content-type"] != "application/json" {
		t.Errorf("anthropic headers: %v", h)
	}
	for _, k := range []string{"date", "cf-ray", "server-timing", "x-echo", "content-length", "user-agent"} {
		if _, ok := h[k]; ok {
			t.Errorf("anthropic: header %s kept", k)
		}
	}
	if !strings.Contains(fx.Response.Body, `"id":"<msg_id>"`) {
		t.Errorf("anthropic body: %s", fx.Response.Body)
	}
	// Order of the fixture keys: the format of spec 018.
	order := []string{`"provider"`, `"name"`, `"recorded_at"`, `"note"`, `"request"`, `"method"`, `"path"`, `"headers"`, `"body"`, `"response"`, `"status"`}
	last := -1
	for _, k := range order {
		i := strings.Index(raw[last+1:], k)
		if i < 0 {
			t.Fatalf("anthropic chat: %s out of order in %s", k, raw)
		}
		last += i + 1
	}

	fx, _ = readFixture(t, filepath.Join(dir, "anthropic", "shape", "error_auth.json"))
	if fx.Request.Headers["x-api-key"] != InvalidKey || fx.Response.Status != 401 {
		t.Errorf("error_auth: %+v", fx)
	}
	fx, raw = readFixture(t, filepath.Join(dir, "anthropic", "shape", "models.json"))
	if fx.Request.Method != "GET" || fx.Request.Body != nil || !strings.Contains(raw, `"body": null`) || !strings.Contains(fx.Response.Body, "claude-haiku-4-5-20251001") {
		t.Errorf("models: %s", raw)
	}

	fx, _ = readFixture(t, filepath.Join(dir, "openai", "shape", "chat_stream.json"))
	h = fx.Response.Headers
	if h["x-request-id"] != "<request_id>" || h["x-ratelimit-reset-tokens"] != "<duration>" || h["openai-processing-ms"] != "<ms>" ||
		h["openai-organization"] != "<organization_id>" || h["openai-project"] != "<project_id>" ||
		!strings.Contains(fx.Response.Body, `"id":"<id>"`) || fx.Request.Headers["authorization"] != "Bearer <key>" {
		t.Errorf("openai stream: %v %s", h, fx.Response.Body)
	}
	fx, _ = readFixture(t, filepath.Join(dir, "openai", "shape", "responses.json"))
	if !strings.Contains(fx.Response.Body, `"id": "<id>"`) && !strings.Contains(fx.Response.Body, `"id":"<id>"`) {
		t.Errorf("openai responses: %s", fx.Response.Body)
	}

	fx, _ = readFixture(t, filepath.Join(dir, "openrouter", "shape", "chat.json"))
	if fx.Request.Path != "/v1/chat/completions" || !strings.Contains(fx.Response.Body, `"id":"<generation_id>"`) {
		t.Errorf("openrouter: %s %s", fx.Request.Path, fx.Response.Body)
	}

	fx, _ = readFixture(t, filepath.Join(dir, "opencode", "shape", "chat.json"))
	if fx.Request.Headers["x-opencode-session"] != "<session>" || !strings.Contains(fx.Response.Body, `"id":"<gw_id>"`) {
		t.Errorf("opencode: %+v", fx)
	}
	fx, _ = readFixture(t, filepath.Join(dir, "opencode", "google", "chat.json"))
	if fx.Provider != ProviderOpenCode || fx.Request.Headers["x-goog-api-key"] != "<key>" || fx.Response.Headers["server-timing"] != "gfet4t7; dur=<ms>" {
		t.Errorf("opencode-google: %+v", fx)
	}
	fx, _ = readFixture(t, filepath.Join(dir, "gemini", "shape", "chat.json"))
	if fx.Response.Headers["server-timing"] != "gfet4t7; dur=<ms>" || !strings.Contains(fx.Response.Body, `"responseId": "<response_id>"`) {
		t.Errorf("gemini: %+v", fx)
	}
	fx, _ = readFixture(t, filepath.Join(dir, "azure", "shape", "chat.json"))
	if fx.Request.Headers["api-key"] != "<key>" || !strings.HasPrefix(fx.Request.Path, "/openai/deployments/gpt-4o-mini/") {
		t.Errorf("azure: %+v", fx)
	}

	_, txt := render(t, rep)
	if !strings.Contains(txt, "record em "+dir) || !strings.Contains(txt, "real=200 → "+filepath.Join(dir, "openai", "shape", "chat.json")) || strings.Contains(txt, sentinel) {
		t.Errorf("text:\n%s", txt)
	}
}

// TestRecordSkipsAndErrors: the gates of check, the selection, the
// unsupported probes and the failures of a probe and of the budget.
func TestRecordSkipsAndErrors(t *testing.T) {
	ctx := context.Background()
	if rep, err := Record(ctx, Options{Providers: []string{"bedrock"}}, ""); rep != nil || ExitCode(rep, err) != ExitUsage {
		t.Errorf("invalid provider: %v %v", rep, err)
	}

	// No --real: nothing is called; default dir.
	rep, err := Record(ctx, Options{Keys: allKeys(sentinel), BudgetFile: filepath.Join(t.TempDir(), "s.json")}, "")
	if err != nil || rep.Dir != DefaultRecordDir || providerReport(t, rep, ProviderOpenAI).Reason != ReasonRealNotConfirmed ||
		providerReport(t, rep, ProviderAzure).Reason != ReasonNotSelected {
		t.Errorf("not confirmed: %v %+v", err, rep)
	}

	// A provider that drops the connection: probe in error, exit 1; only
	// the selected probes; anthropic has no Responses route.
	drop := serveURL(t, hijackClose(false))
	rep, err = Record(ctx, Options{
		Providers: []string{ProviderAnthropic}, Probes: []string{ProbeChat, ProbeResponses},
		Keys: allKeys(sentinel), BaseURLs: map[string]string{ProviderAnthropic: drop}, BudgetFile: filepath.Join(t.TempDir(), "s.json"),
	}, t.TempDir())
	an := providerReport(t, rep, ProviderAnthropic)
	if err != nil || ExitCode(rep, err) != ExitError || an.Status != StatusError || an.Probes[0].Reason != ReasonNotSelected ||
		an.Probes[1].Status != StatusError || !strings.HasPrefix(an.Probes[1].Reason, "real: ") || an.Probes[5].Reason != ReasonNotSupported {
		t.Errorf("drop: %v %+v", err, an)
	}

	wwz := startEmbedded(t)
	urls, _ := fakes(t, wwz, nil)

	// The fixture directory is a file: the probe is in error, the spend
	// still counts.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	budget := filepath.Join(t.TempDir(), "s.json")
	rep, err = Record(ctx, Options{
		Providers: []string{ProviderOpenAI}, Probes: []string{ProbeChat}, Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: budget,
	}, file)
	oa := providerReport(t, rep, ProviderOpenAI)
	if err != nil || oa.Probes[1].Status != StatusError || !strings.HasPrefix(oa.Probes[1].Reason, "record: ") || oa.SpendUSD <= 0 {
		t.Errorf("unwritable dir: %v %+v", err, oa)
	}

	// The budget cannot be written after a paid probe: execution failure,
	// the next providers are not reported.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	rep, err = Record(ctx, Options{
		Providers: []string{ProviderOpenAI, ProviderAnthropic}, Probes: []string{ProbeChat}, Keys: allKeys(sentinel), BaseURLs: urls,
		BudgetFile: filepath.Join(ro, "s.json"),
	}, t.TempDir())
	if err == nil || ExitCode(rep, err) != ExitError || len(rep.Providers) != 1 || rep.Providers[0].Status != StatusOK {
		t.Errorf("budget write: %v %+v", err, rep)
	}

	// Budget exhausted and unreadable budget.
	if _, err := AddSpend(budget, ProviderOpenAI, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	rep, err = Record(ctx, Options{Providers: []string{ProviderOpenAI}, Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: budget}, t.TempDir())
	if err != nil || providerReport(t, rep, ProviderOpenAI).Reason != ReasonBudgetExhausted {
		t.Errorf("exhausted: %v %+v", err, rep)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	rep, err = Record(ctx, Options{Providers: []string{ProviderOpenAI}, Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: bad}, t.TempDir())
	if err == nil || ExitCode(rep, err) != ExitError || rep.Providers[0].Status != StatusError {
		t.Errorf("bad budget: %v %+v", err, rep)
	}
}

// serveURL starts a server for h and returns its URL.
func serveURL(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestNormalizeBody: the markers of the real fixtures (ADR 0014) from
// real-like bodies of each provider.
func TestNormalizeBody(t *testing.T) {
	an, oa, oc, gm := providers[ProviderAnthropic], providers[ProviderOpenAI], providers[ProviderOpenCode], providers[ProviderGemini]
	cases := []struct {
		p      Provider
		probe  string
		stream bool
		in     string
		want   string
	}{
		{
			an, ProbeChat, false, `{"id":"msg_01AbC","request_id":"req_011CX","content":[{"type":"tool_use","id":"toolu_01Xy"},{"signature":"EqQBCkgIAR=="}]}`,
			`{"id":"<msg_id>","request_id":"<request_id>","content":[{"type":"tool_use","id":"<tool_id>"},{"signature":"<signature>"}]}`,
		},
		{an, ProbeChatStream, true, "data: {\"type\":\"ping\"   }\n\ndata: {\"a\":1}\n\n", "data: {\"type\":\"ping\"}\n\ndata: {\"a\":1}\n\n"},
		{oa, ProbeChatStream, true, "data: {\"id\":\"chatcmpl-AbC1\",\"system_fingerprint\":\"fp_0ab\"}\n\n", "data: {\"id\":\"<id>\",\"system_fingerprint\":\"<fp>\"}\n\n"},
		{
			oa, ProbeResponses, false, "{\n  \"id\": \"resp_0a56\",\n  \"output\": [{\"id\": \"msg_0a56d48cab5f888c00\", \"call_id\": \"call_Ab1\"}, {\"item_id\": \"rs_0123456789abcdef01\"}]\n}",
			"{\n  \"id\": \"<id>\",\n  \"output\": [{\"id\": \"<item_id>\", \"call_id\": \"<call_id>\"}, {\"item_id\": \"<item_id>\"}]\n}",
		},
		{oc, ProbeChat, false, `{"id":"gw-1a2b","tool_calls":[{"id":"call_00_x"}]}`, `{"id":"<gw_id>","tool_calls":[{"id":"<call_id>"}]}`},
		{oc, ProbeChat, false, `{"id":"0123456789abcdef0123456789abcdef"}`, `{"id":"<gw_id>"}`},
		{providers[ProviderOpenRouter], ProbeChat, false, `{"id":"gen-1793454928-AbC","user_id":"user_2x"}`, `{"id":"<generation_id>","user_id":"<user_id>"}`},
		{
			gm, ProbeChat, false, "{\"thoughtSignature\": \"CiQB\",\n  \"responseId\": \"AbC\", \"createTime\": \"2026-10-31T12:00:00.1Z\"}",
			"{\"thoughtSignature\": \"<signature>\",\n  \"responseId\": \"<response_id>\", \"createTime\": \"<time>\"}",
		},
		// The model list keeps its ids.
		{oc, ProbeModels, false, `{"data":[{"id":"0123456789abcdef0123456789abcdef"},{"id":"gw-model"}]}`, `{"data":[{"id":"0123456789abcdef0123456789abcdef"},{"id":"gw-model"}]}`},
	}
	for i, c := range cases {
		if got := normalizeBody(c.p, c.probe, c.stream, []byte(c.in)); got != c.want {
			t.Errorf("%d:\n got  %s\n want %s", i, got, c.want)
		}
	}
}

func TestHeaders(t *testing.T) {
	gm, oa := providers[ProviderGemini], providers[ProviderOpenAI]
	h := http.Header{
		"Server": {"scaffolding on HTTPServer2"}, "Content-Length": {"0"}, "Request-Id": {"gen-1793454928-AbC"},
		"X-Frame-Options": {"SAMEORIGIN"}, "Access-Control-Allow-Origin": {"*"}, "Set-Cookie": {"a=b"}, "Vary": {"a", "b"},
	}
	got := responseHeaders(gm, h, true)
	want := map[string]string{"server": "scaffolding on HTTPServer2", "content-length": "0", "request-id": "<generation_id>", "x-frame-options": "SAMEORIGIN", "access-control-allow-origin": "*"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("gemini: %v", got)
	}
	got = responseHeaders(oa, h, false)
	if _, ok := got["server"]; ok || got["content-length"] != "" || len(got) != 2 {
		t.Errorf("openai: %v", got)
	}
	if v := headerValue("x-zen-model", "m"); v != "m" {
		t.Errorf("plain: %q", v)
	}
	if r := requestHeaders(providers[ProviderOpenCode], ProbeErrorAuth); r["authorization"] != "Bearer "+InvalidKey || r["x-opencode-session"] != "<session>" || r["user-agent"] != "" || r["accept"] != "" {
		t.Errorf("request: %v", r)
	}
	if basePath("https://openrouter.ai/api/") != "/api" || basePath("http://127.0.0.1:1") != "" {
		t.Error("basePath")
	}
}

func TestReduceModels(t *testing.T) {
	body := []byte(`{"data":[{"id":"a","x":1},{"id":"m","y":2}],"total_count":2,"links":{"next":null}}`)
	out, n, ok := reduceModels(body, "m")
	if !ok || n != 2 || string(out) != `{"data":[{"id":"m","y":2}],"total_count":1,"links":{"next":null}}` {
		t.Errorf("found: %s %d %v", out, n, ok)
	}
	out, _, ok = reduceModels(body, "z")
	if !ok || string(out) != `{"data":[],"total_count":0,"links":{"next":null}}` {
		t.Errorf("missing: %s", out)
	}
	if _, _, ok := reduceModels([]byte(`[1]`), "m"); ok {
		t.Error("not a list")
	}

	// A list above modelsFullMax is reduced and noted; a big body that is
	// not a list is kept whole.
	r := &runner{o: Options{Now: time.Now}}
	items := make([]string, 0, 2000)
	for i := range 2000 {
		items = append(items, fmt.Sprintf(`{"id":"model-%04d","description":"%s"}`, i, strings.Repeat("x", 40)))
	}
	items = append(items, `{"id":"m"}`)
	big := []byte(`{"data":[` + strings.Join(items, ",") + `],"total_count":2001}`)
	fx := r.fixture(providers[ProviderOpenRouter], ProbeModels, "m", request{method: http.MethodGet, path: "/v1/models"}, answer{status: 200, header: http.Header{}, body: big})
	if fx.Response.Body != `{"data":[{"id":"m"}],"total_count":1}` || !strings.Contains(fx.Note, "o real lista 2001") || fx.Request.Path != "/api/v1/models" {
		t.Errorf("big list: %s %q", fx.Response.Body, fx.Note)
	}
	notList := []byte(`"` + strings.Repeat("x", modelsFullMax) + `"`)
	fx = r.fixture(providers[ProviderOpenRouter], ProbeModels, "m", request{method: http.MethodGet}, answer{status: 200, header: http.Header{}, body: notList})
	if fx.Response.Body != string(notList) || strings.Contains(fx.Note, "reduzida") {
		t.Errorf("big non-list: %q", fx.Note)
	}
}

func TestWriteFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p", "shape", "x.json")
	body := "a " + sentinel + " ses_1 <b> & é"
	if err := writeFixture(path, Fixture{Name: "x", Response: FixtureResponse{Body: body}}, sentinel, "ses_1"); err != nil {
		t.Fatal(err)
	}
	_, raw := readFixture(t, path)
	if !strings.Contains(raw, `"body": "a <key> <session> <b> & é"`) {
		t.Errorf("fixture: %s", raw)
	}
	if err := writeFixture(filepath.Join(path, "y.json"), Fixture{}, "", ""); err == nil {
		t.Error("write under a file")
	}
}
