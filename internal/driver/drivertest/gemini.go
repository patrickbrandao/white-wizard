package drivertest

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The Gemini dialect (spec 006 16.5, ADR 0043): the model and the method
// are in the path (models/{model}:{method}, driver.KindGenerate), the
// listing and cachedContents are driver.KindResource routes served by
// Extra, every answer is google.rpc.Status or a GenerateContentResponse.
// A driver with a KindGenerate route runs these checks instead of the
// Chat Completions and Messages ones; the expectations come from the
// recordings of task 164 (testdata/real/gemini/shape/).

// gemini reports whether the driver speaks the Gemini dialect.
func (s *suite) gemini() bool { return s.hasKind(driver.KindGenerate) }

// runGemini is Run for a Gemini driver.
func (s *suite) runGemini(t *testing.T) {
	s.run(t, "Routes", s.geminiRoutes)
	s.run(t, "Models", s.geminiModels)
	s.run(t, "Auth", s.geminiAuth)
	s.run(t, "Parse", s.geminiParse)
	s.run(t, "Response", s.geminiResponse)
	s.run(t, "Stream", s.geminiStream)
	s.run(t, "Errors", s.geminiErrors)
	s.run(t, "RateLimitHeaders", s.geminiNoRateLimit)
	s.run(t, "IDs", s.geminiIDs)
	s.run(t, "LimitPolicy", s.testLimitPolicy)
}

// geminiHeaders are the headers of a call to models/m-1:method, as the
// server fills them (x-wwz-path, the query and the method slots).
func geminiHeaders(method, query string) driver.MapHeaders {
	return driver.MapHeaders{
		"x-goog-api-key": "AIza-wwztest", "x-wwz-path": "/v1beta/models/m-1:" + method,
		driver.QueryHeader: query, driver.MethodHeader: "POST",
	}
}

func (s *suite) geminiRoutes(t *testing.T) {
	rs := s.routes()
	for _, r := range rs {
		switch r.Kind {
		case driver.KindGenerate:
			if r.Method != "POST" || !strings.HasSuffix(r.Path, "/models/*") {
				t.Errorf("route %+v: generate is POST .../models/*", r)
			}
		case driver.KindResource:
		default:
			t.Errorf("route %+v: a Gemini driver declares generate and resource routes only", r)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "//") {
			t.Errorf("route %+v: path must be absolute and clean", r)
		}
	}
	for _, want := range s.p.Routes {
		found := false
		for _, r := range rs {
			found = found || r == want
		}
		if !found {
			t.Errorf("missing route %s %s (%s)", want.Method, want.Path, want.Kind)
		}
	}
}

// geminiModel is a configured model for the listing checks.
func geminiModel(id string) driver.ModelInfo {
	return driver.ModelInfo{Cfg: &config.ModelConfig{ID: id, DisplayName: "Model " + id, ContextWindow: 1000, MaxOutput: 100}}
}

// extra calls Extra on a resource path.
func (s *suite) extra(method, path, query string, models []driver.ModelInfo) *core.MemWriter {
	w := core.NewMemWriter()
	h := driver.MapHeaders{"x-goog-api-key": "AIza-wwztest", "x-wwz-path": path, driver.QueryHeader: query, driver.MethodHeader: method}
	s.d.Extra(driver.KindResource, w, h, nil, driver.ExtraContext{Models: models})
	return w
}

