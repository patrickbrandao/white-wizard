package opencode_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// shapeFixture is a real answer recorded on 2026-10-31 (task 175) in
// testdata/real/opencode/shape/, normalized as ADR 0014 says: the response,
// call and log ids are markers. The dollar figures depend on the price
// table and on the engine's micro-dollar precision, so they are compared
// by form (see normalize).
type shapeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Path    string            `json:"path"`
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
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "shape", name+".json"))
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
	d, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var (
	reGw    = regexp.MustCompile(`gw-[0-9a-f]{12}-[0-9a-f]{6}`)
	reCall  = regexp.MustCompile(`call_[0-9a-f]{24}`)
	reMsgID = regexp.MustCompile(`"id":"[0-9a-f]{32}"`)
	reLogID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	reCost  = regexp.MustCompile(`"(list_cost|allowance)":[0-9.e+-]+`)
	// /v1/models: created is the instant of the request.
	reCreated = regexp.MustCompile(`"created":[0-9]+`)
)

// normalize brings a body (wwz or real) to the fixture's markers.
func normalize(b string) string {
	b = reGw.ReplaceAllString(b, "<gw_id>")
	b = reCall.ReplaceAllString(b, "<call_id>")
	b = reMsgID.ReplaceAllString(b, `"id":"<msg_id>"`)
	return reCost.ReplaceAllString(b, `"$1":<n>`)
}

// relevant are the headers compared with the fixture's.
var relevant = map[string]bool{
	"content-type": true, "cache-control": true, "x-opencode-log-id": true, "x-zen-model": true,
	"access-control-allow-origin": true, "access-control-allow-headers": true, "access-control-allow-methods": true,
	"x-request-id": true, "request-id": true,
}

// checkHeaders compares the headers of the wwz answer with the fixture's:
// the same names, the same values, the log id a UUIDv7.
func checkHeaders(t *testing.T, fx shapeFixture, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		if k = strings.ToLower(k); relevant[k] {
			low[k] = v
		}
	}
	for k, want := range fx.Response.Headers {
		v, ok := low[k]
		switch {
		case !ok:
			t.Errorf("%s: header %s missing (real %q)", fx.Name, k, want)
		case want == "<log_id>" && !reLogID.MatchString(v):
			t.Errorf("%s: %s = %q, not a UUIDv7", fx.Name, k, v)
		case want != "<log_id>" && v != want:
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
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string   `json:"finish_reason"`
		Message      realDelta `json:"message"`
		Delta        realDelta `json:"delta"`
	} `json:"choices"`
	Usage *realUsage `json:"usage"`
}

type realUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Prompt2    struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	Completion2 struct {
		Reasoning int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u realUsage) core() core.Usage {
	return core.Usage{PromptTokens: u.Prompt, CompletionTokens: u.Completion, CachedTokens: u.Prompt2.Cached, ReasoningTokens: u.Completion2.Reasoning, CostMicroUSD: 13}
}

