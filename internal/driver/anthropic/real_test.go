package anthropic_test

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
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
)

// shapeFixture is a real answer recorded on 2026-10-31 (task 173) in
// testdata/real/anthropic/shape/, normalized as ADR 0014 says: ids are
// <request_id>, <msg_id>, <tool_id>, <signature>; header values that vary
// per call are <rfc3339>, <organization_id>, <workspace_id>; the SSE
// padding (0 to 15 spaces before the closing brace of each data object)
// is removed.
type shapeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Body string `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

func loadShape(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "anthropic", "shape", name+".json"))
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
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func probeReq(stream bool) *core.Request {
	return &core.Request{
		Driver: "anthropic", Endpoint: core.EndpointMessages, Model: "claude-haiku-4-5-20251001", MaxTokens: 16, Stream: stream,
		Headers: map[string]string{"x-api-key": "wwz-key-drift", "anthropic-version": "2023-06-01"}, RequestID: "req_011CAAAAAAAAAAAAAAAAAAAA", Received: time.Now(),
	}
}

var (
	rePad       = regexp.MustCompile(`( *)\}\n`)
	reSignature = regexp.MustCompile(`"signature":"[A-Za-z0-9+/=]{20,}"`)
	reReqIDBody = regexp.MustCompile(`"request_id":"req_[A-Za-z0-9]+"`)
	reStartOut  = regexp.MustCompile(`("type":"message_start".*?"output_tokens":)[0-9]+`)
	reRFC3339   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
	reUUID      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	reWorkspace = regexp.MustCompile(`^wrkspc_01[1-9A-HJ-NP-Za-km-z]{22}$`)
	reReqID     = regexp.MustCompile(`^req_011C[0-9A-Za-z]{20}$`)
)

// normalize brings a wwz body to the fixture's markers; it returns the
// largest SSE padding found.
func normalize(b string) (string, int) {
	most := 0
	b = rePad.ReplaceAllStringFunc(b, func(m string) string {
		most = max(most, len(m)-2)
		return "}\n"
	})
	b = reSignature.ReplaceAllString(b, `"signature":"<signature>"`)
	b = reReqIDBody.ReplaceAllString(b, `"request_id":"<request_id>"`)
	return b, most
}

// checkHeaders compares the dialect headers of the wwz answer with the
// fixture's: the same names, the same fixed values, the markers in their
// real format.
func checkHeaders(t *testing.T, fx shapeFixture, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, "anthropic-") || k == "request-id" || k == "x-should-retry" || k == "content-type" || k == "cache-control" || k == "retry-after" {
			low[k] = v
		}
	}
	for k, want := range fx.Response.Headers {
		v, ok := low[k]
		switch {
		case !ok:
			t.Errorf("%s: header %s missing (real %q)", fx.Name, k, want)
		case want == "<rfc3339>" && !reRFC3339.MatchString(v),
			want == "<organization_id>" && !reUUID.MatchString(v),
			want == "<workspace_id>" && !reWorkspace.MatchString(v),
			want == "<request_id>" && !reReqID.MatchString(v):
			t.Errorf("%s: %s = %q, not in the real format", fx.Name, k, v)
		case !strings.HasPrefix(want, "<") && v != want:
			t.Errorf("%s: %s = %q, real %q", fx.Name, k, v, want)
		}
	}
	for k := range low {
		if _, ok := fx.Response.Headers[k]; !ok {
			t.Errorf("%s: header %s not in the real answer", fx.Name, k)
		}
	}
}

// realMessage is the decoded real body (or message_start message).
type realMessage struct {
	Model   string `json:"model"`
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string    `json:"stop_reason"`
	Usage      realUsage `json:"usage"`
}

type realUsage struct {
	Input       int `json:"input_tokens"`
	CacheWrite  int `json:"cache_creation_input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	Output      int `json:"output_tokens"`
	OutputExtra struct {
		Thinking int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u realUsage) core() core.Usage {
	return core.Usage{PromptTokens: u.Input + u.CacheWrite + u.CacheRead, CompletionTokens: u.Output, ReasoningTokens: u.OutputExtra.Thinking, CachedTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}
}

var finish = map[string]core.FinishReason{"end_turn": core.FinishStop, "max_tokens": core.FinishLength, "tool_use": core.FinishToolCalls}

