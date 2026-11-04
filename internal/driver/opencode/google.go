package opencode

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The Gemini routes of the gateway (POST <base>/google/v1beta/models/
// {model}:generateContent and :streamGenerateContent, only under
// /inference and /inference/go; /zen/go answers an HTML 404), as recorded
// against the real OpenCode Go on 2026-10-31 (task 241;
// testdata/real/opencode/google/). The body is read by the gemini
// driver's parser (the same GenerateContentRequest); the answer is the
// gateway's own:
//
//   - a Gemini model (gemini-*) goes to Vertex AI and answers in its form:
//     createTime, responseId, modelVersion, candidates[{finishReason,
//     content}] without index, usageMetadata with the details first, plus
//     cost and opencode; no thoughts, a thoughtSignature on the text;
//   - any other model is translated: candidates[{content, finishReason,
//     index}], usageMetadata, modelVersion, with the reasoning as thought
//     parts even without includeThoughts (deepseek-v4.1-flash);
//   - the stream is always SSE ("data: ...\n\n"), with or without alt=sse;
//   - the key comes in x-goog-api-key or Authorization (never ?key=);
//   - countTokens, an unknown method and a body that is not JSON are a 400
//     with no body, v1 a 404 with no body, before the key;
//   - the gateway's failures are {"error": {"code", "status": "UNKNOWN",
//     "message": "Upstream request failed: ..."}}; Vertex's refusals are
//     status INVALID_ARGUMENT and "Upstream request failed: [400] <text>",
//     with x-zen-model; MissingSessionID keeps the gateway envelope.

// googleSegment marks the Gemini routes in x-wwz-path.
const googleSegment = "/google/"

// Methods of models/{model}:{method} the gateway serves.
const (
	methodGenerate = "generateContent"
	methodStream   = "streamGenerateContent"
)

// endpointGenerate is the gemini driver's endpoint name (metrics,
// scenarios).
const endpointGenerate core.Endpoint = "generate_content"

// Vertex AI texts (conferred).
const (
	vertexNoContents  = "Unable to submit request because at least one contents field is required. Learn more: https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/gemini"
	vertexTemperature = "Unable to submit request because it has a temperature value of %s but the supported range is from 0 (inclusive) to 2.0001 (exclusive). Update the value and try again."
	vertexNoBudget    = "Thinking budget is not supported for this model. Use thinking level instead. See https://ai.google.dev/gemini-api/docs/thinking for details."
	vertexBudgetLevel = "Unable to submit request because thinking_budget and thinking_level are not supported together.. Learn more: https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/gemini"
)

// vertexThinking is Vertex's refusal of generationConfig.thinkingConfig on
// a Gemini 4 model (gemini-4-argon, the gateway's one; conferred on
// 2026-10-31, task 396): any thinkingBudget, 0 and -1 included, is
// refused, and with a thinkingLevel the text is the pair's; "" when the
// body names no budget. The other Gemini generations pass (not
// conferred: the gateway serves none). Keys in camelCase or snake_case,
// as the gemini parser reads them; a null budget counts as absent
// (neither conferred). The translated models take the budget
// (deepseek-v4.1-flash answers 200).
func vertexThinking(model string, body []byte) string {
	if !strings.HasPrefix(model, "gemini-4") {
		return ""
	}
	tc := protoField(protoField(body, "generationConfig", "generation_config"), "thinkingConfig", "thinking_config")
	switch {
	case protoField(tc, "thinkingBudget", "thinking_budget") == nil:
		return ""
	case protoField(tc, "thinkingLevel", "thinking_level") != nil:
		return vertexBudgetLevel
	}
	return vertexNoBudget
}

// protoField is the value of obj's field under either name; nil when obj
// is no object or the field is missing or null.
func protoField(obj json.RawMessage, camel, snake string) json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(obj, &m)
	for _, k := range []string{camel, snake} {
		if v, ok := m[k]; ok && string(v) != "null" {
			return v
		}
	}
	return nil
}

// geminiParser reads the GenerateContentRequest.
var geminiParser = &gemini.Driver{}

// isGoogle reports whether req came through a Gemini route.
func isGoogle(req *core.Request) bool {
	return req != nil && strings.Contains(req.Header("x-wwz-path"), googleSegment)
}

// nativeGoogle reports whether model goes to Vertex AI (the gateway's
// Gemini models) rather than through the translation.
func nativeGoogle(model string) bool { return strings.HasPrefix(model, "gemini") }

