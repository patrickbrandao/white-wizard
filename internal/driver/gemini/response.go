package gemini

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// GenerateContentResponse (spec 006 16.5), conferred on 2026-10-31 (task
// 164, testdata/real/gemini/shape/): candidates[{content {parts, role},
// finishReason, index[, finishMessage]}], usageMetadata, modelVersion (the
// id), responseId. The thoughtSignature rides on the first functionCall or,
// without calls, on the last part; a function call ends with STOP and
// finishMessage "Model generated function call(s).".

const finishCalls = "Model generated function call(s)."

// finishReason maps the engine's reason.
func finishReason(r core.FinishReason) string {
	switch r {
	case core.FinishLength:
		return "MAX_TOKENS"
	case core.FinishContentFilter, core.FinishRefusal:
		return "SAFETY"
	case core.FinishError:
		return "OTHER"
	}
	return "STOP" // stop and tool_calls
}

// usage is usageMetadata (observed order: promptTokenCount,
// candidatesTokenCount, totalTokenCount, cachedContentTokenCount,
// promptTokensDetails, cacheTokensDetails, thoughtsTokenCount,
// serviceTier). Zero counts are absent, like proto3 JSON omits them.
// candidates excludes the thoughts; the total includes them.
func usage(prompt, candidates, thoughts, cached int) oai.Obj {
	o := oai.O("promptTokenCount", prompt)
	if candidates > 0 {
		o = append(o, oai.Field{Key: "candidatesTokenCount", Val: candidates})
	}
	o = append(o, oai.Field{Key: "totalTokenCount", Val: prompt + candidates + thoughts})
	if cached > 0 {
		o = append(o, oai.Field{Key: "cachedContentTokenCount", Val: cached})
	}
	o = append(o, oai.Field{Key: "promptTokensDetails", Val: []any{oai.O("modality", "TEXT", "tokenCount", prompt)}})
	if cached > 0 {
		o = append(o, oai.Field{Key: "cacheTokensDetails", Val: []any{oai.O("modality", "TEXT", "tokenCount", cached)}})
	}
	if thoughts > 0 {
		o = append(o, oai.Field{Key: "thoughtsTokenCount", Val: thoughts})
	}
	return append(o, oai.Field{Key: "serviceTier", Val: "standard"})
}

// cachedTokens is what the prompt read from a cache: the explicit
// cachedContent the request names (its whole count, observed), else the
// engine's implicit cache.
func cachedTokens(req *core.Request, u core.Usage) int {
	if req != nil {
		if n, err := strconv.Atoi(req.Metadata[metaCachedTokens]); err == nil {
			return n
		}
	}
	return u.CachedTokens
}

// finalUsage is the usageMetadata of a finished response.
func finalUsage(req *core.Request, u core.Usage) oai.Obj {
	return usage(u.PromptTokens, u.CompletionTokens-u.ReasoningTokens, u.ReasoningTokens, cachedTokens(req, u))
}

// signature is a thoughtSignature: a protobuf envelope around opaque
// bytes, longer when the model thought (observed: 98 bytes without
// thoughts, ~1.4 KB after ~450 thought tokens). It is drawn from the
// response id, so a seeded run repeats it.
func signature(respID string, thoughts int) string {
	rng := generate.NewRNG(1, "gemini-signature", respID)
	inner := []byte{0x01, 0x69, 0x14, 0x7d, 0x13}
	n := 89 + 3*thoughts
	for i := 0; i < n; i++ {
		inner = append(inner, byte(rng.IntN(256))) //nolint:gosec // G115: IntN(256) fits a byte
	}
	msg := append([]byte{0x0a}, varint(len(inner))...)
	msg = append(msg, inner...)
	out := append([]byte{0x12}, varint(len(msg))...)
	out = append(out, msg...)
	return base64.StdEncoding.EncodeToString(out)
}

func varint(n int) []byte {
	var b []byte
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n)) //nolint:gosec // G115: n < 0x80 here
}

// argsJSON is a tool call's arguments as a JSON object.
func argsJSON(s string) json.RawMessage {
	if json.Valid([]byte(s)) && len(s) > 0 && s[0] == '{' {
		return json.RawMessage(s)
	}
	return json.RawMessage("{}")
}

// callPart is a functionCall part (observed order: name, args, id).
func callPart(tc core.ToolCall) oai.Obj {
	return oai.O("functionCall", oai.O("name", tc.Name, "args", argsJSON(tc.Arguments), "id", tc.ID))
}

// parts are the parts of a whole response: the thoughts (only with
// includeThoughts), the text, the function calls; then the signature.
func parts(req *core.Request, resp *core.Response) []oai.Obj {
	var ps []oai.Obj
	if resp.Reasoning != "" && (req == nil || !req.Reasoning.Exclude) {
		ps = append(ps, oai.O("text", resp.Reasoning, "thought", true))
	}
	if resp.Content != "" {
		ps = append(ps, oai.O("text", resp.Content))
	}
	firstCall := len(ps)
	for _, tc := range resp.ToolCalls {
		ps = append(ps, callPart(tc))
	}
	if len(ps) == 0 {
		return nil
	}
	sig := oai.Field{Key: "thoughtSignature", Val: signature(resp.ID, resp.Usage.ReasoningTokens)}
	at := len(ps) - 1
	if len(resp.ToolCalls) > 0 {
		at = firstCall
	}
	ps[at] = append(ps[at], sig)
	return ps
}

