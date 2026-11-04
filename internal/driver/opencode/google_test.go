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

// The Gemini routes of OpenCode Go recorded on 2026-10-31 (task 241,
// testdata/real/opencode/google/): Vertex's form for gemini-4-argon, the
// translated form for deepseek-v4.1-flash, the route's checks and the
// gateway's errors.

func loadGoogle(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "google", name+".json"))
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
	reSignature    = regexp.MustCompile(`"(thoughtSignature|continuationToken)":"AY89a1[A-Za-z0-9+/=]+"`)
	reGoogleCallID = regexp.MustCompile(`"id":"call_[0-9]{6}"`)
	reRespID       = regexp.MustCompile(`^[A-Za-z0-9_-]{23}$`)
)

// normalizeGoogle replaces the opaque tokens and the dollar figures.
func normalizeGoogle(b string) string {
	b = reSignature.ReplaceAllString(b, `"$1":"<token>"`)
	b = reGoogleCallID.ReplaceAllString(b, `"id":"<call_id>"`)
	return normalize(b)
}

// googleHeaders are the fixture's headers (Content-Length only when the
// fixture keeps it: the bodiless answers) against the wwz answer's.
func checkGoogleHeaders(t *testing.T, fx shapeFixture, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		k = strings.ToLower(k)
		if _, keep := fx.Response.Headers[k]; relevant[k] || keep {
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

// googleHeaders are the request headers of a fixture as the server hands
// them to the driver.
func googleReqHeaders(fx shapeFixture) driver.MapHeaders {
	path, query, _ := strings.Cut(fx.Request.Path, "?")
	h := driver.MapHeaders{"x-wwz-path": path, driver.QueryHeader: query}
	for k, v := range fx.Request.Headers {
		h[k] = v
	}
	return h
}

type realGoogle struct {
	CreateTime   string `json:"createTime"`
	ResponseID   string `json:"responseId"`
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []struct {
				Text         string `json:"text"`
				Thought      bool   `json:"thought"`
				FunctionCall *struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Usage struct {
		Prompt     int `json:"promptTokenCount"`
		Candidates int `json:"candidatesTokenCount"`
		Thoughts   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

// response is the engine's response that the real answer reports.
func (r realGoogle) response() *core.Response {
	created, _ := time.Parse(time.RFC3339Nano, r.CreateTime)
	resp := &core.Response{
		ID: r.ResponseID, Model: r.ModelVersion, Created: created, FinishReason: core.FinishStop,
		Usage: core.Usage{
			PromptTokens: r.Usage.Prompt, CompletionTokens: r.Usage.Candidates + r.Usage.Thoughts,
			ReasoningTokens: r.Usage.Thoughts, CostMicroUSD: 122,
		},
	}
	if len(r.Candidates) == 0 {
		return resp
	}
	if r.Candidates[0].FinishReason == "MAX_TOKENS" {
		resp.FinishReason = core.FinishLength
	}
	for _, p := range r.Candidates[0].Content.Parts {
		switch {
		case p.FunctionCall != nil:
			resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args)})
			resp.FinishReason = core.FinishToolCalls
		case p.Thought:
			resp.Reasoning += p.Text
		default:
			resp.Content += p.Text
		}
	}
	return resp
}

func googleRequest(t *testing.T, d driver.Driver, fx shapeFixture) *core.Request {
	t.Helper()
	req, err := d.ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), googleReqHeaders(fx))
	if err != nil {
		t.Fatalf("%s: parse: %+v", fx.Name, err)
	}
	if req.Driver != "opencode" || req.Header("x-wwz-path") == "" {
		t.Fatalf("%s: driver %q, headers %v", fx.Name, req.Driver, req.Headers)
	}
	return req
}

// TestRealFixturesGoogleBody: generateContent byte for byte (tokens, call
// ids and dollar figures by form).
func TestRealFixturesGoogleBody(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"gen_goog", "gen_bearer", "gen_tool", "gen_maxtok", "gen_thinking_level", "gen_gpt_model"} {
		t.Run(name, func(t *testing.T) { checkGoogleBody(t, d, loadGoogle(t, name)) })
	}
}