func (s *suite) geminiModels(t *testing.T) {
	models := []driver.ModelInfo{geminiModel("m-1"), geminiModel("m-2"), geminiModel("m-3")}
	s.run(t, "List", func(t *testing.T) {
		body := expectJSONResponse(t, s.extra("GET", "/v1beta/models", "", models), 200)
		if got := arr(body, "models"); len(got) != 3 {
			t.Fatalf("models: %d items, want 3", len(got))
		}
		expectPath(t, body, "models.0.name", "models/m-1")
		expectPath(t, body, "models.0.displayName", "Model m-1")
		expectPath(t, body, "models.0.inputTokenLimit", 1000)
		expectPath(t, body, "models.0.outputTokenLimit", 100)
		expectAbsent(t, body, "nextPageToken")
	})
	s.run(t, "Pages", func(t *testing.T) {
		body := expectJSONResponse(t, s.extra("GET", "/v1beta/models", "pageSize=2", models), 200)
		tok := str(body, "nextPageToken")
		if len(arr(body, "models")) != 2 || tok == "" {
			t.Fatalf("first page: %v", body)
		}
		next := expectJSONResponse(t, s.extra("GET", "/v1beta/models", "pageSize=2&pageToken="+tok, models), 200)
		expectPath(t, next, "models.0.name", "models/m-3")
		expectAbsent(t, next, "nextPageToken")
		bad := expectJSONResponse(t, s.extra("GET", "/v1beta/models", "pageToken=bad", models), 400)
		expectPath(t, bad, "error.status", "INVALID_ARGUMENT")
	})
	s.run(t, "Get", func(t *testing.T) {
		body := expectJSONResponse(t, s.extra("GET", "/v1beta/models/m-2", "", models), 200)
		expectPath(t, body, "name", "models/m-2")
		miss := expectJSONResponse(t, s.extra("GET", "/v1/models/nope", "", models), 404)
		expectPath(t, miss, "error.message", "Model is not found: models/nope for api version v1")
		w := s.extra("GET", "/v1beta/models/m-1:generateContent", "", models)
		if w.StatusCode() != 404 || len(w.Body()) != 0 {
			t.Errorf("GET with a method: %d %q, want the empty 404", w.StatusCode(), w.Body())
		}
	})
}

func (s *suite) geminiAuth(t *testing.T) {
	cases := []struct {
		name   string
		h      driver.MapHeaders
		key    string
		scheme string
	}{
		{"header", driver.MapHeaders{"x-goog-api-key": "AIza-1"}, "AIza-1", "x-goog-api-key"},
		{"query", driver.MapHeaders{driver.QueryHeader: "alt=sse&key=AIza-2"}, "AIza-2", "key"},
		{"bearer", driver.MapHeaders{"authorization": "Bearer oauth-access-token"}, "", "bearer"},
		{"none", driver.MapHeaders{}, "", "none"},
	}
	for _, c := range cases {
		s.run(t, c.name, func(t *testing.T) {
			cr := s.d.Authenticate(c.h)
			if cr.Key != c.key || cr.Scheme != c.scheme || len(cr.Missing) > 0 {
				t.Errorf("Authenticate = %+v, want key %q scheme %q", cr, c.key, c.scheme)
			}
		})
	}
	s.run(t, "Sniff", func(t *testing.T) {
		if !s.d.Sniff(driver.MapHeaders{"x-goog-api-key": "k"}) || !s.d.Sniff(driver.MapHeaders{driver.QueryHeader: "key=k"}) {
			t.Error("Sniff must accept x-goog-api-key and ?key=")
		}
		for _, f := range s.p.Foreign {
			if s.d.Sniff(driver.MapHeaders(f)) {
				t.Errorf("Sniff accepted a foreign credential %v", f)
			}
		}
	})
}