// contentObj is a candidate's content: {} when there is no part
// (observed on a MAX_TOKENS answer with no text).
func contentObj(ps []oai.Obj) oai.Obj {
	if len(ps) == 0 {
		return oai.Obj{}
	}
	return oai.O("parts", ps, "role", "model")
}

// responseObj is a whole GenerateContentResponse.
func responseObj(req *core.Request, resp *core.Response) oai.Obj {
	cand := oai.O("content", contentObj(parts(req, resp)), "finishReason", finishReason(resp.FinishReason), "index", 0)
	if len(resp.ToolCalls) > 0 {
		cand = append(cand, oai.Field{Key: "finishMessage", Val: finishCalls})
	}
	return oai.O("candidates", []any{cand}, "usageMetadata", finalUsage(req, resp.Usage),
		"modelVersion", resp.Model, "responseId", resp.ID)
}

// countObj is the countTokens answer (observed: totalTokens,
// cachedContentTokenCount, promptTokensDetails, cacheTokensDetails).
func countObj(req *core.Request, u core.Usage) oai.Obj {
	cached := cachedTokens(req, u)
	o := oai.O("totalTokens", u.PromptTokens)
	if cached > 0 {
		o = append(o, oai.Field{Key: "cachedContentTokenCount", Val: cached})
	}
	o = append(o, oai.Field{Key: "promptTokensDetails", Val: []any{oai.O("modality", "TEXT", "tokenCount", u.PromptTokens)}})
	if cached > 0 {
		o = append(o, oai.Field{Key: "cacheTokensDetails", Val: []any{oai.O("modality", "TEXT", "tokenCount", cached)}})
	}
	return o
}

// WriteResponse writes generateContent or countTokens. The generate answer
// carries x-gemini-service-tier; countTokens does not (observed).
func (d *Driver) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, _ *core.RateLimitState) error {
	var body []byte
	if req != nil && req.Endpoint == core.EndpointCountTokens {
		body = pretty(countObj(req, resp.Usage))
	} else {
		body = pretty(responseObj(req, resp))
		w.Header("X-Gemini-Service-Tier", "standard")
	}
	w.Status(200)
	w.Header("Content-Type", contentJSON)
	w.Header("Content-Length", strconv.Itoa(len(body)))
	commonHeaders(w, req)
	extraHeaders(w, resp)
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

// blockedObj is the answer to a prompt the safety filter refuses (spec 006
// 16.5, documented; "não conferido" against the real API): no candidates,
// promptFeedback {blockReason, safetyRatings}, usageMetadata with the
// prompt count only, modelVersion, responseId. Only the offending rating
// carries blocked (proto3 JSON omits false).
func blockedObj(req *core.Request) oai.Obj {
	prompt, model, id := 0, "", ""
	if req != nil {
		prompt = gtok.CountPrompt(gtok.Tokenizer{}, req).Total
		model = req.Model
		now := req.Received
		if now.IsZero() {
			now = time.Now()
		}
		id = (&Driver{}).ResponseID(driver.KindGenerate, generate.NewRNG(1, "gemini-blocked", req.RequestID), now)
	}
	rating := func(cat, p string, blocked bool) oai.Obj {
		o := oai.O("category", cat, "probability", p)
		if blocked {
			o = append(o, oai.Field{Key: "blocked", Val: true})
		}
		return o
	}
	ratings := []any{
		rating("HARM_CATEGORY_HATE_SPEECH", "HIGH", true),
		rating("HARM_CATEGORY_DANGEROUS_CONTENT", "NEGLIGIBLE", false),
		rating("HARM_CATEGORY_HARASSMENT", "NEGLIGIBLE", false),
		rating("HARM_CATEGORY_SEXUALLY_EXPLICIT", "NEGLIGIBLE", false),
	}
	return oai.O("promptFeedback", oai.O("blockReason", "SAFETY", "safetyRatings", ratings),
		"usageMetadata", oai.O("promptTokenCount", prompt, "totalTokenCount", prompt),
		"modelVersion", model, "responseId", id)
}

// writeBlocked answers a blocked prompt: HTTP 200 on every route. A stream
// route sends the object as its only chunk (an SSE event with alt=sse, else
// a one-element JSON array).
func writeBlocked(w core.ResponseWriter, req *core.Request) {
	v := blockedObj(req)
	var body []byte
	ct := contentJSON
	switch {
	case routeOfReq(req).method != methodStream:
		body = pretty(v)
	case sse(reqHeaders(req)):
		body, ct = sseEvent(v), contentSSE
		w.Header("Content-Disposition", "attachment")
	default:
		body = append(append([]byte("["), pretty(v)...), ']')
	}
	w.Status(200)
	w.Header("X-Gemini-Service-Tier", "standard")
	w.Header("Content-Type", ct)
	w.Header("Content-Length", strconv.Itoa(len(body)))
	commonHeaders(w, req)
	_, _ = w.Write(body)
	_ = w.Flush()
}
