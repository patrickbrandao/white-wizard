package drivertest

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The Gemini routes of an OpenCode-style gateway (spec 006 16.7, task 241):
// the GenerateContentRequest is the Gemini API's, the answer is Vertex AI's
// form for gemini-* models (createTime, candidates without index, usage
// details first, cost and opencode, no thoughts, a thoughtSignature on the
// text) and the translated form for any other model; the stream is always
// SSE; the key comes in x-goog-api-key or Authorization, never ?key=; a
// request the gateway refuses before the key is a bodiless 400 or 404. A
// profile with Vertex set runs these checks in place of the Gemini API
// ones (gemini.go) on the generate routes of the driver, which keeps the
// checks of the pure gemini driver unchanged.

// runVertex is runGemini for a gateway's Gemini routes.
func (s *suite) runVertex(t *testing.T) {
	s.run(t, "Routes", s.vertexRoutes)
	s.run(t, "Auth", s.vertexAuth)
	s.run(t, "Parse", s.vertexParse)
	s.run(t, "Response", s.vertexResponse)
	s.run(t, "Stream", s.vertexStream)
	s.run(t, "Errors", s.vertexErrors)
	s.run(t, "IDs", s.geminiIDs)
	s.run(t, "LimitPolicy", s.testLimitPolicy)
}

// vertexBase is the path of the v1beta routes, derived from the driver's
// first generate route ("<base>/google/v1beta/models/*").
func (s *suite) vertexBase(t *testing.T) string {
	t.Helper()
	for _, r := range s.routes() {
		if r.Kind == driver.KindGenerate && strings.HasSuffix(r.Path, "/v1beta/models/*") {
			return strings.TrimSuffix(r.Path, "/v1beta/models/*")
		}
	}
	t.Fatal("no v1beta generate route")
	return ""
}

func (s *suite) vertexHeaders(t *testing.T, version, model, method string) driver.MapHeaders {
	t.Helper()
	return driver.MapHeaders{
		"x-goog-api-key": "oc_sk_wwztest", "x-opencode-session": "ses_wwztest",
		"x-wwz-path": s.vertexBase(t) + "/" + version + "/models/" + model + ":" + method,
	}
}

func (s *suite) vertexReq(t *testing.T, model, method string) *core.Request {
	t.Helper()
	return &core.Request{
		Driver: s.d.Name(), Model: model, Headers: s.vertexHeaders(t, "v1beta", model, method), Received: time.Now(),
		Endpoint: "generate_content", Stream: method == "streamGenerateContent", IncludeUsage: true,
	}
}