func (s *suite) geminiParse(t *testing.T) {
	s.run(t, "Full", func(t *testing.T) {
		req, e := s.d.ParseRequest(driver.KindGenerate, readTestdata(t, "generate_full.json"), geminiHeaders("generateContent", ""))
		if e != nil || req == nil {
			t.Fatal(e)
		}
		if req.Model != "m-1" || req.Stream || req.System != "You are terse." || req.MaxTokens != 256 {
			t.Errorf("model/stream/system/max: %q %v %q %d", req.Model, req.Stream, req.System, req.MaxTokens)
		}
		if len(req.Messages) != 4 || req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 ||
			len(req.Messages[2].ToolResults) != 1 || req.Messages[3].Content != "And in Rome?" || req.Messages[3].ImageParts != 1 {
			t.Errorf("messages: %+v", req.Messages)
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
			t.Errorf("allowedFunctionNames must keep get_weather only: %+v", req.Tools)
		}
		if req.ToolChoice.Mode != core.ToolChoiceNamed || req.ToolChoice.Name != "get_weather" {
			t.Errorf("ANY with one allowed name: %+v", req.ToolChoice)
		}
		if !req.Reasoning.Requested || req.Reasoning.BudgetTokens != 128 || req.Reasoning.Exclude {
			t.Errorf("thinkingConfig: %+v", req.Reasoning)
		}
		if req.ResponseFormat != "json_schema" || req.Temperature == nil || *req.Temperature != 0.5 || req.Seed == nil {
			t.Errorf("generationConfig: %q %v %v", req.ResponseFormat, req.Temperature, req.Seed)
		}
	})
	s.run(t, "StreamAndCount", func(t *testing.T) {
		body := []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
		req, e := s.d.ParseRequest(driver.KindGenerate, body, geminiHeaders("streamGenerateContent", "alt=sse"))
		if e != nil || req == nil || !req.Stream {
			t.Errorf("stream: %+v %v", req, e)
		}
		req, e = s.d.ParseRequest(driver.KindGenerate, body, geminiHeaders("countTokens", ""))
		if e != nil || req == nil || req.Endpoint != core.EndpointCountTokens {
			t.Errorf("countTokens: %+v %v", req, e)
		}
		req, e = s.d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generation_config":{"max_output_tokens":"9"}}`), geminiHeaders("generateContent", ""))
		if e != nil || req == nil || req.MaxTokens != 9 {
			t.Errorf("snake_case and numeric strings, as proto3 JSON: %+v %v", req, e)
		}
	})
	s.run(t, "Invalid", func(t *testing.T) {
		cases := []struct {
			name, method, body string
			status             int
			before             bool
			msg                string
		}{
			{"unknown_field", "generateContent", `{"contents":[],"foo":1}`, 400, true, `Invalid JSON payload received. Unknown name "foo": Cannot find field.`},
			{"wrong_type", "generateContent", `{"generationConfig":{"maxOutputTokens":"x"}}`, 400, true, `Invalid value at 'generation_config.max_output_tokens' (TYPE_INT32), "x"`},
			{"syntax", "generateContent", `{"contents": [`, 400, true, "Invalid JSON payload received. Unexpected end of string. Expected a value or ] within an array.\n\n^"},
			{"no_contents", "generateContent", `{}`, 400, false, "* GenerateContentRequest.contents: contents is not specified\n"},
			{"count_empty", "countTokens", `{}`, 400, false, "CountTokens requires generate_content_request or contents to be set."},
			{"model_turn", "generateContent", `{"contents":[{"role":"model","parts":[{"text":"x"}]}]}`, 400, false, "Requests ending with a model turn are not supported."},
			{"method", "fooBar", `{}`, 404, true, ""},
		}
		for _, c := range cases {
			s.run(t, c.name, func(t *testing.T) {
				_, e := s.d.ParseRequest(driver.KindGenerate, []byte(c.body), geminiHeaders(c.method, ""))
				if e == nil {
					t.Fatal("accepted")
				}
				if driver.BeforeAuth(e) != c.before {
					t.Errorf("BeforeAuth = %v, want %v", driver.BeforeAuth(e), c.before)
				}
				w := core.NewMemWriter()
				s.d.WriteError(w, &core.Request{Headers: geminiHeaders(c.method, "")}, e, nil)
				if w.StatusCode() != c.status {
					t.Errorf("status %d, want %d", w.StatusCode(), c.status)
				}
				if c.msg != "" {
					expectPath(t, decodeJSON(t, w.Body()), "error.message", c.msg)
				}
			})
		}
	})
}

// geminiResp is a response with every part.
func geminiResp(reasoning bool) *core.Response {
	r := &core.Response{
		ID: "Zv7latSxD4aiuMYP487m6A8", Model: "m-1", Content: "hello", FinishReason: core.FinishToolCalls,
		ToolCalls: []core.ToolCall{{ID: "call_000001", Name: "f", Arguments: `{"a":1}`}},
		Usage:     core.Usage{PromptTokens: 10, CompletionTokens: 7, CachedTokens: 4},
	}
	if reasoning {
		r.Reasoning, r.Usage.ReasoningTokens, r.Usage.CompletionTokens = "thinking", 3, 10
	}
	return r
}

func geminiReq(method, query string, include bool) *core.Request {
	return &core.Request{
		Driver: "gemini", Model: "m-1", Headers: geminiHeaders(method, query), Received: time.Now(),
		Endpoint: "generate_content", Reasoning: core.ReasoningRequest{Exclude: !include},
	}
}

