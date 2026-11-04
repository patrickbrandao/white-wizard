package openai_test

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
)

// The probes of wwz-drift record (testdata/real/openai/shape/, recorded on
// 2026-10-31, task 387), normalized as ADR 0014 says: per-call ids, the
// fingerprint, request ids and durations are markers. Each one is replayed
// through the driver with the generated values of the real answer
// (content, usage, created) and compared byte for byte.

type shapeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
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

func loadShape(t *testing.T, name string) (shapeFixture, realReply) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "openai", "shape", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx shapeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx, realReply{status: fx.Response.Status, headers: fx.Response.Headers, body: fx.Response.Body}
}

var (
	reShapeChatID = regexp.MustCompile(`"id": ?"chatcmpl-[A-Za-z0-9]{29}"`)
	reShapeFP     = regexp.MustCompile(`"system_fingerprint": ?"fp_[0-9a-f]{10}"`)
	reShapeRespID = regexp.MustCompile(`"(id|response_id)": ?"resp_[0-9a-f]{50}"`)
	reShapeItemID = regexp.MustCompile(`"(id|item_id)": ?"msg_[0-9a-f]{50}"`)
	reShapeTime   = regexp.MustCompile(`"(created_at|completed_at)": ?[0-9]+`)
)

// normalizeShape brings a wwz or real body to the fixture's markers; the
// instants of the Responses object are compared by form.
func normalizeShape(b string) string {
	b = reShapeChatID.ReplaceAllStringFunc(b, func(s string) string { return s[:strings.Index(s, `"chatcmpl`)] + `"<id>"` })
	b = reShapeFP.ReplaceAllStringFunc(b, func(s string) string { return s[:strings.Index(s, `"fp_`)] + `"<fp>"` })
	b = reShapeRespID.ReplaceAllStringFunc(b, func(s string) string { return s[:strings.Index(s, `"resp_`)] + `"<id>"` })
	b = reShapeItemID.ReplaceAllStringFunc(b, func(s string) string { return s[:strings.Index(s, `"msg_`)] + `"<item_id>"` })
	b = reShapeTime.ReplaceAllString(b, `"$1":<t>`)
	return obfuscationRe.ReplaceAllString(b, `"obfuscation":"<o>"`)
}

// engineHeaders are the headers the engine hands the driver with every
// response: the request id it minted ("req_" + 32 hex).
func engineHeaders() map[string]string {
	return map[string]string{"x-request-id": "req_923a1a13e4ca4419bd25f8ef60ef0666"}
}

// shapeRequest parses the fixture's request as the server hands it over.
func shapeRequest(t *testing.T, d driver.Driver, fx shapeFixture, kind driver.Kind) *core.Request {
	t.Helper()
	req, perr := d.ParseRequest(kind, []byte(fx.Request.Body), driver.MapHeaders{})
	if perr != nil {
		t.Fatalf("%s: parse: %+v", fx.Name, perr)
	}
	req.Headers = map[string]string{"authorization": "Bearer wwz-key-drift"}
	req.Received = time.Now()
	return req
}

