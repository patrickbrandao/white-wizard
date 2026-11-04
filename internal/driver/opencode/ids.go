package opencode

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

var _ driver.IDMinter = (*Driver)(nil)

// The ids below were recorded against the real gateway on 2026-10-31
// (spec 006 16.7, task 175; testdata/real/opencode/shape/). The Chat
// Completions upstream of deepseek-v4.1-flash answers gw-<12 hex>-<6 hex>
// with call_<24 hex> tool calls; the Messages route translates and answers
// a bare 32-hex id.

const hexDigits = "0123456789abcdef"

func drawHex(r *generate.RNG, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[r.IntN(len(hexDigits))]
	}
	return string(b)
}

// RequestID is internal: OpenCode sends no request id in headers or bodies.
func (d *Driver) RequestID(kind driver.Kind, unique string) string {
	if kind == driver.KindMessages {
		return ids.AnthropicRequest(ids.FromToken(unique))
	}
	return ids.OpenAIRequest(ids.FromToken(unique))
}

// ResponseID is "gw-<12 hex>-<6 hex>" on Chat Completions and 32 hex on
// Messages.
func (d *Driver) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	switch kind {
	case driver.KindMessages:
		return drawHex(rng, 32)
	case driver.KindResponses:
		return "resp_" + drawHex(rng, 32) // conferred (task 240)
	case driver.KindGenerate:
		return geminiParser.ResponseID(kind, rng, now) // Vertex's responseId (conferred, task 241)
	}
	return "gw-" + drawHex(rng, 12) + "-" + drawHex(rng, 6)
}

// ToolCallID is "call_<24 hex>" on Chat Completions; on Messages the
// Anthropic form (not conferred).
func (d *Driver) ToolCallID(kind driver.Kind, rng *generate.RNG) string {
	switch kind {
	case driver.KindMessages:
		return ids.AnthropicToolUse(rng)
	case driver.KindGenerate:
		return geminiParser.ToolCallID(kind, rng) // call_ and 6 digits (conferred, gen_tool)
	case driver.KindResponses:
		// The DeepSeek upstream's form on the Responses route:
		// call_00_ and 26 base-36 characters (conferred, task 240).
		const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
		b := make([]byte, 0, 34)
		b = append(b, "call_00_"...)
		for range 26 {
			b = append(b, digits[rng.IntN(len(digits))])
		}
		return string(b)
	}
	return "call_" + drawHex(rng, 24)
}

// newLogID is x-opencode-log-id: a UUIDv7 (48-bit Unix milliseconds,
// version 7, RFC 4122 variant), fresh on every answer.
func newLogID(now time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[6:])
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli())) //nolint:gosec // G115: Unix milliseconds are positive
	copy(b[:6], ms[2:])
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