func (s *suite) geminiResponse(t *testing.T) {
	for _, include := range []bool{true, false} {
		s.run(t, map[bool]string{true: "thoughts", false: "hidden"}[include], func(t *testing.T) {
			w := core.NewMemWriter()
			if err := s.d.WriteResponse(w, geminiReq("generateContent", "", include), geminiResp(true), nil); err != nil {
				t.Fatal(err)
			}
			body := expectJSONResponse(t, w, 200)
			expectHeader(t, w.Headers(), "X-Gemini-Service-Tier", "standard")
			first := "candidates.0.content.parts.0"
			if include {
				expectPath(t, body, first+".thought", true)
				first = "candidates.0.content.parts.1"
			}
			expectPath(t, body, first+".text", "hello")
			call := "candidates.0.content.parts." + map[bool]string{true: "2", false: "1"}[include]
			expectPath(t, body, call+".functionCall.name", "f")
			expectPath(t, body, call+".functionCall.args.a", 1)
			expectPresent(t, body, call+".thoughtSignature")
			expectPath(t, body, "candidates.0.content.role", "model")
			expectPath(t, body, "candidates.0.finishReason", "STOP")
			expectPath(t, body, "candidates.0.finishMessage", "Model generated function call(s).")
			expectPath(t, body, "usageMetadata.promptTokenCount", 10)
			expectPath(t, body, "usageMetadata.candidatesTokenCount", 7)
			expectPath(t, body, "usageMetadata.thoughtsTokenCount", 3)
			expectPath(t, body, "usageMetadata.totalTokenCount", 20)
			expectPath(t, body, "usageMetadata.cachedContentTokenCount", 4)
			expectPath(t, body, "modelVersion", "m-1")
			expectPath(t, body, "responseId", "Zv7latSxD4aiuMYP487m6A8")
		})
	}
	s.run(t, "MaxTokens", func(t *testing.T) {
		w := core.NewMemWriter()
		r := &core.Response{ID: "x", Model: "m-1", FinishReason: core.FinishLength, Usage: core.Usage{PromptTokens: 3}}
		_ = s.d.WriteResponse(w, geminiReq("generateContent", "", false), r, nil)
		body := expectJSONResponse(t, w, 200)
		expectPath(t, body, "candidates.0.finishReason", "MAX_TOKENS")
		expectPath(t, body, "candidates.0.content", map[string]any{})
	})
	s.run(t, "CountTokens", func(t *testing.T) {
		w := core.NewMemWriter()
		req := geminiReq("countTokens", "", false)
		req.Endpoint = core.EndpointCountTokens
		_ = s.d.WriteResponse(w, req, &core.Response{Usage: core.Usage{PromptTokens: 7}}, nil)
		body := expectJSONResponse(t, w, 200)
		expectPath(t, body, "totalTokens", 7)
		expectPath(t, body, "promptTokensDetails.0.modality", "TEXT")
		if _, ok := header(w.Headers(), "X-Gemini-Service-Tier"); ok {
			t.Error("countTokens carries no x-gemini-service-tier")
		}
	})
}

// geminiChunks decodes an SSE or JSON-array stream body.
func geminiChunks(t *testing.T, b string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if strings.HasPrefix(b, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(b), &arr); err != nil {
			t.Fatalf("array stream: %v\n%s", err, b)
		}
		for _, a := range arr {
			out = append(out, decodeJSON(t, a))
		}
		return out
	}
	if !strings.HasSuffix(b, "\r\n\r\n") {
		t.Fatalf("SSE events end with \\r\\n\\r\\n: %q", b)
	}
	for _, ev := range strings.Split(strings.TrimSuffix(b, "\r\n\r\n"), "\r\n\r\n") {
		data, ok := strings.CutPrefix(ev, "data: ")
		if !ok || strings.Contains(data, "\n") {
			t.Fatalf("event %q: one data: line", ev)
		}
		out = append(out, decodeJSON(t, []byte(data)))
	}
	return out
}

