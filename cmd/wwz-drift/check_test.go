package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/drift"
)

var update = flag.Bool("update", false, "regrava os arquivos golden")

// sentinel is the fake "real" key: it must only reach the fake provider.
const sentinel = "sk-SENTINEL-cmd-4f2a9b"

// fakeReal is a fake provider: it forwards every request to an embedded wwz
// (swapping the sentinel key for the wwz key), so its answers have the wwz
// shape; mutate, when set, edits the body of the non-stream chat answer.
type fakeReal struct {
	t      *testing.T
	target string
	mutate func(body []byte) []byte

	mu   sync.Mutex
	keys []string
}

func (f *fakeReal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.keys = append(f.keys, r.Header.Get("Authorization")+r.Header.Get("x-api-key")+r.Header.Get("x-goog-api-key")+r.Header.Get("api-key"))
	f.mu.Unlock()
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, f.target+r.URL.RequestURI(), bytes.NewReader(body))
	req.Header = r.Header.Clone()
	if req.Header.Get("Authorization") == "Bearer "+sentinel {
		req.Header.Set("Authorization", "Bearer "+drift.DefaultWWZKey)
	}
	if req.Header.Get("x-api-key") == sentinel {
		req.Header.Set("x-api-key", drift.DefaultWWZKey)
	}
	if req.Header.Get("x-goog-api-key") == sentinel {
		req.Header.Set("x-goog-api-key", drift.DefaultWWZKey)
	}
	if req.Header.Get("api-key") == sentinel {
		req.Header.Set("api-key", drift.DefaultWWZKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Errorf("fake: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	isChat := r.Method == http.MethodPost && !bytes.Contains(body, []byte(`"stream":true`)) &&
		!strings.Contains(r.URL.Path, ":streamGenerateContent") &&
		!bytes.Contains(body, []byte(drift.MissingModel)) && resp.StatusCode == http.StatusOK
	if f.mutate != nil && isChat {
		out = f.mutate(out)
	}
	for k, v := range resp.Header {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// sawSentinel reports whether the fake received the real key.
func (f *fakeReal) sawSentinel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if strings.Contains(k, sentinel) {
			return true
		}
	}
	return false
}

// startWWZ starts a wwz with the embedded drift configuration.
func startWWZ(t *testing.T) string {
	t.Helper()
	w, err := drift.StartWWZ(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w.URL
}

// fakeProvider starts a fake real provider forwarding to the driver of
// name on wwz.
func fakeProvider(t *testing.T, wwz, name string, mutate func([]byte) []byte) (*fakeReal, string) {
	t.Helper()
	p, _ := drift.LookupProvider(name)
	f := &fakeReal{t: t, target: wwz + p.WWZPrefix, mutate: mutate}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// keysFile writes a keys file with the sentinel for the given variables.
func keysFile(t *testing.T, vars ...string) string {
	t.Helper()
	var b strings.Builder
	for _, v := range vars {
		b.WriteString(v + "=" + sentinel + "\n")
	}
	path := filepath.Join(t.TempDir(), "real-keys.env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixedClock(t *testing.T) {
	t.Helper()
	orig := now
	t.Cleanup(func() { now = orig })
	now = func() time.Time { return time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC) }
}

func assertNoSentinel(t *testing.T, where ...string) {
	t.Helper()
	for _, s := range where {
		if strings.Contains(s, sentinel) {
			t.Fatalf("key leaked: %q", s)
		}
	}
}

// TestCheckDriftGolden: openai identical (ok), anthropic with an extra
// field on the real chat (drift → 3), openrouter not selected, opencode
// without key; the JSON report is compared with the golden file.
func TestCheckDriftGolden(t *testing.T) {
	fixedClock(t)
	origVersion := version
	t.Cleanup(func() { version = origVersion })
	version = "v0.0.0-test"
	wwz := startWWZ(t)
	oa, oaURL := fakeProvider(t, wwz, drift.ProviderOpenAI, nil)
	an, anURL := fakeProvider(t, wwz, drift.ProviderAnthropic, func(b []byte) []byte {
		return bytes.Replace(b, []byte("{"), []byte(`{"real_only":"x",`), 1)
	})
	dir := t.TempDir()
	budget := filepath.Join(dir, "tmp", "real-spend.json")
	code, out, errs := runCmd(t, "check",
		"--providers", "openai,anthropic,opencode",
		"--keys-file", keysFile(t, "OPENAI_API_KEY", "ANTHROPIC_API_KEY"),
		"--base-url", "openai="+oaURL, "--base-url", "anthropic="+anURL,
		"--budget-file", budget, "--timeout", "10s", "--output", "json")
	if code != drift.ExitDrift {
		t.Fatalf("code = %d, stderr %q\n%s", code, errs, out)
	}
	assertNoSentinel(t, out, errs)
	if !oa.sawSentinel() || !an.sawSentinel() {
		t.Error("fake providers did not receive the real key")
	}
	if _, err := os.Stat(budget); err != nil {
		t.Errorf("budget file not written: %v", err)
	}
	// The fakes listen on random ports; the report's base is masked.
	out = strings.ReplaceAll(strings.ReplaceAll(out, oaURL, "http://fake-openai"), anURL, "http://fake-anthropic")
	golden := filepath.Join("testdata", "check-drift.golden.json")
	if *update {
		if err := os.WriteFile(golden, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("report differs from %s (go test -run TestCheckDriftGolden -update):\n%s", golden, out)
	}
}

// TestCheckAllOK: every provider (azure too, by --providers) identical against an external wwz, text
// report to --out, raw answers saved under tmp/ → 0.
func TestCheckAllOK(t *testing.T) {
	fixedClock(t)
	wwz := startWWZ(t)
	args := []string{
		"--keys-file", keysFile(t, "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "OPENCODE_API_KEY", "GEMENI_API_KEY", "AZURE_OPENAI_API_KEY"),
		"--providers", strings.Join(drift.ProviderNames, ","),
		"--wwz", wwz, "--model", "opencode=deepseek-v4.1-flash",
	}
	fakes := []*fakeReal{}
	for _, name := range drift.ProviderNames {
		f, u := fakeProvider(t, wwz, name, nil)
		fakes = append(fakes, f)
		args = append(args, "--base-url", name+"="+u)
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "tmp", "raw")
	report := filepath.Join(dir, "report.txt")
	args = append(args, "--budget-file", filepath.Join(dir, "spend.json"), "--save-raw", raw, "--out", report)
	code, out, errs := runCmd(t, args...)
	if code != exitOK || out != "" {
		t.Fatalf("code = %d, stdout %q, stderr %q", code, out, errs)
	}
	text, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "resumo: ok=7 drift=0 skipped=0 error=0") || !strings.Contains(string(text), "wwz "+wwz) {
		t.Errorf("text report:\n%s", text)
	}
	entries, _ := os.ReadDir(raw)
	want := 0
	for _, name := range drift.ProviderNames {
		n := len(drift.ProbeNames)
		if p, _ := drift.LookupProvider(name); p.ResponsesPath == "" {
			n -= 2 // responses probes: skipped/not_supported, no raw answer
		}
		want += 2 * n
	}
	if len(entries) != want {
		t.Errorf("raw files = %d", len(entries))
	}
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(raw, e.Name()))
		assertNoSentinel(t, string(data))
	}
	for i, f := range fakes {
		if !f.sawSentinel() {
			t.Errorf("fake %s did not receive the key", drift.ProviderNames[i])
		}
	}
	assertNoSentinel(t, string(text), errs)
}

// TestCheckProbeError: the real provider is unreachable → probe error → 1.
func TestCheckProbeError(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	dir := t.TempDir()
	code, out, errs := runCmd(t, "check", "--providers", "openai", "--probes", "models",
		"--keys-file", keysFile(t, "OPENAI_API_KEY"), "--base-url", "openai="+deadURL,
		"--budget-file", filepath.Join(dir, "spend.json"), "--output", "json")
	if code != exitError || !strings.Contains(out, `"status": "error"`) {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
	assertNoSentinel(t, out, errs)
}

// TestCheckWWZUnreachable: an external wwz that does not answer is an
// execution failure (1) and the partial report is still written.
func TestCheckWWZUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	code, out, errs := runCmd(t, "--wwz", deadURL, "--keys-file", keysFile(t), "--budget-file", filepath.Join(t.TempDir(), "s.json"))
	if code != exitError || !strings.Contains(errs, "wwz inalcançável") || !strings.Contains(out, "resumo:") {
		t.Fatalf("code = %d\n%q\n%q", code, out, errs)
	}
}

// TestCheckNoKeys: without keys every default provider is skipped/no_key
// (azure, outside the default, not_selected) → 0.
func TestCheckNoKeys(t *testing.T) {
	code, out, errs := runCmd(t, "check", "--keys-file", keysFile(t), "--output", "json",
		"--budget-file", filepath.Join(t.TempDir(), "s.json"))
	if code != exitOK || strings.Count(out, `"reason": "no_key"`) != len(drift.DefaultProviders) || !strings.Contains(out, `"reason": "not_selected"`) || !strings.Contains(out, `"url": "in-process"`) {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
}

// TestCheckRealNeedsOptIn: a key without --base-url and without --real never
// reaches the real API; the report shows the base that would be probed.
func TestCheckRealNeedsOptIn(t *testing.T) {
	code, out, errs := runCmd(t, "check", "--providers", "openai", "--keys-file", keysFile(t, "OPENAI_API_KEY"),
		"--output", "json", "--budget-file", filepath.Join(t.TempDir(), "s.json"))
	if code != exitOK || !strings.Contains(out, `"reason": "real_not_confirmed"`) || !strings.Contains(out, `"base": "https://api.openai.com"`) {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
	assertNoSentinel(t, out, errs)
}

// TestCheckDefaultKeysFile: without --keys-file, the file under the home
// directory is read when it exists; no home directory means no file.
func TestCheckDefaultKeysFile(t *testing.T) {
	orig := userHomeDir
	t.Cleanup(func() { userHomeDir = orig })
	home := t.TempDir()
	userHomeDir = func() (string, error) { return home, nil }
	cfg := filepath.Join(home, ".config", "white-wizard")
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := resolveKeysFile(defaultKeysFile, false); got != filepath.Join(cfg, "real-keys.env") {
		t.Errorf("resolve = %q", got)
	}
	if got := resolveKeysFile(defaultKeysFile, true); got != defaultKeysFile {
		t.Errorf("explicit = %q", got)
	}
	budget := filepath.Join(t.TempDir(), "s.json")
	// Missing default file: not an error.
	if code, out, errs := runCmd(t, "--providers", "openai", "--probes", "models", "--budget-file", budget); code != exitOK || !strings.Contains(out, "no_key") {
		t.Fatalf("missing default: %d %q %q", code, out, errs)
	}
	if err := os.WriteFile(filepath.Join(cfg, "real-keys.env"), []byte("OPENAI_API_KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wwz := startWWZ(t)
	f, u := fakeProvider(t, wwz, drift.ProviderOpenAI, nil)
	code, out, errs := runCmd(t, "--providers", "openai", "--probes", "models", "--base-url", "openai="+u, "--budget-file", budget)
	if code != exitOK || !f.sawSentinel() || !strings.Contains(out, "openai: ok") {
		t.Fatalf("default file: %d %q %q", code, out, errs)
	}
	userHomeDir = func() (string, error) { return "", errors.New("sem home") }
	if got := resolveKeysFile(defaultKeysFile, false); got != "" {
		t.Errorf("no home = %q", got)
	}
}

// TestCheckEnvWins: the environment variable wins over the keys file.
func TestCheckEnvWins(t *testing.T) {
	orig := getenv
	t.Cleanup(func() { getenv = orig })
	getenv = func(k string) string {
		if k == "OPENAI_API_KEY" {
			return sentinel
		}
		return ""
	}
	wwz := startWWZ(t)
	f, u := fakeProvider(t, wwz, drift.ProviderOpenAI, nil)
	path := filepath.Join(t.TempDir(), "keys.env")
	_ = os.WriteFile(path, []byte("OPENAI_API_KEY=sk-from-file\n"), 0o600)
	code, out, errs := runCmd(t, "--providers", "openai", "--probes", "models", "--keys-file", path,
		"--base-url", "openai="+u, "--budget-file", filepath.Join(t.TempDir(), "s.json"))
	if code != exitOK || !f.sawSentinel() {
		t.Fatalf("code = %d %q %q", code, out, errs)
	}
}

func TestCheckUsage(t *testing.T) {
	dir := t.TempDir()
	keys := keysFile(t)
	badIgnore := filepath.Join(dir, "ignore.yaml")
	_ = os.WriteFile(badIgnore, []byte("- {provider: openai, kind: extra_field, path: x}\n"), 0o600)
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"help", []string{"-h"}, exitOK, "-providers"},
		{"unknown flag", []string{"--nope"}, exitUsage, "nope"},
		{"positional", []string{"check", "extra"}, exitUsage, `"extra"`},
		{"output", []string{"--output", "yaml"}, exitUsage, "--output"},
		{"empty providers", []string{"--providers", " , "}, exitUsage, "vazios"},
		{"empty probes", []string{"--probes", ""}, exitUsage, "vazios"},
		{"bad pair", []string{"--model", "openai"}, exitUsage, "PROVEDOR=VALOR"},
		{"empty pair value", []string{"--base-url", "openai="}, exitUsage, "PROVEDOR=VALOR"},
		{"unknown provider", []string{"--keys-file", keys, "--providers", "bedrock"}, exitUsage, "bedrock"},
		{"unknown model provider", []string{"--keys-file", keys, "--model", "bedrock=x"}, exitUsage, "bedrock"},
		{"bad base url", []string{"--keys-file", keys, "--base-url", "openai=ftp://x"}, exitUsage, "--base-url"},
		{"save-raw outside tmp", []string{"--keys-file", keys, "--save-raw", dir}, exitUsage, "tmp/"},
		{"negative budget", []string{"--keys-file", keys, "--budget-usd", "-1"}, exitUsage, "--budget-usd"},
		{"invalid ignore", []string{"--keys-file", keys, "--ignore", badIgnore}, exitUsage, "reason"},
		{"missing ignore", []string{"--keys-file", keys, "--ignore", filepath.Join(dir, "none.yaml")}, exitError, "none.yaml"},
		{"missing explicit keys file", []string{"--keys-file", filepath.Join(dir, "none.env")}, exitError, "arquivo de chaves"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errs := runCmd(t, tc.args...)
			if code != tc.code || !strings.Contains(out+errs, tc.want) {
				t.Errorf("code = %d (want %d), out %q, stderr %q", code, tc.code, out, errs)
			}
		})
	}
}

// TestCheckIgnoreFile: a valid --ignore file accepts the divergence of the
// golden scenario → 0.
func TestCheckIgnoreFile(t *testing.T) {
	wwz := startWWZ(t)
	_, u := fakeProvider(t, wwz, drift.ProviderAnthropic, func(b []byte) []byte {
		return bytes.Replace(b, []byte("{"), []byte(`{"real_only":"x",`), 1)
	})
	dir := t.TempDir()
	ignore := filepath.Join(dir, "ignore.yaml")
	_ = os.WriteFile(ignore, []byte("- {provider: anthropic, probe: chat, kind: missing_field, path: real_only, reason: teste}\n"), 0o600)
	code, out, errs := runCmd(t, "--providers", "anthropic", "--probes", "chat", "--keys-file", keysFile(t, "ANTHROPIC_API_KEY"),
		"--base-url", "anthropic="+u, "--ignore", ignore, "--budget-file", filepath.Join(dir, "s.json"))
	if code != exitOK || !strings.Contains(out, "[ignorada]") {
		t.Fatalf("code = %d\n%s\n%s", code, out, errs)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disco cheio") }

// TestCheckWriteFailure: a report that cannot be written turns 0 into 1.
func TestCheckWriteFailure(t *testing.T) {
	keys := keysFile(t)
	budget := filepath.Join(t.TempDir(), "s.json")
	var errb bytes.Buffer
	code := run(context.Background(), []string{"--keys-file", keys, "--budget-file", budget}, failWriter{}, &errb)
	if code != exitError || !strings.Contains(errb.String(), "disco cheio") {
		t.Errorf("stdout failure: %d %q", code, errb.String())
	}
	out := filepath.Join(t.TempDir(), "missing", "report.json")
	if code, _, errs := runCmd(t, "--keys-file", keys, "--budget-file", budget, "--out", out); code != exitError || !strings.Contains(errs, "report.json") {
		t.Errorf("--out failure: %d %q", code, errs)
	}
}

// TestCheckWriteFailureKeepsDrift: 3 is not lowered by a write failure.
func TestCheckWriteFailureKeepsDrift(t *testing.T) {
	wwz := startWWZ(t)
	_, u := fakeProvider(t, wwz, drift.ProviderOpenAI, func(b []byte) []byte {
		return bytes.Replace(b, []byte("{"), []byte(`{"real_only":"x",`), 1)
	})
	var errb bytes.Buffer
	code := run(context.Background(), []string{
		"--providers", "openai", "--probes", "chat", "--keys-file", keysFile(t, "OPENAI_API_KEY"),
		"--base-url", "openai=" + u, "--budget-file", filepath.Join(t.TempDir(), "s.json"),
	}, failWriter{}, &errb)
	if code != drift.ExitDrift {
		t.Errorf("code = %d %q", code, errb.String())
	}
}

func TestPairsAndSplit(t *testing.T) {
	p := pairs{}
	if p.String() != "" {
		t.Error("empty String")
	}
	_ = p.Set(" openai = m1 ")
	_ = p.Set("anthropic=m2")
	if p.String() != "anthropic=m2,openai=m1" {
		t.Errorf("String = %q", p.String())
	}
	if got := splitList(" a, ,b,"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitList = %q", got)
	}
}