// checkGoogleBody writes the engine's response that fx reports and
// compares it with fx's body and headers.
func checkGoogleBody(t *testing.T, d driver.Driver, fx shapeFixture) {
	t.Helper()
	var real realGoogle
	if err := json.Unmarshal([]byte(fx.Response.Body), &real); err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	if err := d.WriteResponse(w, googleRequest(t, d, fx), real.response(), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeGoogle(string(w.Body())), normalizeGoogle(fx.Response.Body); got != want {
		t.Errorf("body\n wwz  %s\n real %s", got, want)
	}
	checkGoogleHeaders(t, fx, w.Headers())
}

// TestRealFixturesGoogleStream: streamGenerateContent as the engine drives
// it; SSE with or without alt=sse.
func TestRealFixturesGoogleStream(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{"stream_sse", "stream_arr", "ds_stream"} {
		t.Run(name, func(t *testing.T) { checkGoogleStream(t, d, loadGoogle(t, name)) })
	}
}

// checkGoogleStream drives the stream writer with fx's deltas and usage
// and compares the stream with fx's body and headers.
func checkGoogleStream(t *testing.T, d driver.Driver, fx shapeFixture) {
	t.Helper()
	req := googleRequest(t, d, fx)
	if !req.Stream {
		t.Fatal("not a stream")
	}
	w := core.NewMemWriter()
	sw := d.StreamWriter(w, req, nil)
	var final realGoogle
	started := false
	for _, line := range strings.Split(fx.Response.Body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ch realGoogle
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			t.Fatal(err)
		}
		if !started {
			started = true
			r := ch.response()
			if err := sw.Start(&core.Response{ID: r.ID, Model: r.Model, Created: r.Created}); err != nil {
				t.Fatal(err)
			}
		}
		if ch.Candidates[0].FinishReason != "" {
			final = ch
			continue
		}
		for _, p := range ch.Candidates[0].Content.Parts {
			var err error
			if p.Thought {
				err = sw.ReasoningDelta(p.Text)
			} else {
				err = sw.TextDelta(p.Text)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	r := final.response()
	if err := sw.Finish(r.FinishReason, r.Usage, true); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := normalizeGoogle(string(w.Body())), normalizeGoogle(fx.Response.Body); got != want {
		t.Errorf("stream\n wwz  %s\n real %s", got, want)
	}
	checkGoogleHeaders(t, fx, w.Headers())
}

// TestRealFixturesGoogleChecks: the route's own checks and Vertex's
// refusals, with the real status, body and headers.
func TestRealFixturesGoogleChecks(t *testing.T) {
	d := newDriver(t)
	for _, name := range []string{
		"count", "gen_v1", "gen_badjson", "gen_badjson_nokey", "gen_unknown_method",
		"gen_unknown_field", "gen_temp", "gen_nocontents", "gen_model_turn",
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadGoogle(t, name)
			h := googleReqHeaders(fx)
			req, perr := d.ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), h)
			if perr == nil {
				t.Fatalf("accepted: %+v", req)
			}
			if perr.Status != fx.Response.Status {
				t.Errorf("error status %d, real %d", perr.Status, fx.Response.Status)
			}
			w := core.NewMemWriter()
			d.WriteError(w, &core.Request{Headers: h}, perr, nil)
			if w.StatusCode() != fx.Response.Status {
				t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
			}
			if got, want := string(w.Body()), fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
			checkGoogleHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesGoogleEngineErrors: the key, model and session errors
// the engine raises, in the route's envelope.
func TestRealFixturesGoogleEngineErrors(t *testing.T) {
	d := newDriver(t)
	for _, c := range []struct {
		name string
		e    core.Error
	}{
		{"gen_badgoog", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"gen_nokey", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"gen_querykey", core.Error{Kind: core.ErrInvalidAPIKey}},
		{"gen_unknown_model", core.Error{Kind: core.ErrModelNotFound}},
		{"gen_nosess", core.Error{Kind: core.ErrMissingSession}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := loadGoogle(t, c.name)
			h := googleReqHeaders(fx)
			for _, req := range []*core.Request{{Headers: h}, {Endpoint: "generate_content", Headers: h}} {
				w := core.NewMemWriter()
				d.WriteError(w, req, &c.e, nil)
				if w.StatusCode() != fx.Response.Status {
					t.Errorf("status %d, real %d", w.StatusCode(), fx.Response.Status)
				}
				if got, want := string(w.Body()), fx.Response.Body; got != want {
					t.Errorf("body\n wwz  %s\n real %s", got, want)
				}
				checkGoogleHeaders(t, fx, w.Headers())
			}
		})
	}
}

// TestGoogleAuthenticate: x-goog-api-key on the Gemini routes only; the
// bearer first; ?key= is no credential; the session stays required.
func TestGoogleAuthenticate(t *testing.T) {
	d := newDriver(t)
	g := "/inference/go/google/v1beta/models/gemini-4-argon:generateContent"
	for _, c := range []struct {
		name         string
		h            driver.MapHeaders
		key, scheme  string
		missingCount int
	}{
		{"goog", driver.MapHeaders{"x-wwz-path": g, "x-goog-api-key": "oc_sk_a", "x-opencode-session": "s"}, "oc_sk_a", "x-goog-api-key", 0},
		{"bearer_first", driver.MapHeaders{"x-wwz-path": g, "authorization": "Bearer oc_sk_b", "x-goog-api-key": "oc_sk_a", "x-opencode-session": "s"}, "oc_sk_b", "bearer", 0},
		{"no_session", driver.MapHeaders{"x-wwz-path": g, "x-goog-api-key": "oc_sk_a"}, "oc_sk_a", "x-goog-api-key", 1},
		{"chat_route", driver.MapHeaders{"x-wwz-path": "/inference/go/openai/v1/chat/completions", "x-goog-api-key": "oc_sk_a"}, "", "none", 0},
		{"query", driver.MapHeaders{"x-wwz-path": g, driver.QueryHeader: "key=oc_sk_a", "x-opencode-session": "s"}, "", "none", 0},
	} {
		cred := d.Authenticate(c.h)
		if cred.Key != c.key || cred.Scheme != c.scheme || len(cred.Missing) != c.missingCount {
			t.Errorf("%s: %+v", c.name, cred)
		}
	}
}

// TestGoogleNoKeyDefersVertex: without a key the body never reaches
// Vertex, so its refusal gives way to the engine's 401 (conferred,
// gen_nocontents_nokey).
func TestGoogleNoKeyDefersVertex(t *testing.T) {
	d := newDriver(t)
	fx := loadGoogle(t, "gen_nocontents_nokey")
	req, err := d.ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), googleReqHeaders(fx))
	if err != nil || req.Model != "gemini-4-argon" || req.Stream || req.Endpoint != "generate_content" {
		t.Fatalf("req %+v, err %+v", req, err)
	}
	w := core.NewMemWriter()
	d.WriteError(w, req, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	if got := string(w.Body()); got != fx.Response.Body || w.StatusCode() != 401 {
		t.Errorf("%d %s, real %s", w.StatusCode(), got, fx.Response.Body)
	}
}

// TestGoogleReasoning: Vertex hides the thoughts; the translation shows
// them by default.
func TestGoogleReasoning(t *testing.T) {
	d := newDriver(t)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"oi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW","includeThoughts":true}}}`)
	h := driver.MapHeaders{"x-wwz-path": "/inference/go/google/v1beta/models/gemini-4-argon:generateContent", "x-goog-api-key": "k"}
	req, err := d.ParseRequest(driver.KindGenerate, body, h)
	if err != nil || !req.Reasoning.Exclude {
		t.Fatalf("native: %+v %+v", req, err)
	}
	h["x-wwz-path"] = "/inference/google/v1beta/models/deepseek-v4.1-flash:streamGenerateContent"
	req, err = d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[{"parts":[{"text":"oi"}]}]}`), h)
	if err != nil || req.Reasoning.Exclude || !req.Reasoning.Requested || !req.Stream {
		t.Fatalf("translated: %+v %+v", req, err)
	}
	w := core.NewMemWriter()
	sw := d.StreamWriter(w, req, nil)
	_ = sw.Start(&core.Response{ID: "x", Model: "gemini-4-argon"})
	_ = sw.ReasoningDelta("hmm") // Vertex shows no thoughts
	if len(w.Body()) != 0 {
		t.Errorf("thought sent: %s", w.Body())
	}
}

