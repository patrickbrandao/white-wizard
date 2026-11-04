package engine_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

var (
	reChatcmpl = regexp.MustCompile(`^chatcmpl-[0-9A-Za-z]{29}$`)
	reCmpl     = regexp.MustCompile(`^cmpl-[0-9A-Za-z]{29}$`)
	reMsg      = regexp.MustCompile(`^msg_01[0-9A-Za-z]{22}$`)
	// OpenCode (task 175): gw-<12 hex>-<6 hex>, call_<24 hex>, 32 hex on Messages.
	reGw        = regexp.MustCompile(`^gw-[0-9a-f]{12}-[0-9a-f]{6}$`)
	reCallHex   = regexp.MustCompile(`^call_[0-9a-f]{24}$`)
	reHex32     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reGen       = regexp.MustCompile(`^gen-[0-9]{10}-[0-9A-Za-z]{20}$`)
	reCall      = regexp.MustCompile(`^call_[0-9A-Za-z]{24}$`)
	reToolu     = regexp.MustCompile(`^toolu_01[0-9A-Za-z]{22}$`)
	reReqOpenAI = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	reReqAnth   = regexp.MustCompile(`^req_01[0-9A-Za-z]{22}$`)
)

const toolsJSON = `"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}],"tool_choice":"required"`

const anthToolsJSON = `"tools":[{"name":"read_file","input_schema":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}],"tool_choice":{"type":"any"}`

type surface struct {
	drv, path string
	messages  bool // Anthropic Messages surface
	respID    *regexp.Regexp
	toolID    *regexp.Regexp
	reqID     *regexp.Regexp
	reqHeader string
}

var surfaces = []surface{
	{"openai", "/v1/chat/completions", false, reChatcmpl, reCall, reReqOpenAI, "x-request-id"},
	{"anthropic", "/v1/messages", true, reMsg, reToolu, reReqAnth, "request-id"},
	// The gateways send no request id header (spec 006 16.7), except
	// OpenRouter's Messages route, whose request-id is the generation id
	// and whose body id is the upstream message's (task 174).
	{"openrouter", "/api/v1/chat/completions", false, reGen, reCall, nil, ""},
	{"openrouter", "/api/v1/messages", true, reMsg, reToolu, reGen, "request-id"},
	{"opencode", "/zen/go/v1/chat/completions", false, reGw, reCallHex, nil, ""},
	{"opencode", "/zen/go/v1/messages", true, reHex32, reToolu, nil, ""},
}

// checkRequestID checks the request id header of s: in the provider's format
// where the real sends one, absent (neither x-request-id nor request-id
// from the engine) where it sends none. Only OpenAI sends x-request-id.
func (s surface) checkRequestID(t *testing.T, h map[string]string) {
	t.Helper()
	if s.reqHeader == "" {
		if v, ok := h["x-request-id"]; ok {
			t.Errorf("unexpected x-request-id %q (headers %v)", v, h)
		}
		if v, ok := h["request-id"]; ok {
			t.Errorf("unexpected request-id %q (headers %v)", v, h)
		}
		return
	}
	if v := h[s.reqHeader]; !s.reqID.MatchString(v) {
		t.Errorf("%s %q does not match %s (headers %v)", s.reqHeader, v, s.reqID, h)
	}
	if s.drv != "openai" {
		if v, ok := h["x-request-id"]; ok {
			t.Errorf("x-request-id %q outside OpenAI (headers %v)", v, h)
		}
	}
}

func (s surface) headers() map[string]string {
	if s.drv == "anthropic" {
		return map[string]string{"x-api-key": "wwz-key-01", "anthropic-version": "2023-06-01"}
	}
	return map[string]string{"Authorization": "Bearer wwz-key-01"}
}

func (s surface) body(extra string) string {
	if s.messages {
		return `{"model":"wwz-fast","max_tokens":4096,"messages":[{"role":"user","content":"hello"}]` + extra + `}`
	}
	return `{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]` + extra + `}`
}

func (s surface) tools() string {
	if s.messages {
		return "," + anthToolsJSON
	}
	return "," + toolsJSON
}

// wire is the subset of both dialects the tests look at.
type wire struct {
	ID      string `json:"id"`
	Choices []struct {
		Message map[string]any `json:"message"`
	} `json:"choices"`
	Content []map[string]any `json:"content"`
}

func decode(t *testing.T, b []byte) wire {
	t.Helper()
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return w
}

func (w wire) toolIDs() []string {
	var out []string
	for _, c := range w.Choices {
		tcs, _ := c.Message["tool_calls"].([]any)
		for _, tc := range tcs {
			out = append(out, tc.(map[string]any)["id"].(string))
		}
	}
	for _, b := range w.Content {
		if b["type"] == "tool_use" {
			out = append(out, b["id"].(string))
		}
	}
	return out
}