func TestShapeFixturesChat(t *testing.T) {
	d := newDriver(t)
	fx, real := loadShape(t, "chat")
	resp := &core.Response{
		ID: "chatcmpl-Ef2YMuGdqwgKVZW6Q1InrPJS2S1hR", Model: "gpt-4o-mini-2024-07-18", Created: time.Unix(1793465688, 0),
		Content: "Ok.", FinishReason: core.FinishStop, Usage: core.Usage{PromptTokens: 13, CompletionTokens: 2},
		Headers: engineHeaders(),
	}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, shapeRequest(t, d, fx, driver.KindChat), resp, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeShape(string(w.Body())), normalizeShape(fx.Response.Body); got != want {
		t.Errorf("body\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, real, w.Headers(), "openai-version", "x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests")
}

func TestShapeFixturesChatStream(t *testing.T) {
	d := newDriver(t)
	fx, real := loadShape(t, "chat_stream")
	req := shapeRequest(t, d, fx, driver.KindChat)
	if !req.Stream || !req.IncludeUsage {
		t.Fatalf("stream %v include_usage %v", req.Stream, req.IncludeUsage)
	}
	w := core.NewMemWriter()
	s := d.StreamWriter(w, req, nil)
	for _, err := range []error{
		s.Start(&core.Response{ID: "chatcmpl-Ef2YNZkVXtXK9ZR7XlrVAAzqxhdyR", Model: "gpt-4o-mini-2024-07-18", Created: time.Unix(1793465689, 0), Headers: engineHeaders()}),
		s.TextDelta("Ok"), s.TextDelta("."),
		s.Finish(core.FinishStop, core.Usage{PromptTokens: 13, CompletionTokens: 2}, true),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, want := normalizeShape(string(w.Body())), normalizeShape(fx.Response.Body); got != want {
		t.Errorf("stream\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, real, w.Headers(), "openai-version", "x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests")
}

func TestShapeFixturesErrors(t *testing.T) {
	d := newDriver(t)
	for _, c := range []struct {
		name string
		e    core.Error
		same []string
	}{
		{"error_auth", core.Error{Kind: core.ErrInvalidAPIKey}, []string{"x-error-json", "x-openai-authorization-error", "x-openai-ide-error-code"}},
		{"error_model", core.Error{Kind: core.ErrModelNotFound}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx, real := loadShape(t, c.name)
			var probe struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(fx.Request.Body), &probe); err != nil {
				t.Fatal(err)
			}
			req := &core.Request{Endpoint: core.EndpointChat, Model: probe.Model, Headers: map[string]string{}}
			for k, v := range fx.Request.Headers {
				req.Headers[k] = strings.ReplaceAll(v, "<key>", "wwz-key-drift")
			}
			w := core.NewMemWriter()
			d.WriteError(w, req, &c.e, nil)
			if w.StatusCode() != real.status || string(w.Body()) != real.body {
				t.Errorf("%d %q\n real %d %q", w.StatusCode(), w.Body(), real.status, real.body)
			}
			checkHeaders(t, real, w.Headers(), c.same...)
		})
	}
}

// TestShapeFixturesModels: the probe model's item and the listing around
// it are the real ones, in the real layout.
func TestShapeFixturesModels(t *testing.T) {
	fx, real := loadShape(t, "models")
	start := strings.Index(real.body, "    {\n      \"id\": \"gpt-4o-mini\",")
	if start < 0 {
		t.Fatal("gpt-4o-mini not in the real listing")
	}
	end := strings.Index(real.body[start:], "\n    }") + start + len("\n    }")
	want := "{\n  \"object\": \"list\",\n  \"data\": [\n" + real.body[start:end] + "\n  ]\n}"
	if !strings.HasPrefix(real.body, "{\n  \"object\": \"list\",\n  \"data\": [\n") || !strings.HasSuffix(strings.TrimSpace(real.body), "\n  ]\n}") {
		t.Fatalf("listing frame: %.60q … %q", real.body, real.body[len(real.body)-12:])
	}
	m := &config.ModelConfig{ID: "gpt-4o-mini", Created: 1721172741, OwnedBy: "system", RetiresAt: "2027-01-14"}
	w := core.NewMemWriter()
	newDriver(t).ListModels(w, nil, []driver.ModelInfo{{Cfg: m}})
	if string(w.Body()) != want {
		t.Errorf("models\n wwz  %q\n real %q", w.Body(), want)
	}
	if fx.Request.Method != "GET" || fx.Request.Path != "/v1/models" {
		t.Errorf("request %s %s", fx.Request.Method, fx.Request.Path)
	}
	checkHeaders(t, real, w.Headers(), "openai-version")
}

// realResponses is the decoded real Responses object (or the response of
// an event).
type realResponses struct {
	CreatedAt int64  `json:"created_at"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Output    []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func (r realResponses) response() *core.Response {
	resp := &core.Response{ID: "resp_" + strings.Repeat("0", 50), Model: r.Model, Created: time.Unix(r.CreatedAt, 0), FinishReason: core.FinishStop, Headers: engineHeaders()}
	if len(r.Output) > 0 && len(r.Output[0].Content) > 0 {
		resp.Content = r.Output[0].Content[0].Text
	}
	if r.Usage != nil {
		resp.Usage = core.Usage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output, CachedTokens: r.Usage.Details.Cached}
	}
	return resp
}

func TestShapeFixturesResponses(t *testing.T) {
	d := newDriver(t)
	fx, real := loadShape(t, "responses")
	var r realResponses
	if err := json.Unmarshal([]byte(fx.Response.Body), &r); err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, shapeRequest(t, d, fx, driver.KindResponses), r.response(), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeShape(string(w.Body())), normalizeShape(fx.Response.Body); got != want {
		t.Errorf("body\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, real, w.Headers(), "openai-version", "x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests")
}

func TestShapeFixturesResponsesStream(t *testing.T) {
	d := newDriver(t)
	fx, real := loadShape(t, "responses_stream")
	req := shapeRequest(t, d, fx, driver.KindResponses)
	w := core.NewMemWriter()
	s := d.StreamWriter(w, req, nil)
	for _, line := range strings.Split(fx.Response.Body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type     string        `json:"type"`
			Delta    string        `json:"delta"`
			Response realResponses `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		var err error
		switch ev.Type {
		case "response.created":
			r := ev.Response.response()
			err = s.Start(&core.Response{ID: r.ID, Model: r.Model, Created: r.Created, Headers: r.Headers})
		case "response.output_text.delta":
			err = s.TextDelta(ev.Delta)
		case "response.completed", "response.incomplete":
			r := ev.Response.response()
			err = s.Finish(r.FinishReason, r.Usage, true)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, want := normalizeShape(string(w.Body())), normalizeShape(fx.Response.Body); got != want {
		t.Errorf("stream\n wwz  %s\n real %s", got, want)
	}
	checkHeaders(t, real, w.Headers(), "openai-version", "x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests")
}
