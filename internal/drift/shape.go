// Package drift is the core of wwz-drift check (spec 017, ADR 0035): it
// sends the same probes to a real provider and to wwz and compares the
// shape of the answers (JSON paths with type and order, SSE event sequence,
// relevant header names and status), never the values.
package drift

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
)

// JSON value types of a shape.
const (
	TypeString = "string"
	TypeNumber = "number"
	TypeBool   = "bool"
	TypeObject = "object"
	TypeArray  = "array"
	TypeNull   = "null"
)

// RootPath is the path of the top-level JSON value.
const RootPath = "$"

// Field is one (path, type) of a JSON shape. Paths follow the style
// choices[].message.content; the items of an array are fused under "[]".
type Field struct {
	Path string `json:"path"`
	Type string `json:"type"`
	// Empty marks an object or array without members (on every occurrence
	// fused into this path). The items of an empty array are unknown, not
	// absent, so the comparison does not report them.
	Empty bool `json:"empty,omitempty"`

	parent string // path of the enclosing value ("" for the root)
	name   string // last segment ("[]" for array items)
}

// Shape is the ordered list of fields of a JSON document, in order of first
// appearance.
type Shape []Field

type shapeBuilder struct {
	fields Shape
	idx    map[string]int
}

// add records path with typ and returns its index. A path seen before
// keeps its position; a null type gives way to the first concrete one.
func (b *shapeBuilder) add(parent, name, path, typ string) int {
	if i, ok := b.idx[path]; ok {
		if b.fields[i].Type == TypeNull {
			b.fields[i].Type = typ
			b.fields[i].Empty = typ == TypeObject || typ == TypeArray
		}
		return i
	}
	b.idx[path] = len(b.fields)
	b.fields = append(b.fields, Field{
		Path: path, Type: typ, Empty: typ == TypeObject || typ == TypeArray,
		parent: parent, name: name,
	})
	return len(b.fields) - 1
}

func childPath(parent, key string) string {
	if parent == RootPath {
		return key
	}
	return parent + "." + key
}

func itemPath(parent string) string {
	if parent == RootPath {
		return "[]"
	}
	return parent + "[]"
}

// value walks one JSON value whose first token is tok.
func (b *shapeBuilder) value(dec *json.Decoder, tok json.Token, parent, name, path string) error {
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			i := b.add(parent, name, path, TypeObject)
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := kt.(string)
				vt, err := dec.Token()
				if err != nil {
					return err
				}
				if err := b.value(dec, vt, path, key, childPath(path, key)); err != nil {
					return err
				}
				b.markFull(i)
			}
		} else {
			i := b.add(parent, name, path, TypeArray)
			for dec.More() {
				vt, err := dec.Token()
				if err != nil {
					return err
				}
				if err := b.value(dec, vt, path, "[]", itemPath(path)); err != nil {
					return err
				}
				b.markFull(i)
			}
		}
		_, err := dec.Token() // closing delimiter
		return err
	case string:
		b.add(parent, name, path, TypeString)
	case json.Number:
		b.add(parent, name, path, TypeNumber)
	case bool:
		b.add(parent, name, path, TypeBool)
	default: // nil
		b.add(parent, name, path, TypeNull)
	}
	return nil
}

func (b *shapeBuilder) markFull(i int) {
	if b.fields[i].Type == TypeObject || b.fields[i].Type == TypeArray {
		b.fields[i].Empty = false
	}
}

// JSONShape returns the shape of a JSON document.
func JSONShape(data []byte) (Shape, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	b := &shapeBuilder{idx: map[string]int{}}
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("drift: JSON inválido: %w", err)
	}
	if err := b.value(dec, tok, "", RootPath, RootPath); err != nil {
		return nil, fmt.Errorf("drift: JSON inválido: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("drift: JSON inválido: dados depois do documento")
	}
	return b.fields, nil
}

// String renders the shape as "path:type" items separated by commas; it is
// the identity used to collapse repeated SSE events.
func (s Shape) String() string {
	parts := make([]string, len(s))
	for i, f := range s {
		parts[i] = f.Path + ":" + f.Type
	}
	return strings.Join(parts, ",")
}

