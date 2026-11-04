package openrouter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Driver)
}

// TestKeepAliveOnlyWhileWaiting: a stream that waits longer than the
// keep-alive interval for its first byte opens with the 200 headers and
// PROCESSING comments; Start stops them; a fast stream gets none.
func TestKeepAliveOnlyWhileWaiting(t *testing.T) {
	old := keepAliveEvery
	keepAliveEvery = 10 * time.Millisecond
	defer func() { keepAliveEvery = old }()
	d := newTestDriver(t)
	req := &core.Request{Endpoint: core.EndpointChat, Model: "m", Stream: true}

	w := core.NewMemWriter()
	sw := d.StreamWriter(w, req, nil)
	time.Sleep(35 * time.Millisecond)
	if err := sw.Start(&core.Response{ID: "gen-1-AAAAAAAAAAAAAAAAAAAA", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	n := strings.Count(string(w.Body()), ": OPENROUTER PROCESSING\n\n")
	time.Sleep(30 * time.Millisecond)
	if err := sw.TextDelta("ok"); err != nil {
		t.Fatal(err)
	}
	body := string(w.Body())
	if n < 2 || strings.Count(body, ": OPENROUTER PROCESSING\n\n") != n || !strings.HasPrefix(body, ": OPENROUTER PROCESSING\n\n") {
		t.Fatalf("%d comments before Start, body %q", n, body)
	}
	if w.StatusCode() != 200 || w.Headers()["Content-Type"] != "text/event-stream" {
		t.Fatalf("status %d, headers %v", w.StatusCode(), w.Headers())
	}

	w = core.NewMemWriter()
	sw = d.StreamWriter(w, req, nil)
	if err := sw.Start(&core.Response{ID: "gen-1-AAAAAAAAAAAAAAAAAAAA", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := sw.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(w.Body()); got != ": OPENROUTER PROCESSING\n\n" {
		t.Fatalf("fast stream: %q (only the engine's ping, as PROCESSING)", got)
	}
}

// TestKeepAliveStops: an Error before Start ends the comments, and so does
// a stream the engine abandons (keepAliveFor).
func TestKeepAliveStops(t *testing.T) {
	oldEvery, oldFor := keepAliveEvery, keepAliveFor
	keepAliveEvery, keepAliveFor = 5*time.Millisecond, 20*time.Millisecond
	defer func() { keepAliveEvery, keepAliveFor = oldEvery, oldFor }()
	d := newTestDriver(t)
	req := &core.Request{Endpoint: core.EndpointChat, Model: "m", Stream: true}

	w := core.NewMemWriter()
	_ = d.StreamWriter(w, req, nil)
	time.Sleep(60 * time.Millisecond)
	n := len(w.Body())
	time.Sleep(30 * time.Millisecond)
	if n == 0 || len(w.Body()) != n {
		t.Fatalf("abandoned stream: %d bytes, then %d", n, len(w.Body()))
	}

	w = core.NewMemWriter()
	sw := d.StreamWriter(w, req, nil)
	if err := sw.Error(&core.Error{Kind: core.ErrServerError}); err != nil {
		t.Fatal(err)
	}
	n = len(w.Body())
	time.Sleep(20 * time.Millisecond)
	if len(w.Body()) != n || strings.Contains(string(w.Body()), "PROCESSING") {
		t.Fatalf("comments after Error: %q", w.Body())
	}
}

// TestLenientChat: the gateway takes the first of models without model and
// ignores max_tokens of the wrong type (validation fixtures, task 161).
func TestLenientChat(t *testing.T) {
	d := newTestDriver(t)
	cases := map[string]func(*core.Request) bool{
		`{"models":["openai/gpt-4o-mini","x/y"],"messages":[{"role":"user","content":"hi"}]}`: func(r *core.Request) bool { return r.Model == "openai/gpt-4o-mini" },
		`{"model":"m","max_tokens":"x","messages":[{"role":"user","content":"hi"}]}`:          func(r *core.Request) bool { return r.Model == "m" && r.MaxTokens == 0 },
		`{"model":"m","max_tokens":null,"n":"2","messages":[{"role":"user","content":"hi"}]}`: func(r *core.Request) bool { return r.N == 0 },
		`{"model":"m","max_tokens":7,"messages":[{"role":"user","content":"hi"}]}`:            func(r *core.Request) bool { return r.MaxTokens == 7 },
	}
	for body, ok := range cases {
		req, e := d.ParseRequest(driver.KindChat, []byte(body), driver.MapHeaders{})
		if e != nil || !ok(req) || string(req.Raw) != body {
			t.Errorf("%s: %+v %v", body, req, e)
		}
	}
	if _, e := d.ParseRequest(driver.KindChat, []byte(`{"models":[],"messages":[{"role":"user","content":"hi"}]}`), driver.MapHeaders{}); e == nil {
		t.Error("no model and no models: want an error")
	}
	if got := string(lenientChat([]byte(`not json`))); got != "not json" {
		t.Errorf("not JSON: %q", got)
	}
}

// TestLenientMessages: /api/v1/messages accepts a body without max_tokens.
func TestLenientMessages(t *testing.T) {
	d := newTestDriver(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req, e := d.ParseRequest(driver.KindMessages, []byte(body), driver.MapHeaders{})
	if e != nil || req.MaxTokens != 0 || string(req.Raw) != body {
		t.Fatalf("%+v %v", req, e)
	}
	if _, e := d.ParseRequest(driver.KindMessages, []byte(`{"model":`), driver.MapHeaders{}); e == nil {
		t.Error("malformed JSON: want an error")
	}
	if got := string(lenientMessages([]byte(`{"max_tokens":3}`))); got != `{"max_tokens":3}` {
		t.Errorf("with max_tokens: %q", got)
	}
}

func TestPerToken(t *testing.T) {
	for perM, want := range map[float64]string{
		0: "0", 0.1: "0.0000001", 0.4: "0.0000004", 0.03: "0.00000003", 15: "0.000015", 1.25: "0.00000125",
		2500000: "2.5", 3000000: "3", 10000000: "10",
	} {
		if got := perToken(perM); got != want {
			t.Errorf("perToken(%v) = %q, want %q", perM, got, want)
		}
	}
}

func TestCanonicalSlug(t *testing.T) {
	for _, c := range [][3]string{
		{"openai/gpt-4.1-nano", "", "openai/gpt-4.1-nano"},
		{"openai/gpt-4.1-nano", "gpt-4.1-nano-2025-04-14", "openai/gpt-4.1-nano-2025-04-14"},
		{"openai/gpt-4.1-nano", "x/snap", "x/snap"},
		{"wwz-fast", "wwz-fast-2026", "white-wizard/wwz-fast-2026"},
		{"wwz-fast", "", "white-wizard/wwz-fast"},
	} {
		if got := canonicalSlug(c[0], c[1]); got != c[2] {
			t.Errorf("canonicalSlug(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// TestNativeFinish: the upstream's vocabulary by the model's author.
func TestNativeFinish(t *testing.T) {
	cases := []struct {
		model string
		fr    core.FinishReason
		want  any
	}{
		{"openai/gpt-4.1-nano", core.FinishStop, "completed"},
		{"OpenAI/gpt-4.1-nano", core.FinishToolCalls, "completed"},
		{"openai/gpt-4.1-nano", core.FinishLength, "max_output_tokens"},
		{"openai/gpt-4.1-nano", core.FinishContentFilter, "content_filter"},
		{"anthropic/claude-haiku-4.5", core.FinishStop, "end_turn"},
		{"anthropic/claude-haiku-4.5", core.FinishLength, "max_tokens"},
		{"anthropic/claude-haiku-4.5", core.FinishToolCalls, "tool_use"},
		{"white-wizard/wwz-fast", core.FinishStop, "stop"},
		{"openai/gpt-4.1-nano", "", nil},
	}
	for _, c := range cases {
		if got := nativeFinish(&core.Request{Model: c.model}, c.fr); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.model, c.fr, got, c.want)
		}
	}
	if got := nativeFinish(nil, core.FinishStop); got != "stop" {
		t.Errorf("nil request: %v", got)
	}
}

// TestModelItemFromConfig: what the configuration drives in an item:
// expiration_date from retires_at, reasoning and its parameters, cache
// prices, zero prices omitted, extra pricing merged in the real order,
// unknown extra fields kept at the end.
func TestModelItemFromConfig(t *testing.T) {
	yes := true
	cfg := &config.ModelConfig{
		ID: "wwz-think", DisplayName: "Think", RetiresAt: "2027-01-02", Capabilities: config.Capabilities{Reasoning: &yes},
		Pricing: config.Pricing{InputPerM: 1, OutputPerM: 2, CacheWritePerM: 1.25},
		Extra:   map[string]any{"pricing": map[string]any{"image": "0.001", "overrides": []any{}}, "zz_custom": 1},
	}
	b, err := jsonFormat.Marshal(modelJSON(driver.ModelInfo{Cfg: cfg}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"pricing":{"prompt":"0.000001","completion":"0.000002","image":"0.001","input_cache_write":"0.00000125","overrides":[]}`,
		`"expiration_date":"2027-01-02","links":{"details":"/api/v1/models/white-wizard/wwz-think/endpoints"}`,
		`"supported_parameters":["frequency_penalty","include_reasoning","max_tokens","presence_penalty","reasoning","response_format","seed","stop","temperature","tool_choice","tools","top_p"]`,
		`"reasoning":{"mandatory":false,"default_enabled":true,"supported_efforts":["high","medium","low"],"default_effort":"medium"},"zz_custom":1}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("%s not in %s", want, s)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "White Wizard: Think" || m["hugging_face_id"] != "" {
		t.Errorf("defaults: %v", m)
	}
}

// TestGenerationIDFresh: an answer without a minted request id still
// carries a generation id, a new one each time.
func TestGenerationIDFresh(t *testing.T) {
	a, b := generationID(nil), generationID(&core.Request{RequestID: "req_x"})
	if !strings.HasPrefix(a, "gen-") || !strings.HasPrefix(b, "gen-") || a == b {
		t.Fatalf("%q %q", a, b)
	}
	if got := generationID(&core.Request{RequestID: "gen-1-x"}); got != "gen-1-x" {
		t.Fatalf("minted id not kept: %q", got)
	}
}

// TestExtraMapsInRealOrder: models[].extra maps (from YAML, without order)
// come out in the real nested order of the known fields.
func TestExtraMapsInRealOrder(t *testing.T) {
	cfg := &config.ModelConfig{ID: "a/b", Extra: map[string]any{
		"benchmarks": map[string]any{
			"artificial_analysis": map[string]any{"agentic_index": nil, "coding_index": 11.1, "intelligence_index": nil},
			"design_arena":        []any{map[string]any{"rank": 1, "win_rate": 2, "elo": 3, "category": "c", "arena": "a"}},
		},
		"pricing":      map[string]any{"overrides": []any{map[string]any{"prompt": "1", "min_prompt_tokens": 9}}},
		"alias_target": map[string]any{"slug": "a/b-1", "name": "B"},
	}}
	b, err := jsonFormat.Marshal(modelJSON(driver.ModelInfo{Cfg: cfg}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"canonical_slug":"a/b","alias_target":{"name":"B","slug":"a/b-1"},"hugging_face_id"`,
		`"overrides":[{"min_prompt_tokens":9,"prompt":"1"}]`,
		`"benchmarks":{"design_arena":[{"arena":"a","category":"c","elo":3,"win_rate":2,"rank":1}],"artificial_analysis":{"intelligence_index":null,"coding_index":11.1,"agentic_index":null}}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s not in %s", want, b)
		}
	}
}