// TestRealFixturesMessages: the non-streaming body is the real one byte for byte
// when the generated values (content, usage) are the same, with the
// headers of every Messages 200.
func TestRealFixturesMessages(t *testing.T) {
	for _, name := range []string{"chat", "assistant_first", "user_user", "max_tokens", "tool_use", "thinking"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			var m realMessage
			if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
				t.Fatal(err)
			}
			resp := &core.Response{ID: "<msg_id>", Model: m.Model, FinishReason: finish[m.StopReason], Usage: m.Usage.core()}
			for _, b := range m.Content {
				switch b.Type {
				case "thinking":
					resp.Reasoning = b.Thinking
				case "text":
					resp.Content = b.Text
				case "tool_use":
					resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: "<tool_id>", Name: b.Name, Arguments: string(b.Input)})
				}
			}
			w := core.NewMemWriter()
			if err := newDriver(t).WriteResponse(w, probeReq(false), resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, _ := normalize(string(w.Body())); got != fx.Response.Body {
				t.Errorf("body\n wwz  %s\n real %s", got, fx.Response.Body)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// realEvent is one decoded SSE event of a fixture.
type realEvent struct {
	Type         string      `json:"type"`
	Message      realMessage `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage realUsage `json:"usage"`
}

// TestRealFixturesStreams replays each recorded stream through the driver's
// StreamWriter, the way the engine drives it, and compares the frames
// byte for byte (padding aside, which must stay within 0–15 spaces);
// message_start's output_tokens is the real's estimate, not compared.
func TestRealFixturesStreams(t *testing.T) {
	for _, name := range []string{"chat_stream", "max_tokens_stream", "tool_use_stream", "thinking_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			w := core.NewMemWriter()
			sw := newDriver(t).StreamWriter(w, probeReq(true), nil)
			tools, open, firstArgs := 0, "", false
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
				case "message_start":
					err = sw.Start(&core.Response{ID: "<msg_id>", Model: ev.Message.Model, Usage: ev.Message.Usage.core()})
				case "content_block_start":
					open = ev.ContentBlock.Type
					if open == "tool_use" {
						err, firstArgs = sw.ToolCallStart(tools, "<tool_id>", ev.ContentBlock.Name), true
					}
				case "content_block_delta":
					switch ev.Delta.Type {
					case "text_delta":
						err = sw.TextDelta(ev.Delta.Text)
					case "thinking_delta":
						err = sw.ReasoningDelta(ev.Delta.Thinking)
					case "input_json_delta":
						if firstArgs && ev.Delta.PartialJSON == "" {
							firstArgs = false // ToolCallStart sends it
							continue
						}
						err = sw.ToolArgsDelta(tools, ev.Delta.PartialJSON)
					}
				case "content_block_stop":
					if open == "tool_use" {
						err = sw.ToolCallEnd(tools)
						tools++
					}
				case "message_delta":
					err = sw.Finish(finish[ev.Delta.StopReason], ev.Usage.core(), true)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got, most := normalize(string(w.Body()))
			if most > 15 {
				t.Errorf("padding of %d spaces, the real pads 0–15", most)
			}
			got = reStartOut.ReplaceAllString(got, "${1}N")
			want := reStartOut.ReplaceAllString(fx.Response.Body, "${1}N")
			if got != want {
				t.Errorf("stream\n wwz  %s\n real %s", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealStreamPadding: the data of each event carries 0–15 spaces before
// its closing brace, varying, and the ping none (as the real).
func TestRealStreamPadding(t *testing.T) {
	w := core.NewMemWriter()
	sw := newDriver(t).StreamWriter(w, probeReq(true), nil)
	if err := sw.Start(&core.Response{ID: "msg_01pad", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	for range 60 {
		if err := sw.TextDelta("x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Finish(core.FinishStop, core.Usage{}, true); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, line := range strings.Split(string(w.Body()), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == `{"type": "ping"}` {
			continue
		}
		trimmed := strings.TrimRight(strings.TrimSuffix(data, "}"), " ")
		pad := len(data) - 1 - len(trimmed)
		if pad > 15 || !json.Valid([]byte(data)) {
			t.Fatalf("frame %q: padding %d", data, pad)
		}
		seen[pad] = true
	}
	if len(seen) < 5 {
		t.Errorf("paddings %v: the real varies them", seen)
	}
	if !strings.Contains(string(w.Body()), "event: ping\ndata: {\"type\": \"ping\"}\n\n") {
		t.Error("no ping after the first content_block_start")
	}
}

// TestRealFixturesErrors: the error answers, body and headers: request-id equal to
// the body's, x-should-retry, the account headers except on the 401, which
// has Cache-Control: no-store; < > & unescaped.
func TestRealFixturesErrors(t *testing.T) {
	d := newDriver(t)
	cases := []struct {
		name  string
		model string
		e     core.Error
	}{
		{"error_auth", "claude-haiku-4-5-20251001", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"error_badkey_stream", "claude-haiku-4-5-20251001", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"models_badkey", "", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"error_model", "wwz-drift-modelo-inexistente", core.Error{Kind: core.ErrModelNotFound}},
		{"error_model_html", "a<b>&c", core.Error{Kind: core.ErrModelNotFound}},
		{"count_tokens_unknown", "wwz-nao-existe", core.Error{Kind: core.ErrModelNotFound}},
		{"error_400", "claude-haiku-4-5-20251001", core.Error{Kind: core.ErrInvalidRequest, Message: "max_tokens: Field required"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := loadShape(t, c.name)
			req := probeReq(false)
			req.Model = c.model
			w := core.NewMemWriter()
			d.WriteError(w, req, &c.e, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, _ := normalize(string(w.Body())); got != fx.Response.Body {
				t.Errorf("body\n wwz  %s\n real %s", got, fx.Response.Body)
			}
			if !strings.Contains(string(w.Body()), `"request_id":"`+w.Headers()["request-id"]+`"`) {
				t.Errorf("request-id %q is not the body's request_id: %s", w.Headers()["request-id"], w.Body())
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealErrorWithoutRequest: an error found before the request exists
// (parse, key) still carries a fresh request id, the same in the header
// and the body.
func TestRealErrorWithoutRequest(t *testing.T) {
	w := core.NewMemWriter()
	newDriver(t).WriteError(w, nil, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	id := w.Headers()["request-id"]
	if !reReqID.MatchString(id) || !strings.Contains(string(w.Body()), `"request_id":"`+id+`"`) {
		t.Fatalf("request-id %q, body %s", id, w.Body())
	}
}

// TestRealFixturesCountTokens: {"input_tokens": n} with the account headers and
// request-id, without anthropic-ratelimit-*.
func TestRealFixturesCountTokens(t *testing.T) {
	fx := loadShape(t, "count_tokens")
	req := probeReq(false)
	req.Endpoint = core.EndpointCountTokens
	w := core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, req, &core.Response{Usage: core.Usage{PromptTokens: 13}}, nil); err != nil {
		t.Fatal(err)
	}
	if string(w.Body()) != fx.Response.Body {
		t.Errorf("body %s, real %s", w.Body(), fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())
}

// realModel is one item of the real listing.
type realModel struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	CreatedAt      time.Time      `json:"created_at"`
	MaxInputTokens int            `json:"max_input_tokens"`
	MaxTokens      int            `json:"max_tokens"`
	Capabilities   map[string]any `json:"capabilities"`
	DeprecatedAt   *time.Time     `json:"deprecated_at"`
	RetiresAt      *time.Time     `json:"retires_at"`
}

// info is the configuration that lists m: lifecycle dates in models[]
// (YYYY-MM-DD), the exact instants in models[].extra.
func (m realModel) info() driver.ModelInfo {
	cfg := &config.ModelConfig{
		ID: m.ID, DisplayName: m.DisplayName, Created: m.CreatedAt.Unix(),
		ContextWindow: m.MaxInputTokens, MaxOutput: m.MaxTokens,
		Extra: map[string]any{"capabilities": m.Capabilities},
	}
	if m.DeprecatedAt != nil {
		cfg.DeprecatedAt = m.DeprecatedAt.Format(time.DateOnly)
		cfg.Extra["deprecated_at"] = m.DeprecatedAt.Format(time.RFC3339)
	}
	if m.RetiresAt != nil {
		cfg.RetiresAt = m.RetiresAt.Format(time.DateOnly)
		cfg.Extra["retires_at"] = m.RetiresAt.Format(time.RFC3339)
	}
	return driver.ModelInfo{Cfg: cfg}
}

// TestRealFixturesModels: the whole real listing (14 models) comes out byte for
// byte when models[] carries the same ids, dates, limits and
// extra.capabilities; one item (by id and by alias) derives everything
// from the configuration of claude-haiku-4-5. Headers: request-id and the
// account, no anthropic-ratelimit-*.
func TestRealFixturesModels(t *testing.T) {
	d := newDriver(t)
	fx := loadShape(t, "models")
	var list struct {
		Data []realModel `json:"data"`
	}
	if err := json.Unmarshal([]byte(fx.Response.Body), &list); err != nil {
		t.Fatal(err)
	}
	var infos []driver.ModelInfo
	for _, m := range list.Data {
		infos = append(infos, m.info())
	}
	w := core.NewMemWriter()
	d.ListModels(w, nil, infos)
	if string(w.Body()) != fx.Response.Body {
		t.Errorf("listing\n wwz  %s\n real %s", w.Body(), fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())

	haiku := &config.ModelConfig{
		ID: "claude-haiku-4-5-20251001", Aliases: []string{"claude-haiku-4-5"}, DisplayName: "Claude Haiku 4.5",
		Created: time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC).Unix(), ContextWindow: 200000, MaxOutput: 64000,
	}
	for _, name := range []string{"models_one", "models_alias"} {
		fx := loadShape(t, name)
		w := core.NewMemWriter()
		d.GetModel(w, nil, &driver.ModelInfo{Cfg: haiku}, "claude-haiku-4-5")
		if string(w.Body()) != fx.Response.Body {
			t.Errorf("%s\n wwz  %s\n real %s", name, w.Body(), fx.Response.Body)
		}
		checkHeaders(t, fx, w.Headers())
	}

	fx = loadShape(t, "models_unknown")
	w = core.NewMemWriter()
	d.GetModel(w, nil, nil, "wwz-nao-existe")
	if got, _ := normalize(string(w.Body())); w.StatusCode() != 404 || got != fx.Response.Body {
		t.Errorf("unknown: %d %s, real %s", w.StatusCode(), got, fx.Response.Body)
	}
	checkHeaders(t, fx, w.Headers())
}

// TestModelLifecycle: lifecycle, deprecated_at and retires_at from the
// configuration ("deprecated" and the RFC 3339 form conferred on the real
// listing: claude-sonnet-4-5-20250929, fixture models).
func TestModelLifecycle(t *testing.T) {
	d := newDriver(t)
	cfg := &config.ModelConfig{ID: "claude-old-1", DeprecatedAt: "2020-01-02", RetiresAt: "2099-03-04", Extra: map[string]any{"line": "old", "extra_field": "x"}}
	w := core.NewMemWriter()
	d.GetModel(w, nil, &driver.ModelInfo{Cfg: cfg}, cfg.ID)
	body := string(w.Body())
	for _, want := range []string{`"display_name":"claude-old-1"`, `"line":"old"`, `"lifecycle":"deprecated","deprecated_at":"2020-01-02T00:00:00Z","retires_at":"2099-03-04T00:00:00Z","extra_field":"x"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s not in %s", want, body)
		}
	}
	// models[].capabilities drive the derived capabilities.
	no := false
	cfg = &config.ModelConfig{ID: "claude-sonnet-x", Capabilities: config.Capabilities{Reasoning: &no, Vision: &no}}
	w = core.NewMemWriter()
	d.GetModel(w, nil, &driver.ModelInfo{Cfg: cfg}, cfg.ID)
	body = string(w.Body())
	for _, want := range []string{`"line":"sonnet"`, `"image_input":{"supported":false}`, `"thinking":{"supported":false,"types":{"enabled":{"supported":false}`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s not in %s", want, body)
		}
	}
}