// Event is one entry of a collapsed SSE sequence: the event name (empty
// when the stream has no "event:" lines) and the signature of its data.
// Comments become Event "comment"; non-JSON data keeps its literal text
// when it is a terminator ("[DONE]") and becomes "text" otherwise.
type Event struct {
	Event string `json:"event"`
	Data  string `json:"data"`
	// Count is how many consecutive identical events were collapsed.
	Count int `json:"count"`

	key   string // data type/object of an event without name
	shape Shape  // nil for comments and non-JSON data
}

// Key returns the alignment key of the event: the "event:" name or, without
// it, the "type" (else "object") string of the JSON data; comments,
// terminators and plain text use their Data.
func (e Event) Key() string {
	switch {
	case e.Event != "":
		return e.Event
	case e.key != "":
		return e.key
	}
	return e.Data
}

// EventComment is the name given to SSE comment lines.
const EventComment = "comment"

// DoneData is the OpenAI-style stream terminator.
const DoneData = "[DONE]"

func (e Event) String() string {
	if e.Event == "" {
		return "data=" + e.Data
	}
	return "event=" + e.Event + " data=" + e.Data
}

// rawEvent is one dispatched SSE event or comment line.
type rawEvent struct {
	name    string
	data    string
	comment bool
}

// parseSSE splits an event stream into events and comments, in order.
func parseSSE(body []byte) []rawEvent {
	var out []rawEvent
	var name string
	var data []string
	pending := false
	dispatch := func() {
		if pending {
			out = append(out, rawEvent{name: name, data: strings.Join(data, "\n")})
		}
		name, data, pending = "", nil, false
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			dispatch()
			continue
		}
		if strings.HasPrefix(line, ":") {
			out = append(out, rawEvent{comment: true})
			continue
		}
		field, val, _ := strings.Cut(line, ":")
		val = strings.TrimPrefix(val, " ")
		switch field {
		case "event":
			name, pending = val, true
		case "data":
			data, pending = append(data, val), true
		}
	}
	dispatch()
	return out
}

// sseData returns the data payloads of an event stream.
func sseData(body []byte) [][]byte {
	var out [][]byte
	for _, e := range parseSSE(body) {
		if !e.comment {
			out = append(out, []byte(e.data))
		}
	}
	return out
}

// SSEShape parses an event stream and returns its collapsed sequence.
// Comments before the first event are dropped: real providers send them
// only while waiting (": OPENROUTER PROCESSING"), so they depend on timing.
func SSEShape(body []byte) []Event {
	var out []Event
	for _, r := range parseSSE(body) {
		if r.comment && len(out) == 0 {
			continue
		}
		e := Event{Event: EventComment}
		if !r.comment {
			e = eventOf(r.name, r.data)
		}
		if n := len(out); n > 0 && out[n-1].Event == e.Event && out[n-1].Data == e.Data {
			out[n-1].Count++
			continue
		}
		e.Count = 1
		out = append(out, e)
	}
	return out
}

func eventOf(name, data string) Event {
	if strings.TrimSpace(data) == DoneData {
		return Event{Event: name, Data: DoneData}
	}
	if s, err := JSONShape([]byte(data)); err == nil {
		return Event{Event: name, Data: s.String(), shape: s, key: dataKind(data)}
	}
	return Event{Event: name, Data: "text"}
}

