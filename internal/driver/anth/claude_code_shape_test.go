package anth_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
)

// ccShape is an answer of the real API to the Claude Code form, recorded on
// 2026-11-03 (task 441) in testdata/real/anthropic/cc-shape/ with
// claude-haiku-5-5: ids are <msg_id>, <request_id>, <signature>; the
// limit values (the account's tier) are <int>; the SSE padding is removed.
type ccShape struct {
	Name    string `json:"name"`
	Request struct {
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

func loadCCShape(t *testing.T, name string) ccShape {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "anthropic", "cc-shape", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx ccShape
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

var (
	ccPad       = regexp.MustCompile(`( *)\}\n`)
	ccSignature = regexp.MustCompile(`"signature":"[A-Za-z0-9+/=]{20,}"`)
	ccMsgID     = regexp.MustCompile(`"id":"msg_[A-Za-z0-9]+"`)
	ccStartOut  = regexp.MustCompile(`("type":"message_start".*?"output_tokens":)[0-9]+`)
	// inference_geo is "global" on claude-haiku-5-5 and "not_available"
	// on claude-haiku-4-5, the value wwz writes for every model: a model
	// difference outside the Claude Code form (task 450).
	ccGeo     = regexp.MustCompile(`"inference_geo":"[a-z_]+"`)
	ccRFC3339 = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
	ccUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ccWrkspc  = regexp.MustCompile(`^wrkspc_01[1-9A-HJ-NP-Za-km-z]{22}$`)
	ccReqID   = regexp.MustCompile(`^req_011C[0-9A-Za-z]{20}$`)
)

func ccNormalize(b string) string {
	b = ccPad.ReplaceAllString(b, "}\n")
	b = ccSignature.ReplaceAllString(b, `"signature":"<signature>"`)
	b = ccMsgID.ReplaceAllString(b, `"id":"<msg_id>"`)
	b = ccStartOut.ReplaceAllString(b, "${1}N")
	return ccGeo.ReplaceAllString(b, `"inference_geo":"<geo>"`)
}

// ccRequest parses the fixture's request with the anthropic driver, as the
// engine does, and gives it the request id and headers of an answer.
func ccRequest(t *testing.T, d driver.Driver, fx ccShape) *core.Request {
	t.Helper()
	req, e := d.ParseRequest(driver.KindMessages, []byte(fx.Request.Body), driver.MapHeaders(fx.Request.Headers))
	if e != nil {
		t.Fatalf("parse: %v", e)
	}
	req.RequestID = "req_011CAAAAAAAAAAAAAAAAAAAA"
	req.Headers = map[string]string{"x-api-key": "wwz-key-cc", "anthropic-version": "2023-06-01"}
	return req
}

func ccHeaders(t *testing.T, fx ccShape, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, "anthropic-") || k == "request-id" || k == "content-type" || k == "cache-control" {
			low[k] = v
		}
	}
	for k, want := range fx.Response.Headers {
		v, ok := low[k]
		switch {
		case !ok:
			t.Errorf("header %s missing (real %q)", k, want)
		case want == "<rfc3339>" && !ccRFC3339.MatchString(v),
			want == "<organization_id>" && !ccUUID.MatchString(v),
			want == "<workspace_id>" && !ccWrkspc.MatchString(v),
			want == "<request_id>" && !ccReqID.MatchString(v),
			want == "<int>" && (v == "" || strings.Trim(v, "0123456789") != ""):
			t.Errorf("header %s = %q, not in the real format %s", k, v, want)
		case !strings.HasPrefix(want, "<") && v != want:
			t.Errorf("header %s = %q, real %q", k, v, want)
		}
	}
	for k := range low {
		if _, ok := fx.Response.Headers[k]; !ok {
			t.Errorf("header %s not in the real answer", k)
		}
	}
}

type ccUsage struct {
	Input   int `json:"input_tokens"`
	Output  int `json:"output_tokens"`
	Details struct {
		Thinking int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u ccUsage) core() core.Usage {
	return core.Usage{PromptTokens: u.Input, CompletionTokens: u.Output, ReasoningTokens: u.Details.Thinking}
}

// generated stands for the reasoning the engine generates and counts:
// display omitted and updates must not show it.
const generated = "Let me multiply 37 by 43 step by step."

// TestRealFixturesClaudeCodeShape: thinking adaptive with display omitted,
// updates and summarized, and context_management, as the real writes them
// (body); omitted and updates blank the generated reasoning.
func TestRealFixturesClaudeCodeShape(t *testing.T) {
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"thinking_omitted", "thinking_updates", "thinking_summarized", "context_management"} {
		t.Run(name, func(t *testing.T) {
			fx := loadCCShape(t, name)
			var m struct {
				Model   string `json:"model"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"content"`
				Usage ccUsage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
				t.Fatal(err)
			}
			resp := &core.Response{ID: "msg_011CccShape", Model: m.Model, FinishReason: core.FinishStop, Usage: m.Usage.core()}
			for _, b := range m.Content {
				switch b.Type {
				case "thinking":
					resp.Reasoning = b.Thinking
					if resp.Reasoning == "" {
						resp.Reasoning = generated
					}
				case "text":
					resp.Content = b.Text
				}
			}
			req := ccRequest(t, d, fx)
			if !req.Reasoning.Requested || req.Reasoning.Effort != "high" {
				t.Errorf("adaptive thinking: reasoning %+v, want requested with effort high", req.Reasoning)
			}
			w := core.NewMemWriter()
			if err := d.WriteResponse(w, req, resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, want := ccNormalize(string(w.Body())), ccNormalize(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			ccHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesClaudeCodeStreams replays the recorded streams through
// the driver's StreamWriter, the way the engine drives it; for omitted and
// updates the engine's reasoning deltas carry text that must not show.
func TestRealFixturesClaudeCodeStreams(t *testing.T) {
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"thinking_omitted_stream", "thinking_updates_stream", "thinking_summarized_stream", "context_management_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadCCShape(t, name)
			w := core.NewMemWriter()
			sw := d.StreamWriter(w, ccRequest(t, d, fx), nil)
			for _, line := range strings.Split(fx.Response.Body, "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok {
					continue
				}
				var ev struct {
					Type    string `json:"type"`
					Message struct {
						Model string  `json:"model"`
						Usage ccUsage `json:"usage"`
					} `json:"message"`
					Delta struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
					} `json:"delta"`
					Usage ccUsage `json:"usage"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					t.Fatal(err)
				}
				var err error
				switch ev.Type {
				case "message_start":
					err = sw.Start(&core.Response{ID: "msg_011CccStream", Model: ev.Message.Model, Usage: ev.Message.Usage.core()})
				case "content_block_delta":
					switch ev.Delta.Type {
					case "text_delta":
						err = sw.TextDelta(ev.Delta.Text)
					case "thinking_delta":
						if ev.Delta.Thinking == "" {
							// The engine sends its generated text in chunks.
							for _, chunk := range strings.Fields(generated) {
								if err = sw.ReasoningDelta(chunk + " "); err != nil {
									break
								}
							}
						} else {
							err = sw.ReasoningDelta(ev.Delta.Thinking)
						}
					}
				case "message_delta":
					err = sw.Finish(core.FinishStop, ev.Usage.core(), true)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, want := ccNormalize(string(w.Body())), ccNormalize(fx.Response.Body); got != want {
				t.Errorf("stream\n wwz  %s\n real %s", got, want)
			}
			ccHeaders(t, fx, w.Headers())
		})
	}
}

// TestClaudeCodeParse: with Hooks.ClaudeCode a role "system" message is a
// system message of the conversation, adaptive thinking asks for reasoning
// sized by output_config.effort, and display/context_management leave
// their markers; without it (the gateways) role "system" keeps its refusal
// and none of that happens.
func TestClaudeCodeParse(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"env "},{"type":"text","text":"block"}],"output_config":{"effort":"medium"}}],"thinking":{"type":"adaptive","display":"omitted"},"context_management":{"edits":[]},"output_config":{"effort":"low"},"metadata":{"user_id":"u"}}`)
	h := anth.APIHooks()
	h.ClaudeCode = true
	req, e := anth.ParseMessages(&h, body, nil, "anthropic")
	if e != nil {
		t.Fatal(e)
	}
	// One system message per text block (task 453: the real counts
	// [s, t] as two system messages).
	if len(req.Messages) != 3 || req.Messages[1].Role != "system" || req.Messages[1].Content != "env " || req.Messages[2].Role != "system" || req.Messages[2].Content != "block" {
		t.Errorf("messages %+v", req.Messages)
	}
	if !req.Reasoning.Requested || req.Reasoning.Effort != "low" || req.Reasoning.BudgetTokens != 0 {
		t.Errorf("reasoning %+v", req.Reasoning)
	}
	if req.Metadata[anth.MetaThinkingDisplay] != "omitted" || req.Metadata[anth.MetaContextManagement] == "" || req.Metadata["user_id"] != "u" {
		t.Errorf("metadata %q", req.Metadata)
	}
	if h.ForRequest(req, false) == &h {
		t.Error("ForRequest kept the hooks of a request with markers")
	}

	for _, c := range []struct{ body, display string }{
		{`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive"}}`, ""},
		{`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive","display":"summarized"},"output_config":"bad","context_management":null}`, ""},
		{`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive","display":"updates"}}`, "updates"},
	} {
		req, e := anth.ParseMessages(&h, []byte(c.body), nil, "anthropic")
		if e != nil {
			t.Fatal(e)
		}
		if !req.Reasoning.Requested || req.Reasoning.Effort != "" || req.Metadata[anth.MetaThinkingDisplay] != c.display || req.Metadata[anth.MetaContextManagement] != "" {
			t.Errorf("%s: reasoning %+v metadata %q", c.body, req.Reasoning, req.Metadata)
		}
		if c.display == "" && h.ForRequest(req, true) != &h {
			t.Errorf("%s: ForRequest copied the hooks without markers", c.body)
		}
	}
	if h.ForRequest(&core.Request{Metadata: map[string]string{"user_id": "u"}}, false) != &h {
		t.Error("ForRequest copied the hooks for metadata.user_id alone")
	}
	if h.ForRequest(nil, false) != &h {
		t.Error("ForRequest(nil) copied the hooks")
	}

	gw := anth.DefaultHooks()
	if _, e := anth.ParseMessages(&gw, body, nil, "opencode"); e == nil || e.Message != "messages.1.role: Input should be 'user' or 'assistant'" {
		t.Errorf("gateway: %v", e)
	}
	req, e = anth.ParseMessages(&gw, []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive","display":"omitted"},"context_management":{}}`), nil, "opencode")
	if e != nil || req.Reasoning.Requested || len(req.Metadata) != 0 {
		t.Errorf("gateway adaptive: %v %+v %q", e, req.Reasoning, req.Metadata)
	}
}

// TestClaudeCodeBlankThinking: with display omitted a second thinking
// block (after a text one) opens with its own empty delta, and the other
// writers pass through.
func TestClaudeCodeBlankThinking(t *testing.T) {
	h := anth.APIHooks()
	h.ClaudeCode = true
	req, e := anth.ParseMessages(&h, []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"adaptive","display":"omitted"}}`), nil, "anthropic")
	if e != nil {
		t.Fatal(e)
	}
	w := core.NewMemWriter()
	sw := anth.NewRequestStream(&h, w, req, nil)
	steps := []func() error{
		func() error { return sw.Start(&core.Response{ID: "msg_011Cblank", Model: "m"}) },
		func() error { return sw.ReasoningDelta("one") },
		func() error { return sw.ReasoningDelta("two") },
		func() error { return sw.TextDelta("ok") },
		func() error { return sw.ReasoningDelta("three") },
		func() error { return sw.Finish(core.FinishStop, core.Usage{}, true) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	body := string(w.Body())
	if strings.Contains(body, "one") || strings.Contains(body, "two") || strings.Contains(body, "three") {
		t.Errorf("reasoning text in %s", body)
	}
	if n := strings.Count(body, `"type":"thinking_delta","thinking":""`); n != 2 {
		t.Errorf("%d empty thinking deltas, want one per thinking block: %s", n, body)
	}
	plain := anth.NewRequestStream(&h, core.NewMemWriter(), nil, nil)
	if _, ok := plain.(*anth.Stream); !ok {
		t.Errorf("a request without markers gets %T", plain)
	}
}
