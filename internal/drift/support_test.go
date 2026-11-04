package drift

import (
	"bytes"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

func TestCostUSD(t *testing.T) {
	// The real OpenCode chat of 2026-10-31: 35 in, 13 out, list_cost
	// 0.00001305.
	if got := CostUSD("deepseek-v4.1-flash", 35, 13, true); math.Abs(got-0.00001305) > 1e-12 {
		t.Errorf("opencode = %v", got)
	}
	if got := CostUSD("gpt-4.1-nano", 1_000_000, 1_000_000, true); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("known = %v", got)
	}
	if got := CostUSD("claude-haiku-4-5", 0, 0, false); math.Abs(got-(32*1.0+16*5.0)/1e6) > 1e-12 {
		t.Errorf("estimated = %v", got)
	}
	if got := CostUSD("glm-4.6", 10, 10, true); got != UnknownCallUSD {
		t.Errorf("unknown = %v", got)
	}
}

func TestBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "real-spend.json")
	m, err := ReadBudget(path)
	if err != nil || len(m) != 0 {
		t.Fatalf("missing: %v %v", m, err)
	}
	now := time.Date(2026, 10, 31, 6, 0, 0, 0, time.FixedZone("x", 3600))
	if _, err := AddSpend(path, "openai", 0.25, now); err != nil {
		t.Fatal(err)
	}
	s, err := AddSpend(path, "openai", 0.5, now)
	if err != nil || s.Calls != 2 || math.Abs(s.USD-0.75) > 1e-9 || !s.Updated.Equal(now) || s.Updated.Location() != time.UTC {
		t.Fatalf("AddSpend = %+v, %v", s, err)
	}
	m, err = ReadBudget(path)
	if err != nil || m["openai"].Calls != 2 {
		t.Fatalf("ReadBudget = %+v, %v", m, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := ReadBudget(bad); err == nil {
		t.Error("invalid JSON accepted")
	}
	if _, err := AddSpend(bad, "openai", 1, now); err == nil {
		t.Error("AddSpend on invalid file")
	}
	if _, err := ReadBudget(dir); err == nil {
		t.Error("directory read as budget")
	}
	// The parent is a file: MkdirAll fails.
	if _, err := AddSpend(filepath.Join(bad, "x.json"), "openai", 1, now); err == nil {
		t.Error("AddSpend under a file")
	}
	// The temporary file name is taken by a directory: WriteFile fails.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked+"."+strconv.Itoa(os.Getpid())+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := AddSpend(blocked, "openai", 1, now); err == nil {
		t.Error("AddSpend with a blocked temporary file")
	}
	if err := writeAtomic(filepath.Join(bad, "x.json"), []byte("{}")); err == nil {
		t.Error("writeAtomic under a file")
	}
	// The target is a non-empty directory: Rename fails and the temporary
	// file is removed.
	target := filepath.Join(dir, "target")
	_ = os.MkdirAll(filepath.Join(target, "child"), 0o750)
	if err := writeAtomic(target, []byte("{}")); err == nil {
		t.Error("rename over a directory")
	}
	if _, err := os.Stat(target + "." + strconv.Itoa(os.Getpid()) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file left: %v", err)
	}
}

func TestUsageOf(t *testing.T) {
	if in, out, ok := usageOf([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":3}}`), false); !ok || in != 9 || out != 3 {
		t.Errorf("openai = %d %d %v", in, out, ok)
	}
	stream := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n" +
		": ping\n\ndata: not json\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":7}}\n\n" +
		"data: {\"message\":{}}\n\n"
	if in, out, ok := usageOf([]byte(stream), true); !ok || in != 12 || out != 7 {
		t.Errorf("anthropic stream = %d %d %v", in, out, ok)
	}
	if _, _, ok := usageOf([]byte(`{"id":"x"}`), false); ok {
		t.Error("usage without block")
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor("", "sk-abc", "sk-abcdef")
	if got := r.String("a sk-abcdef b sk-abc"); got != "a *** b ***" {
		t.Errorf("String = %q", got)
	}
	if got := r.Bytes([]byte("x sk-abc")); string(got) != "x ***" {
		t.Errorf("Bytes = %q", got)
	}
}

func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "real-keys.env")
	file := "# comment\n\nexport OPENAI_API_KEY=\"sk-file-openai\"\nANTHROPIC_API_KEY='sk-file-anth'\nOPENROUTER_API_KEY = sk-file-or \nbroken line\nOPENCODE_API_KEY=\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-env-anth", "OPENCODE_API_KEY": "sk-env-oc"}
	keys, err := LoadKeys(path, true, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"openai": "sk-file-openai", "anthropic": "sk-env-anth", "openrouter": "sk-file-or", "opencode": "sk-env-oc"}
	for k, v := range want {
		if keys[k] != v {
			t.Errorf("%s = %q, want %q", k, keys[k], v)
		}
	}
	missing := filepath.Join(dir, "missing.env")
	if keys, err := LoadKeys(missing, false, func(string) string { return "" }); err != nil || len(keys) != 0 {
		t.Errorf("missing default: %v %v", keys, err)
	}
	if _, err := LoadKeys(missing, true, nil); err == nil {
		t.Error("missing explicit file accepted")
	}
	t.Setenv("OPENAI_API_KEY", "sk-os-env")
	if keys, err := LoadKeys("", false, nil); err != nil || keys["openai"] != "sk-os-env" {
		t.Errorf("os env: %v %v", keys, err)
	}
}