// pathHeaders shows the gemini parser the path relative to /google.
type pathHeaders struct {
	driver.Headers
	path string
}

func (p pathHeaders) Get(name string) string {
	if name == "x-wwz-path" {
		return p.path
	}
	return p.Headers.Get(name)
}

// emptyReply is a check's answer with no body (status 400 or 404).
func emptyReply(status int, msg string) *core.Error {
	e := rawError(msg, rawReply{status: status})
	e.Status = status
	return e
}

// googleCredential is the key of a Gemini route: Authorization, else
// x-goog-api-key.
func googleCredential(h driver.Headers) string {
	if auth := h.Get("authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return h.Get("x-goog-api-key")
}

// parseGoogle reads models/{model}:{method}.
func (d *Driver) parseGoogle(body []byte, h driver.Headers) (*core.Request, *core.Error) {
	_, rest, _ := strings.Cut(h.Get("x-wwz-path"), googleSegment)
	version, res, _ := strings.Cut(rest, "/")
	if version != "v1beta" {
		return nil, emptyReply(404, "Not Found")
	}
	id := strings.TrimPrefix(res, "models/")
	model, method := id, ""
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		model, method = id[:i], id[i+1:]
	}
	if model == "" || (method != methodGenerate && method != methodStream) {
		return nil, emptyReply(400, "Bad Request")
	}
	if !json.Valid(body) {
		return nil, emptyReply(400, "invalid JSON")
	}
	req, err := geminiParser.ParseRequest(driver.KindGenerate, body, pathHeaders{Headers: h, path: "/" + rest})
	if err != nil {
		if googleCredential(h) == "" {
			// Vertex never sees a request without a key: the engine
			// refuses it with the gateway's 401 (conferred,
			// gen_nocontents_nokey).
			return bareGoogle(model, method, body, h), nil
		}
		return nil, vertexError(err, body, model)
	}
	req.Driver = d.Name()
	if mh, ok := h.(driver.MapHeaders); ok {
		req.Headers = mh
	}
	if nativeGoogle(model) {
		if msg := vertexThinking(model, body); msg != "" && googleCredential(h) != "" {
			return nil, vertexError(&core.Error{Message: msg}, body, model)
		}
		// Vertex keeps the thoughts to itself (conferred with
		// includeThoughts and thinkingLevel LOW).
		req.Reasoning.Exclude = true
	} else {
		// The translated upstreams think by default and the gateway
		// shows the thoughts (conferred, gen_gpt_model).
		req.Reasoning.Exclude = false
		req.Reasoning.Requested = true
	}
	return req, nil
}

// bareGoogle is a request the engine only resolves the model and the key
// of.
func bareGoogle(model, method string, body []byte, h driver.Headers) *core.Request {
	req := &core.Request{
		Driver: "opencode", Endpoint: endpointGenerate, Model: model, Raw: body,
		Stream: method == methodStream, IncludeUsage: true, Received: time.Now(), N: 1,
	}
	if mh, ok := h.(driver.MapHeaders); ok {
		req.Headers = mh
	}
	return req
}

// vertexError is the upstream's refusal of a body the gemini parser
// refused: the Vertex text where it differs from the Gemini API's.
func vertexError(e *core.Error, body []byte, model string) *core.Error {
	msg := strings.TrimSuffix(e.Message, "\n")
	switch {
	case strings.Contains(msg, "contents is not specified"):
		msg = vertexNoContents
	case strings.Contains(msg, "generation_config.temperature"):
		var b struct {
			GenerationConfig struct {
				Temperature json.Number `json:"temperature"`
			} `json:"generationConfig"`
		}
		_ = json.Unmarshal(body, &b)
		msg = strings.Replace(vertexTemperature, "%s", b.GenerationConfig.Temperature.String(), 1)
	}
	code, status := 400, "INVALID_ARGUMENT"
	if e.Kind == core.ErrPermission {
		code, status = 403, "PERMISSION_DENIED"
	}
	b, _ := jsonFormat.Marshal(oai.O("error", oai.O("code", code, "status", status, "message", upstreamPrefix+"["+strconv.Itoa(code)+"] "+msg)))
	r := rawError(msg, rawReply{status: code, body: string(b), model: model})
	r.Status = code
	return r
}