// TestAccountHeaders: anthropic-organization-id and anthropic-workspace-id
// derive from keys[].account and keys[].project (ADR 0049): stable for the
// same account, different for another, the workspace per project.
func TestAccountHeaders(t *testing.T) {
	d := newDriver(t)
	headers := func(account, project, key string) map[string]string {
		req := probeReq(false)
		req.Headers["x-api-key"] = key
		driver.SetAccount(req.Headers, driver.Account{Key: key, Account: account, Project: project})
		w := core.NewMemWriter()
		if err := d.WriteResponse(w, req, &core.Response{ID: "msg_1", Model: "m", Content: "ok"}, nil); err != nil {
			t.Fatal(err)
		}
		return w.Headers()
	}
	a1, a2 := headers("acme", "p1", "k1"), headers("acme", "p1", "k2")
	if a1["anthropic-organization-id"] != a2["anthropic-organization-id"] || a1["anthropic-workspace-id"] != a2["anthropic-workspace-id"] {
		t.Errorf("same account, different ids: %v %v", a1, a2)
	}
	p2 := headers("acme", "p2", "k1")
	if p2["anthropic-organization-id"] != a1["anthropic-organization-id"] || p2["anthropic-workspace-id"] == a1["anthropic-workspace-id"] {
		t.Errorf("another project: org must stay, workspace change: %v %v", a1, p2)
	}
	b := headers("other", "p1", "k1")
	if b["anthropic-organization-id"] == a1["anthropic-organization-id"] {
		t.Errorf("another account, same organization %q", b["anthropic-organization-id"])
	}
	n1, n2 := headers("", "", "k1"), headers("", "", "k2")
	if n1["anthropic-organization-id"] == n2["anthropic-organization-id"] {
		t.Error("keys without account share the organization")
	}
	if !reUUID.MatchString(a1["anthropic-organization-id"]) || !reWorkspace.MatchString(a1["anthropic-workspace-id"]) {
		t.Errorf("formats: %v", a1)
	}
}