func TestProviders(t *testing.T) {
	// Fixed probe models (spec 017, ADR 0049), served by the embedded wwz
	// and priced.
	cfg, err := config.Parse(WWZConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		ProviderOpenAI: "gpt-4o-mini", ProviderAnthropic: "claude-haiku-4-5-20251001",
		ProviderOpenRouter: "openai/gpt-4.1-nano", ProviderOpenCode: "deepseek-v4.1-flash",
		ProviderGemini: "gemini-3.5-flash-lite", ProviderOpenCodeGoogle: "deepseek-v4.1-flash",
		ProviderAzure: "gpt-4o-mini",
	}
	for _, name := range ProviderNames {
		p, ok := LookupProvider(name)
		if !ok || p.Name != name || p.DefaultModel != want[name] || p.EnvKey == "" {
			t.Errorf("provider %s: %+v", name, p)
		}
		m := cfg.FindModel(p.DefaultModel)
		if m == nil || !slices.Contains(m.Drivers, p.DriverName()) {
			t.Errorf("embedded wwz does not serve %s for %s", p.DefaultModel, name)
		}
		if _, ok := prices[p.DefaultModel]; !ok {
			t.Errorf("no price for %s", p.DefaultModel)
		}
	}
	oc := cfg.FindModel("deepseek-v4.1-flash")
	if g, err := cfg.GenerationFor(oc); err != nil || g.ReasoningField != "reasoning_content" || oc.Capabilities.Reasoning == nil || !*oc.Capabilities.Reasoning {
		t.Errorf("opencode model: %+v %+v %v", oc, g, err)
	}
	if m := cfg.FindModel("claude-haiku-4-5"); m == nil || m.ID != "claude-haiku-4-5-20251001" {
		t.Errorf("anthropic alias: %+v", m)
	}
	if _, ok := LookupProvider("bedrock"); ok {
		t.Error("bedrock known")
	}
	// azure: no real base, never probed by default (ADR 0043); the
	// deployment in the path, api-version in the query, the key in api-key.
	az := providers[ProviderAzure]
	if az.BaseURL != "" || slices.Contains(DefaultProviders, ProviderAzure) || len(DefaultProviders) != len(ProviderNames)-1 {
		t.Errorf("azure defaults: %+v %v", az, DefaultProviders)
	}
	if rq := az.probeRequest(ProbeErrorModel, "gpt-4o-mini"); rq.path != "/openai/deployments/"+MissingModel+"/chat/completions?api-version="+AzureAPIVersion {
		t.Errorf("azure path: %s", rq.path)
	}
	if h := az.headers("k", "ses", "ua"); h.Get("api-key") != "k" || h.Get("Authorization") != "" || !isSensitive("Api-Key") {
		t.Errorf("azure headers: %v", h)
	}
	// opencode-google: Gemini body with the opencode tokens, session and
	// User-Agent of the gateway, served by the opencode driver.
	og := providers[ProviderOpenCodeGoogle]
	if og.DriverName() != ProviderOpenCode || providers[ProviderGemini].DriverName() != ProviderGemini {
		t.Errorf("driver names: %+v", og)
	}
	if rq := og.probeRequest(ProbeChatStream, "m"); rq.path != "/v1beta/models/m:streamGenerateContent?alt=sse" || !bytes.Contains(rq.body, []byte(`"maxOutputTokens":256`)) {
		t.Errorf("opencode-google request: %s %s", rq.path, rq.body)
	}
	if h := og.headers("k", "ses_1", "ua"); h.Get("x-goog-api-key") != "k" || h.Get("User-Agent") != OpenCodeUserAgent || h.Get("x-opencode-session") != "ses_1" {
		t.Errorf("opencode-google headers: %v", h)
	}
	oa := providers[ProviderOpenAI]
	if rq := oa.probeRequest(ProbeModels, "m"); rq.method != http.MethodGet || rq.path != "/v1/models" || rq.body != nil {
		t.Errorf("models: %+v", rq)
	}
	rq := oa.probeRequest(ProbeChatStream, "gpt-4.1-nano")
	wantBody := `{"model":"gpt-4.1-nano","messages":[{"role":"user","content":"Responda só: ok"}],"max_tokens":16,"stream":true,"stream_options":{"include_usage":true}}`
	if rq.method != http.MethodPost || string(rq.body) != wantBody {
		t.Errorf("openai stream body = %s", rq.body)
	}
	if rq := providers[ProviderOpenCode].probeRequest(ProbeChat, "m"); !bytes.Contains(rq.body, []byte(`"max_tokens":256`)) {
		t.Errorf("opencode body = %s", rq.body)
	}
	or := providers[ProviderOpenRouter]
	if rq := or.probeRequest(ProbeChatStream, "m"); !bytes.Contains(rq.body, []byte(`"stream":true}`)) {
		t.Errorf("openrouter stream body = %s", rq.body)
	}
	if rq := or.probeRequest(ProbeErrorModel, "m"); !bytes.Contains(rq.body, []byte(MissingModel)) || bytes.Contains(rq.body, []byte("stream")) {
		t.Errorf("error_model body = %s", rq.body)
	}
	h := providers[ProviderAnthropic].headers("k", "ses", "ua")
	if h.Get("x-api-key") != "k" || h.Get("anthropic-version") != AnthropicVersion || h.Get("Authorization") != "" || h.Get("User-Agent") != "ua" {
		t.Errorf("anthropic headers: %v", h)
	}
	h = providers[ProviderOpenCode].headers("k", "ses_1", "ua")
	if h.Get("Authorization") != "Bearer k" || h.Get("User-Agent") != OpenCodeUserAgent || h.Get("x-opencode-session") != "ses_1" {
		t.Errorf("opencode headers: %v", h)
	}
	if !isSensitive("authorization") || !isSensitive("x-api-key") || isSensitive("x-request-id") {
		t.Error("isSensitive")
	}
	if s := NewSessionID(); !strings.HasPrefix(s, "ses_") || len(s) != 4+64 {
		t.Errorf("NewSessionID = %q", s)
	}
}
