package azure

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func TestOptions(t *testing.T) {
	if _, err := driver.New("azure", map[string]string{"reasoning_field": "hidden"}); err != nil {
		t.Errorf("hidden: %v", err)
	}
	for _, o := range []map[string]string{{"reasoning_field": "reasoning_content"}, {"x": "y"}} {
		if _, err := driver.New("azure", o); err == nil {
			t.Errorf("%v accepted", o)
		}
	}
}

func TestStrictSchema(t *testing.T) {
	tool := func(params string) string {
		return `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":` + params + `}}]}`
	}
	cases := []struct {
		name, body, msg, param, code string
	}{
		{
			"no additionalProperties", tool(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`),
			"Invalid schema for function 'f': In context=(), 'additionalProperties' is required to be supplied and to be false.", "tools[0].function.parameters", "invalid_function_parameters",
		},
		{
			"missing required", tool(`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`),
			"Invalid schema for function 'f': In context=(), 'required' is required to be supplied and to be an array including every key in properties. Missing 'a'.", "tools[0].function.parameters", "invalid_function_parameters",
		},
		{
			"nested property", tool(`{"type":"object","properties":{"a":{"type":"object","properties":{}}},"required":["a"],"additionalProperties":false}`),
			"Invalid schema for function 'f': In context=('properties', 'a'), 'additionalProperties' is required to be supplied and to be false.", "", "",
		},
		{
			"items", tool(`{"type":"object","properties":{"a":{"type":"array","items":{"type":["object","null"]}}},"required":["a"],"additionalProperties":false}`),
			"Invalid schema for function 'f': In context=('properties', 'a', 'items'), 'additionalProperties' is required to be supplied and to be false.", "", "",
		},
		{
			"anyOf", tool(`{"type":"object","properties":{},"additionalProperties":false,"anyOf":[{"type":"object"}]}`),
			"Invalid schema for function 'f': In context=('anyOf', '0'), 'additionalProperties' is required to be supplied and to be false.", "", "",
		},
		{
			"defs with quote", tool(`{"type":"object","properties":{},"additionalProperties":false,"$defs":{"it's":{"type":"object"}}}`),
			`Invalid schema for function 'f': In context=('$defs', "it's"), 'additionalProperties' is required to be supplied and to be false.`, "", "",
		},
		{
			"response_format", `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"r","strict":true,"schema":{"type":"object","properties":{}}}}}`,
			"Invalid schema for response_format 'r': In context=(), 'additionalProperties' is required to be supplied and to be false.", "response_format", "invalid_json_schema",
		},
		{"two unknown", `{"model":"m","messages":[{"role":"user","content":"x"}],"zz":1,"aa":2}`, "Unrecognized request arguments supplied: aa, zz", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := validate(core.EndpointChat, []byte(c.body))
			if e == nil {
				t.Fatal("accepted")
			}
			if e.Message != c.msg {
				t.Errorf("message %q\nwant    %q", e.Message, c.msg)
			}
			if c.param != "" && (e.Param != c.param || e.Code != c.code) {
				t.Errorf("param %q code %q", e.Param, e.Code)
			}
		})
	}
	for _, ok := range []string{
		tool(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`),
		`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
		`{"model":"m","messages":[],"tools":"x","response_format":{"type":"json_schema","json_schema":{"name":"r","strict":false}}}`,
		`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":"x"}}]}`,
		`{"model":"m","messages":[],"data_sources":[],"response_format":"x"}`,
		`not json`,
	} {
		if e := validate(core.EndpointChat, []byte(ok)); e != nil {
			t.Errorf("%s: %v", ok, e.Message)
		}
	}
	if e := validate(core.EndpointCompletions, []byte(`{"model":"m","prompt":"x","messages":[]}`)); e == nil || e.Message != "Unrecognized request argument supplied: messages" {
		t.Errorf("completions: %v", e)
	}
	if e := validate(core.EndpointCompletions, []byte(`{"model":"m","prompt":"x","suffix":"y"}`)); e != nil {
		t.Errorf("completions suffix: %v", e)
	}
	if e := strictResponseFormat(json.RawMessage(`{"json_schema":{"name":"r","strict":true,"schema":{"type":"object","properties":{},"required":[],"additionalProperties":false}}}`)); e != nil {
		t.Errorf("valid strict response_format: %v", e.Message)
	}
}

