// Package ids shapes identifiers like the real providers do (chatcmpl-,
// msg_01, toolu_01, req_, gen-). Every function draws from the generator it
// is given, so ids are reproducible under a fixed seed.
package ids

import (
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

const (
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	hex    = "0123456789abcdef"
)

// Sizes of the random part of each id, measured on real responses.
const (
	OpenAIChatLen    = 29 // chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT
	OpenAICallLen    = 24 // call_DdmO9pD3xa9XTPNJ32zg2hcA
	OpenAIRequestLen = 32 // req_ + 32 hex
	AnthropicLen     = 22 // msg_01 + 22 = msg_01XFDUDYJgAACzvnptvVoYEL (24 after "msg_")
	// AnthropicNewLen is the random part after "msg_011C"/"req_011C", the
	// prefix of the current message and request ids (24 after "msg_" as
	// well: msg_011Cgabzfnu6w5BgM1qV3TQ6, req_011Cgabt8hTTYTtQJqu8WHKK,
	// observed on 2026-10-31, task 235).
	AnthropicNewLen  = 20
	OpenRouterGenLen = 20 // gen-1729282847-kVXyqVP8MBLkSjvx8kB5
)

func draw(r *generate.RNG, alphabet string, n int, prefix string) string {
	// Built on the stack; the string is the only allocation (task 270).
	var buf [64]byte
	b := append(buf[:0], prefix...)
	for range n {
		b = append(b, alphabet[r.IntN(len(alphabet))])
	}
	return string(b)
}

// OpenAIChat returns "chatcmpl-" + 29 base62 chars.
func OpenAIChat(r *generate.RNG) string { return draw(r, base62, OpenAIChatLen, "chatcmpl-") }

// OpenAICompletion returns "cmpl-" + 29 base62 chars (legacy /v1/completions).
func OpenAICompletion(r *generate.RNG) string { return draw(r, base62, OpenAIChatLen, "cmpl-") }

// OpenAICall returns "call_" + 24 base62 chars.
func OpenAICall(r *generate.RNG) string { return draw(r, base62, OpenAICallLen, "call_") }

// OpenAIRequest returns "req_" + 32 lowercase hex chars (x-request-id).
func OpenAIRequest(r *generate.RNG) string { return draw(r, hex, OpenAIRequestLen, "req_") }

// AnthropicMessage returns "msg_011C" + 20 base62 chars.
func AnthropicMessage(r *generate.RNG) string { return draw(r, base62, AnthropicNewLen, "msg_011C") }

// AnthropicToolUse returns "toolu_01" + 22 base62 chars (still the real
// form on 2026-10-31).
func AnthropicToolUse(r *generate.RNG) string { return draw(r, base62, AnthropicLen, "toolu_01") }

// AnthropicRequest returns "req_011C" + 20 base62 chars (request-id header
// and request_id in error bodies).
func AnthropicRequest(r *generate.RNG) string { return draw(r, base62, AnthropicNewLen, "req_011C") }

// OpenRouterGen returns "gen-<unix seconds>-<20 base62 chars>".
func OpenRouterGen(r *generate.RNG, now time.Time) string {
	return draw(r, base62, OpenRouterGenLen, "gen-"+strconv.FormatInt(now.Unix(), 10)+"-")
}

// FromToken derives a generator from a unique token, so a unique token always
// yields the same id. Request ids use it: they must stay unique per request
// even under a fixed seed (the limiter tracks in-flight requests by id).
func FromToken(token string) *generate.RNG { return generate.NewRNG(1, "request-id", token) }