// writeGoogleError is the error envelope of a Gemini route.
func (d *Driver) writeGoogleError(w core.ResponseWriter, req *core.Request, e *core.Error) {
	if req.Endpoint == "" {
		r := *req
		r.Endpoint = endpointGenerate // not the models listing's 401
		req = &r
	}
	status, typ, msg, meta, hdr := d.errorParts(req, e)
	var body oai.Obj
	switch {
	case typ == "MissingSessionID":
		body = oai.O("type", "error", "error", oai.O("type", typ, "message", msg)) // conferred, gen_nosess
	case gatewayTypes[typ]:
		// The quota errors in Google's envelope (spec 006 16.3; not
		// conferred).
		rpc := "UNKNOWN"
		if status == 429 {
			rpc = "RESOURCE_EXHAUSTED"
		}
		info := oai.O("reason", typ)
		if meta != nil {
			info = append(info, oai.Field{Key: "metadata", Val: meta})
		}
		body = oai.O("error", oai.O("code", status, "status", rpc, "message", msg, "details", []any{info}))
	default:
		body = oai.O("error", oai.O("code", status, "status", "UNKNOWN", "message", msg))
	}
	b, _ := jsonFormat.Marshal(body)
	w.Status(status)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	for k, v := range hdr {
		w.Header(k, v)
	}
	_, _ = w.Write(b)
	_ = w.Flush()
}

// googleFinish maps the engine's finish reason.
func googleFinish(r core.FinishReason) string {
	switch r {
	case core.FinishLength:
		return "MAX_TOKENS"
	case core.FinishContentFilter, core.FinishRefusal:
		return "SAFETY"
	case core.FinishError:
		return "OTHER"
	}
	return "STOP"
}

// vertexToken is an opaque Vertex token (thoughtSignature, 85 bytes;
// continuationToken, 149 bytes): the observed 01 8f 3d 6b 5f prefix and
// bytes drawn from the response id.
// A seeded run repeats it.
func vertexToken(respID, salt string, n int) string {
	rng := generate.NewRNG(1, "opencode-vertex-"+salt, respID)
	b := make([]byte, n)
	copy(b, []byte{0x01, 0x8f, 0x3d, 0x6b, 0x5f})
	for i := 5; i < n; i++ {
		b[i] = byte(rng.IntN(256)) //nolint:gosec // G115: IntN(256) fits a byte
	}
	return base64.StdEncoding.EncodeToString(b)
}

// googleUsage is usageMetadata. Vertex lists the details first and counts
// no thoughts (conferred). The translation counts the reasoning apart:
// candidatesTokenCount without it, thoughtsTokenCount after the total and
// opencode.tokens.reasoning, with output and the total still counting it
// (conferred by the drift of task 156: 35 + 3 + 10 = 48, output 13); an
// upstream that reports no reasoning tokens leaves thoughtsTokenCount out
// (gen_gpt_model). Both end with cost and opencode.
func googleUsage(req *core.Request, u core.Usage, native bool) oai.Obj {
	p, c := u.PromptTokens, u.CompletionTokens
	if !native {
		r := min(max(u.ReasoningTokens, 0), c)
		u.ReasoningTokens = r
		cost, oc := opencodeUsage(req, u, true)
		o := oai.O("promptTokenCount", p, "candidatesTokenCount", c-r, "totalTokenCount", p+c)
		if r > 0 {
			o = append(o, oai.Field{Key: "thoughtsTokenCount", Val: r})
		}
		return append(o, oai.Field{Key: "cost", Val: cost}, oai.Field{Key: "opencode", Val: oc})
	}
	cost, oc := opencodeUsage(req, u, false)
	o := oai.O(
		"candidatesTokensDetails", []any{oai.O("modality", "TEXT", "tokenCount", c)},
		"promptTokensDetails", []any{oai.O("modality", "TEXT", "tokenCount", p)},
		"candidatesTokenCount", c, "promptTokenCount", p, "totalTokenCount", p+c,
	)
	if u.CachedTokens > 0 {
		o = append(o, oai.Field{Key: "cachedContentTokenCount", Val: u.CachedTokens}) // not conferred
	}
	return append(o, oai.Field{Key: "cost", Val: cost}, oai.Field{Key: "opencode", Val: oc})
}

// googleArgs is a tool call's arguments as a JSON object.
func googleArgs(s string) json.RawMessage {
	if len(s) > 0 && s[0] == '{' && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage("{}")
}

