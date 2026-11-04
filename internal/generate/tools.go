package generate

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Tool definitions, structured-output schemas and tool-call history as the
// OpenAI models see them (spec 007, task 252). Derived from gpt-4o-mini
// prompt counts on Chat Completions and Responses (testdata/real/openai/
// tools/, 31/10/2026); every constant below is one of those measurements.
const (
	// toolsFrame is what the functions namespace costs besides its text
	// (tokens(ToolsText) + 3).
	toolsFrame = 3
	// chatInstructionFrame: on Chat Completions, tools or a json_schema
	// open a section of the system prompt that costs 6 tokens, 2 when the
	// request already has a system/developer message. Responses has none.
	chatInstructionFrame       = 6
	chatInstructionFrameSystem = 2
	// toolChoiceNamed + tokens(name) when tool_choice forces one function;
	// toolChoiceNone when it is "none". "required" costs nothing, except on
	// Responses with a single tool, where it costs as forcing that tool.
	toolChoiceNamed = 7
	toolChoiceNone  = 1
	// responsesSchemaFrame: a json_schema format on Responses costs its
	// text + 1; on Chat, text + tokens(name).
	responsesSchemaFrame = 1
	// responsesNoParallel: parallel_tool_calls: false on Responses (with
	// tools); Chat Completions charges nothing.
	responsesNoParallel = 3
	// A previous call and its result cost callFrame + resultFrame + 2 ×
	// tokens(name) + tokens(compact arguments) + tokens(output); Chat sums
	// 13, Responses 11.
	callFrame              = 8
	chatResultFrame        = 5
	responsesResultFrame   = 3
	parallelFrame          = 20 // Chat, two or more calls in one turn
	parallelResultOverhead = 2  // each result of such a turn
)

// ToolsText renders function tools as the TypeScript-like namespace the
// OpenAI models read:
//
//	namespace functions {
//
//	// description
//	type name = (_: { a: string, b?: number }) => any;
//
//	} // namespace functions
//
// An object goes on one line unless a property carries a description or a
// default, which become comment lines.
func ToolsText(tools []core.ToolDef) string {
	return toolsText(tools, true)
}

// toolsText renders the namespace; with open false an object never gets
// [key: string]: any (GPT-4.1 ignores additionalProperties: true).
func toolsText(tools []core.ToolDef, open bool) string {
	var b strings.Builder
	b.WriteString("namespace functions {\n\n")
	for _, t := range tools {
		writeComment(&b, t.Description)
		b.WriteString("type ")
		b.WriteString(t.Name)
		params := parseJSON(t.Parameters)
		if params.kind == '{' && len(params.get("properties").keys) > 0 {
			b.WriteString(" = (_: ")
			writeTSObject(&b, params, open)
			b.WriteString(") => any;\n\n")
		} else {
			b.WriteString(" = () => any;\n\n")
		}
	}
	b.WriteString("} // namespace functions")
	return b.String()
}

// ResponseFormatText renders a json_schema response format: a header with
// the name, the description as a comment and the schema as compact JSON
// without "required" and "additionalProperties". Besides this text, Chat
// Completions counts the name once more and Responses 1 token.
func ResponseFormatText(name, description string, schema json.RawMessage) string {
	var b strings.Builder
	b.WriteString("# Response Formats\n\n## ")
	b.WriteString(name)
	b.WriteString("\n\n")
	writeComment(&b, description)
	writeSchemaJSON(&b, parseJSON(schema), false)
	return b.String()
}

// ToolSchemaTokens is the prompt cost of the tool definitions, tool_choice,
// parallel_tool_calls and the json_schema response format of req.
func ToolSchemaTokens(tk Tokenizer, req *core.Request) int {
	if anthropicTemplate(req) {
		return anthropicToolTokens(tk, req)
	}
	gpt41 := isGPT41(req.Model)
	// lean: the template of the Responses API, which the GPT-4.1 models also
	// use on Chat Completions (openai/gpt-4.1-nano via OpenRouter, task 259).
	lean := req.Endpoint == core.EndpointResponses || gpt41
	n := 0
	if len(req.Tools) > 0 {
		n += tk.Count(toolsText(req.Tools, !gpt41)) + toolsFrame
		switch tc := req.ToolChoice; {
		case tc.Mode == core.ToolChoiceNamed:
			n += toolChoiceNamed + tk.Count(tc.Name)
		case tc.Mode == core.ToolChoiceNone:
			n += toolChoiceNone
		case tc.Mode == core.ToolChoiceRequired && lean && len(req.Tools) == 1:
			n += toolChoiceNamed + tk.Count(req.Tools[0].Name)
		}
		if lean && req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
			n += responsesNoParallel
		}
	}
	schema := req.ResponseFormat == "json_schema"
	if schema {
		name, desc := schemaNameDesc(req.Raw)
		n += tk.Count(ResponseFormatText(name, desc, req.ResponseSchema))
		if lean {
			n += responsesSchemaFrame
		} else {
			n += tk.Count(name)
		}
	}
	if !lean && (len(req.Tools) > 0 || schema) {
		if hasSystem(req) {
			n += chatInstructionFrameSystem
		} else {
			n += chatInstructionFrame
		}
	}
	return n
}