// TestGoogleToolCalls: the calls of a native and of a translated answer,
// whole and in a stream; Vertex signs the first only.
func TestGoogleToolCalls(t *testing.T) {
	d := newDriver(t)
	calls := []core.ToolCall{{ID: "call_000001", Name: "a", Arguments: `{"x":1}`}, {ID: "call_000002", Name: "b", Arguments: "not json"}}
	for _, c := range []struct{ model, want string }{
		{"gemini-4-argon", `"parts":[{"text":"pre"},{"thoughtSignature":"<token>","functionCall":{"id":"<call_id>","name":"a","args":{"x":1}}},{"functionCall":{"id":"<call_id>","name":"b","args":{}}}]`},
		{"deepseek-v4.1-flash", `"parts":[{"text":"pre"},{"functionCall":{"name":"a","args":{"x":1}}},{"functionCall":{"name":"b","args":{}}}]`},
	} {
		req := &core.Request{Model: c.model, Headers: map[string]string{"x-wwz-path": "/inference/google/v1beta/models/" + c.model + ":generateContent"}}
		w := core.NewMemWriter()
		resp := &core.Response{ID: "id", Model: c.model, Content: "pre", ToolCalls: calls, FinishReason: core.FinishToolCalls}
		if err := d.WriteResponse(w, req, resp, nil); err != nil {
			t.Fatal(err)
		}
		if got := normalizeGoogle(string(w.Body())); !strings.Contains(got, c.want) {
			t.Errorf("%s: %s", c.model, got)
		}
		if !strings.Contains(string(w.Body()), `"billing_source":"balance"`) {
			t.Errorf("%s: not billed to the balance outside the Go base: %s", c.model, w.Body())
		}

		w = core.NewMemWriter()
		sw := d.StreamWriter(w, req, nil)
		_ = sw.Start(resp)
		for i, tc := range calls {
			_ = sw.ToolCallStart(i, tc.ID, tc.Name)
			_ = sw.ToolArgsDelta(i, tc.Arguments)
			_ = sw.ToolCallEnd(i)
		}
		_ = sw.ToolCallEnd(9) // unknown index: nothing
		_ = sw.ToolArgsDelta(9, "x")
		_ = sw.Ping()
		_ = sw.Finish(core.FinishToolCalls, core.Usage{PromptTokens: 3, CompletionTokens: 4, CachedTokens: 1}, true)
		if n := strings.Count(normalizeGoogle(string(w.Body())), `"thoughtSignature":"<token>"`); c.model == "gemini-4-argon" && n != 1 {
			t.Errorf("%s: %d signatures in %s", c.model, n, w.Body())
		}
		if c.model == "gemini-4-argon" && !strings.Contains(string(w.Body()), `"cachedContentTokenCount":1`) {
			t.Errorf("cached tokens missing: %s", w.Body())
		}
		if !strings.Contains(string(sw.LastFrame()), `"finishReason":"STOP"`) {
			t.Errorf("last frame %s", sw.LastFrame())
		}
	}
}

