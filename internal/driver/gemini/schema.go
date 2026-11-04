package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// The Gemini API decodes a body as proto3 JSON before anything else (the
// JSON transcoder in front of the service): a syntax error, a field the
// message does not have or a value of the wrong type is a 400 that wins
// over the key (observed, task 164). This file is that transcoder: it
// checks the body against the request messages and returns it normalized
// (camelCase keys; snake_case is accepted, as proto3 JSON accepts both).

// ftype is the JSON type of a field.
type ftype int

const (
	tMsg    ftype = iota // a message: an object checked field by field
	tString              // string (and enums: their values are not checked)
	tInt32               // number or numeric string, integral
	tFloat               // number or numeric string
	tBool                // true or false
	tAny                 // Struct, Value or Schema: kept raw, not checked
)

// protoName of a scalar type in "Invalid value" messages (observed:
// TYPE_INT32).
var protoName = map[ftype]string{tString: "TYPE_STRING", tInt32: "TYPE_INT32", tFloat: "TYPE_FLOAT", tBool: "TYPE_BOOL"}

type field struct {
	camel    string
	t        ftype
	msg      *message
	repeated bool
}

type message struct {
	name   string // full proto name, for type errors
	fields map[string]*field
}

const protoPkg = "google.ai.generativelanguage.v1beta."

// newMessage builds a message from (camelName, field) pairs; each field is
// reachable by its camelCase and snake_case names.
func newMessage(name string, fs ...any) *message {
	m := &message{name: protoPkg + name, fields: map[string]*field{}}
	for i := 0; i+1 < len(fs); i += 2 {
		n := fs[i].(string)
		f := fs[i+1].(*field)
		f.camel = n
		m.fields[n] = f
		m.fields[snake(n)] = f
	}
	return m
}

func scalar(t ftype) *field           { return &field{t: t} }
func list(t ftype) *field             { return &field{t: t, repeated: true} }
func sub(m *message) *field           { return &field{t: tMsg, msg: m} }
func subList(m *message) *field       { return &field{t: tMsg, msg: m, repeated: true} }
func anyField() *field                { return &field{t: tAny} }
func anyList() *field                 { return &field{t: tAny, repeated: true} }
func strField() *field                { return scalar(tString) }
func setFields(m *message, fs ...any) { m.fields = newMessage("", fs...).fields }

// snake turns a camelCase name into snake_case (maxOutputTokens ->
// max_output_tokens).
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// The request messages (discovery v1beta, revision 20261030).
var (
	msgContent = newMessage("Content")
	msgPart    = newMessage("Part",
		"text", strField(),
		"inlineData", sub(newMessage("Blob", "mimeType", strField(), "data", strField())),
		"fileData", sub(newMessage("FileData", "mimeType", strField(), "fileUri", strField())),
		"functionCall", sub(newMessage("FunctionCall", "id", strField(), "name", strField(), "args", anyField())),
		"functionResponse", sub(newMessage("FunctionResponse",
			"id", strField(), "name", strField(), "response", anyField(), "parts", anyList(),
			"willContinue", scalar(tBool), "scheduling", strField())),
		"executableCode", anyField(), "codeExecutionResult", anyField(),
		"thought", scalar(tBool), "thoughtSignature", strField(),
		"videoMetadata", anyField(), "partMetadata", anyField(), "mediaResolution", anyField(),
	)
	msgTool = newMessage("Tool",
		"functionDeclarations", subList(newMessage("FunctionDeclaration",
			"name", strField(), "description", strField(), "behavior", strField(),
			"parameters", anyField(), "parametersJsonSchema", anyField(),
			"response", anyField(), "responseJsonSchema", anyField())),
		"googleSearchRetrieval", anyField(), "codeExecution", anyField(), "googleSearch", anyField(),
		"computerUse", anyField(), "urlContext", anyField(), "fileSearch", anyField(),
		"googleMaps", anyField(), "mcpServers", anyList(),
	)
	msgToolConfig = newMessage("ToolConfig",
		"functionCallingConfig", sub(newMessage("FunctionCallingConfig", "mode", strField(), "allowedFunctionNames", list(tString))),
		"retrievalConfig", anyField(), "includeServerSideToolInvocations", scalar(tBool),
	)
	msgGenerationConfig = newMessage("GenerationConfig",
		"stopSequences", list(tString), "responseMimeType", strField(),
		"responseSchema", anyField(), "responseJsonSchema", anyField(), "_responseJsonSchema", anyField(),
		"responseModalities", list(tString), "candidateCount", scalar(tInt32),
		"maxOutputTokens", scalar(tInt32), "temperature", scalar(tFloat), "topP", scalar(tFloat),
		"topK", scalar(tInt32), "seed", scalar(tInt32), "presencePenalty", scalar(tFloat),
		"frequencyPenalty", scalar(tFloat), "responseLogprobs", scalar(tBool), "logprobs", scalar(tInt32),
		"enableEnhancedCivicAnswers", scalar(tBool), "speechConfig", anyField(),
		"thinkingConfig", sub(newMessage("ThinkingConfig",
			"includeThoughts", scalar(tBool), "thinkingBudget", scalar(tInt32), "thinkingLevel", strField())),
		"imageConfig", anyField(), "mediaResolution", strField(),
	)
	msgGenerate = newMessage("GenerateContentRequest",
		"model", strField(), "contents", subList(msgContent),
		"systemInstruction", sub(msgContent), "tools", subList(msgTool),
		"toolConfig", sub(msgToolConfig),
		"safetySettings", subList(newMessage("SafetySetting", "category", strField(), "threshold", strField(), "method", strField())),
		"generationConfig", sub(msgGenerationConfig), "cachedContent", strField(),
		"serviceTier", strField(), "store", scalar(tBool), "labels", anyField(),
	)
	msgCount = newMessage("CountTokensRequest", "contents", subList(msgContent), "generateContentRequest", sub(msgGenerate))
	// msgCached is CachedContent, the body of create and update.
	msgCached = newMessage("CachedContent",
		"name", strField(), "displayName", strField(), "model", strField(),
		"systemInstruction", sub(msgContent), "contents", subList(msgContent),
		"tools", subList(msgTool), "toolConfig", sub(msgToolConfig),
		"ttl", strField(), "expireTime", strField(), "createTime", strField(), "updateTime", strField(),
		"usageMetadata", anyField(),
	)
)

