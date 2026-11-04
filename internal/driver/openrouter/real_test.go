package openrouter_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// shapeFixture is a real answer recorded on 2026-10-31 (task 174) in
// testdata/real/openrouter/shape/, normalized as ADR 0014 says: the
// generation, call, message and user ids are markers. What depends on the
// upstream (provider) is compared by form only (see normalize); the cost
// figures are compared by value (task 246), the usage priced as the engine
// prices it (see priced).
type shapeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

func loadShape(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "openrouter", "shape", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx shapeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

func newDriver(t *testing.T) driver.Driver {
	t.Helper()
	d, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var (
	reGenID    = regexp.MustCompile(`gen-[0-9]{10}-[A-Za-z0-9]{20}`)
	reCallID   = regexp.MustCompile(`call_[A-Za-z0-9]{24}`)
	reMsgID    = regexp.MustCompile(`msg_01[A-Za-z0-9]{22}`)
	reUserID   = regexp.MustCompile(`"user_id":"user_[A-Za-z0-9]+"`)
	reProvider = regexp.MustCompile(`"provider":"[^"]+"`)
)

// prices are the list prices (USD per million tokens, input and output) of
// the fixtures' models on 2026-10-31.
var prices = map[string][2]float64{
	"openai/gpt-4.1-nano":        {0.1, 0.4},
	"openai/gpt-4o-mini":         {0.15, 0.6},
	"anthropic/claude-haiku-4.5": {1, 5},
}

// priced is u with the cost the engine puts in it for model (spec 007:
// per-million price x tokens, in pico-dollars, split).
func priced(t *testing.T, model string, u core.Usage) core.Usage {
	t.Helper()
	p, ok := prices[model]
	if !ok {
		t.Fatalf("no price for %s", model)
	}
	u.PromptCostPicoUSD = int64(math.Round(float64(u.PromptTokens) * p[0] * 1e6))
	u.CompletionCostPicoUSD = int64(math.Round(float64(u.CompletionTokens) * p[1] * 1e6))
	u.CostMicroUSD = (u.PromptCostPicoUSD + u.CompletionCostPicoUSD) / 1e6
	return u
}

// normalize brings a body (wwz or real) to the fixture's markers.
func normalize(b string) string {
	b = reGenID.ReplaceAllString(b, "<generation_id>")
	b = reCallID.ReplaceAllString(b, "<call_id>")
	b = reMsgID.ReplaceAllString(b, "<msg_id>")
	b = reUserID.ReplaceAllString(b, `"user_id":"<user_id>"`)
	return reProvider.ReplaceAllString(b, `"provider":"<provider>"`)
}

// checkHeaders compares the dialect headers of the wwz answer with the
// fixture's: the same names, the same fixed values, the generation id in
// its real form.
func checkHeaders(t *testing.T, fx shapeFixture, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		switch k = strings.ToLower(k); k {
		case "content-type", "cache-control", "x-generation-id", "request-id", "access-control-expose-headers", "access-control-allow-origin", "x-request-id":
			low[k] = v
		}
	}
	for k, want := range fx.Response.Headers {
		v, ok := low[k]
		switch {
		case !ok:
			t.Errorf("%s: header %s missing (real %q)", fx.Name, k, want)
		case want == "<generation_id>" && !reGenID.MatchString(v):
			t.Errorf("%s: %s = %q, not a generation id", fx.Name, k, v)
		case want != "<generation_id>" && v != want:
			t.Errorf("%s: %s = %q, real %q", fx.Name, k, v, want)
		}
	}
	for k := range low {
		if _, ok := fx.Response.Headers[k]; !ok {
			t.Errorf("%s: header %s not in the real answer", fx.Name, k)
		}
	}
}

// realChat is the decoded real chat body (or chunk).
type realChat struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string   `json:"finish_reason"`
		Message      realDelta `json:"message"`
		Delta        realDelta `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
	} `json:"usage"`
}

type realDelta struct {
	Content   *string `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

var chatFinish = map[string]core.FinishReason{"stop": core.FinishStop, "length": core.FinishLength, "tool_calls": core.FinishToolCalls}

func chatReq(stream bool) *core.Request {
	return &core.Request{
		Driver: "openrouter", Endpoint: core.EndpointChat, Model: "openai/gpt-4.1-nano", MaxTokens: 16, Stream: stream,
		Headers: map[string]string{"authorization": "Bearer wwz-key-drift"}, RequestID: "gen-1793454927-AAAAAAAAAAAAAAAAAAAA", Received: time.Now(),
	}
}

// TestRealFixturesChat: the non-streaming body is the real one byte for
// byte, keep-alive whitespace included, with x-generation-id equal to the
// body's id.
func TestRealFixturesChat(t *testing.T) {
	for _, name := range []string{"chat", "max_tokens", "tool_use"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			var m realChat
			if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
				t.Fatal(err)
			}
			c := m.Choices[0]
			resp := &core.Response{
				ID: "gen-1793454927-MNojMTgv1yOGFqo3uVjP", Model: m.Model, Created: time.Unix(m.Created, 0),
				FinishReason: chatFinish[*c.FinishReason], Usage: priced(t, m.Model, core.Usage{PromptTokens: m.Usage.Prompt, CompletionTokens: m.Usage.Completion}),
			}
			if c.Message.Content != nil {
				resp.Content = *c.Message.Content
			}
			for _, tc := range c.Message.ToolCalls {
				resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: "call_AAAAAAAAAAAAAAAAAAAAAAAA", Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			w := core.NewMemWriter()
			if err := newDriver(t).WriteResponse(w, chatReq(false), resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %q\n real %q", got, want)
			}
			if w.Headers()["x-generation-id"] != resp.ID {
				t.Errorf("x-generation-id %q, want the body's id %q", w.Headers()["x-generation-id"], resp.ID)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesStreams replays each recorded stream through the driver's
// StreamWriter the way the engine drives it (include_usage off: the usage
// chunk comes anyway) and compares the bytes.
func TestRealFixturesStreams(t *testing.T) {
	for _, name := range []string{"chat_stream", "max_tokens_stream", "tool_use_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			w := core.NewMemWriter()
			sw := newDriver(t).StreamWriter(w, chatReq(true), nil)
			started, tools := false, 0
			var finish core.FinishReason
			for _, line := range strings.Split(fx.Response.Body, "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok || data == "[DONE]" {
					continue
				}
				var ch realChat
				if err := json.Unmarshal([]byte(data), &ch); err != nil {
					t.Fatal(err)
				}
				if !started {
					started = true
					if err := sw.Start(&core.Response{ID: "gen-1793454928-D94cRWf17UlwsOhNsacg", Model: ch.Model, Created: time.Unix(ch.Created, 0)}); err != nil {
						t.Fatal(err)
					}
				}
				c := ch.Choices[0]
				if ch.Usage != nil {
					if err := sw.Finish(finish, priced(t, ch.Model, core.Usage{PromptTokens: ch.Usage.Prompt, CompletionTokens: ch.Usage.Completion}), false); err != nil {
						t.Fatal(err)
					}
					break
				}
				if c.FinishReason != nil {
					finish = chatFinish[*c.FinishReason]
					continue // Finish sends the closing chunk and the usage chunk
				}
				var err error
				switch {
				case len(c.Delta.ToolCalls) > 0 && c.Delta.ToolCalls[0].ID != "":
					err = sw.ToolCallStart(tools, "call_AAAAAAAAAAAAAAAAAAAAAAAA", c.Delta.ToolCalls[0].Function.Name)
				case len(c.Delta.ToolCalls) > 0:
					err = sw.ToolArgsDelta(tools, c.Delta.ToolCalls[0].Function.Arguments)
				case c.Delta.Content != nil:
					err = sw.TextDelta(*c.Delta.Content)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("stream\n wwz  %q\n real %q", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesErrors: the 400 of an unknown model (with
// x-generation-id) and the 401s by key form (without), on both routes.
func TestRealFixturesErrors(t *testing.T) {
	d := newDriver(t)
	cases := []struct {
		name     string
		endpoint core.Endpoint
		model    string
		e        core.Error
	}{
		{"error_model", core.EndpointChat, "wwz-drift-modelo-inexistente", core.Error{Kind: core.ErrModelNotFound}},
		{"error_auth", core.EndpointChat, "openai/gpt-4.1-nano", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"messages_model", core.EndpointMessages, "wwz-drift-modelo-inexistente", core.Error{Kind: core.ErrModelNotFound}},
		{"messages_nokey", core.EndpointMessages, "wwz-nao-existe", core.Error{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."}},
		{"messages_wellformed", core.EndpointMessages, "wwz-nao-existe", core.Error{Kind: core.ErrInvalidAPIKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := loadShape(t, c.name)
			hdr := driver.MapHeaders{}
			for k, v := range fx.Request.Headers {
				hdr[k] = v
			}
			req := &core.Request{Driver: "openrouter", Endpoint: c.endpoint, Model: c.model, Headers: hdr}
			if c.e.Kind != core.ErrInvalidAPIKey {
				req.RequestID = "gen-1793454937-xyWpOVtp0le8jZFwbkNH"
			}
			w := core.NewMemWriter()
			d.WriteError(w, req, &c.e, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealAuthByKeyForm: the 401 text follows the key presented (spec 006
// 16.7): none, outside sk-or-v1-<64 hex>, or well formed.
func TestRealAuthByKeyForm(t *testing.T) {
	d := newDriver(t)
	for key, want := range map[string]string{
		"":                                    "No cookie auth credentials found",
		"sk-invalid-wwz-drift":                "Missing Authentication header",
		"sk-or-v1-" + strings.Repeat("0", 64): "User not found.",
		"sk-or-v1-" + strings.Repeat("A", 64): "Missing Authentication header",
	} {
		hdr := driver.MapHeaders{}
		driver.SetAccount(hdr, driver.Account{Key: key})
		w := core.NewMemWriter()
		d.WriteError(w, &core.Request{Endpoint: core.EndpointChat, Headers: hdr}, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
		if body := `{"error":{"message":"` + want + `","code":401}}`; string(w.Body()) != body {
			t.Errorf("key %q: %s, want %s", key, w.Body(), body)
		}
	}
	// Engine texts other than the missing key are kept, in the same envelope.
	w := core.NewMemWriter()
	d.WriteError(w, &core.Request{Endpoint: core.EndpointChat}, &core.Error{Kind: core.ErrInvalidAPIKey, Message: "This API key has expired."}, nil)
	if string(w.Body()) != `{"error":{"message":"This API key has expired.","code":401}}` {
		t.Errorf("expired: %s", w.Body())
	}
}

// TestRealAuthenticateXAPIKey: x-api-key is accepted on both routes
// (fixtures chat_xapikey and messages_xapikey answer the model's 400).
func TestRealAuthenticateXAPIKey(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"chat_xapikey", "messages_xapikey"} {
		fx := loadShape(t, name)
		if fx.Response.Status != 400 || fx.Request.Headers["x-api-key"] == "" {
			t.Fatalf("%s: fixture is not the x-api-key probe", name)
		}
	}
	if c := d.Authenticate(driver.MapHeaders{"x-api-key": "k1"}); c.Key != "k1" {
		t.Errorf("x-api-key: %+v", c)
	}
	if c := d.Authenticate(driver.MapHeaders{"authorization": "Bearer k2", "x-api-key": "k1"}); c.Key != "k2" {
		t.Errorf("Authorization wins: %+v", c)
	}
}

func messagesReq(stream bool) *core.Request {
	return &core.Request{
		Driver: "openrouter", Endpoint: core.EndpointMessages, Model: "anthropic/claude-haiku-4.5", MaxTokens: 16, Stream: stream,
		Headers: map[string]string{"authorization": "Bearer wwz-key-drift"}, RequestID: "gen-1793454958-VO6mh6wBDlr14h5lye9l", Received: time.Now(),
	}
}

// TestRealFixturesMessages: /api/v1/messages in the gateway's form (body
// and stream), request-id and x-generation-id carrying the generation id.
func TestRealFixturesMessages(t *testing.T) {
	d := newDriver(t)
	usage := priced(t, "anthropic/claude-haiku-4.5", core.Usage{PromptTokens: 13, CompletionTokens: 4})
	resp := &core.Response{ID: "msg_01AAAAAAAAAAAAAAAAAAAAAA", Model: "anthropic/claude-haiku-4.5", Content: "ok", FinishReason: core.FinishStop, Usage: usage}

	fx := loadShape(t, "messages")
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, messagesReq(false), resp, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
		t.Errorf("body\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, fx, w.Headers())

	fx = loadShape(t, "messages_stream")
	w = core.NewMemWriter()
	sw := d.StreamWriter(w, messagesReq(true), nil)
	for _, err := range []error{
		sw.Start(&core.Response{ID: resp.ID, Model: resp.Model, Usage: core.Usage{PromptTokens: 13}}),
		sw.TextDelta("ok"),
		sw.Finish(core.FinishStop, usage, true),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
		t.Errorf("stream\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, fx, w.Headers())
	if h := w.Headers(); h["request-id"] != "gen-1793454958-VO6mh6wBDlr14h5lye9l" || h["x-generation-id"] != h["request-id"] {
		t.Errorf("request-id %q, x-generation-id %q: both the generation id", h["request-id"], h["x-generation-id"])
	}
}

// realModel is the item of the probe's model.
type realModel struct {
	ID            string          `json:"id"`
	CanonicalSlug string          `json:"canonical_slug"`
	Name          string          `json:"name"`
	Created       int64           `json:"created"`
	Description   string          `json:"description"`
	ContextLength int             `json:"context_length"`
	Architecture  json.RawMessage `json:"architecture"`
	TopProvider   struct {
		MaxCompletion int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	SupportedParameters json.RawMessage `json:"supported_parameters"`
	KnowledgeCutoff     string          `json:"knowledge_cutoff"`
	Endpoints           json.RawMessage `json:"endpoints"`
	Benchmarks          json.RawMessage `json:"benchmarks"`
}

// info is the configuration that lists m: snapshot in version, prices in
// pricing, the rest in models[].extra.
func (m realModel) info() driver.ModelInfo {
	return driver.ModelInfo{Cfg: &config.ModelConfig{
		ID: m.ID, Version: strings.TrimPrefix(m.CanonicalSlug, "openai/"), Created: m.Created,
		ContextWindow: m.ContextLength, MaxOutput: m.TopProvider.MaxCompletion,
		Pricing: config.Pricing{InputPerM: 0.1, OutputPerM: 0.4, CacheReadPerM: 0.03},
		Extra: map[string]any{
			"name": m.Name, "description": m.Description, "architecture": m.Architecture,
			"pricing":              map[string]any{"web_search": "0.01"},
			"supported_parameters": m.SupportedParameters, "knowledge_cutoff": m.KnowledgeCutoff,
			"endpoints": m.Endpoints, "benchmarks": m.Benchmarks,
		},
	}}
}

// TestRealFixturesModels: the listing ({data, total_count, links}) and one
// model come out byte for byte from a configuration with the same version,
// prices and extra fields; zero prices are omitted.
func TestRealFixturesModels(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "models")
	var list struct {
		Data []realModel `json:"data"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &list); err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	d.ListModels(w, nil, []driver.ModelInfo{list.Data[0].info()})
	if string(w.Body()) != fx.Response.Body {
		t.Errorf("listing\n wwz  %s\n real %s", w.Body(), fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())

	fx = loadShape(t, "model_one")
	info := list.Data[0].info()
	w = core.NewMemWriter()
	d.GetModel(w, nil, &info, "openai/gpt-4.1-nano")
	if string(w.Body()) != fx.Response.Body {
		t.Errorf("one\n wwz  %s\n real %s", w.Body(), fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())
}

// TestRealFixturesShapeResponses: the responses probes of wwz-drift record
// (shape/responses*.json, task 387), byte for byte like the Responses
// fixtures (keep-alive, item ids and completed_at by form).
func TestRealFixturesShapeResponses(t *testing.T) {
	d := newDriver(t)
	// The recorder kept the id as a marker: the engine mints one.
	const genID = "gen-1793465700-AAAAAAAAAAAAAAAAAAAA"
	compare := func(what, got, want string) {
		got, want = normalizeResp(got), normalizeResp(want)
		if got != want {
			t.Errorf("%s\n wwz  %s\n real %s", what, got, want)
		}
	}
	fx := loadShape(t, "responses")
	var real realResp
	if err := json.Unmarshal([]byte(strings.TrimSpace(fx.Response.Body)), &real); err != nil {
		t.Fatal(err)
	}
	resp := real.response(t)
	resp.ID = genID
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, respRequest(t, d, fx), resp, nil); err != nil {
		t.Fatal(err)
	}
	compare("body", string(w.Body()), fx.Response.Body)
	checkHeaders(t, fx, w.Headers())

	fx = loadShape(t, "responses_stream")
	w = core.NewMemWriter()
	sw := d.StreamWriter(w, respRequest(t, d, fx), nil)
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
			err = sw.Start(&core.Response{ID: genID, Model: r.Model, Created: r.Created})
		case "response.output_text.delta":
			err = sw.TextDelta(ev.Delta)
		case "response.completed", "response.incomplete":
			r := ev.Response.response(t)
			err = sw.Finish(r.FinishReason, r.Usage, true)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	compare("stream", string(w.Body()), fx.Response.Body)
	checkHeaders(t, fx, w.Headers())
}