func (s *suite) vertexRoutes(t *testing.T) {
	rs := s.routes()
	for _, r := range rs {
		if r.Kind != driver.KindGenerate || r.Method != "POST" || !strings.HasSuffix(r.Path, "/models/*") || !strings.Contains(r.Path, "/google/") {
			t.Errorf("route %+v: the Gemini routes of a gateway are POST .../google/{v1beta,v1}/models/*", r)
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

func (s *suite) vertexAuth(t *testing.T) {
	path := s.vertexHeaders(t, "v1beta", "m-1", "generateContent")["x-wwz-path"]
	cases := []struct {
		name   string
		h      driver.MapHeaders
		key    string
		scheme string
	}{
		{"x-goog-api-key", driver.MapHeaders{"x-goog-api-key": "oc_sk_1", "x-opencode-session": "s"}, "oc_sk_1", "x-goog-api-key"},
		{"bearer", driver.MapHeaders{"authorization": "Bearer oc_sk_2", "x-opencode-session": "s"}, "oc_sk_2", "bearer"},
		{"query_key_ignored", driver.MapHeaders{driver.QueryHeader: "key=oc_sk_3", "x-opencode-session": "s"}, "", "none"},
		{"none", driver.MapHeaders{}, "", "none"},
	}
	for _, c := range cases {
		s.run(t, c.name, func(t *testing.T) {
			c.h["x-wwz-path"] = path
			cr := s.d.Authenticate(c.h)
			if cr.Key != c.key || cr.Scheme != c.scheme || len(cr.Missing) > 0 {
				t.Errorf("Authenticate = %+v, want key %q scheme %q", cr, c.key, c.scheme)
			}
		})
	}
	s.run(t, "MissingSession", func(t *testing.T) {
		cr := s.d.Authenticate(driver.MapHeaders{"x-goog-api-key": "oc_sk_1", "x-wwz-path": path})
		if len(cr.Missing) != 1 || cr.Missing[0] != "x-opencode-session" {
			t.Errorf("Missing = %v, want x-opencode-session", cr.Missing)
		}
	})
}

func (s *suite) vertexParse(t *testing.T) {
	s.run(t, "Full", func(t *testing.T) {
		req, e := s.d.ParseRequest(driver.KindGenerate, readTestdata(t, "generate_full.json"), s.vertexHeaders(t, "v1beta", "gemini-m-1", "generateContent"))
		if e != nil || req == nil {
			t.Fatal(e)
		}
		if req.Model != "gemini-m-1" || req.Stream || req.System != "You are terse." || req.MaxTokens != 256 || len(req.Messages) != 4 || len(req.Tools) != 1 {
			t.Errorf("model/stream/system/max/messages/tools: %+v", req)
		}
		if !req.Reasoning.Exclude {
			t.Error("Vertex keeps the thoughts of a gemini-* model to itself")
		}
	})
	s.run(t, "TranslatedModelShowsThoughts", func(t *testing.T) {
		req, e := s.d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`), s.vertexHeaders(t, "v1beta", "m-1", "generateContent"))
		if e != nil || req == nil || req.Reasoning.Exclude || !req.Reasoning.Requested {
			t.Errorf("a translated model thinks by default and shows it: %+v %v", req, e)
		}
	})
	s.run(t, "Stream", func(t *testing.T) {
		req, e := s.d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`), s.vertexHeaders(t, "v1beta", "gemini-m-1", "streamGenerateContent"))
		if e != nil || req == nil || !req.Stream {
			t.Errorf("stream: %+v %v", req, e)
		}
	})
	body := `{"contents":[{"parts":[{"text":"hi"}]}]}`
	bodiless := []struct {
		name, version, method, body string
		status                      int
	}{
		{"count_tokens", "v1beta", "countTokens", body, 400},
		{"unknown_method", "v1beta", "fooBar", body, 400},
		{"not_json", "v1beta", "generateContent", `{"contents": [`, 400},
		{"v1", "v1", "generateContent", body, 404},
	}
	for _, c := range bodiless {
		s.run(t, c.name, func(t *testing.T) {
			h := s.vertexHeaders(t, c.version, "gemini-m-1", c.method)
			_, e := s.d.ParseRequest(driver.KindGenerate, []byte(c.body), h)
			if e == nil {
				t.Fatal("accepted")
			}
			w := core.NewMemWriter()
			s.d.WriteError(w, &core.Request{Headers: h}, e, nil)
			if w.StatusCode() != c.status || len(w.Body()) != 0 {
				t.Errorf("%d %q, want the bodiless %d", w.StatusCode(), w.Body(), c.status)
			}
		})
	}
	s.run(t, "VertexRefusal", func(t *testing.T) {
		h := s.vertexHeaders(t, "v1beta", "gemini-m-1", "generateContent")
		_, e := s.d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[],"foo":1}`), h)
		if e == nil {
			t.Fatal("accepted")
		}
		w := core.NewMemWriter()
		s.d.WriteError(w, &core.Request{Headers: h, Model: "gemini-m-1"}, e, nil)
		body := expectJSONResponse(t, w, 400)
		expectPath(t, body, "error.status", "INVALID_ARGUMENT")
		expectPath(t, body, "error.code", 400)
		if m := str(body, "error.message"); !strings.HasPrefix(m, "Upstream request failed: [400] ") {
			t.Errorf("message %q: Vertex's refusal is wrapped by the gateway", m)
		}
	})
	s.run(t, "NoContentsVertexText", func(t *testing.T) {
		h := s.vertexHeaders(t, "v1beta", "gemini-m-1", "generateContent")
		_, e := s.d.ParseRequest(driver.KindGenerate, []byte(`{}`), h)
		if e == nil {
			t.Fatal("accepted")
		}
		w := core.NewMemWriter()
		s.d.WriteError(w, &core.Request{Headers: h, Model: "gemini-m-1"}, e, nil)
		if m := str(decodeJSON(t, w.Body()), "error.message"); !strings.Contains(m, "Unable to submit request because at least one contents field is required") {
			t.Errorf("message %q: Vertex's text, not the Gemini API's", m)
		}
	})
}

func vertexResp(model string, reasoning bool) *core.Response {
	r := &core.Response{
		ID: "Zv7latSxD4aiuMYP487m6A8", Model: model, Content: "hello", FinishReason: core.FinishStop,
		Created: time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC),
		Usage:   core.Usage{PromptTokens: 10, CompletionTokens: 7},
	}
	if reasoning {
		r.Reasoning = "thinking"
	}
	return r
}

func (s *suite) vertexResponse(t *testing.T) {
	s.run(t, "Native", func(t *testing.T) {
		w := core.NewMemWriter()
		resp := vertexResp("gemini-m-1", true)
		resp.ToolCalls = []core.ToolCall{{ID: "call_000001", Name: "f", Arguments: `{"a":1}`}}
		resp.FinishReason = core.FinishToolCalls
		if err := s.d.WriteResponse(w, s.vertexReq(t, "gemini-m-1", "generateContent"), resp, nil); err != nil {
			t.Fatal(err)
		}
		body := expectJSONResponse(t, w, 200)
		expectPath(t, body, "createTime", "2026-10-31T12:00:00.123456Z")
		expectPath(t, body, "responseId", resp.ID)
		expectPath(t, body, "modelVersion", "gemini-m-1")
		expectPath(t, body, "candidates.0.finishReason", "STOP")
		expectAbsent(t, body, "candidates.0.index")
		expectPath(t, body, "candidates.0.content.role", "model")
		expectPath(t, body, "candidates.0.content.parts.0.text", "hello")
		expectAbsent(t, body, "candidates.0.content.parts.0.thought")
		expectPresent(t, body, "candidates.0.content.parts.1.thoughtSignature")
		expectPath(t, body, "candidates.0.content.parts.1.functionCall.id", "call_000001")
		expectPath(t, body, "candidates.0.content.parts.1.functionCall.args.a", 1)
		expectPath(t, body, "usageMetadata.promptTokenCount", 10)
		expectPath(t, body, "usageMetadata.candidatesTokenCount", 7)
		expectPath(t, body, "usageMetadata.totalTokenCount", 17)
		expectPath(t, body, "usageMetadata.promptTokensDetails.0.modality", "TEXT")
		expectPresent(t, body, "usageMetadata.cost")
		expectPresent(t, body, "usageMetadata.opencode")
		for _, h := range []string{"x-opencode-log-id", "x-zen-model"} {
			if _, ok := header(w.Headers(), h); !ok {
				t.Errorf("header %s missing", h)
			}
		}
	})
	s.run(t, "MaxTokens", func(t *testing.T) {
		w := core.NewMemWriter()
		resp := vertexResp("gemini-m-1", false)
		resp.FinishReason = core.FinishLength
		_ = s.d.WriteResponse(w, s.vertexReq(t, "gemini-m-1", "generateContent"), resp, nil)
		body := expectJSONResponse(t, w, 200)
		expectPath(t, body, "candidates.0.finishReason", "MAX_TOKENS")
		expectPresent(t, body, "candidates.0.continuationToken")
	})
	s.run(t, "Translated", func(t *testing.T) {
		w := core.NewMemWriter()
		if err := s.d.WriteResponse(w, s.vertexReq(t, "m-1", "generateContent"), vertexResp("m-1", true), nil); err != nil {
			t.Fatal(err)
		}
		body := expectJSONResponse(t, w, 200)
		expectAbsent(t, body, "createTime")
		expectAbsent(t, body, "responseId")
		expectPath(t, body, "modelVersion", "m-1")
		expectPath(t, body, "candidates.0.index", 0)
		expectPath(t, body, "candidates.0.finishReason", "STOP")
		expectPath(t, body, "candidates.0.content.parts.0.thought", true)
		expectPath(t, body, "candidates.0.content.parts.1.text", "hello")
		expectAbsent(t, body, "usageMetadata.promptTokensDetails")
		expectPath(t, body, "usageMetadata.totalTokenCount", 17)
	})
}

// vertexEvents decodes the "data: ...\n\n" events of a stream body.
func vertexEvents(t *testing.T, b string) []map[string]any {
	t.Helper()
	if !strings.HasSuffix(b, "\n\n") {
		t.Fatalf("SSE events end with a blank line: %q", b)
	}
	var out []map[string]any
	for _, ev := range strings.Split(strings.TrimSuffix(b, "\n\n"), "\n\n") {
		data, ok := strings.CutPrefix(ev, "data: ")
		if !ok || strings.Contains(data, "\n") {
			t.Fatalf("event %q: one data: line", ev)
		}
		out = append(out, decodeJSON(t, []byte(data)))
	}
	return out
}

func (s *suite) vertexStream(t *testing.T) {
	for _, query := range []string{"alt=sse", ""} {
		s.run(t, map[string]string{"alt=sse": "native_sse", "": "native_no_alt"}[query], func(t *testing.T) {
			w := core.NewMemWriter()
			req := s.vertexReq(t, "gemini-m-1", "streamGenerateContent")
			req.Headers[driver.QueryHeader] = query
			resp := vertexResp("gemini-m-1", true)
			sw := s.d.StreamWriter(w, req, nil)
			noErr(t, sw.Start(resp))
			noErr(t, sw.ReasoningDelta("thinking"))
			noErr(t, sw.TextDelta("hel"))
			noErr(t, sw.TextDelta("lo"))
			noErr(t, sw.Finish(core.FinishStop, resp.Usage, true))
			noErr(t, sw.Close())
			if ct, _ := header(w.Headers(), "Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("Content-Type %q: the stream is SSE whatever alt says", ct)
			}
			ev := vertexEvents(t, string(w.Body()))
			if len(ev) != 3 {
				t.Fatalf("%d events, want 2 texts and the finish (no thoughts on Vertex)", len(ev))
			}
			expectPath(t, ev[0], "candidates.0.content.parts.0.text", "hel")
			expectPath(t, ev[0], "usageMetadata", map[string]any{})
			expectAbsent(t, ev[0], "candidates.0.finishReason")
			last := ev[2]
			expectPath(t, last, "candidates.0.finishReason", "STOP")
			expectPath(t, last, "candidates.0.content.parts.0.text", "")
			expectPresent(t, last, "candidates.0.content.parts.0.thoughtSignature")
			expectPath(t, last, "usageMetadata.totalTokenCount", 17)
			for _, e := range ev {
				expectPath(t, e, "responseId", resp.ID)
			}
		})
	}
	s.run(t, "Translated", func(t *testing.T) {
		w := core.NewMemWriter()
		resp := vertexResp("m-1", true)
		sw := s.d.StreamWriter(w, s.vertexReq(t, "m-1", "streamGenerateContent"), nil)
		noErr(t, sw.Start(resp))
		noErr(t, sw.ReasoningDelta("thinking"))
		noErr(t, sw.TextDelta("hello"))
		noErr(t, sw.Finish(core.FinishStop, resp.Usage, true))
		noErr(t, sw.Close())
		ev := vertexEvents(t, string(w.Body()))
		if len(ev) != 3 {
			t.Fatalf("%d events, want thought, text and the finish", len(ev))
		}
		expectPath(t, ev[0], "candidates.0.content.parts.0.thought", true)
		expectPath(t, ev[0], "candidates.0.index", 0)
		expectAbsent(t, ev[0], "usageMetadata")
		expectPath(t, ev[2], "candidates.0.finishReason", "STOP")
		expectPath(t, ev[2], "usageMetadata.totalTokenCount", 17)
	})
	s.run(t, "Error", func(t *testing.T) {
		w := core.NewMemWriter()
		sw := s.d.StreamWriter(w, s.vertexReq(t, "gemini-m-1", "streamGenerateContent"), nil)
		noErr(t, sw.Start(vertexResp("gemini-m-1", false)))
		noErr(t, sw.Error(&core.Error{Kind: core.ErrOverloaded}))
		ev := vertexEvents(t, string(w.Body()))
		if st := str(ev[0], "error.status"); st == "" {
			t.Errorf("error event without status: %v", ev[0])
		}
	})
}

func (s *suite) vertexErrors(t *testing.T) {
	s.run(t, "AllKinds", func(t *testing.T) {
		for _, kind := range AllErrorKinds {
			s.run(t, string(kind), func(t *testing.T) {
				w := core.NewMemWriter()
				s.d.WriteError(w, s.vertexReq(t, "gemini-m-1", "generateContent"), &core.Error{Kind: kind}, nil)
				st := w.StatusCode()
				if st < 400 || st > 599 {
					t.Fatalf("status %d, want 4xx/5xx", st)
				}
				checkContentLength(t, w)
				if ct, _ := header(w.Headers(), "Content-Type"); strings.HasPrefix(ct, "application/json") {
					body := decodeJSON(t, w.Body())
					if str(body, "error.message") == "" {
						t.Errorf("error without message: %s", w.Body())
					}
					if kind != core.ErrMissingSession {
						expectPath(t, body, "error.code", st)
						if str(body, "error.status") == "" {
							t.Errorf("Google envelope without status: %s", w.Body())
						}
					}
				}
			})
		}
	})
	s.run(t, "MissingSession", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.WriteError(w, s.vertexReq(t, "gemini-m-1", "generateContent"), &core.Error{Kind: core.ErrMissingSession}, nil)
		body := expectJSONResponse(t, w, 400)
		expectPath(t, body, "type", "error")
		expectPath(t, body, "error.type", "MissingSessionID")
	})
	s.run(t, "RateLimit", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.WriteError(w, s.vertexReq(t, "gemini-m-1", "generateContent"), &core.Error{Kind: core.ErrRateLimit, Extra: lim("rpm")}, nil)
		if w.StatusCode() != 429 {
			t.Errorf("status %d, want 429", w.StatusCode())
		}
	})
}