func TestRouteOf(t *testing.T) {
	for _, c := range []struct {
		path string
		kind driver.Kind
		fam  family
		dep  string
		ep   core.Endpoint
	}{
		{"/openai/deployments/d/chat/completions", driver.KindChat, familyDeployment, "d", core.EndpointChat},
		{"/openai/deployments/d/completions", driver.KindChat, familyDeployment, "d", core.EndpointCompletions},
		{"/openai/deployments//chat/completions", driver.KindChat, familyDeployment, "", ""},
		{"/openai/deployments/d/embeddings", driver.KindChat, familyDeployment, "d", ""},
		{"/openai/v1/responses", driver.KindResponses, familyV1, "", core.EndpointResponses},
		{"", driver.KindCompletions, familyUnknown, "", core.EndpointCompletions},
		{"", driver.KindModels, familyUnknown, "", ""},
	} {
		r := routeOf(c.path, c.kind)
		if r.family != c.fam || r.deployment != c.dep || r.endpoint != c.ep {
			t.Errorf("routeOf(%q, %s) = %+v", c.path, c.kind, r)
		}
	}
	if r := routeOfHeaders(nil, driver.KindChat); r.endpoint != core.EndpointChat {
		t.Errorf("nil headers: %+v", r)
	}
	for ep, want := range map[core.Endpoint]string{core.EndpointCompletions: "Completions_Create", core.EndpointResponses: "Responses_Create", core.EndpointChat: "ChatCompletions_Create"} {
		if got := operationOf(&core.Request{Endpoint: ep}); got != want {
			t.Errorf("operationOf(%s) = %s", ep, got)
		}
	}
	if operationOf(nil) != "ChatCompletions_Create" || apiVersionOf(nil) != "2024-10-21" {
		t.Error("nil request defaults")
	}
}

func TestParseDeployment(t *testing.T) {
	d := &Driver{hooks: hooks()}
	h := driver.MapHeaders{"x-wwz-path": "/openai/deployments/dep-1/chat/completions", driver.QueryHeader: "api-version=2024-10-21"}
	body := `{"model":"ignored","messages":[{"role":"user","content":"hi"}]}`
	req, e := d.ParseRequest(driver.KindChat, []byte(body), h)
	if e != nil {
		t.Fatal(e.Message)
	}
	if req.Model != "dep-1" || string(req.Raw) != body {
		t.Errorf("model %q raw %q", req.Model, req.Raw)
	}
	// A body that is not an object goes to the parser untouched.
	if _, e := d.ParseRequest(driver.KindChat, []byte(`[1]`), h); e == nil || e.Kind != core.ErrInvalidRequest {
		t.Errorf("array body: %v", e)
	}
	h["x-wwz-path"] = "/openai/deployments/dep-1/completions"
	if req, e = d.ParseRequest(driver.KindChat, []byte(`{"prompt":"x"}`), h); e != nil || req.Endpoint != core.EndpointCompletions || req.Model != "dep-1" {
		t.Errorf("completions: %+v %v", req, e)
	}
	if _, e = d.ParseRequest(driver.KindChat, []byte(`{"prompt":"x","bogus":1}`), h); e == nil {
		t.Error("completions unknown field accepted")
	}
}