func init() {
	setFields(msgContent, "role", strField(), "parts", subList(msgPart))
}

// transcoder walks one body.
type transcoder struct {
	dec *json.Decoder
	vs  []violation
	// stack of open containers ('[' or '{') for the syntax messages.
	stack []byte
}

// errSyntax is a body that is not JSON; msg is the real's text.
type errSyntax struct{ msg string }

// transcode checks body against m. It returns the normalized object, the
// violations (unknown fields, wrong types) or the JSON syntax error.
func transcode(body []byte, m *message) (map[string]any, *errSyntax, []violation) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil, nil
	}
	t := &transcoder{dec: json.NewDecoder(bytes.NewReader(body))}
	t.dec.UseNumber()
	tok, err := t.dec.Token()
	if err != nil {
		return nil, t.syntax(err, body), nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		// Not conferred: the transcoder's text for a root that is not an
		// object.
		return nil, &errSyntax{msg: `Invalid JSON payload received. Unknown name "": Root element must be a message.`}, nil
	}
	t.stack = append(t.stack, '{')
	out, err := t.object(m, "")
	if err != nil {
		return nil, t.syntax(err, body), nil
	}
	if _, err := t.dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &errSyntax{msg: "Invalid JSON payload received. Unexpected token."}, nil
	}
	return out, nil, t.vs
}

// syntax renders a decoder error: the end of the input inside an array is
// the observed text; the other cases are não conferido.
func (t *transcoder) syntax(err error, body []byte) *errSyntax {
	var se *json.SyntaxError
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &se) && strings.Contains(se.Error(), "unexpected end") {
		what := "Expected , or } after key:value pair."
		if n := len(t.stack); n > 0 && t.stack[n-1] == '[' {
			what = "Expected a value or ] within an array."
		}
		return &errSyntax{msg: "Invalid JSON payload received. Unexpected end of string. " + what + "\n\n^"}
	}
	off := min(int(t.dec.InputOffset()), len(body))
	start := bytes.LastIndexByte(body[:off], '\n') + 1
	end := bytes.IndexByte(body[off:], '\n')
	if end < 0 {
		end = len(body) - off
	}
	snippet := string(body[start : off+end])
	return &errSyntax{msg: "Invalid JSON payload received. Unexpected token.\n" + snippet + "\n" + strings.Repeat(" ", off-start) + "^"}
}