func (s *suite) geminiStream(t *testing.T) {
	for _, mode := range []string{"alt=sse", ""} {
		s.run(t, map[string]string{"alt=sse": "sse", "": "array"}[mode], func(t *testing.T) {
			w := core.NewMemWriter()
			resp := geminiResp(true)
			sw := s.d.StreamWriter(w, geminiReq("streamGenerateContent", mode, true), nil)
			noErr(t, sw.Start(resp))
			noErr(t, sw.Ping())
			noErr(t, sw.ReasoningDelta("thinking"))
			noErr(t, sw.TextDelta("hel"))
			noErr(t, sw.TextDelta("lo"))
			noErr(t, sw.ToolCallStart(0, "call_000001", "f"))
			noErr(t, sw.ToolArgsDelta(0, `{"a":`))
			noErr(t, sw.ToolArgsDelta(0, `1}`))
			noErr(t, sw.ToolCallEnd(0))
			noErr(t, sw.Finish(core.FinishToolCalls, resp.Usage, true))
			noErr(t, sw.Close())
			ct, _ := header(w.Headers(), "Content-Type")
			if (mode == "alt=sse") != (ct == "text/event-stream") {
				t.Errorf("Content-Type %q for %q", ct, mode)
			}
			chunks := geminiChunks(t, string(w.Body()))
			if len(chunks) != 5 {
				t.Fatalf("%d chunks, want thought, 2 texts, call, finish", len(chunks))
			}
			expectPath(t, chunks[0], "candidates.0.content.parts.0.thought", true)
			expectPath(t, chunks[1], "candidates.0.content.parts.0.text", "hel")
			expectPath(t, chunks[3], "candidates.0.content.parts.0.functionCall.args.a", 1)
			expectPresent(t, chunks[3], "candidates.0.content.parts.0.thoughtSignature")
			last := chunks[4]
			expectPath(t, last, "candidates.0.content.parts.0.text", "")
			expectAbsent(t, last, "candidates.0.content.parts.0.thoughtSignature")
			expectPath(t, last, "candidates.0.finishReason", "STOP")
			expectPath(t, last, "usageMetadata.totalTokenCount", 20)
			for i, c := range chunks {
				expectPath(t, c, "responseId", resp.ID)
				expectPath(t, c, "modelVersion", "m-1")
				if i < 4 {
					expectAbsent(t, c, "candidates.0.finishReason")
				}
			}
		})
	}
	s.run(t, "HiddenThoughts", func(t *testing.T) {
		w := core.NewMemWriter()
		resp := geminiResp(true)
		resp.ToolCalls = nil
		sw := s.d.StreamWriter(w, geminiReq("streamGenerateContent", "alt=sse", false), nil)
		noErr(t, sw.Start(resp))
		noErr(t, sw.ReasoningDelta("thinking"))
		noErr(t, sw.TextDelta("hello"))
		noErr(t, sw.Finish(core.FinishStop, resp.Usage, true))
		noErr(t, sw.Close())
		chunks := geminiChunks(t, string(w.Body()))
		if len(chunks) != 2 {
			t.Fatalf("%d chunks: hidden thoughts send nothing", len(chunks))
		}
		expectPath(t, chunks[0], "usageMetadata.thoughtsTokenCount", 3)
		expectPresent(t, chunks[1], "candidates.0.content.parts.0.thoughtSignature")
		if !strings.Contains(string(sw.LastFrame()), `"finishReason": "STOP"`) {
			t.Errorf("LastFrame %q", sw.LastFrame())
		}
	})
	s.run(t, "Error", func(t *testing.T) {
		w := core.NewMemWriter()
		sw := s.d.StreamWriter(w, geminiReq("streamGenerateContent", "alt=sse", false), nil)
		noErr(t, sw.Start(geminiResp(false)))
		noErr(t, sw.Error(&core.Error{Kind: core.ErrOverloaded}))
		noErr(t, sw.Raw([]byte("data: x\r\n\r\n")))
		chunks := geminiChunks(t, strings.TrimSuffix(string(w.Body()), "data: x\r\n\r\n"))
		expectPath(t, chunks[0], "error.code", 503)
		expectPath(t, chunks[0], "error.status", "UNAVAILABLE")
	})
	s.run(t, "ErrorBeforeStream", func(t *testing.T) {
		for _, mode := range []string{"alt=sse", ""} {
			w := core.NewMemWriter()
			s.d.WriteError(w, geminiReq("streamGenerateContent", mode, false), &core.Error{Kind: core.ErrModelNotFound}, nil)
			b := string(w.Body())
			if mode == "" {
				b = strings.TrimSuffix(strings.TrimPrefix(b, "["), "]")
			}
			if w.StatusCode() != 404 || strings.HasPrefix(b, "data:") {
				t.Errorf("%q: %d %q (the bare status, no data: frame)", mode, w.StatusCode(), b)
			}
			expectPath(t, decodeJSON(t, []byte(b)), "error.status", "NOT_FOUND")
		}
	})
}

