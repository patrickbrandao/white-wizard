package openrouter_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The Responses API of OpenRouter recorded on 2026-10-31 (task 240,
// testdata/real/openrouter/responses/): bodies and streams through the
// writers, every check of the route through ParseRequest and WriteError.

func loadResp(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "responses", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx shapeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

var (
	reItem      = regexp.MustCompile(`(msg|fc|rs)_tmp_[a-z0-9]{10,11}`)
	reCompleted = regexp.MustCompile(`"completed_at":[0-9]+`)
	// The real writes one keep-alive block per second of wait.
	rePrefix = regexp.MustCompile(`^(\n {9}\n)+`)
)

func normalizeResp(b string) string {
	b = rePrefix.ReplaceAllString(b, "<keep-alive>")
	b = reItem.ReplaceAllString(b, "${1}_tmp_<id>")
	b = reCompleted.ReplaceAllString(b, `"completed_at":<ts>`)
	return normalize(b)
}

// realResp is the decoded real response object.
type realResp struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Output    []struct {
		Type    string `json:"type"`
		CallID  string `json:"call_id"`
		Name    string `json:"name"`
		Args    string `json:"arguments"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

func (r realResp) response(t *testing.T) *core.Response {
	t.Helper()
	resp := &core.Response{
		ID: r.ID, Model: r.Model, Created: time.Unix(r.CreatedAt, 0), FinishReason: core.FinishStop,
		Usage: core.Usage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output},
	}
	if r.Model != "" {
		resp.Usage = priced(t, r.Model, resp.Usage)
	}
	if r.Status == "incomplete" {
		resp.FinishReason = core.FinishLength
	}
	for _, it := range r.Output {
		switch it.Type {
		case "message":
			resp.Content = it.Content[0].Text
		case "function_call":
			resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: it.CallID, Name: it.Name, Arguments: it.Args})
			resp.FinishReason = core.FinishToolCalls
		}
	}
	return resp
}

// respRequest parses the fixture's request the way the engine does and
// adds what the engine sets (route, request id).
func respRequest(t *testing.T, d driver.Driver, fx shapeFixture) *core.Request {
	t.Helper()
	req, err := d.ParseRequest(driver.KindResponses, []byte(fx.Request.Body), driver.MapHeaders{})
	if err != nil {
		t.Fatalf("%s: parse: %+v", fx.Name, err)
	}
	if req.Endpoint != core.EndpointChat {
		t.Fatalf("%s: endpoint %q: the gateway translates to Chat", fx.Name, req.Endpoint)
	}
	req.Headers = map[string]string{"x-wwz-path": "/api/v1/responses"}
	req.RequestID = "gen-1793458702-AAAAAAAAAAAAAAAAAAAA"
	return req
}

// TestRealFixturesResponsesBody: the non-streaming object byte for byte
// (keep-alive count, item ids and completed_at by form; cost figures by
// value, task 246).
func TestRealFixturesResponsesBody(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{
		"text", "instructions", "incomplete", "function_call", "input_items", "short_model_id", "models_field",
		"store_false", "unknown_param", "invalid_json", "max_output_tokens_below_min",
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadResp(t, name)
			var real realResp
			if err := json.Unmarshal([]byte(strings.TrimSpace(fx.Response.Body)), &real); err != nil {
				t.Fatal(err)
			}
			req := respRequest(t, d, fx)
			resp := real.response(t)
			w := core.NewMemWriter()
			if err := d.WriteResponse(w, req, resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, want := normalizeResp(string(w.Body())), normalizeResp(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			if w.Headers()["x-generation-id"] != resp.ID {
				t.Errorf("x-generation-id %q", w.Headers()["x-generation-id"])
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// realEvent is one decoded stream event.
type realEvent struct {
	Type     string   `json:"type"`
	Response realResp `json:"response"`
	Delta    string   `json:"delta"`
	Item     struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
}

// TestRealFixturesResponsesStream replays each stream through the
// StreamWriter as the engine drives it and compares the bytes.
func TestRealFixturesResponsesStream(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"text_stream", "function_call_stream", "incomplete_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadResp(t, name)
			req := respRequest(t, d, fx)
			w := core.NewMemWriter()
			sw := d.StreamWriter(w, req, nil)
			for _, line := range strings.Split(fx.Response.Body, "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok || data == "[DONE]" {
					continue
				}
				var ev realEvent
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					t.Fatal(err)
				}
				var err error
				switch ev.Type {
				case "response.created":
					r := ev.Response.response(t)
					err = sw.Start(&core.Response{ID: r.ID, Model: r.Model, Created: r.Created})
				case "response.output_item.added":
					if ev.Item.Type == "function_call" {
						err = sw.ToolCallStart(0, ev.Item.CallID, ev.Item.Name)
					}
				case "response.output_text.delta":
					err = sw.TextDelta(ev.Delta)
				case "response.function_call_arguments.delta":
					err = sw.ToolArgsDelta(0, ev.Delta)
				case "response.completed", "response.incomplete":
					r := ev.Response.response(t)
					err = sw.Finish(r.FinishReason, r.Usage, true)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, want := normalizeResp(string(w.Body())), normalizeResp(fx.Response.Body); got != want {
				t.Errorf("stream\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesResponsesChecks: the route's own checks, in the real
// order, with the real envelopes.
func TestRealFixturesResponsesChecks(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{
		"empty_body", "non_object", "empty_object", "invalid_json_truncated", "type_model", "type_input",
		"type_max_output_tokens", "type_stream", "range_temperature", "store_true", "previous_response_id",
		"multiple_issues", "order_type_before_model", "order_type_before_unknown_model", "order_input_before_model",
		"missing_input", "chat_messages_field", "input_empty_string", "input_empty_array", "missing_model",
		"strict_true_no_additional",
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadResp(t, name)
			req, perr := d.ParseRequest(driver.KindResponses, []byte(fx.Request.Body), driver.MapHeaders{})
			if perr == nil {
				t.Fatalf("accepted: %+v", req)
			}
			w := core.NewMemWriter()
			d.WriteError(w, &core.Request{Headers: map[string]string{}}, perr, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			got, want := string(w.Body()), fx.Response.Body
			if name == "strict_true_no_additional" {
				got = strings.Replace(got, `"provider_name":"WhiteWizard"`, `"provider_name":"OpenAI"`, 1)
			}
			if got, want := normalize(got), normalize(want); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesResponsesEngineErrors: the errors past the parse (model,
// key) are the Chat route's (unknown_model, previous_response_id_null,
// error_auth, error_nokey).
func TestRealFixturesResponsesEngineErrors(t *testing.T) {
	d := newDriver(t)
	for _, c := range []struct {
		name string
		e    core.Error
		key  string
	}{
		{"unknown_model", core.Error{Kind: core.ErrModelNotFound}, ""},
		{"previous_response_id_null", core.Error{Kind: core.ErrModelNotFound}, ""},
		{"error_auth", core.Error{Kind: core.ErrInvalidAPIKey}, "sk-or-v1-" + strings.Repeat("0", 64)},
		{"error_nokey", core.Error{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := loadResp(t, c.name)
			hdr := driver.MapHeaders{}
			req := &core.Request{Headers: hdr}
			if c.e.Kind == core.ErrModelNotFound {
				req = respRequest(t, d, fx)
			} else {
				driver.SetAccount(hdr, driver.Account{Key: c.key})
			}
			w := core.NewMemWriter()
			d.WriteError(w, req, &c.e, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestResponsesRoute: the route is served and is stateless; the request
// keeps the body for the echo and leaves the engine's Responses state off.
func TestResponsesRoute(t *testing.T) {
	d := newDriver(t)
	found := false
	for _, r := range d.Routes() {
		found = found || (r.Method == "POST" && r.Path == "/api/v1/responses" && r.Kind == driver.KindResponses)
	}
	if !found {
		t.Fatal("no /api/v1/responses route")
	}
	req, err := d.ParseRequest(driver.KindResponses, []byte(`{"model":"openai/gpt-4o-mini","input":[{"role":"developer","content":"x"},{"role":"user","content":"hi"}],"stream":true}`), driver.MapHeaders{})
	if err != nil || !req.Stream || len(req.Messages) != 2 {
		t.Fatalf("%+v %+v", req, err)
	}
	// A Chat request outside the route keeps the Chat writers.
	req.Headers = map[string]string{"x-wwz-path": "/api/v1/chat/completions"}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, req, &core.Response{ID: "gen-1", Model: "openai/gpt-4o-mini", Content: "x"}, nil); err != nil || !strings.Contains(string(w.Body()), `"chat.completion"`) {
		t.Errorf("chat body: %v %s", err, w.Body())
	}
}