// object reads the members of an object whose '{' was consumed.
func (t *transcoder) object(m *message, path string) (map[string]any, error) {
	out := map[string]any{}
	for t.dec.More() {
		kt, err := t.dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		f := m.fields[key]
		if f == nil {
			var skip json.RawMessage
			if err := t.dec.Decode(&skip); err != nil {
				return nil, err
			}
			t.unknown(key, path)
			continue
		}
		v, err := t.value(f, join(path, snake(f.camel)))
		if err != nil {
			return nil, err
		}
		if v != nil {
			out[f.camel] = v
		}
	}
	if _, err := t.dec.Token(); err != nil { // '}'
		return nil, err
	}
	t.stack = t.stack[:len(t.stack)-1]
	return out, nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// unknown records a field the message does not have (observed texts).
func (t *transcoder) unknown(key, path string) {
	q := quote(key)
	if path == "" {
		t.vs = append(t.vs, violation{desc: "Invalid JSON payload received. Unknown name " + q + ": Cannot find field."})
		return
	}
	t.vs = append(t.vs, violation{field: path, desc: "Invalid JSON payload received. Unknown name " + q + " at '" + path + "': Cannot find field."})
}

// quote is a JSON string as the message embeds it: < > & stay as they are
// in the text (the body's encoding escapes < and >, observed).
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string: cannot fail
	return strings.TrimSuffix(buf.String(), "\n")
}

// value reads the value of f at path.
func (t *transcoder) value(f *field, path string) (any, error) {
	if f.t == tAny && !f.repeated {
		var raw json.RawMessage
		if err := t.dec.Decode(&raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	tok, err := t.dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil // null: the field's default
	}
	if f.repeated {
		if d, ok := tok.(json.Delim); ok && d == '[' {
			t.stack = append(t.stack, '[')
			var items []any
			for i := 0; t.dec.More(); i++ {
				if f.t == tAny {
					var raw json.RawMessage
					if err := t.dec.Decode(&raw); err != nil {
						return nil, err
					}
					items = append(items, raw)
					continue
				}
				it, err := t.dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := t.single(f, it, path+"["+strconv.Itoa(i)+"]")
				if err != nil {
					return nil, err
				}
				items = append(items, v)
			}
			if _, err := t.dec.Token(); err != nil { // ']'
				return nil, err
			}
			t.stack = t.stack[:len(t.stack)-1]
			return items, nil
		}
		// A single value for a repeated field is one element (observed:
		// "contents": "oi" is reported at 'contents').
		v, err := t.single(f, tok, path)
		if v == nil || err != nil {
			return nil, err
		}
		return []any{v}, nil
	}
	return t.single(f, tok, path)
}

// single checks one value whose first token is tok.
func (t *transcoder) single(f *field, tok json.Token, path string) (any, error) {
	if d, ok := tok.(json.Delim); ok {
		if f.t == tMsg && d == '{' {
			t.stack = append(t.stack, '{')
			return t.object(f.msg, path)
		}
		// A container where a scalar (or a message where an array) is
		// expected: skip it.
		if err := t.skip(); err != nil {
			return nil, err
		}
		t.invalid(f, path, string(d)+string(closer(d)))
		return nil, nil
	}
	switch f.t {
	case tMsg:
		t.invalid(f, path, raw(tok))
		return nil, nil
	case tString:
		if s, ok := tok.(string); ok {
			return s, nil
		}
	case tBool:
		if b, ok := tok.(bool); ok {
			return b, nil
		}
	case tInt32:
		if n, ok := number(tok); ok {
			if i, err := strconv.ParseInt(n, 10, 32); err == nil {
				return i, nil
			}
		}
	case tFloat:
		if n, ok := number(tok); ok {
			if x, err := strconv.ParseFloat(n, 64); err == nil {
				return x, nil
			}
		}
	}
	t.invalid(f, path, raw(tok))
	return nil, nil
}

// number is the text of a JSON number or numeric string.
func number(tok json.Token) (string, bool) {
	switch v := tok.(type) {
	case json.Number:
		return v.String(), true
	case string:
		return v, true
	}
	return "", false
}

func closer(d json.Delim) byte {
	if d == '[' {
		return ']'
	}
	return '}'
}

// skip consumes the rest of a container whose opening was read.
func (t *transcoder) skip() error {
	depth := 1
	for depth > 0 {
		tok, err := t.dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := tok.(json.Delim); ok {
			if dd == '[' || dd == '{' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// raw is the JSON text of a scalar token.
func raw(tok json.Token) string {
	switch v := tok.(type) {
	case string:
		return quote(v)
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	}
	return "null"
}

// invalid records a value of the wrong type (observed texts: TYPE_INT32
// for a scalar, the message's type URL for a message).
func (t *transcoder) invalid(f *field, path, rawVal string) {
	typ := protoName[f.t]
	if f.t == tMsg {
		typ = "type.googleapis.com/" + f.msg.name
	}
	desc := "Invalid value at '" + path + "' (" + typ + "), " + rawVal
	t.vs = append(t.vs, violation{field: path, desc: desc})
}
