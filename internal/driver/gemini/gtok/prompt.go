package gtok

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// The prompt template, measured with countTokens on 2026-10-31 (task 265,
// tmp/t265): every Content costs 1 plus its parts; the systemInstruction
// costs only its text; there is no reply priming (promptTokenCount of
// generateContent equals countTokens). toolConfig and responseSchema cost
// nothing. An image costs ImageTokens (a 1x1 PNG; the same on 3.1-flash-lite
// and 3.8-flash) and an audio part AudioTokens (a 1 s WAV; 3 s cost 96,
// the length is not looked at). Function declarations, calls and responses
// render the way Gemma 4 writes them, with special tokens around:
//
//	<start_function_declaration>declaration:NAME{description:<e>…<e>,parameters:{…}}<end_function_declaration>
//	<start_function_call>call:NAME{city:<e>Recife<e>}<end_function_call>
//	<start_function_response>response:NAME{temp:30}<end_function_response>
//
// where <e> is the string escape token, schema keys are sorted, types are
// upper case and numbers go bare. Calls and responses come out exact for
// the probes; declarations within 2 tokens.
const (
	ContentOverhead = 1
	ImageTokens     = 1089
	AudioTokens     = 33
)

// CountPrompt counts a Gemini request's prompt with tk for the text.
func CountPrompt(tk generate.Tokenizer, req *core.Request) generate.PromptBreakdown {
	var b generate.PromptBreakdown
	if req.System != "" {
		b.System = tk.Count(req.System)
	}
	for _, t := range req.Tools {
		b.ToolSchemas += Declaration(tk, t)
	}
	names := map[string]string{} // call id -> function name
	for _, m := range req.Messages {
		n := ContentOverhead + tk.Count(m.Content)
		if m.Role == "assistant" {
			b.Assistant += n
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					names[tc.ID] = tc.Name
				}
				b.ToolCalls += Call(tk, tc.Name, tc.Arguments)
			}
		} else {
			b.User += n
			for _, tr := range m.ToolResults {
				name := tr.ToolCallID
				if fn, ok := names[name]; ok {
					name = fn
				}
				b.ToolResults += Response(tk, name, tr.Content)
			}
		}
		b.Images += m.ImageParts*ImageTokens + m.AudioParts*AudioTokens
	}
	b.Total = b.System + b.ToolSchemas + b.User + b.Assistant + b.ToolCalls + b.ToolResults + b.Images
	return b
}

// render accumulates the tokens of a rendered structure: text runs are
// counted together (punctuation merges across them), special tokens cost
// one each and break the run.
type render struct {
	tk  generate.Tokenizer
	buf strings.Builder
	n   int
}

func (r *render) text(s string) { r.buf.WriteString(s) }

func (r *render) special() {
	r.flush()
	r.n++
}

func (r *render) flush() {
	if r.buf.Len() > 0 {
		r.n += r.tk.Count(r.buf.String())
		r.buf.Reset()
	}
}

func (r *render) total() int {
	r.flush()
	return r.n
}

// str writes an escaped string.
func (r *render) str(s string) {
	r.special()
	r.text(s)
	r.special()
}

// Declaration is the cost of one function declaration.
func Declaration(tk generate.Tokenizer, t core.ToolDef) int {
	r := &render{tk: tk}
	r.special()
	r.special() // "declaration": one piece of the vocabulary
	r.text(":" + t.Name + "{")
	sep := false
	if t.Description != "" {
		r.special() // "description"
		r.text(":")
		r.str(t.Description)
		sep = true
	}
	if len(t.Parameters) > 0 {
		var v any
		if json.Unmarshal(t.Parameters, &v) == nil {
			if sep {
				r.text(",")
			}
			r.special() // "parameters"
			r.text(":")
			r.schema(v)
		}
	}
	r.text("}")
	r.special()
	return r.total()
}

// keyword are the schema keys that are one piece of the vocabulary.
var keyword = map[string]bool{"description": true, "properties": true, "required": true, "type": true, "enum": true, "items": true}

// schema writes a JSON Schema: keys sorted, types upper case, property
// names bare.
func (r *render) schema(v any) {
	o, ok := v.(map[string]any)
	if !ok {
		r.value(v)
		return
	}
	r.text("{")
	for i, k := range sortedKeys(o) {
		if i > 0 {
			r.text(",")
		}
		r.key(k)
		r.text(":")
		switch k {
		case "properties":
			props, _ := o[k].(map[string]any)
			r.text("{")
			for j, name := range sortedKeys(props) {
				if j > 0 {
					r.text(",")
				}
				r.text(name + ":")
				r.schema(props[name])
			}
			r.text("}")
		case "type":
			if s, ok := o[k].(string); ok {
				r.str(strings.ToUpper(s))
			} else {
				r.value(o[k])
			}
		case "items":
			r.schema(o[k])
		default:
			r.value(o[k])
		}
	}
	r.text("}")
}

func (r *render) key(k string) {
	if keyword[k] {
		r.special()
		return
	}
	r.text(k)
}

// value writes a JSON value: strings escaped, numbers and booleans bare.
func (r *render) value(v any) {
	switch x := v.(type) {
	case string:
		r.str(x)
	case map[string]any:
		r.text("{")
		for i, k := range sortedKeys(x) {
			if i > 0 {
				r.text(",")
			}
			r.text(k + ":")
			r.value(x[k])
		}
		r.text("}")
	case []any:
		r.text("[")
		for i, e := range x {
			if i > 0 {
				r.text(",")
			}
			r.value(e)
		}
		r.text("]")
	case float64:
		r.text(strconv.FormatFloat(x, 'f', -1, 64))
	case bool:
		r.text(strconv.FormatBool(x))
	case nil:
		r.text("null")
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Call is the cost of a functionCall part.
func Call(tk generate.Tokenizer, name, args string) int {
	return wrapped(tk, "call", name, args)
}

// Response is the cost of a functionResponse part.
func Response(tk generate.Tokenizer, name, response string) int {
	return wrapped(tk, "response", name, response)
}

func wrapped(tk generate.Tokenizer, _ string, name, raw string) int {
	r := &render{tk: tk}
	r.special()
	r.special() // "call" or "response"
	r.text(":" + name)
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		v = map[string]any{}
	}
	if o, ok := v.(map[string]any); ok && len(o) == 0 {
		r.text("{}")
	} else {
		r.value(v)
	}
	r.special()
	return r.total()
}