func (s *suite) geminiErrors(t *testing.T) {
	sf := s.surface(driver.KindGenerate)
	s.run(t, "AllKinds", func(t *testing.T) {
		for _, kind := range AllErrorKinds {
			if kind == core.ErrContentFilter {
				continue // a blocked prompt is a 200 (PromptBlocked)
			}
			s.run(t, string(kind), func(t *testing.T) {
				w := core.NewMemWriter()
				s.d.WriteError(w, geminiReq("generateContent", "", false), &core.Error{Kind: kind}, nil)
				st := w.StatusCode()
				if st < 400 || st > 599 {
					t.Fatalf("status %d, want 4xx/5xx", st)
				}
				checkContentLength(t, w)
				if ct, _ := header(w.Headers(), "Content-Type"); strings.HasPrefix(ct, "application/json") {
					body := decodeJSON(t, w.Body())
					expectPath(t, body, "error.code", st)
					if str(body, "error.message") == "" || str(body, "error.status") == "" {
						t.Errorf("google.rpc.Status without message or status: %s", w.Body())
					}
				}
				if _, ok := header(w.Headers(), "retry-after"); ok {
					t.Error("the Gemini API sends no retry-after")
				}
			})
		}
	})
	s.run(t, "StatusOverride", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.WriteError(w, geminiReq("generateContent", "", false), &core.Error{Kind: core.ErrServerError, Status: 418}, nil)
		if w.StatusCode() != 418 {
			t.Errorf("status %d, want the Error.Status override 418", w.StatusCode())
		}
	})
	s.run(t, "Cases", func(t *testing.T) {
		for _, c := range sf.Errors {
			s.run(t, c.Name, func(t *testing.T) {
				req := geminiReq("generateContent", "", false)
				switch c.Name {
				case "bearer":
					req.Headers["authorization"] = "Bearer oauth-access-token"
					delete(req.Headers, "x-goog-api-key")
				case "missing_key":
					delete(req.Headers, "x-goog-api-key")
				}
				w := core.NewMemWriter()
				s.d.WriteError(w, req, cloneErr(c.Err), nil)
				if w.StatusCode() != c.Status {
					t.Errorf("status %d, want %d; body %s", w.StatusCode(), c.Status, w.Body())
				}
				body := decodeJSON(t, w.Body())
				for p, want := range c.Fields {
					expectPath(t, body, p, want)
				}
				for _, name := range c.NoHeaders {
					if v, ok := header(w.Headers(), name); ok {
						t.Errorf("header %s = %q, want absent", name, v)
					}
				}
			})
		}
	})
	s.run(t, "Blocked", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.WriteError(w, geminiReq("generateContent", "", false), &core.Error{Kind: core.ErrAccountSuspended, RetryAfter: time.Hour}, testRL())
		if w.StatusCode() != 403 {
			t.Errorf("blocked: %d, want 403", w.StatusCode())
		}
		if _, ok := header(w.Headers(), "retry-after"); ok {
			t.Error("blocked: no retry-after")
		}
	})
	s.run(t, "PromptBlocked", func(t *testing.T) {
		for _, mode := range []string{"generateContent", "alt=sse", "array"} {
			req := geminiReq("generateContent", "", false)
			switch mode {
			case "alt=sse":
				req = geminiReq("streamGenerateContent", "alt=sse", false)
			case "array":
				req = geminiReq("streamGenerateContent", "", false)
			}
			req.Messages = []core.Message{{Role: "user", Content: "hello there"}}
			w := core.NewMemWriter()
			s.d.WriteError(w, req, &core.Error{Kind: core.ErrContentFilter}, nil)
			if w.StatusCode() != 200 {
				t.Fatalf("%s: status %d, want 200", mode, w.StatusCode())
			}
			checkContentLength(t, w)
			var body map[string]any
			switch mode {
			case "generateContent":
				body = decodeJSON(t, w.Body())
			case "array":
				b := strings.TrimSuffix(strings.TrimPrefix(string(w.Body()), "["), "]")
				body = decodeJSON(t, []byte(b))
			default:
				body = geminiChunks(t, string(w.Body()))[0]
			}
			expectPath(t, body, "promptFeedback.blockReason", "SAFETY")
			expectPath(t, body, "promptFeedback.safetyRatings.0.blocked", true)
			expectAbsent(t, body, "candidates")
			expectPresent(t, body, "usageMetadata.promptTokenCount")
			expectAbsent(t, body, "usageMetadata.candidatesTokenCount")
			expectPath(t, body, "modelVersion", "m-1")
			expectPresent(t, body, "responseId")
		}
	})
	s.run(t, "NoRoute", func(t *testing.T) {
		for _, kind := range []core.ErrorKind{core.ErrNotFound, core.ErrMethodNotAllowed} {
			w := core.NewMemWriter()
			s.d.WriteError(w, nil, &core.Error{Kind: kind}, nil)
			if ct, _ := header(w.Headers(), "Content-Type"); w.StatusCode() != 404 || ct != "text/html" || len(w.Body()) != 0 {
				t.Errorf("%s: %d %q %q, want the empty text/html 404", kind, w.StatusCode(), ct, w.Body())
			}
		}
	})
}