func (w wire) hasReasoning() bool {
	for _, c := range w.Choices {
		// OpenRouter sends reasoning: null without reasoning (task 174).
		if c.Message["reasoning_content"] != nil || c.Message["reasoning"] != nil {
			return true
		}
	}
	for _, b := range w.Content {
		if b["type"] == "thinking" || b["type"] == "redacted_thinking" {
			return true
		}
	}
	return false
}

// TestProviderIDFormats: response, tool call and request ids follow the real
// provider format on every driver surface (task 010).
func TestProviderIDFormats(t *testing.T) {
	e := newEngine(t, fastCfg)
	for _, s := range surfaces {
		t.Run(s.drv+s.path, func(t *testing.T) {
			w := call(t, e, s.drv, s.path, s.headers(), s.body(s.tools()))
			if w.StatusCode() != 200 {
				t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
			}
			got := decode(t, w.Body())
			if !s.respID.MatchString(got.ID) {
				t.Errorf("response id %q does not match %s", got.ID, s.respID)
			}
			tids := got.toolIDs()
			if len(tids) == 0 {
				t.Fatalf("no tool call: %s", w.Body())
			}
			for _, id := range tids {
				if !s.toolID.MatchString(id) {
					t.Errorf("tool call id %q does not match %s", id, s.toolID)
				}
			}
			s.checkRequestID(t, w.Headers())
		})
	}
}

// TestProviderIDFormatsStream: the streamed message/chunk carries the same id
// format as the non-streaming body.
func TestProviderIDFormatsStream(t *testing.T) {
	e := newEngine(t, fastCfg)
	reID := regexp.MustCompile(`"id":"([^"]+)"`)
	for _, s := range surfaces {
		t.Run(s.drv+s.path, func(t *testing.T) {
			w := call(t, e, s.drv, s.path, s.headers(), s.body(`,"stream":true`))
			m := reID.FindStringSubmatch(string(w.Body()))
			if w.StatusCode() != 200 || m == nil {
				t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
			}
			if !s.respID.MatchString(m[1]) {
				t.Errorf("stream id %q does not match %s", m[1], s.respID)
			}
		})
	}
}