// TestGoogleStreamErrorAndRaw: a mid-stream error is a data line with the
// route's envelope; Raw writes verbatim.
func TestGoogleStreamErrorAndRaw(t *testing.T) {
	d := newDriver(t)
	req := &core.Request{Headers: map[string]string{"x-wwz-path": "/inference/google/v1beta/models/gemini-4-argon:streamGenerateContent"}}
	w := core.NewMemWriter()
	sw := d.StreamWriter(w, req, nil)
	_ = sw.Start(&core.Response{ID: "x", Model: "gemini-4-argon"})
	_ = sw.Error(&core.Error{Kind: core.ErrServerError})
	_ = sw.Error(&core.Error{Kind: core.ErrOverloaded, Message: upstreamText("Overloaded")})
	_ = sw.Raw([]byte("data: {bad\n\n"))
	want := `data: {"error":{"code":500,"status":"UNKNOWN","message":"Upstream request failed: Internal error"}}` + "\n\n" +
		`data: {"error":{"code":500,"status":"UNKNOWN","message":"Upstream request failed: Overloaded"}}` + "\n\n" + "data: {bad\n\n"
	if got := string(w.Body()); got != want {
		t.Errorf("got %q", got)
	}
}

func upstreamText(s string) string { return "Upstream request failed: " + s }