// geminiNoRateLimit: the Gemini API sends no rate-limit headers (spec 006
// 16.5), on success, stream start or error.
func (s *suite) geminiNoRateLimit(t *testing.T) {
	check := func(t *testing.T, h map[string]string) {
		t.Helper()
		for k := range h {
			if l := strings.ToLower(k); strings.Contains(l, "ratelimit") || l == "retry-after" {
				t.Errorf("rate-limit header %s", k)
			}
		}
	}
	w := core.NewMemWriter()
	_ = s.d.WriteResponse(w, geminiReq("generateContent", "", false), geminiResp(false), testRL())
	check(t, w.Headers())
	w = core.NewMemWriter()
	_ = s.d.StreamWriter(w, geminiReq("streamGenerateContent", "alt=sse", false), testRL()).Start(geminiResp(false))
	check(t, w.Headers())
	w = core.NewMemWriter()
	s.d.WriteError(w, geminiReq("generateContent", "", false), &core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second}, testRL())
	check(t, w.Headers())
}

// Gemini ids (observed, task 164).
var (
	reGeminiResp = regexp.MustCompile(`^[A-Za-z0-9_-]{23}$`)
	reGeminiCall = regexp.MustCompile(`^call_[0-9]{6}$`)
	idsGemini    = &IDFormat{Response: reGeminiResp, ToolCall: reGeminiCall}
)

func (s *suite) geminiIDs(t *testing.T) {
	m, ok := s.minter()
	if !ok {
		t.Skip("driver mints no ids")
	}
	f := s.surface(driver.KindGenerate).IDs
	if f == nil {
		t.Skip("no id formats in the profile")
	}
	now := time.Unix(1793457766, 0)
	id := m.ResponseID(driver.KindGenerate, generate.NewRNG(1, "a"), now)
	if !f.Response.MatchString(id) {
		t.Errorf("responseId %q", id)
	}
	if b, err := base64.RawURLEncoding.DecodeString(id); err != nil || len(b) < 4 || int64(b[0])|int64(b[1])<<8|int64(b[2])<<16|int64(b[3])<<24 != now.Unix() {
		t.Errorf("responseId %q must start with the Unix seconds (little-endian)", id)
	}
	if id2 := m.ResponseID(driver.KindGenerate, generate.NewRNG(1, "a"), now); id2 != id {
		t.Errorf("not reproducible under a seed: %q %q", id, id2)
	}
	if c := m.ToolCallID(driver.KindGenerate, generate.NewRNG(1, "b")); !f.ToolCall.MatchString(c) {
		t.Errorf("call id %q", c)
	}
	if r := m.RequestID(driver.KindGenerate, "tok"); r == "" {
		t.Error("empty request id")
	}
}