// googleCall is a functionCall part: id, name, args after the signature
// on Vertex (conferred, gen_tool); name and args on the translation (not
// conferred).
func googleCall(tc core.ToolCall, native bool, sig string) oai.Obj {
	if !native {
		return oai.O("functionCall", oai.O("name", tc.Name, "args", googleArgs(tc.Arguments)))
	}
	call := oai.O("functionCall", oai.O("id", tc.ID, "name", tc.Name, "args", googleArgs(tc.Arguments)))
	if sig == "" {
		return call
	}
	return append(oai.O("thoughtSignature", sig), call...)
}

// googleParts are the parts of a whole answer.
func googleParts(resp *core.Response, native bool) []any {
	var ps []any
	if !native && resp.Reasoning != "" {
		ps = append(ps, oai.O("text", resp.Reasoning, "thought", true))
	}
	if resp.Content != "" || (native && len(resp.ToolCalls) == 0) {
		text := oai.O("text", resp.Content)
		if native && len(resp.ToolCalls) == 0 {
			text = append(text, oai.Field{Key: "thoughtSignature", Val: vertexToken(resp.ID, "sig", 85)})
		}
		ps = append(ps, text)
	}
	for i, tc := range resp.ToolCalls {
		sig := ""
		if i == 0 {
			sig = vertexToken(resp.ID, "sig", 85)
		}
		ps = append(ps, googleCall(tc, native, sig))
	}
	if ps == nil {
		ps = []any{}
	}
	return ps
}