// isGPT41 reports the GPT-4.1 family (gpt-4.1, -mini, -nano, with or
// without the openai/ author of OpenRouter), whose Chat Completions template
// is the lean one of the Responses API: no instruction frame, required with
// one tool costs as naming it, parallel_tool_calls: false costs 3, calls are
// charged one by one with their arguments as sent, and additionalProperties
// is not rendered.
func isGPT41(model string) bool {
	return strings.Contains(model, "gpt-4.1")
}

// hasSystem reports a system prompt besides the tools section.
func hasSystem(req *core.Request) bool {
	if req.System != "" {
		return true
	}
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			return true
		}
	}
	return false
}

// schemaNameDesc reads the name and description of the json_schema format
// from the body: response_format.json_schema (Chat) or text.format
// (Responses).
func schemaNameDesc(raw []byte) (name, desc string) {
	var body struct {
		ResponseFormat *struct {
			JSONSchema *struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Text *struct {
			Format *struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"format"`
		} `json:"text"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", ""
	}
	if f := body.ResponseFormat; f != nil && f.JSONSchema != nil {
		return f.JSONSchema.Name, f.JSONSchema.Description
	}
	if t := body.Text; t != nil && t.Format != nil {
		return t.Format.Name, t.Format.Description
	}
	return "", ""
}

// CompactArguments is the arguments of a tool call as the model saw them:
// compact JSON in the original key order (raw text when it is not JSON).
func CompactArguments(args string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(args)) != nil {
		return args
	}
	return buf.String()
}

// callCosts is the cost of the tool calls of one assistant turn and of the
// results that answer them.
type callCosts struct {
	name     string
	parallel bool
}

// toolCallTokens counts one assistant turn's calls and records, per call id,
// how its result is charged.
func toolCallTokens(tk Tokenizer, req *core.Request, m core.Message, ids map[string]callCosts) int {
	gpt41 := isGPT41(req.Model)
	parallel := len(m.ToolCalls) > 1 && req.Endpoint != core.EndpointResponses && !gpt41
	if parallel {
		// Chat renders a turn with several calls as one multi_tool_use
		// call whose arguments list them.
		var b strings.Builder
		b.WriteString(`{"tool_uses":[`)
		for i, tc := range m.ToolCalls {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"recipient_name":"functions.`)
			b.WriteString(tc.Name)
			b.WriteString(`","parameters":`)
			b.WriteString(CompactArguments(tc.Arguments))
			b.WriteByte('}')
			ids[tc.ID] = callCosts{name: tc.Name, parallel: true}
		}
		b.WriteString("]}")
		return parallelFrame + tk.Count(b.String())
	}
	n := 0
	for _, tc := range m.ToolCalls {
		args := tc.Arguments
		if !gpt41 {
			args = CompactArguments(args)
		}
		n += callFrame + tk.Count(tc.Name) + tk.Count(args)
		ids[tc.ID] = callCosts{name: tc.Name}
	}
	return n
}

// toolResultTokens counts a tool message (Chat role tool, Responses
// function_call_output).
func toolResultTokens(tk Tokenizer, req *core.Request, m core.Message, ids map[string]callCosts) int {
	c := ids[m.ToolCallID]
	if c.parallel {
		return tk.Count(m.Content) + parallelResultOverhead
	}
	frame := chatResultFrame
	if req.Endpoint == core.EndpointResponses || isGPT41(req.Model) {
		frame = responsesResultFrame
	}
	return frame + tk.Count(c.name) + tk.Count(m.Content)
}

func writeComment(b *strings.Builder, text string) {
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("// ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// writeTSObject renders an object schema as a TypeScript type.
func writeTSObject(b *strings.Builder, o *jsonNode, allowOpen bool) {
	props := o.get("properties")
	required := map[string]bool{}
	for _, r := range o.get("required").items {
		required[r.str] = true
	}
	open := allowOpen && o.get("additionalProperties").raw == "true"
	lines := false
	for _, p := range props.vals {
		if p.get("description").kind == '"' || p.get("default").kind != 0 {
			lines = true
		}
	}
	if len(props.keys) == 0 && !open {
		b.WriteString("object")
		return
	}
	if lines {
		b.WriteString("{\n")
	} else {
		b.WriteString("{ ")
	}
	for i, k := range props.keys {
		p := props.vals[i]
		if lines {
			writeComment(b, p.get("description").str)
		} else if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		if !required[k] {
			b.WriteByte('?')
		}
		b.WriteString(": ")
		writeTSType(b, p, allowOpen)
		if lines {
			b.WriteByte(',')
			if d := p.get("default"); d.kind != 0 {
				b.WriteString(" // default: ")
				writeSchemaJSON(b, d, true)
			}
			b.WriteByte('\n')
		}
	}
	switch {
	case open && lines:
		b.WriteString("[key: string]: any,\n")
	case open && len(props.keys) > 0:
		b.WriteString(", [key: string]: any")
	case open:
		b.WriteString("[key: string]: any")
	}
	if lines {
		b.WriteByte('}')
	} else {
		b.WriteString(" }")
	}
}

// writeTSType renders a property schema as a TypeScript type.
func writeTSType(b *strings.Builder, p *jsonNode, allowOpen bool) {
	for _, union := range []string{"anyOf", "oneOf"} {
		if alts := p.get(union); len(alts.items) > 0 {
			for i, a := range alts.items {
				if i > 0 {
					b.WriteString(" | ")
				}
				writeTSType(b, a, allowOpen)
			}
			return
		}
	}
	typ := p.get("type")
	if typ.kind == '[' {
		for i, t := range typ.items {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(tsName(t.str))
		}
		return
	}
	if enum := p.get("enum"); len(enum.items) > 0 {
		for i, v := range enum.items {
			if i > 0 {
				b.WriteString(" | ")
			}
			writeSchemaJSON(b, v, true)
		}
		return
	}
	switch typ.str {
	case "object":
		writeTSObject(b, p, allowOpen)
	case "array":
		if items := p.get("items"); items.kind == '{' {
			writeTSType(b, items, allowOpen)
		} else {
			b.WriteString("any")
		}
		b.WriteString("[]")
	default:
		b.WriteString(tsName(typ.str))
	}
}

func tsName(t string) string {
	switch t {
	case "string", "number", "boolean", "null":
		return t
	case "integer":
		return "number"
	}
	return "any"
}

// writeSchemaJSON writes n as compact JSON; with keep false it drops the
// "required" and "additionalProperties" keywords of every schema object
// (the keys of a "properties" map are property names and stay).
func writeSchemaJSON(b *strings.Builder, n *jsonNode, keep bool) {
	switch n.kind {
	case '{':
		b.WriteByte('{')
		first := true
		for i, k := range n.keys {
			if !keep && (k == "required" || k == "additionalProperties") {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(n.keyRaw[i])
			b.WriteByte(':')
			v := n.vals[i]
			if !keep && (k == "properties" || k == "$defs" || k == "definitions") && v.kind == '{' {
				writePropsJSON(b, v)
			} else {
				writeSchemaJSON(b, v, keep)
			}
		}
		b.WriteByte('}')
	case '[':
		b.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				b.WriteByte(',')
			}
			writeSchemaJSON(b, it, keep)
		}
		b.WriteByte(']')
	default:
		b.WriteString(n.raw)
	}
}

// writePropsJSON writes a name → schema map.
func writePropsJSON(b *strings.Builder, n *jsonNode) {
	b.WriteByte('{')
	for i := range n.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n.keyRaw[i])
		b.WriteByte(':')
		writeSchemaJSON(b, n.vals[i], false)
	}
	b.WriteByte('}')
}