// --- profile ---

var credGemini = map[string]string{"x-goog-api-key": "AIza-wwztest"}

func geminiErrorCases() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "bad"}, Status: 400, Fields: map[string]any{"error.status": "INVALID_ARGUMENT", "error.message": "bad"}},
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 400, Fields: map[string]any{
			"error.status": "INVALID_ARGUMENT", "error.message": "API key not valid. Please pass a valid API key.",
			"error.details.0.reason": "API_KEY_INVALID", "error.details.1.locale": "en-US",
		}},
		{Name: "missing_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 403, Fields: map[string]any{"error.status": "PERMISSION_DENIED"}},
		{Name: "bearer", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{
			"error.status": "UNAUTHENTICATED", "error.details.0.reason": "CREDENTIALS_MISSING",
			"error.details.0.metadata.method": "google.ai.generativelanguage.v1beta.GenerativeService.GenerateContent",
		}},
		{Name: "model_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 404, Fields: map[string]any{
			"error.status":  "NOT_FOUND",
			"error.message": "models/m-1 is not found for API version v1beta, or is not supported for generateContent. Call ModelService.ListModels to see the list of available models and their supported methods.",
		}},
		{Name: "rate_limit", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond, Extra: lim("rpm")}, Status: 429, NoHeaders: []string{"retry-after"}, Fields: map[string]any{
			"error.status": "RESOURCE_EXHAUSTED", "error.details.0.violations.0.quotaId": "GenerateRequestsPerMinutePerProjectPerModel",
			"error.details.2.retryDelay": "2s",
		}},
		{Name: "rate_limit_tpm", Err: core.Error{Kind: core.ErrRateLimit, Extra: lim("tpm")}, Status: 429, Fields: map[string]any{"error.details.0.violations.0.quotaId": "GenerateContentPaidTierInputTokensPerModelPerMinute"}},
		{Name: "quota", Err: core.Error{Kind: core.ErrQuota}, Status: 429, Fields: map[string]any{"error.status": "RESOURCE_EXHAUSTED"}},
		{Name: "geo_blocked", Err: core.Error{Kind: core.ErrGeoBlocked}, Status: 400, Fields: map[string]any{"error.status": "FAILED_PRECONDITION"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission}, Status: 403, Fields: map[string]any{"error.status": "PERMISSION_DENIED"}},
		{Name: "account_banned", Err: core.Error{Kind: core.ErrAccountBanned}, Status: 403, Fields: map[string]any{"error.details.0.reason": "CONSUMER_SUSPENDED"}},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.status": "INTERNAL"}},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded}, Status: 503, Fields: map[string]any{"error.status": "UNAVAILABLE", "error.message": "The model is overloaded. Please try again later."}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 504, Fields: map[string]any{"error.status": "DEADLINE_EXCEEDED"}},
	}
}

func geminiProfile() Profile {
	var rs []driver.Route
	for _, v := range []string{"/v1beta", "/v1"} {
		rs = concat(rs,
			routes("POST", "generate", v+"/models/*"),
			routes("GET", "resource", v+"/models", v+"/models/*", v+"/cachedContents", v+"/cachedContents/*"),
			routes("POST", "resource", v+"/cachedContents"),
			routes("PATCH", "resource", v+"/cachedContents/*"),
			routes("DELETE", "resource", v+"/cachedContents/*"),
		)
	}
	return Profile{
		Routes:     rs,
		Credential: credGemini, Key: "AIza-wwztest",
		AltCredential: map[string]string{driver.QueryHeader: "key=AIza-wwztest"},
		Foreign:       []map[string]string{credOpenAI, credAnthropic, credOpenCode, credOpenRouter},
		Models:        "gemini",
		Policy:        driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account"}, TPMReserve: "none", ResetFormat: "seconds"},
		Surfaces:      map[driver.Kind]Surface{driver.KindGenerate: {RateLimit: RLNone, Errors: geminiErrorCases(), IDs: idsGemini}},
	}
}
