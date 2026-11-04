package opencode_test

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
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The Responses API of OpenCode Go recorded on 2026-10-31 (task 240,
// testdata/real/opencode/responses/): bodies and streams through the
// writers, the route's checks through ParseRequest and WriteError.

func loadResp(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "responses", name+".json"))
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
	reRespItem = regexp.MustCompile(`"(rs|msg|fc)_[0-9a-f]{32}"`)
	reTrace    = regexp.MustCompile(`trace_id: [0-9a-f]{32}`)
)

func normalizeResp(b string) string {
	b = reRespItem.ReplaceAllString(b, `"${1}_<id>"`)
	b = reTrace.ReplaceAllString(b, "trace_id: <trace>")
	return normalize(b)
}

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
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		Input   int `json:"input_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		Output     int `json:"output_tokens"`
		OutDetails struct {
			Reasoning int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (r realResp) response() *core.Response {
	resp := &core.Response{
		ID: r.ID, Model: r.Model, Created: time.Unix(r.CreatedAt, 0), FinishReason: core.FinishStop,
		Usage: core.Usage{PromptTokens: r.Usage.Input, CachedTokens: r.Usage.Details.Cached, CompletionTokens: r.Usage.Output, ReasoningTokens: r.Usage.OutDetails.Reasoning, CostMicroUSD: 27},
	}
	if r.Status == "incomplete" {
		resp.FinishReason = core.FinishLength
	}
	for _, it := range r.Output {
		switch it.Type {
		case "reasoning":
			resp.Reasoning = it.Summary[0].Text
		case "message":
			resp.Content = it.Content[0].Text
		case "function_call":
			resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: it.CallID, Name: it.Name, Arguments: it.Args})
			resp.FinishReason = core.FinishToolCalls
		}
	}
	return resp
}

func respRequest(t *testing.T, d driver.Driver, fx shapeFixture) *core.Request {
	t.Helper()
	req, err := d.ParseRequest(driver.KindResponses, []byte(fx.Request.Body), driver.MapHeaders{})
	if err != nil {
		t.Fatalf("%s: parse: %+v", fx.Name, err)
	}
	if req.Endpoint != core.EndpointChat || !req.Reasoning.Requested {
		t.Fatalf("%s: %q, reasoning %v: the portable route is a Chat call that reasons by default", fx.Name, req.Endpoint, req.Reasoning.Requested)
	}
	req.Headers = map[string]string{"x-wwz-path": fx.Request.Path}
	return req
}

// TestRealFixturesResponsesBody: the object byte for byte (item ids,
// trace ids and dollar figures by form).
func TestRealFixturesResponsesBody(t *testing.T) {
	d := newDriver(t)
	names := []string{"text", "function_call", "incomplete", "qwen_text", "plain_path"}
	for _, f := range []string{
		"background", "include", "instructions", "metadata", "parallel_tool_calls", "previous_response_id", "prompt_cache_key",
		"reasoning", "safety_identifier", "service_tier", "store", "temperature", "text", "tool_choice", "top_p", "truncation", "user",
	} {
		names = append(names, "field_"+f)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fx := loadResp(t, name)
			var real realResp
			if err := json.Unmarshal([]byte(fx.Response.Body), &real); err != nil {
				t.Fatal(err)
			}
			w := core.NewMemWriter()
			if err := d.WriteResponse(w, respRequest(t, d, fx), real.response(), nil); err != nil {
				t.Fatal(err)
			}
			if got, want := normalizeResp(string(w.Body())), normalizeResp(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

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

// TestRealFixturesResponsesStream: the events as the engine drives the
// StreamWriter; every done event waits for the end.
func TestRealFixturesResponsesStream(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"text_stream", "function_call_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadResp(t, name)
			w := replayRespStream(t, d, fx)
			if got, want := normalizeResp(string(w.Body())), normalizeResp(fx.Response.Body); got != want {
				t.Errorf("stream\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// replayRespStream drives the StreamWriter with the events of fx's stream
// the way the engine does.
func replayRespStream(t *testing.T, d driver.Driver, fx shapeFixture) *core.MemWriter {
	t.Helper()
	w := core.NewMemWriter()
	sw := d.StreamWriter(w, respRequest(t, d, fx), nil)
	for _, line := range strings.Split(fx.Response.Body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev realEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		var err error
		switch ev.Type {
		case "response.created":
			r := ev.Response.response()
			err = sw.Start(&core.Response{ID: r.ID, Model: r.Model, Created: r.Created})
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				err = sw.ToolCallStart(0, ev.Item.CallID, ev.Item.Name)
			}
		case "response.reasoning_summary_text.delta":
			err = sw.ReasoningDelta(ev.Delta)
		case "response.output_text.delta":
			err = sw.TextDelta(ev.Delta)
		case "response.function_call_arguments.delta":
			err = sw.ToolArgsDelta(0, ev.Delta)
		case "response.function_call_arguments.done":
			err = sw.ToolCallEnd(0)
		case "response.completed":
			r := ev.Response.response()
			err = sw.Finish(r.FinishReason, r.Usage, true)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// TestRealFixturesResponsesChecks: the portable route's checks with the
// real status, envelope and headers.
func TestRealFixturesResponsesChecks(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{
		"invalid_json", "missing_model", "type_model", "empty_object", "missing_input", "order_unknown_before_missing_input",
		"type_input", "input_empty_array", "field_stream_options", "unknown_param", "chat_messages_field", "field_conversation",
		"field_max_tool_calls", "field_prompt_cache_retention", "field_top_logprobs", "store_true", "previous_response_id",
		"order_store_before_unknown", "input_empty_string", "type_max_output_tokens",
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
			if got, want := normalizeResp(string(w.Body())), normalizeResp(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesResponsesEngineErrors: the model, key and session errors
// are the Chat route's.
func TestRealFixturesResponsesEngineErrors(t *testing.T) {
	d := newDriver(t)
	for _, c := range []struct {
		name string
		e    core.Error
	}{
		{"unknown_model", core.Error{Kind: core.ErrModelNotFound}},
		{"error_auth", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"no_session", core.Error{Kind: core.ErrMissingSession}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := loadResp(t, c.name)
			hdr := driver.MapHeaders{"x-wwz-path": fx.Request.Path}
			driver.SetAccount(hdr, driver.Account{Key: "oc_sk_invalid-wwz-drift"})
			w := core.NewMemWriter()
			d.WriteError(w, &core.Request{Endpoint: core.EndpointChat, Headers: hdr}, &c.e, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, want := string(w.Body()), fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestResponsesRoutesAndIDs: both paths on every base, resp_ + 32 hex and
// call_00_ + 26 base-36 on the route.
func TestResponsesRoutesAndIDs(t *testing.T) {
	d := newDriver(t)
	want := map[string]bool{}
	for _, base := range []string{"/inference", "/inference/go", "/zen", "/zen/go"} {
		want[base+"/openai/v1/responses"], want[base+"/v1/responses"] = true, true
	}
	for _, r := range d.Routes() {
		if r.Kind == driver.KindResponses && r.Method == "POST" {
			delete(want, r.Path)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing routes %v", want)
	}
	m := d.(driver.IDMinter)
	rng := generate.NewRNG(1, "t")
	if id := m.ResponseID(driver.KindResponses, rng, time.Now()); !regexp.MustCompile(`^resp_[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("response id %q", id)
	}
	if id := m.ToolCallID(driver.KindResponses, rng); !regexp.MustCompile(`^call_00_[0-9a-z]{26}$`).MatchString(id) {
		t.Errorf("call id %q", id)
	}
}