// jsonNode is a JSON value that keeps the key order of objects and the raw
// text of scalars. kind is '{', '[', '"' (string), 'v' (other scalar) or 0
// (absent).
type jsonNode struct {
	kind   byte
	raw    string // scalars: compact JSON text
	str    string // strings: the decoded value
	keys   []string
	keyRaw []string
	vals   []*jsonNode
	items  []*jsonNode
}

var absent = &jsonNode{}

// get returns the value of key k of an object (absent when missing).
func (n *jsonNode) get(k string) *jsonNode {
	for i, key := range n.keys {
		if key == k {
			return n.vals[i]
		}
	}
	return absent
}

// parseJSON parses raw keeping the order of object keys; invalid JSON is
// absent.
func parseJSON(raw []byte) *jsonNode {
	if !json.Valid(raw) {
		return absent
	}
	return parseValue(raw)
}

// parseValue parses valid JSON.
func parseValue(raw []byte) *jsonNode {
	raw = bytes.TrimSpace(raw)
	switch raw[0] {
	case '{':
		dec := json.NewDecoder(bytes.NewReader(raw))
		_, _ = dec.Token()
		n := &jsonNode{kind: '{'}
		for dec.More() {
			t, _ := dec.Token()
			k, _ := t.(string)
			var v json.RawMessage
			_ = dec.Decode(&v)
			kr, _ := json.Marshal(k)
			n.keys = append(n.keys, k)
			n.keyRaw = append(n.keyRaw, string(kr))
			n.vals = append(n.vals, parseValue(v))
		}
		return n
	case '[':
		var items []json.RawMessage
		_ = json.Unmarshal(raw, &items)
		n := &jsonNode{kind: '['}
		for _, it := range items {
			n.items = append(n.items, parseValue(it))
		}
		return n
	}
	var buf bytes.Buffer
	_ = json.Compact(&buf, raw)
	n := &jsonNode{kind: 'v', raw: buf.String()}
	if raw[0] == '"' {
		n.kind = '"'
		_ = json.Unmarshal(raw, &n.str)
	}
	return n
}