type realDelta struct {
	Content          *string `json:"content"`
	ReasoningContent *string `json:"reasoning_content"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

var chatFinish = map[string]core.FinishReason{"stop": core.FinishStop, "length": core.FinishLength, "tool_calls": core.FinishToolCalls}

const goPath = "/inference/go/openai/v1/chat/completions"

func chatReq(stream bool) *core.Request {
	return &core.Request{
		Driver: "opencode", Endpoint: core.EndpointChat, Model: "deepseek-v4.1-flash", MaxTokens: 16, Stream: stream,
		Headers: map[string]string{"authorization": "Bearer oc_sk_drift", "x-wwz-path": goPath}, RequestID: "req_1", Received: time.Now(),
	}
}

const (
	gwID   = "gw-76b2cf3fbcc1-477bf4"
	callID = "call_b4708f99a1b7488da0301502"
)

// TestRealFixturesChat: the non-streaming body is the real one byte for
// byte, with x-opencode-log-id and x-zen-model.
func TestRealFixturesChat(t *testing.T) {
	for _, name := range []string{"chat", "max_tokens", "tool_use"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			var m realChat
			if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
				t.Fatal(err)
			}
			c := m.Choices[0]
			resp := &core.Response{ID: gwID, Model: m.Model, Created: time.Unix(m.Created, 0), FinishReason: chatFinish[*c.FinishReason], Usage: m.Usage.core()}
			if c.Message.Content != nil {
				resp.Content = *c.Message.Content
			}
			if c.Message.ReasoningContent != nil {
				resp.Reasoning = *c.Message.ReasoningContent
			}
			for _, tc := range c.Message.ToolCalls {
				resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: callID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			w := core.NewMemWriter()
			if err := newDriver(t).WriteResponse(w, chatReq(false), resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesStreams replays each recorded stream through the
// driver's StreamWriter the way the engine drives it and compares the
// bytes: usage on the closing chunk, with or without include_usage.
func TestRealFixturesStreams(t *testing.T) {
	for _, name := range []string{"chat_stream", "chat_stream_usage", "tool_use_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			req := chatReq(true)
			req.IncludeUsage = name == "chat_stream_usage"
			w := core.NewMemWriter()
			sw := newDriver(t).StreamWriter(w, req, nil)
			started, tools := false, -1
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
					if err := sw.Start(&core.Response{ID: gwID, Model: ch.Model, Created: time.Unix(ch.Created, 0)}); err != nil {
						t.Fatal(err)
					}
					continue // the role chunk
				}
				c := ch.Choices[0]
				var err error
				switch d := c.Delta; {
				case c.FinishReason != nil:
					err = sw.Finish(chatFinish[*c.FinishReason], ch.Usage.core(), req.IncludeUsage)
				case len(d.ToolCalls) > 0 && d.ToolCalls[0].ID != "":
					tools++
					err = sw.ToolCallStart(tools, callID, d.ToolCalls[0].Function.Name)
				case len(d.ToolCalls) > 0:
					err = sw.ToolArgsDelta(tools, d.ToolCalls[0].Function.Arguments)
				case d.ReasoningContent != nil:
					err = sw.ReasoningDelta(*d.ReasoningContent)
				case d.Content != nil:
					err = sw.TextDelta(*d.Content)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
				t.Errorf("stream\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

func messagesReq(stream bool) *core.Request {
	return &core.Request{
		Driver: "opencode", Endpoint: core.EndpointMessages, Model: "deepseek-v4.1-flash", MaxTokens: 16, Stream: stream,
		Headers: map[string]string{"authorization": "Bearer oc_sk_drift", "x-wwz-path": "/inference/go/anthropic/v1/messages"}, RequestID: "req_1", Received: time.Now(),
	}
}

// TestRealFixturesMessages: the Messages route in the gateway's
// translation (body and stream): no signature, no ping, no service_tier,
// usage with cost and opencode (reasoning 0).
func TestRealFixturesMessages(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "messages")
	var m struct {
		Content []struct {
			Thinking string `json:"thinking"`
			Text     string `json:"text"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
		t.Fatal(err)
	}
	usage := core.Usage{PromptTokens: m.Usage.In, CompletionTokens: m.Usage.Out, ReasoningTokens: 10, CostMicroUSD: 12}
	resp := &core.Response{ID: "df8331f9128e746819ca73a8071b45d7", Model: "deepseek-v4.1-flash", Reasoning: m.Content[0].Thinking, Content: m.Content[1].Text, FinishReason: core.FinishStop, Usage: usage}
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
	steps := []error{sw.Start(&core.Response{ID: resp.ID, Model: resp.Model, Usage: core.Usage{PromptTokens: 35}})}
	for _, line := range strings.Split(fx.Response.Body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
				Text     string `json:"text"`
			} `json:"delta"`
			Usage struct {
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		switch {
		case ev.Delta.Type == "thinking_delta":
			steps = append(steps, sw.ReasoningDelta(ev.Delta.Thinking))
		case ev.Delta.Type == "text_delta":
			steps = append(steps, sw.TextDelta(ev.Delta.Text))
		case ev.Type == "message_delta":
			steps = append(steps, sw.Finish(core.FinishStop, core.Usage{PromptTokens: 35, CompletionTokens: ev.Usage.Out, CostMicroUSD: 12}, false))
		}
	}
	for _, err := range steps {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, want := normalize(string(w.Body())), normalize(fx.Response.Body); got != want {
		t.Errorf("stream\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, fx, w.Headers())
}

// TestRealFixturesErrors: the envelopes by route, byte for byte.
func TestRealFixturesErrors(t *testing.T) {
	d := newDriver(t)
	cases := []struct {
		name     string
		endpoint core.Endpoint
		e        core.Error
	}{
		{"error_auth", core.EndpointChat, core.Error{Kind: core.ErrInvalidAPIKey}},
		{"zen_chat_path", core.EndpointChat, core.Error{Kind: core.ErrInvalidAPIKey}},
		{"error_nokey", core.EndpointChat, core.Error{Kind: core.ErrInvalidAPIKey, Message: "You didn't provide an API key."}},
		{"error_model", core.EndpointChat, core.Error{Kind: core.ErrModelNotFound}},
		{"error_model_validkey", core.EndpointChat, core.Error{Kind: core.ErrModelNotFound}},
		{"no_session", core.EndpointChat, core.Error{Kind: core.ErrMissingSession}},
		{"messages_auth", core.EndpointMessages, core.Error{Kind: core.ErrInvalidAPIKey}},
		{"messages_model", core.EndpointMessages, core.Error{Kind: core.ErrModelNotFound}},
		{"messages_no_session", core.EndpointMessages, core.Error{Kind: core.ErrMissingSession}},
		{"models_badkey", "", core.Error{Kind: core.ErrInvalidAPIKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := loadShape(t, c.name)
			hdr := driver.MapHeaders{}
			for k, v := range fx.Request.Headers {
				hdr[k] = v
			}
			req := &core.Request{Driver: "opencode", Endpoint: c.endpoint, Model: "deepseek-v4.1-flash", Headers: hdr}
			w := core.NewMemWriter()
			d.WriteError(w, req, &c.e, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got := string(w.Body()); got != fx.Response.Body {
				t.Errorf("body\n wwz  %s\n real %s", got, fx.Response.Body)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealMessagesWithoutMaxTokens: the gateway refuses a Messages body
// without max_tokens with 400 {"model": "<model>"} and x-zen-model.
func TestRealMessagesWithoutMaxTokens(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "messages_no_max_tokens")
	_, e := d.ParseRequest(driver.KindMessages, []byte(fx.Request.Body), driver.MapHeaders{})
	if e == nil {
		t.Fatal("accepted a body without max_tokens")
	}
	for _, req := range []*core.Request{{}, {Endpoint: core.EndpointMessages}} {
		w := core.NewMemWriter()
		d.WriteError(w, req, e, nil)
		if w.StatusCode() != 400 || string(w.Body()) != fx.Response.Body {
			t.Errorf("%d %s, real %d %s", w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
		}
		checkHeaders(t, fx, w.Headers())
	}
	if _, e := d.ParseRequest(driver.KindMessages, []byte(`not json`), driver.MapHeaders{}); e == nil || e.Extra != nil {
		t.Errorf("invalid JSON goes to the parser: %+v", e)
	}
}

// TestRealFixturesModels: the listing comes out byte for byte from a
// configuration with the same prices and extra fields; GET
// /v1/models/{id} is 404 with an empty body for any id.
func TestRealFixturesModels(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "models")
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) == 0 {
		t.Fatal("the fixture lists no model")
	}
	var item map[string]any
	if err := json.Unmarshal(list.Data[0], &item); err != nil {
		t.Fatal(err)
	}
	extra := map[string]any{}
	for _, k := range []string{"canonical_id", "description", "family", "open_weights", "knowledge", "release_date", "last_updated", "modalities", "api"} {
		extra[k] = item[k]
	}
	caps := item["capabilities"].(map[string]any)
	extra["capabilities"] = map[string]any{"reasoning": caps["reasoning"]}
	extra["cost"] = map[string]any{"multipliers": item["cost"].(map[string]any)["multipliers"]}
	yes := true
	info := driver.ModelInfo{Cfg: &config.ModelConfig{
		ID: "deepseek-v4.1-flash", DisplayName: "DeepSeek V4.1 Flash", OwnedBy: "opencode",
		ContextWindow: 1000000, MaxOutput: 384000,
		Capabilities: config.Capabilities{Tools: &yes, Reasoning: &yes, JSONSchema: &yes},
		Pricing:      config.Pricing{InputPerM: 0.15, OutputPerM: 0.6, CacheReadPerM: 0.003},
		Extra:        extra,
	}}
	w := core.NewMemWriter()
	d.ListModels(w, nil, []driver.ModelInfo{info})
	// The real lists every model; the driver here holds the probe's one
	// (data[0]): that item must match byte for byte, and so the top shape.
	var got struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body(), &got); err != nil || len(got.Data) != 1 {
		t.Fatalf("listing %s (%v)", w.Body(), err)
	}
	if g, r := reCreated.ReplaceAllString(string(got.Data[0]), `"created":0`), reCreated.ReplaceAllString(string(list.Data[0]), `"created":0`); g != r {
		t.Errorf("item\n wwz  %s\n real %s", g, r)
	}
	var gotTop, realTop map[string]json.RawMessage
	if err := json.Unmarshal(w.Body(), &gotTop); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &realTop); err != nil {
		t.Fatal(err)
	}
	for k, v := range realTop {
		if k != "data" && string(gotTop[k]) != string(v) {
			t.Errorf("listing field %q: wwz %s, real %s", k, gotTop[k], v)
		}
	}
	if len(gotTop) != len(realTop) {
		t.Errorf("listing fields: wwz %d, real %d", len(gotTop), len(realTop))
	}
	checkHeaders(t, fx, w.Headers())
	for _, name := range []string{"model_one", "model_unknown"} {
		fx := loadShape(t, name)
		w := core.NewMemWriter()
		if name == "model_one" {
			d.GetModel(w, nil, &info, "deepseek-v4.1-flash")
		} else {
			d.GetModel(w, nil, nil, "wwz-nao-existe")
		}
		if w.StatusCode() != fx.Response.Status || len(w.Body()) != 0 {
			t.Errorf("%s: %d %q", name, w.StatusCode(), w.Body())
		}
		checkHeaders(t, fx, w.Headers())
	}
}

// TestRealRoutes: the forms without the dialect segment and the legacy
// /zen base answer like the canonical ones (conferred).
func TestRealRoutes(t *testing.T) {
	want := map[string]driver.Kind{}
	for _, name := range []string{"plain_chat_path", "plain_messages_path", "zen_chat_path"} {
		fx := loadShape(t, name)
		if fx.Response.Status != 401 {
			t.Fatalf("%s: the real refused the route (%d)", name, fx.Response.Status)
		}
		kind := driver.KindChat
		if strings.HasSuffix(fx.Request.Path, "/messages") {
			kind = driver.KindMessages
		}
		want[fx.Request.Path] = kind
	}
	got := map[string]driver.Kind{}
	for _, r := range newDriver(t).Routes() {
		got[r.Path] = r.Kind
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("route %s: %q, want %q", p, got[p], k)
		}
	}
}

// TestRealFixturesUsage: GET /zen/go/v1/usage in the real form; the
// percentages and instants are compared by form.
func TestRealFixturesUsage(t *testing.T) {
	fx := loadShape(t, "usage_zen")
	w := core.NewMemWriter()
	newDriver(t).Extra(driver.KindUsage, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
	reVals := regexp.MustCompile(`"percent":[0-9]+,"resetsAt":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z"`)
	norm := func(b string) string { return reVals.ReplaceAllString(b, `"percent":<n>,"resetsAt":"<t>"`) }
	if got, want := norm(string(w.Body())), norm(fx.Response.Body); got != want {
		t.Errorf("usage\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, fx, w.Headers())
	var u struct {
		Usage struct {
			Weekly struct {
				ResetsAt string `json:"resetsAt"`
			} `json:"weekly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body(), &u); err != nil {
		t.Fatal(err)
	}
	if at, err := time.Parse(time.RFC3339, u.Usage.Weekly.ResetsAt); err != nil || at.Weekday() != time.Monday || at.Hour() != 0 || !at.After(time.Now()) {
		t.Errorf("weekly resetsAt %q, want the next Monday 00:00 UTC", u.Usage.Weekly.ResetsAt)
	}
	w = core.NewMemWriter()
	newDriver(t).Extra(driver.KindModels, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
	if w.StatusCode() != 404 {
		t.Errorf("Extra for another kind: %d", w.StatusCode())
	}
}

// TestUsageBilling: off the Go bases the cost is the list cost and there
// is no allowance; tokens split prompt into input, cache read and writes.
func TestUsageBilling(t *testing.T) {
	d := newDriver(t)
	req := chatReq(false)
	req.Headers["x-wwz-path"] = "/inference/openai/v1/chat/completions"
	u := core.Usage{PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, CachedTokens: 30, CacheWriteTokens: 10, CacheWrite1hTokens: 4, CostMicroUSD: 1234}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, req, &core.Response{ID: gwID, Model: "m", Content: "x", FinishReason: core.FinishStop, Usage: u}, nil); err != nil {
		t.Fatal(err)
	}
	want := `"cost":0.001234,"opencode":{"billing_source":"balance","list_cost":0.001234,"tokens":{"input":60,"output":20,"reasoning":5,"cache_read":30,"cache_write_5m":6,"cache_write_1h":4}}}`
	if !strings.Contains(string(w.Body()), want) {
		t.Errorf("usage %s\nwant …%s", w.Body(), want)
	}
}

// TestRealFixturesNoSessionFirst: without a session the gateway refuses
// before looking at the key and the model (shape/no_session_badkey.json,
// task 387): the driver reports the session missing and the envelope is
// the real one.
func TestRealFixturesNoSessionFirst(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "no_session_badkey")
	hdr := driver.MapHeaders{"x-wwz-path": fx.Request.Path}
	for k, v := range fx.Request.Headers {
		hdr[k] = v
	}
	if cred := d.Authenticate(hdr); len(cred.Missing) == 0 {
		t.Fatalf("session not reported missing: %+v", cred)
	}
	req := &core.Request{Driver: "opencode", Endpoint: core.EndpointChat, Model: "wwz-drift-modelo-inexistente", Headers: hdr}
	w := core.NewMemWriter()
	d.WriteError(w, req, &core.Error{Kind: core.ErrMissingSession}, nil)
	if w.StatusCode() != fx.Response.Status || string(w.Body()) != fx.Response.Body {
		t.Errorf("%d %s\n real %d %s", w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())
}

var (
	// The recorder kept the response and item ids as markers.
	reShapeRespID = regexp.MustCompile(`"id":"[0-9a-f]{32}"`)
	reShapeItemID = regexp.MustCompile(`"(id|item_id)":"(rs|msg|fc)_[0-9a-f]{32}"`)
	reShapeTime   = regexp.MustCompile(`"created_at":[0-9]+`)
)

// normalizeShapeResp brings a Responses body to the markers of the drift
// recording (<id>, <item_id>); the instants are compared by form.
func normalizeShapeResp(b string) string {
	b = reShapeItemID.ReplaceAllString(b, `"$1":"<item_id>"`)
	b = reShapeRespID.ReplaceAllString(b, `"id":"<id>"`)
	b = reShapeTime.ReplaceAllString(b, `"created_at":<t>`)
	return normalize(b)
}

// TestRealFixturesShapeResponses: the responses probes of wwz-drift record
// (shape/responses*.json, task 387), byte for byte, with the reasoning
// count of the recording (task 396).
func TestRealFixturesShapeResponses(t *testing.T) {
	d := newDriver(t)
	const respID = "df8331f9128e746819ca73a8071b45d7"
	compare := func(what, got, want string) {
		if got, want = normalizeShapeResp(got), normalizeShapeResp(want); got != want {
			t.Errorf("%s\n wwz  %s\n real %s", what, got, want)
		}
	}
	fx := loadShape(t, "responses")
	var real realResp
	if err := json.Unmarshal([]byte(fx.Response.Body), &real); err != nil {
		t.Fatal(err)
	}
	resp := real.response()
	resp.ID = respID
	resp.Usage.ReasoningTokens = 10
	resp.Usage.CostMicroUSD = 13
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
			err = sw.Start(&core.Response{ID: respID, Model: r.Model, Created: r.Created})
		case "response.reasoning_summary_text.delta":
			err = sw.ReasoningDelta(ev.Delta)
		case "response.output_text.delta":
			err = sw.TextDelta(ev.Delta)
		case "response.completed":
			r := ev.Response.response()
			r.Usage.ReasoningTokens, r.Usage.CostMicroUSD = 10, 13
			err = sw.Finish(r.FinishReason, r.Usage, true)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	compare("stream", string(w.Body()), fx.Response.Body)
	checkHeaders(t, fx, w.Headers())
}

// loadGoogleProbe is a drift probe of the Gemini route (google/, recorded
// by wwz-drift record) without the infrastructure header server (ADR
// 0049).
func loadGoogleProbe(t *testing.T, name string) shapeFixture {
	t.Helper()
	fx := loadGoogle(t, name)
	delete(fx.Response.Headers, "server")
	return fx
}

// TestRealFixturesGoogleProbes: the probes of wwz-drift record on the
// Gemini route (google/{chat,chat_stream,error_auth,error_model,models},
// task 387) and the thinkingBudget refusal of gemini-4-argon
// (gen_thinking), byte for byte.
func TestRealFixturesGoogleProbes(t *testing.T) {
	d := newDriver(t)
	checkGoogleBody(t, d, loadGoogleProbe(t, "chat"))
	checkGoogleStream(t, d, loadGoogleProbe(t, "chat_stream"))

	for _, c := range []struct {
		name string
		e    core.Error
	}{
		{"error_auth", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"error_model", core.Error{Kind: core.ErrModelNotFound}},
	} {
		fx := loadGoogleProbe(t, c.name)
		h := googleReqHeaders(fx)
		req, perr := d.ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), h)
		if perr != nil {
			t.Fatalf("%s: parse %+v", c.name, perr)
		}
		w := core.NewMemWriter()
		d.WriteError(w, req, &c.e, nil)
		if w.StatusCode() != fx.Response.Status || string(w.Body()) != fx.Response.Body {
			t.Errorf("%s: %d %s\n real %d %s", c.name, w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
		}
		checkGoogleHeaders(t, fx, w.Headers())
	}

	fx := loadGoogleProbe(t, "models")
	rm, ok := driver.As[driver.RouteMisser](d)
	if !ok {
		t.Fatal("opencode is not a RouteMisser")
	}
	w := core.NewMemWriter()
	rm.WriteRouteMiss(w, &core.Request{}, driver.RouteMiss{Method: "GET", Path: fx.Request.Path}, nil)
	if w.StatusCode() != fx.Response.Status || string(w.Body()) != fx.Response.Body {
		t.Errorf("models: %d %q, real %d %q", w.StatusCode(), w.Body(), fx.Response.Status, fx.Response.Body)
	}
	checkGoogleHeaders(t, fx, w.Headers())
}