// createTime is the instant Vertex took the request, in microseconds.
func createTime(resp *core.Response) string {
	t := resp.Created
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

// vertexCandidate is a Vertex candidate: finishReason (and the
// continuationToken of a MAX_TOKENS) before content (conferred).
func vertexCandidate(resp *core.Response, finish core.FinishReason, parts []any) oai.Obj {
	cand := oai.O("finishReason", googleFinish(finish))
	if finish == core.FinishLength {
		cand = append(cand, oai.Field{Key: "continuationToken", Val: vertexToken(resp.ID, "continuation", 149)})
	}
	return append(cand, oai.Field{Key: "content", Val: oai.O("role", "model", "parts", parts)})
}

// googleResponse is the whole answer of generateContent.
func googleResponse(req *core.Request, resp *core.Response) oai.Obj {
	native := nativeGoogle(resp.Model)
	parts := googleParts(resp, native)
	u := googleUsage(req, resp.Usage, native)
	if !native {
		cand := oai.O("content", oai.O("role", "model", "parts", parts), "finishReason", googleFinish(resp.FinishReason), "index", 0)
		return oai.O("candidates", []any{cand}, "usageMetadata", u, "modelVersion", resp.Model)
	}
	return oai.O("createTime", createTime(resp), "responseId", resp.ID, "modelVersion", resp.Model,
		"candidates", []any{vertexCandidate(resp, resp.FinishReason, parts)}, "usageMetadata", u)
}

// googleHeaders are the headers of a 200.
func googleHeaders(w core.ResponseWriter, resp *core.Response) {
	w.Header("x-opencode-log-id", newLogID(time.Now()))
	if resp.Model != "" {
		w.Header("x-zen-model", resp.Model)
	}
	for k, v := range resp.Headers {
		w.Header(k, v)
	}
}

// writeGoogle writes generateContent.
func writeGoogle(w core.ResponseWriter, req *core.Request, resp *core.Response) error {
	b, _ := jsonFormat.Marshal(googleResponse(req, resp))
	w.Status(200)
	w.Header("Content-Type", "application/json")
	w.Header("Content-Length", strconv.Itoa(len(b)))
	googleHeaders(w, resp)
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// googleStream is streamGenerateContent: SSE whatever alt says. Vertex
// sends each delta with an empty usageMetadata and closes with the finish
// reason, an empty text part with the signature and the usage; the
// translation sends the thoughts and the text with index 0 and closes with
// empty parts and the usage (conferred, stream_sse, stream_arr,
// ds_stream).
type googleStream struct {
	w      core.ResponseWriter
	req    *core.Request
	resp   *core.Response
	native bool
	signed bool
	calls  map[int]*core.ToolCall
	last   []byte
}

func newGoogleStream(w core.ResponseWriter, req *core.Request) *googleStream {
	return &googleStream{w: w, req: req, calls: map[int]*core.ToolCall{}}
}

func (s *googleStream) Start(resp *core.Response) error {
	s.resp = resp
	s.native = nativeGoogle(resp.Model)
	s.w.Status(200)
	s.w.Header("Content-Type", streamContentType)
	s.w.Header("Cache-Control", "no-cache")
	googleHeaders(s.w, resp)
	return nil
}

// chunk is one answer of the stream.
func (s *googleStream) chunk(parts []any, finish *core.FinishReason, u oai.Obj) oai.Obj {
	if s.native {
		cand := oai.O("content", oai.O("role", "model", "parts", parts))
		if finish != nil {
			cand = vertexCandidate(s.resp, *finish, parts)
		}
		return oai.O("createTime", createTime(s.resp), "responseId", s.resp.ID, "modelVersion", s.resp.Model,
			"candidates", []any{cand}, "usageMetadata", u)
	}
	cand := oai.O("content", oai.O("role", "model", "parts", parts))
	if finish != nil {
		cand = append(cand, oai.Field{Key: "finishReason", Val: googleFinish(*finish)})
	}
	cand = append(cand, oai.Field{Key: "index", Val: 0})
	o := oai.O("candidates", []any{cand})
	if u != nil {
		o = append(o, oai.Field{Key: "usageMetadata", Val: u})
	}
	return append(o, oai.Field{Key: "modelVersion", Val: s.resp.Model})
}

// running is the usageMetadata of a delta: {} on Vertex, absent on the
// translation.
func (s *googleStream) running() oai.Obj {
	if s.native {
		return oai.Obj{}
	}
	return nil
}

func (s *googleStream) frame(v oai.Obj) error {
	b, _ := jsonFormat.Marshal(v)
	out := make([]byte, 0, len(b)+8)
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, "\n\n"...)
	s.last = out
	if _, err := s.w.Write(out); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *googleStream) ReasoningDelta(text string) error {
	if s.native {
		return nil
	}
	return s.frame(s.chunk([]any{oai.O("text", text, "thought", true)}, nil, s.running()))
}

func (s *googleStream) TextDelta(text string) error {
	return s.frame(s.chunk([]any{oai.O("text", text)}, nil, s.running()))
}

func (s *googleStream) ToolCallStart(index int, id, name string) error {
	s.calls[index] = &core.ToolCall{ID: id, Name: name}
	return nil
}

func (s *googleStream) ToolArgsDelta(index int, text string) error {
	if c := s.calls[index]; c != nil {
		c.Arguments += text
	}
	return nil
}

// ToolCallEnd sends the whole call; on Vertex the first carries the
// signature (not conferred in a stream).
func (s *googleStream) ToolCallEnd(index int) error {
	c := s.calls[index]
	if c == nil {
		return nil
	}
	sig := ""
	if s.native && !s.signed {
		s.signed = true
		sig = vertexToken(s.resp.ID, "sig", 85)
	}
	return s.frame(s.chunk([]any{googleCall(*c, s.native, sig)}, nil, s.running()))
}

func (s *googleStream) Ping() error { return nil }

func (s *googleStream) Finish(reason core.FinishReason, u core.Usage, _ bool) error {
	parts := []any{}
	if s.native {
		p := oai.O("text", "")
		if !s.signed {
			s.signed = true
			p = append(p, oai.Field{Key: "thoughtSignature", Val: vertexToken(s.resp.ID, "sig", 85)})
		}
		parts = append(parts, p)
	}
	return s.frame(s.chunk(parts, &reason, googleUsage(s.req, u, s.native)))
}

// Error after the 200: a data line with the route's error envelope (not
// conferred).
func (s *googleStream) Error(e *core.Error) error {
	b, _ := jsonFormat.Marshal(oai.O("error", oai.O("code", 500, "status", "UNKNOWN", "message", upstreamPrefix+orDefault(e))))
	out := append(append([]byte("data: "), b...), "\n\n"...)
	s.last = out
	if _, err := s.w.Write(out); err != nil {
		return err
	}
	return s.w.Flush()
}

// orDefault is e's text, or the gateway's default for its kind.
func orDefault(e *core.Error) string {
	if e.Message != "" {
		return strings.TrimPrefix(e.Message, upstreamPrefix)
	}
	return strings.TrimPrefix(openCodeDefaultMessage(e), upstreamPrefix)
}

func (s *googleStream) Raw(p []byte) error {
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *googleStream) LastFrame() []byte { return s.last }

func (s *googleStream) Close() error { return nil }