func TestErrorBodies(t *testing.T) {
	d := &Driver{hooks: hooks()}
	for _, c := range []struct {
		e      core.Error
		status int
		code   any
	}{
		{core.Error{Kind: core.ErrGeoBlocked}, 403, "PermissionDenied"},
		{core.Error{Kind: core.ErrAccountBanned, Message: "gone"}, 403, "PermissionDenied"},
		{core.Error{Kind: core.ErrMethodNotAllowed}, 405, "405"},
		{core.Error{Kind: core.ErrRequestTooLarge}, 413, "request_too_large"},
		{core.Error{Kind: core.ErrQuota}, 429, "429"},
		{core.Error{Kind: core.ErrBadGateway}, 502, "BadGateway"},
		{core.Error{Kind: core.ErrMissingSession}, 500, "InternalServerError"},
	} {
		w := core.NewMemWriter()
		d.WriteError(w, &core.Request{Headers: map[string]string{"api-key": "k"}}, &c.e, nil)
		var m struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(w.Body(), &m); err != nil {
			t.Fatal(err)
		}
		if w.StatusCode() != c.status || m.Error["code"] != c.code {
			t.Errorf("%s: %d %s", c.e.Kind, w.StatusCode(), w.Body())
		}
	}
	// The 401 of a presented key and of no key at all.
	w := core.NewMemWriter()
	d.WriteError(w, &core.Request{Headers: map[string]string{"api-key": "bad"}}, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	if !strings.Contains(string(w.Body()), "invalid subscription key") {
		t.Errorf("401 with key: %s", w.Body())
	}
	if !hasCredential(nil) {
		t.Error("hasCredential(nil)")
	}
	// The 429 names the api-version of the request and rounds up.
	req := &core.Request{Endpoint: core.EndpointCompletions, Headers: map[string]string{driver.QueryHeader: "api-version=2024-06-01"}}
	msg := rateLimitMessage(req, &core.Error{Kind: core.ErrRateLimit, RetryAfter: 100 * time.Millisecond})
	if !strings.HasPrefix(msg, "Requests to the Completions_Create Operation under Azure OpenAI API version 2024-06-01 have exceeded call rate limit") || !strings.Contains(msg, "retry after 1 seconds") {
		t.Errorf("429 text %q", msg)
	}
}

func TestModelsAndExtra(t *testing.T) {
	d := &Driver{hooks: hooks()}
	m := &config.ModelConfig{ID: "m", Created: 1, RetiresAt: "2026-12-31"}
	w := core.NewMemWriter()
	d.ListModels(w, nil, []driver.ModelInfo{{Cfg: m}})
	if !strings.Contains(string(w.Body()), `"deprecation":{"inference":1798675200}`) {
		t.Errorf("listing %s", w.Body())
	}
	w = core.NewMemWriter()
	d.GetModel(w, nil, &driver.ModelInfo{Cfg: &config.ModelConfig{ID: "x", Extra: map[string]any{"bad": func() {}}}}, "x")
	if w.StatusCode() != 500 {
		t.Errorf("unencodable extra: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.Extra(driver.KindResource, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
	if w.StatusCode() != 404 {
		t.Errorf("extra: %d", w.StatusCode())
	}
	if id := d.ResponseID(driver.KindCompletions, generate.NewRNG(1, "x"), time.Now()); !strings.HasPrefix(id, "cmpl-") {
		t.Errorf("completions id %q", id)
	}
	if id := d.ResponseID(driver.KindResponses, generate.NewRNG(1, "x"), time.Now()); !strings.HasPrefix(id, "resp_") {
		t.Errorf("responses id %q", id)
	}
}

func TestChoiceResults(t *testing.T) {
	d := &Driver{hooks: hooks()}
	req := &core.Request{Endpoint: core.EndpointCompletions}
	for _, content := range []string{"", "x"} {
		w := core.NewMemWriter()
		resp := &core.Response{ID: "chatcmpl-abc", Model: "m", Content: content, FinishReason: core.FinishStop}
		if err := d.WriteResponse(w, req, resp, nil); err != nil {
			t.Fatal(err)
		}
		full := strings.Contains(string(w.Body()), `"finish_reason":"stop","content_filter_results":{"hate"`)
		if full != (content != "") || !strings.Contains(string(w.Body()), `"id":"cmpl-abc"`) {
			t.Errorf("content %q: %s", content, w.Body())
		}
	}
}

// failWriter fails every Write after the first n.
type failWriter struct {
	*core.MemWriter
	n int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("broken")
	}
	f.n--
	return f.MemWriter.Write(p)
}

func TestPreWriterErrors(t *testing.T) {
	pw := &preWriter{ResponseWriter: &failWriter{MemWriter: core.NewMemWriter()}, pre: []byte("p")}
	if _, err := pw.Write([]byte("x")); err == nil {
		t.Error("Write: no error")
	}
	pw = &preWriter{ResponseWriter: &failWriter{MemWriter: core.NewMemWriter()}, pre: []byte("p")}
	if err := pw.send(); err == nil {
		t.Error("send: no error")
	}
	// Start fails when the role chunk cannot be written.
	d := &Driver{hooks: hooks()}
	sw := d.StreamWriter(&failWriter{MemWriter: core.NewMemWriter()}, &core.Request{Endpoint: core.EndpointChat}, nil)
	if err := sw.Start(&core.Response{ID: "chatcmpl-x"}); err == nil {
		t.Error("Start: no error")
	}
	// With hidden reasoning the role chunk waits: the frame goes out at Start.
	mw := core.NewMemWriter()
	sw = d.StreamWriter(mw, &core.Request{Endpoint: core.EndpointChat}, nil)
	if err := sw.Start(&core.Response{ID: "chatcmpl-x", Reasoning: "r"}); err != nil {
		t.Fatal(err)
	}
	if string(mw.Body()) != string(promptFilterFrame) {
		t.Errorf("body after Start %q, want only the prompt filter frame", mw.Body())
	}
}