// TestLegacyCompletionsID: /v1/completions uses the "cmpl-" prefix.
func TestLegacyCompletionsID(t *testing.T) {
	e := newEngine(t, fastCfg)
	w := call(t, e, "openai", "/v1/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, `{"model":"wwz-fast","prompt":"hello"}`)
	if got := decode(t, w.Body()).ID; !reCmpl.MatchString(got) {
		t.Fatalf("completion id %q (status %d): %s", got, w.StatusCode(), w.Body())
	}
}

// TestIDsDeterministicBySeed: under a fixed seed the same conversation turn
// yields the same response and tool call ids, while request ids stay unique.
func TestIDsDeterministicBySeed(t *testing.T) {
	for _, s := range surfaces {
		t.Run(s.drv+s.path, func(t *testing.T) {
			e := newEngine(t, fastCfg)
			w1 := call(t, e, s.drv, s.path, s.headers(), s.body(s.tools()))
			w2 := call(t, e, s.drv, s.path, s.headers(), s.body(s.tools()))
			a, b := decode(t, w1.Body()), decode(t, w2.Body())
			if s.respID != reGen && a.ID != b.ID {
				t.Errorf("response ids differ under a fixed seed: %q vs %q", a.ID, b.ID)
			}
			if s.respID == reGen && a.ID[strings.LastIndexByte(a.ID, '-'):] != b.ID[strings.LastIndexByte(b.ID, '-'):] {
				t.Errorf("generation ids differ under a fixed seed: %q vs %q", a.ID, b.ID)
			}
			if strings.Join(a.toolIDs(), ",") != strings.Join(b.toolIDs(), ",") {
				t.Errorf("tool ids differ under a fixed seed: %v vs %v", a.toolIDs(), b.toolIDs())
			}
			if s.reqHeader != "" && w1.Headers()[s.reqHeader] == w2.Headers()[s.reqHeader] {
				t.Errorf("request ids repeat: %q", w1.Headers()[s.reqHeader])
			}
			// A different seed changes the ids.
			h := s.headers()
			h["x-wwz-seed"] = "7"
			c := decode(t, call(t, e, s.drv, s.path, h, s.body(s.tools())).Body())
			if c.ID == a.ID {
				t.Errorf("seed override kept id %q", c.ID)
			}
		})
	}
}

// TestIDsRandomWithoutSeed: with seed 0 two requests get different ids.
func TestIDsRandomWithoutSeed(t *testing.T) {
	e := newEngine(t, "profiles:\n  latency:\n    fast: {ttft: 0ms, itl: 0ms}\n")
	s := surfaces[0]
	a := decode(t, call(t, e, s.drv, s.path, s.headers(), s.body("")).Body())
	b := decode(t, call(t, e, s.drv, s.path, s.headers(), s.body("")).Body())
	if a.ID == b.ID || !reChatcmpl.MatchString(a.ID) {
		t.Fatalf("ids %q %q", a.ID, b.ID)
	}
}

// TestErrorRequestIDFormat: errors carry the minted request id too.
func TestErrorRequestIDFormat(t *testing.T) {
	e := newEngine(t, fastCfg)
	for _, s := range surfaces {
		t.Run(s.drv+s.path, func(t *testing.T) {
			body := strings.Replace(s.body(""), "wwz-fast", "no-such-model", 1)
			w := call(t, e, s.drv, s.path, s.headers(), body)
			if w.StatusCode() < 400 {
				t.Fatalf("status %d", w.StatusCode())
			}
			s.checkRequestID(t, w.Headers())
			if s.drv == "anthropic" {
				var eb struct {
					RequestID string `json:"request_id"`
				}
				_ = json.Unmarshal(w.Body(), &eb)
				if eb.RequestID != w.Headers()["request-id"] {
					t.Errorf("body request_id %q != header %q", eb.RequestID, w.Headers()["request-id"])
				}
			}
		})
	}
}

// TestReasoningOnlyWhenRequested (spec 007 R7): the default profile does not
// emit thinking/reasoning unless the client asks for it. OpenCode's
// upstreams think without being asked (conferred, task 175): there the
// default is reasoning.
func TestReasoningOnlyWhenRequested(t *testing.T) {
	e := newEngine(t, fastCfg)
	for _, s := range surfaces {
		t.Run(s.drv+s.path, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				extra := ""
				if stream {
					extra = `,"stream":true`
				}
				w := call(t, e, s.drv, s.path, s.headers(), s.body(extra))
				out := string(w.Body())
				if s.drv == "opencode" {
					if !strings.Contains(out, `"reasoning_content"`) && !strings.Contains(out, `"thinking"`) {
						t.Errorf("opencode answered without its default reasoning: %s", out)
					}
					continue
				}
				if stream {
					// usage.output_tokens_details.thinking_tokens is not reasoning.
					if strings.Contains(out, `"reasoning_content"`) || strings.Contains(out, `"reasoning":"`) || strings.Contains(out, `"type":"thinking"`) || strings.Contains(out, `"thinking_delta"`) {
						t.Errorf("unrequested reasoning in stream:\n%s", out)
					}
				} else if decode(t, w.Body()).hasReasoning() {
					t.Errorf("unrequested reasoning: %s", out)
				}
			}
		})
	}
}

func TestReasoningWhenRequested(t *testing.T) {
	e := newEngine(t, fastCfg)
	// openai hides the reasoning text (ADR 0022): see reasoning_test.go.
	asks := map[string][]string{
		"anthropic":  {`,"thinking":{"type":"enabled","budget_tokens":1024}`},
		"openrouter": {`,"reasoning":{"effort":"low"}`, `,"reasoning":{"max_tokens":100}`},
		"opencode":   {`,"reasoning_effort":"low"`, `,"reasoning":{"effort":"high"}`},
	}
	for _, s := range surfaces {
		list := asks[s.drv]
		if s.messages {
			list = asks["anthropic"]
		}
		for _, ask := range list {
			t.Run(s.drv+s.path+ask, func(t *testing.T) {
				w := call(t, e, s.drv, s.path, s.headers(), s.body(ask))
				if w.StatusCode() != 200 || !decode(t, w.Body()).hasReasoning() {
					t.Errorf("status %d, reasoning missing: %s", w.StatusCode(), w.Body())
				}
			})
		}
	}
}

// TestReasoningNotRequested: disabled reasoning stays off (exclude: see
// reasoning_test.go), and an
// Anthropic thinking block other than "enabled" does not turn it on.
func TestReasoningNotRequested(t *testing.T) {
	e := newEngine(t, fastCfg)
	cases := []struct {
		s   surface
		ask string
	}{
		{surfaces[2], `,"reasoning":{"enabled":false}`},
		{surfaces[1], `,"thinking":{"type":"disabled"}`},
	}
	for _, tc := range cases {
		t.Run(tc.s.drv+tc.ask, func(t *testing.T) {
			w := call(t, e, tc.s.drv, tc.s.path, tc.s.headers(), tc.s.body(tc.ask))
			if w.StatusCode() != 200 || decode(t, w.Body()).hasReasoning() {
				t.Errorf("status %d, unexpected reasoning: %s", w.StatusCode(), w.Body())
			}
		})
	}
}
