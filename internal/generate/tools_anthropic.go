package generate

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Tool definitions as the Claude models see them (spec 007, tasks 259 and
// 264). Derived from /v1/messages/count_tokens on claude-haiku-4-5 (identical
// on claude-sonnet-4-5 and claude-opus-4-5) and confirmed by /v1/messages
// (testdata/real/anthropic/tools/, 31/10/2026), then on claude-fable-5 and
// claude-haiku-5-5 (tools-claude-fable-5/, tools-claude-haiku-5-5/). The
// tools enter a system prompt of their own: a fixed text that depends on
// tool_choice and the generation (claudeFamily) plus the definitions
// rendered by AnthropicToolsText. The constants are real tokens, exact; the
// definitions are counted by the model's tokenizer (the simulated one for
// Claude, so that part is approximate).

// anthropicTemplate reports a request that Claude counts (Messages API and
// count_tokens of the anthropic driver).
func anthropicTemplate(req *core.Request) bool {
	return req.Driver == "anthropic" && (req.Endpoint == core.EndpointMessages || req.Endpoint == core.EndpointCountTokens)
}

// AnthropicToolsText renders tool definitions as Claude 4.5 reads them: one
// <function> per tool with the JSON of description (when not empty), name
// and parameters (the input_schema), keys sorted at every level, ", " and
// ": " separators and non-ASCII escaped as \uXXXX (Python's
// json.dumps(sort_keys=True)), inside <functions> … </functions>.
func AnthropicToolsText(tools []core.ToolDef) string {
	return anthropicToolsText(tools, true)
}

// AnthropicToolsTextFor renders the tools as model reads them: from Claude
// 4.6 on, non-ASCII stays as UTF-8 (ensure_ascii=False; task 264).
func AnthropicToolsTextFor(model string, tools []core.ToolDef) string {
	return anthropicToolsText(tools, claudeFamilyOf(model).asciiTools)
}

func anthropicToolsText(tools []core.ToolDef, ascii bool) string {
	var b strings.Builder
	b.WriteString("<functions>\n")
	for i, t := range tools {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("<function>{")
		if t.Description != "" {
			b.WriteString(`"description": `)
			writePyString(&b, t.Description, ascii)
			b.WriteString(", ")
		}
		b.WriteString(`"name": `)
		writePyString(&b, t.Name, ascii)
		b.WriteString(`, "parameters": `)
		params := parseJSON(t.Parameters)
		if params.kind == 0 {
			b.WriteString("{}")
		} else {
			writePyJSON(&b, params, ascii)
		}
		b.WriteString("}</function>")
	}
	b.WriteString("\n</functions>")
	return b.String()
}

// anthropicToolTokens is the cost of the tools system prompt of a Claude
// request: nothing without tools. Its system header is the request's when
// it has a system prompt (which then joins with sysJoin).
func anthropicToolTokens(tk Tokenizer, req *core.Request) int {
	if len(req.Tools) == 0 {
		return 0
	}
	f := claudeFamilyOf(req.Model)
	noParallel := req.ParallelToolCalls != nil && !*req.ParallelToolCalls
	n := countToolsText(tk, anthropicToolsText(req.Tools, f.asciiTools))
	if _, ok := tk.(Simulated); ok && f.toolsTextPct > 0 {
		n = (n*f.toolsTextPct + 50) / 100
	}
	switch tc := req.ToolChoice; tc.Mode {
	case core.ToolChoiceRequired:
		n += f.toolsAny
	case core.ToolChoiceNamed:
		n += f.toolsNamed + tk.Count(tc.Name)
	default:
		if noParallel {
			n += f.toolsSerial
		} else {
			n += f.toolsAuto
		}
		noParallel = false
	}
	if noParallel {
		n += f.noParallelForced
	}
	if req.System != "" {
		n += f.sysJoin
	} else {
		n += f.sysHeader
	}
	return n
}

// jsonMerges replaces the JSON and tag sequences that Claude's tokenizer
// reads as one token by a single punctuation mark (longest first, since
// strings.Replacer tries them in argument order).
var jsonMerges = strings.NewReplacer(
	"</function>", "\x01", "<function>", "\x01",
	`}, "`, "\x01", `"},`, "\x01", ` {"`, "\x01", `": `, "\x01", `, "`, "\x01", ": {", "\x01", ">\n<", "\x01",
	`{"`, "\x01", `"}`, "\x01", "}}", "\x01", "{}", "\x01", "</", "\x01", `":`, "\x01", `"]`, "\x01", `["`, "\x01", "]}", "\x01",
)

// countToolsText counts the rendered tools. The simulated tokenizer splits
// every punctuation mark and overcounts this JSON by ~40%; for it, each
// sequence of jsonMerges counts as one token and each \uXXXX escape as four,
// calibrated on the probes of testdata/real/anthropic/tools/. A BPE
// tokenizer counts the text as is.
func countToolsText(tk Tokenizer, text string) int {
	if _, ok := tk.(Simulated); !ok {
		return tk.Count(text)
	}
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+5 < len(text) && text[i+1] == 'u' && isHex4(text[i+2:i+6]) {
			b.WriteString("\x02\x02\x02\x02")
			i += 5
			continue
		}
		b.WriteByte(text[i])
	}
	return tk.Count(jsonMerges.Replace(b.String()))
}

func isHex4(s string) bool {
	for i := range 4 {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// writePyJSON writes n like Python's json.dumps(n, sort_keys=True,
// ensure_ascii=ascii): ", " and ": " separators, keys sorted; numbers keep
// their text.
func writePyJSON(b *strings.Builder, n *jsonNode, ascii bool) {
	switch n.kind {
	case '{':
		idx := make([]int, len(n.keys))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, c int) bool { return n.keys[idx[a]] < n.keys[idx[c]] })
		b.WriteByte('{')
		for j, i := range idx {
			if j > 0 {
				b.WriteString(", ")
			}
			writePyString(b, n.keys[i], ascii)
			b.WriteString(": ")
			writePyJSON(b, n.vals[i], ascii)
		}
		b.WriteByte('}')
	case '[':
		b.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, it, ascii)
		}
		b.WriteByte(']')
	case '"':
		writePyString(b, n.str, ascii)
	default:
		b.WriteString(n.raw)
	}
}

// writePyString writes s as a JSON string like Python's json.dumps: control
// characters are escaped (\n, \r, \t, \b, \f or \uXXXX) and, with ascii
// (ensure_ascii), every rune above ~ too (\uXXXX, surrogate pairs above the
// BMP).
func writePyString(b *strings.Builder, s string, ascii bool) {
	const hex = "0123456789abcdef"
	u := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hex[r>>12&0xf])
		b.WriteByte(hex[r>>8&0xf])
		b.WriteByte(hex[r>>4&0xf])
		b.WriteByte(hex[r&0xf])
	}
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case !ascii && r >= ' ':
			if r == utf8.RuneError {
				r = 0xfffd
			}
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			u(0xd800 + r>>10)
			u(0xdc00 + r&0x3ff)
		default:
			if r == utf8.RuneError {
				r = 0xfffd
			}
			u(r)
		}
	}
	b.WriteByte('"')
}