// dataKind returns the top-level "type" string of a JSON object, else its
// "object" string, else "".
func dataKind(data string) string {
	var top struct {
		Type   any `json:"type"`
		Object any `json:"object"`
	}
	_ = json.Unmarshal([]byte(data), &top) // not an object: both stay nil
	for _, v := range []any{top.Type, top.Object} {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// headerPrefixes and headerExact are the header names (lower case) that
// count; any other header is ignored (spec 017, ADR 0049).
var (
	headerPrefixes = []string{
		"x-ratelimit-", "anthropic-ratelimit-", "retry-after", "request-id",
		"x-request-id", "openai-", "anthropic-",
	}
	headerExact = []string{
		"x-should-retry", "x-openai-authorization-error", "x-openai-ide-error-code",
		"x-error-json", "x-generation-id", "x-opencode-log-id", "x-zen-model",
	}
)

// relevantHeader reports whether the lower-case name k counts.
func relevantHeader(k string) bool {
	if slices.Contains(headerExact, k) {
		return true
	}
	return slices.ContainsFunc(headerPrefixes, func(p string) bool { return strings.HasPrefix(k, p) })
}

// HeaderNames returns the sorted relevant header names of h, in lower case.
// Infrastructure headers (cf-*, server, date, set-cookie, via,
// x-openai-proxy-wasm, x-openai-internal-caller) never count.
func HeaderNames(h http.Header) []string {
	var out []string
	for k := range h {
		if k = strings.ToLower(k); relevantHeader(k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ContentType returns the media type of h without parameters, in lower case.
func ContentType(h http.Header) string {
	v := h.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(v); err == nil {
		return mt
	}
	mt, _, _ := strings.Cut(v, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// Signature is the shape of one HTTP answer.
type Signature struct {
	Status      int      `json:"status"`
	ContentType string   `json:"content_type"`
	Headers     []string `json:"headers"`
	Stream      bool     `json:"stream"`
	Body        Shape    `json:"body,omitempty"`
	Events      []Event  `json:"events,omitempty"`
	// Item is the model id whose /models item was kept (SignModels); empty
	// when the body is not a model list. ItemFound reports whether the list
	// has it.
	Item      string `json:"item,omitempty"`
	ItemFound bool   `json:"item_found,omitempty"`
}

// Sign builds the signature of an answer. A text/event-stream body is read
// as SSE; any other non-empty body must be JSON.
func Sign(status int, h http.Header, body []byte) (Signature, error) {
	s := Signature{Status: status, ContentType: ContentType(h), Headers: HeaderNames(h)}
	if s.ContentType == "text/event-stream" {
		s.Stream = true
		s.Events = SSEShape(body)
		return s, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return s, nil
	}
	shape, err := JSONShape(body)
	if err != nil {
		return s, fmt.Errorf("resposta ilegível (status %d, %s): %w", status, s.ContentType, err)
	}
	s.Body = shape
	return s, nil
}

// SignModels is Sign for a model list: the top level keeps every key, in
// order, but data keeps only the item whose id is model. The other items
// are heterogeneous (dozens of models, each with its own optional fields),
// and fusing them would report fields the probe model does not have.
func SignModels(status int, h http.Header, body []byte, model string) (Signature, error) {
	focused, found, ok := focusModel(body, model)
	if !ok {
		return Sign(status, h, body)
	}
	s, err := Sign(status, h, focused)
	s.Item, s.ItemFound = model, found
	return s, err
}

// focusModel rewrites a JSON object keeping its top-level keys in order and
// data reduced to the first item whose "id" is model (or to [] without
// one). ok is false when body is not an object with a data array.
func focusModel(body []byte, model string) (out []byte, found, ok bool) {
	if !json.Valid(body) {
		return nil, false, false
	}
	// body is valid JSON: the decoder below cannot fail.
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		return nil, false, false
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for n := 0; dec.More(); n++ {
		kt, _ := dec.Token()
		var raw json.RawMessage
		_ = dec.Decode(&raw)
		key, _ := kt.(string)
		if key == "data" || key == "models" { // Gemini lists under "models"
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) != nil || items == nil {
				return nil, false, false
			}
			ok = true
			raw, found = itemByID(items, model)
		}
		if n > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(key) // a string: cannot fail
		b.Write(kj)
		b.WriteByte(':')
		b.Write(raw)
	}
	b.WriteByte('}')
	return b.Bytes(), found, ok
}

// itemByID returns a one-item array with the first item whose "id" is
// model (Gemini: whose "name" is models/<model>), or an empty array.
func itemByID(items []json.RawMessage, model string) (json.RawMessage, bool) {
	for _, it := range items {
		var m struct {
			ID   any `json:"id"`
			Name any `json:"name"`
		}
		if json.Unmarshal(it, &m) == nil && (m.ID == model || m.Name == "models/"+model) {
			return append(append(json.RawMessage("["), it...), ']'), true
		}
	}
	return json.RawMessage("[]"), false
}
