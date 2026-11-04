package generate

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Tokenizer counts and splits text into tokens.
type Tokenizer interface {
	// Count returns the token count of text (exact for the BPE encodings of
	// internal/tokenize, approximate for Simulated).
	Count(text string) int
	// Split returns the text as a sequence of token strings whose
	// concatenation equals the input (used to stream token by token).
	Split(text string) []string
}

// Simulated is a fast regex-free tokenizer: a token is a run of letters or
// digits (with a leading space attached, like BPE tokenizers), or a single
// punctuation/whitespace character. Long words are cut every 6 characters
// to approximate subword splitting. It costs no startup and no allocation
// beyond the output slice.
type Simulated struct{}

func (Simulated) Count(text string) int {
	// Same state machine as Split, without building strings.
	n := 0
	run := 0
	buf := false // a pending token exists in the buffer
	flush := func() {
		if buf {
			n++
			buf = false
		}
		run = 0
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			buf = true
			run++
			if run == 6 {
				flush()
			}
		case unicode.IsSpace(r):
			flush()
			buf = true
		default:
			flush()
			buf = true
			flush()
		}
	}
	flush()
	return n
}

func (t Simulated) Split(text string) []string {
	if text == "" {
		return nil
	}
	if utf8.ValidString(text) {
		// The tokens are substrings of text (Each): one allocation, the
		// slice, sized by Count (task 270). Invalid UTF-8 takes the path
		// below, which writes U+FFFD for each bad byte.
		out := make([]string, 0, t.Count(text))
		t.Each(text, func(tok string) { out = append(out, tok) })
		return out
	}
	return splitRunes(text)
}

// splitRunes is Split rune by rune, writing each rune again: invalid UTF-8
// comes out as U+FFFD, as Split always did.
func splitRunes(text string) []string {
	var out []string
	var b strings.Builder
	run := 0
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
		run = 0
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			run++
			if run == 6 {
				flush()
			}
		case unicode.IsSpace(r):
			flush()
			b.WriteRune(r)
		default:
			flush()
			b.WriteRune(r)
			flush()
		}
	}
	flush()
	return out
}

// Each calls fn with every token of text, in order, as substrings of text
// (no allocation); the tokens are the ones Split returns.
func (Simulated) Each(text string, fn func(tok string)) {
	start, run := 0, 0
	flush := func(i int) {
		if i > start {
			fn(text[start:i])
		}
		start, run = i, 0
	}
	for i, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			run++
			if run == 6 {
				flush(i + utf8.RuneLen(r))
			}
		case unicode.IsSpace(r):
			flush(i)
		default:
			flush(i)
			flush(i + utf8.RuneLen(r))
		}
	}
	flush(len(text))
}

// EachToken calls fn with every token of text: through Each when tk has it,
// else over Split.
func EachToken(tk Tokenizer, text string, fn func(tok string)) {
	if e, ok := tk.(interface{ Each(string, func(string)) }); ok {
		e.Each(text, fn)
		return
	}
	for _, t := range tk.Split(text) {
		fn(t)
	}
}

// PromptBreakdown is the input token composition (ManagedCode-style).
type PromptBreakdown struct {
	System      int `json:"system"`
	ToolSchemas int `json:"tool_schemas"`
	User        int `json:"user"`
	Assistant   int `json:"assistant"`
	ToolCalls   int `json:"tool_calls"`
	ToolResults int `json:"tool_results"`
	Images      int `json:"images"`
	// Cached is the part of Total read from the prompt cache; CountPrompt
	// leaves it 0 (the engine fills it once the cache hit is decided).
	Cached int `json:"cached"`
	// Total is the input count that feeds usage and the limits, before
	// discounting the cache.
	Total int `json:"total"`
}

// AppendJSON appends the compact JSON used by x-wwz-input-breakdown (spec
// 013, I16), in the order system, tool_schemas, user, assistant, tool_calls,
// tool_results, images, cached, total.
func (b PromptBreakdown) AppendJSON(dst []byte) []byte {
	fields := [...]struct {
		name string
		v    int
	}{
		{"system", b.System},
		{"tool_schemas", b.ToolSchemas},
		{"user", b.User},
		{"assistant", b.Assistant},
		{"tool_calls", b.ToolCalls},
		{"tool_results", b.ToolResults},
		{"images", b.Images},
		{"cached", b.Cached},
		{"total", b.Total},
	}
	dst = append(dst, '{')
	for i, f := range fields {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = append(dst, f.name...)
		dst = append(dst, '"', ':')
		dst = strconv.AppendInt(dst, int64(f.v), 10)
	}
	return append(dst, '}')
}

// Chat template overhead (spec 007, ADR 0049), the rule OpenAI documents for
// Chat Completions: every message costs MessageOverhead + tokens(role) +
// tokens(content), plus NameOverhead when it has a name, and the request adds
// ReplyPriming tokens that open the assistant's reply.
const (
	MessageOverhead = 3
	NameOverhead    = 1
	ReplyPriming    = 3
)

// messageCost is the template cost of one message besides its content.
func messageCost(tk Tokenizer, role, name string) int {
	n := MessageOverhead + tk.Count(role)
	if name != "" {
		n += NameOverhead
	}
	return n
}

// CountPrompt counts prompt tokens with the chat template overhead (3 +
// role per message, 3 per request, 1 per name; spec 007) and returns the
// composition by origin. The top-level system prompt (Anthropic) is a system
// message and the reply priming counts as assistant. Tool definitions,
// json_schema formats, tool calls and tool results cost as the OpenAI models
// render them (tools.go); images keep their own cost. Claude requests follow
// their own template (countAnthropicPrompt).
func CountPrompt(tk Tokenizer, req *core.Request, imageTokens int) PromptBreakdown {
	if anthropicTemplate(req) {
		return countAnthropicPrompt(tk, req, imageTokens)
	}
	var b PromptBreakdown
	if sys := req.System; sys != "" {
		b.System = tk.Count(sys) + messageCost(tk, "system", "")
	}
	b.ToolSchemas = ToolSchemaTokens(tk, req)
	var calls map[string]callCosts
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			b.System += tk.Count(m.Content) + messageCost(tk, m.Role, m.Name)
		case "user":
			b.User += tk.Count(m.Content) + messageCost(tk, m.Role, m.Name)
			for _, tr := range m.ToolResults {
				b.ToolResults += tk.Count(tr.Content) + 6
			}
		case "assistant":
			// A turn with calls and no text has no text message.
			if m.Content != "" || len(m.ToolCalls) == 0 {
				b.Assistant += tk.Count(m.Content) + messageCost(tk, m.Role, m.Name)
			}
			if len(m.ToolCalls) > 0 {
				if calls == nil {
					calls = map[string]callCosts{}
				}
				b.ToolCalls += toolCallTokens(tk, req, m, calls)
			}
		case "tool":
			b.ToolResults += toolResultTokens(tk, req, m, calls)
		default:
			b.User += tk.Count(m.Content) + messageCost(tk, m.Role, m.Name)
		}
		b.Images += (m.ImageParts + m.AudioParts) * imageTokens
	}
	b.Assistant += ReplyPriming
	b.Total = b.System + b.ToolSchemas + b.User + b.Assistant + b.ToolCalls + b.ToolResults + b.Images
	return b
}