// TestGoogleGatewayErrors: the quota errors in Google's envelope (spec 006
// 16.3, not conferred), the finish reasons and an engine error.
func TestGoogleGatewayErrors(t *testing.T) {
	d := newDriver(t)
	h := map[string]string{"x-wwz-path": "/inference/go/google/v1beta/models/gemini-4-argon:generateContent"}
	for _, c := range []struct {
		e    core.Error
		want string
	}{
		{core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "quota_5h"}}, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Go usage limit exceeded","details":[{"reason":"GoUsageLimitError","metadata":{"workspace":"wrk_WERC8GMRZGE196QVYK49JVXS4G","limitName":"5 hour"}}]}}`},
		{core.Error{Kind: core.ErrQuota}, `{"error":{"code":402,"status":"UNKNOWN","message":"Credit limit exceeded.","details":[{"reason":"CreditLimitExceeded"}]}}`},
		{core.Error{Kind: core.ErrRateLimit}, `{"error":{"code":429,"status":"UNKNOWN","message":"Upstream request failed: Rate limit exceeded."}}`},
	} {
		w := core.NewMemWriter()
		d.WriteError(w, &core.Request{Endpoint: "generate_content", Headers: h}, &c.e, nil)
		if got := string(w.Body()); got != c.want {
			t.Errorf("%s: %s", c.e.Kind, got)
		}
	}
	for _, c := range []struct {
		r    core.FinishReason
		want string
	}{{core.FinishContentFilter, "SAFETY"}, {core.FinishError, "OTHER"}, {core.FinishLength, "MAX_TOKENS"}} {
		req := &core.Request{Model: "deepseek-v4.1-flash", Headers: h}
		w := core.NewMemWriter()
		_ = d.WriteResponse(w, req, &core.Response{ID: "x", Model: "deepseek-v4.1-flash", FinishReason: c.r}, nil)
		if !strings.Contains(string(w.Body()), `"parts":[]},"finishReason":"`+c.want+`"`) {
			t.Errorf("%s: %s", c.r, w.Body())
		}
	}
	// A native answer without text or calls still carries the signed
	// empty text part.
	w := core.NewMemWriter()
	_ = d.WriteResponse(w, &core.Request{Headers: h}, &core.Response{ID: "x", Model: "gemini-4-argon"}, nil)
	if !strings.Contains(normalizeGoogle(string(w.Body())), `"parts":[{"text":"","thoughtSignature":"<token>"}]`) {
		t.Errorf("empty: %s", w.Body())
	}
}

// TestGoogleRoutesAndIDs: the two versions under /inference and
// /inference/go (not /zen); Vertex's responseId and call_ + 6 digits.
func TestGoogleRoutesAndIDs(t *testing.T) {
	d := newDriver(t)
	want := map[string]bool{}
	for _, base := range []string{"/inference", "/inference/go"} {
		want[base+"/google/v1beta/models/*"], want[base+"/google/v1/models/*"] = true, true
	}
	for _, r := range d.Routes() {
		if r.Kind == driver.KindGenerate {
			if !want[r.Path] || r.Method != "POST" {
				t.Errorf("unexpected route %s %s", r.Method, r.Path)
			}
			delete(want, r.Path)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing routes %v", want)
	}
	m := d.(driver.IDMinter)
	rng := generate.NewRNG(1, "t")
	if id := m.ResponseID(driver.KindGenerate, rng, time.Now()); !reRespID.MatchString(id) {
		t.Errorf("response id %q", id)
	}
	if id := m.ToolCallID(driver.KindGenerate, rng); !regexp.MustCompile(`^call_[0-9]{6}$`).MatchString(id) {
		t.Errorf("call id %q", id)
	}
	if !d.Sniff(driver.MapHeaders{"authorization": "Bearer oc_sk_x"}) || d.Protocol() != "opencode" {
		t.Error("sniff or protocol")
	}
}

// TestGoogleCachedContent: the gateway serves no cachedContents, so a
// request naming one is the upstream's 403 (not conferred); the bearer
// key counts as a key.
func TestGoogleCachedContent(t *testing.T) {
	d := newDriver(t)
	h := driver.MapHeaders{"x-wwz-path": "/inference/google/v1beta/models/gemini-4-argon:generateContent", "authorization": "Bearer oc_sk_x"}
	_, perr := d.ParseRequest(driver.KindGenerate, []byte(`{"contents":[{"parts":[{"text":"oi"}]}],"cachedContent":"cachedContents/x"}`), h)
	if perr == nil {
		t.Fatal("accepted")
	}
	w := core.NewMemWriter()
	d.WriteError(w, &core.Request{Headers: h}, perr, nil)
	want := `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Upstream request failed: [403] CachedContent not found (or permission denied)"}}`
	if w.StatusCode() != 403 || string(w.Body()) != want {
		t.Errorf("%d %s", w.StatusCode(), w.Body())
	}
	// An invalid key presented in x-goog-api-key is "Invalid credential".
	w = core.NewMemWriter()
	d.WriteError(w, &core.Request{Endpoint: "generate_content", Headers: map[string]string{"x-wwz-path": h["x-wwz-path"], "x-goog-api-key": "bad"}}, &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	if !strings.Contains(string(w.Body()), "Invalid credential") {
		t.Errorf("%s", w.Body())
	}
}
