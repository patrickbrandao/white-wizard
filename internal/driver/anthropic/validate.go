package anthropic

import (
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	js "github.com/patrickbrandao/white-wizard/internal/driver/jsonscan"
)

var _ driver.RequestValidator = (*Driver)(nil)

// ValidateRequest refuses what the real API refuses (spec 004), in the
// order below.
func (d *Driver) ValidateRequest(kind driver.Kind, body []byte, h driver.Headers, _ driver.Models) *core.Error {
	return validateAnthropic(kind, body, h)
}

// Anthropic (driver anthropic). Order the real API checks a Messages body
// in, as recorded on 2026-10-31 (testdata/real/anthropic/validation/):
//  1. the body is JSON (serde_json's message, with line and column), then
//     the anthropic-version header (auth-order/, ADR 0053);
//  2. output_format (deprecated) and mcp_servers without its beta;
//  3. each field, in anthropicOrder (pydantic: type, then nested members,
//     the message roles included);
//  4. unknown top-level fields, the first in body order;
//  5. empty messages;
//  6. the role "system" messages (anthSystemMessages; tasks 441, 453);
//  7. the sampling fields, in anthropicSampling: type, then range
//     (a temperature range error wins over a later top_p type error, and an
//     unknown field wins over both, recorded);
//  8. tools that are all deferred (task 441).

// anthropicField checks one field; v is nil when the field is absent.
type anthropicField struct {
	name  string
	check func(name string, v []byte, h driver.Headers) string
}

// anthropicOrder is the order the real API reports field errors in.
var anthropicOrder = []anthropicField{
	{"model", anthModel},
	{"tool_choice", anthOptional(anthIs(js.Object, "Input should be an object"))},
	{"tools", anthOptional(anthTools)},
	{"messages", anthMessages},
	{"system", anthOptional(anthIs2(js.String, js.Array, "Input should be a valid array"))},
	{"thinking", anthOptional(anthThinking)},
	{"output_config", anthOptional(anthIs(js.Object, "Input does not match the expected shape."))},
	{"context_management", anthBeta("context-management-")},
	{"safeguards", anthSafeguards},
	{"max_tokens", anthMaxTokens},
	{"metadata", anthOptional(anthMetadata)},
	{"stop_sequences", anthOptional(anthIs(js.Array, "Input should be a valid array"))},
	{"service_tier", anthOptional(anthServiceTier)},
	{"stream", anthOptional(anthIs(js.Bool, "Input should be a valid boolean"))},
	{"container", anthOptional(anthContainer)},
	{"inference_geo", anthOptional(anthIs(js.String, "Input should be a valid string"))},
}

// anthropicSampling are checked after the unknown fields.
var anthropicSampling = []anthropicField{
	{"temperature", anthOptional(anthTemperature)},
	{"top_k", anthOptional(anthIs(js.Int, "Input should be a valid integer"))},
	{"top_p", anthOptional(anthTopP)},
}

// anthropicFields are the top-level fields of each route. count_tokens has
// no max_tokens and the generation parameters (max_tokens there is "Extra
// inputs are not permitted", recorded).
var anthropicFields = map[driver.Kind]map[string]struct{}{
	driver.KindMessages: js.Set(
		"model", "messages", "max_tokens", "system", "metadata", "stop_sequences", "stream",
		"temperature", "top_k", "top_p", "tools", "tool_choice", "thinking", "service_tier",
		"container", "context_management", "mcp_servers", "output_config", "output_format",
		"inference_geo", "safeguards",
	),
	driver.KindCountTokens: js.Set(
		"model", "messages", "system", "tools", "tool_choice", "thinking",
		"context_management", "mcp_servers", "output_config", "output_format",
	),
}

func validateAnthropic(kind driver.Kind, body []byte, h driver.Headers) *core.Error {
	allowed, ok := anthropicFields[kind]
	if !ok {
		return nil
	}
	if msg := anthropicCheck(body, allowed, h); msg != "" {
		return &core.Error{Kind: core.ErrInvalidRequest, Message: msg}
	}
	return nil
}

func anthropicCheck(body []byte, allowed map[string]struct{}, h driver.Headers) string {
	if msg := serdeError(body); msg != "" {
		return "The request body is not valid JSON: " + msg
	}
	if h != nil && h.Get("anthropic-version") == "" {
		// After the JSON and before every field and the model
		// (auth-order/validkey_no_version_*, ADR 0053).
		return "anthropic-version: header is required"
	}
	fs, ok := js.Members(body)
	if !ok {
		return "" // a non-object document: the driver answers
	}
	if _, ok := js.Lookup(fs, "output_format"); ok {
		return "output_format: This field is deprecated. Use 'output_config.format' instead. See https://platform.claude.com/docs/en/build-with-claude/structured-outputs for more information."
	}
	if _, ok := js.Lookup(fs, "mcp_servers"); ok && !hasBeta(h, "mcp-client-") {
		return "mcp_servers: this parameter requires anthropic-beta: mcp-client-2026-09-15 (or mcp-client-2025-11-20)"
	}
	if msg := anthFields(anthropicOrder, fs, allowed, h); msg != "" {
		return msg
	}
	for _, f := range fs {
		if _, ok := allowed[f.Key]; !ok {
			return f.Key + ": Extra inputs are not permitted"
		}
	}
	msgs, _ := js.Lookup(fs, "messages")
	if len(js.Elements(msgs)) == 0 {
		return "messages: at least one message is required"
	}
	if msg := anthSystemMessages(msgs); msg != "" {
		return msg
	}
	if msg := anthFields(anthropicSampling, fs, allowed, h); msg != "" {
		return msg
	}
	if tools, _ := js.Lookup(fs, "tools"); allDeferred(tools) {
		return "At least one tool must have defer_loading=false. All tools cannot be deferred."
	}
	return ""
}

// anthFields runs the checks of the fields in order that the route has.
func anthFields(order []anthropicField, fs []js.Field, allowed map[string]struct{}, h driver.Headers) string {
	for _, f := range order {
		if _, ok := allowed[f.name]; !ok {
			continue
		}
		v, has := js.Lookup(fs, f.name)
		if !has {
			v = nil
		}
		if msg := f.check(f.name, v, h); msg != "" {
			return msg
		}
	}
	return ""
}

// hasBeta reports whether the anthropic-beta header names a feature
// starting with prefix.
func hasBeta(h driver.Headers, prefix string) bool {
	if h == nil {
		return false
	}
	for _, b := range strings.Split(h.Get("anthropic-beta"), ",") {
		if strings.HasPrefix(strings.TrimSpace(b), prefix) {
			return true
		}
	}
	return false
}

// anthOptional skips a field that is absent or null.
func anthOptional(check func(string, []byte, driver.Headers) string) func(string, []byte, driver.Headers) string {
	return func(name string, v []byte, h driver.Headers) string {
		if v == nil || js.KindOf(v) == js.Null {
			return ""
		}
		return check(name, v, h)
	}
}

func anthIs(k js.Kind, msg string) func(string, []byte, driver.Headers) string {
	return anthIs2(k, k, msg)
}

func anthIs2(a, b js.Kind, msg string) func(string, []byte, driver.Headers) string {
	return func(name string, v []byte, _ driver.Headers) string {
		if k := js.KindOf(v); k != a && k != b {
			return name + ": " + msg
		}
		return ""
	}
}

func anthTemperature(name string, v []byte, _ driver.Headers) string {
	t, ok := js.NumberValue(v)
	switch {
	case !ok:
		return name + ": Input should be a valid number"
	case t < 0:
		return "`" + name + "` cannot be set to " + string(js.TrimSpace(v)) + " for this model."
	case t > 1:
		return name + ": range: 0..1"
	}
	return ""
}

func anthTopP(name string, v []byte, _ driver.Headers) string {
	p, ok := js.NumberValue(v)
	if !ok {
		return name + ": Input should be a valid number"
	}
	if p < 0 || p > 1 {
		return name + ": range: 0..1"
	}
	return ""
}

// anthCustomTool are the members of a custom tool (no type or "custom"),
// all accepted by the real API (recorded).
var anthCustomTool = js.Set(
	"type", "name", "description", "input_schema", "cache_control", "strict",
	"input_examples", "defer_loading", "allowed_callers", "eager_input_streaming",
)

// anthTools checks tools: an array whose custom tools carry only known
// members. Server tools (other types) are left to the driver.
func anthTools(name string, v []byte, _ driver.Headers) string {
	if js.KindOf(v) != js.Array {
		return name + ": Input should be a valid array"
	}
	for i, t := range js.Elements(v) {
		fs, ok := js.Members(t)
		if !ok {
			continue
		}
		if typ, has := js.Lookup(fs, "type"); has {
			if s, _ := js.StringValue(typ); s != "custom" {
				continue
			}
		}
		for _, f := range fs {
			if _, ok := anthCustomTool[f.Key]; !ok {
				return name + "." + strconv.Itoa(i) + ".custom." + f.Key + ": Extra inputs are not permitted"
			}
		}
	}
	return ""
}

func anthModel(name string, v []byte, _ driver.Headers) string {
	if v == nil {
		return name + ": Field required"
	}
	s, ok := js.StringValue(v)
	if !ok {
		return name + ": Input should be a valid string"
	}
	if s == "" {
		return name + ": String should have at least 1 character"
	}
	return ""
}

func anthMaxTokens(name string, v []byte, _ driver.Headers) string {
	if v == nil {
		return name + ": Field required"
	}
	if js.KindOf(v) != js.Int {
		return name + ": Input should be a valid integer"
	}
	if n, _ := js.NumberValue(v); n < 0 {
		return name + ": must be greater than or equal to 0"
	}
	return ""
}

// anthBeta refuses a field the account only gets with an anthropic-beta.
func anthBeta(prefix string) func(string, []byte, driver.Headers) string {
	return func(name string, v []byte, h driver.Headers) string {
		if v != nil && !hasBeta(h, prefix) {
			return name + ": Extra inputs are not permitted"
		}
		return ""
	}
}

func anthMetadata(name string, v []byte, _ driver.Headers) string {
	fs, ok := js.Members(v)
	if !ok {
		return name + ": Input does not match the expected shape."
	}
	for _, f := range fs {
		if f.Key != "user_id" {
			return name + "." + f.Key + ": Extra inputs are not permitted"
		}
	}
	return ""
}

func anthServiceTier(name string, v []byte, _ driver.Headers) string {
	if s, _ := js.StringValue(v); s != "auto" && s != "standard_only" {
		return name + ": Input should be 'auto' or 'standard_only'"
	}
	return ""
}

func anthContainer(name string, v []byte, _ driver.Headers) string {
	if k := js.KindOf(v); k != js.String && k != js.Object {
		return name + ".ContainerParams: Input does not match the expected shape."
	}
	return ""
}

// anthTextBlock are the members of a text content block.
var anthTextBlock = js.Set("type", "text", "cache_control", "citations")

// anthMessages checks messages and, inside each message, the role, the
// content, (for text blocks) the block members and output_config, which
// only a role "system" message has, with the beta per-turn-control (the
// form of Claude Code 2.1.308, recorded on 2026-11-03: without the beta it
// is an extra input; with it, on another role, its own refusal, before the
// unknown top-level fields). Other block types are left to the driver.
func anthMessages(name string, v []byte, h driver.Headers) string {
	if v == nil {
		return name + ": Field required"
	}
	if js.KindOf(v) != js.Array {
		return name + ": Input should be a valid array"
	}
	for i, m := range js.Elements(v) {
		mf, ok := js.Members(m)
		if !ok {
			continue
		}
		p := name + "." + strconv.Itoa(i)
		role, has := js.Lookup(mf, "role")
		r, isStr := js.StringValue(role)
		switch {
		case !has:
			return p + ".role: Field required"
		case !isStr:
			return p + ".role: Input should be 'user', 'assistant', 'system'"
		case r != "user" && r != "assistant" && r != "system":
			return name + `: Unexpected role "` + r + `". Allowed roles are "user", "assistant" or "system"`
		}
		content, has := js.Lookup(mf, "content")
		switch js.KindOf(content) {
		case js.String:
		case js.Array:
			for j, b := range js.Elements(content) {
				if msg := anthBlock(p+".content."+strconv.Itoa(j), b); msg != "" {
					return msg
				}
			}
		default:
			if !has {
				return p + ".content: Field required"
			}
			return p + ".content: Input should be a valid array"
		}
		for _, f := range mf {
			switch {
			case f.Key == "role" || f.Key == "content":
			case f.Key == "output_config" && hasBeta(h, "per-turn-control-"):
				if r != "system" {
					return p + ".output_config: output_config is only permitted on role 'system' messages"
				}
				if msg := anthTurnControl(p+".output_config", f.Val); msg != "" {
					return msg
				}
			default:
				return p + "." + f.Key + ": Extra inputs are not permitted"
			}
		}
	}
	return ""
}

// anthEfforts are the values of output_config.effort.
var anthEfforts = js.Set("low", "medium", "high", "xhigh", "max")

// anthTurnControl checks the output_config of a role "system" message:
// effort only (format there is an extra input, recorded).
func anthTurnControl(p string, v []byte) string {
	fs, ok := js.Members(v)
	if !ok {
		return p + ": Input does not match the expected shape."
	}
	for _, f := range fs {
		if f.Key != "effort" {
			return p + "." + f.Key + ": Extra inputs are not permitted"
		}
		if s, _ := js.StringValue(f.Val); !has(anthEfforts, s) {
			return p + ".effort: Input should be 'low', 'medium', 'high', 'xhigh' or 'max'"
		}
	}
	return ""
}

func has(set map[string]struct{}, k string) bool {
	_, ok := set[k]
	return ok
}

// The refusals of a role "system" message (recorded on 2026-11-03, tasks
// 441 and 453), after the unknown fields and before the sampling fields.
// The messages are checked in order; a run is a stretch of consecutive
// system messages and its first one is the first that is not
// directive-only (content [] with output_config, accepted anywhere: a run
// of directives alone is never refused). The first message of a run is
// refused for empty content, then a block other than text, tool_addition
// or tool_removal, then its position (the first message of the array;
// after a message other than a user one or an assistant one ending in a
// server tool result), then, at the end of the run, what follows it
// (anything but an assistant message), then a text that is only
// whitespace. Each later message of the run: the blocks, what follows the
// run when it ends it, the empty content, the whitespace. A directive
// that ends a run is refused for what follows it, like the others.
const (
	sysDirective = "; the directive-only form (content: [] with output_config) is accepted at any position"
	sysFirst     = ": use the top-level 'system' parameter for the initial system prompt" + sysDirective
	sysFollow    = ": role 'system' must follow a 'user' message or an 'assistant' message ending in a server tool result" + sysDirective
	sysPrecede   = ": role 'system' must precede an 'assistant' message or end the array" + sysDirective
	sysEmpty     = ": system content must contain at least one block"
	sysBlocks    = ": role 'system' supports text, tool_addition, and tool_removal blocks only"
	sysBlank     = ": system text blocks must contain non-whitespace text"
)

var anthSystemBlocks = js.Set("text", "tool_addition", "tool_removal")

func anthSystemMessages(v []byte) string {
	msgs := js.Elements(v)
	runStart, first := -1, true // the run's first message, and whether the next one opens it
	for i, m := range msgs {
		mf, _ := js.Members(m)
		if role(mf) != "system" {
			runStart, first = -1, true
			continue
		}
		if runStart < 0 {
			runStart = i
		}
		p := "messages." + strconv.Itoa(i)
		content, _ := js.Lookup(mf, "content")
		blocks := js.Elements(content)
		_, directive := js.Lookup(mf, "output_config")
		directive = directive && js.KindOf(content) == js.Array && len(blocks) == 0
		precede := !first && runEnds(msgs, i) // a run with a message besides directives
		switch {
		case directive:
			if precede {
				return p + sysPrecede
			}
			continue
		case first:
			first = false
			if sysEmptyContent(content, blocks) {
				return p + sysEmpty
			}
			if !sysKnownBlocks(blocks) {
				return p + sysBlocks
			}
			if runStart == 0 {
				return p + sysFirst
			}
			if !systemMayFollow(msgs[:runStart]) {
				return p + sysFollow
			}
			if runEnds(msgs, i) {
				return p + sysPrecede
			}
		default:
			if !sysKnownBlocks(blocks) {
				return p + sysBlocks
			}
			if precede {
				return p + sysPrecede
			}
			if sysEmptyContent(content, blocks) {
				return p + sysEmpty
			}
		}
		if sysBlankText(content, blocks) {
			return p + sysBlank
		}
	}
	return ""
}

// runEnds reports whether the system message at i ends its run followed by
// a message other than an assistant one (the array ending is accepted).
func runEnds(msgs [][]byte, i int) bool {
	if i+1 >= len(msgs) {
		return false
	}
	mf, _ := js.Members(msgs[i+1])
	r := role(mf)
	return r != "system" && r != "assistant"
}

func sysEmptyContent(content []byte, blocks [][]byte) bool {
	if s, ok := js.StringValue(content); ok {
		return s == ""
	}
	return js.KindOf(content) == js.Array && len(blocks) == 0
}

func sysKnownBlocks(blocks [][]byte) bool {
	for _, b := range blocks {
		bf, _ := js.Members(b)
		t, _ := js.Lookup(bf, "type")
		if s, _ := js.StringValue(t); !has(anthSystemBlocks, s) {
			return false
		}
	}
	return true
}

// sysBlankText reports a string content, or a text block, with only
// whitespace (an empty text block included).
func sysBlankText(content []byte, blocks [][]byte) bool {
	if s, ok := js.StringValue(content); ok {
		return strings.TrimSpace(s) == ""
	}
	for _, b := range blocks {
		bf, _ := js.Members(b)
		t, _ := js.Lookup(bf, "type")
		if s, _ := js.StringValue(t); s != "text" {
			continue
		}
		x, _ := js.Lookup(bf, "text")
		if s, _ := js.StringValue(x); strings.TrimSpace(s) == "" {
			return true
		}
	}
	return false
}

func role(mf []js.Field) string {
	r, _ := js.Lookup(mf, "role")
	s, _ := js.StringValue(r)
	return s
}

// systemMayFollow reports whether a role "system" message may come after
// prev: the last message that is not a system one is a user message, or an
// assistant message whose last block is a server tool result
// (web_search_tool_result and the like; not conferred).
func systemMayFollow(prev [][]byte) bool {
	for i := len(prev) - 1; i >= 0; i-- {
		mf, _ := js.Members(prev[i])
		switch role(mf) {
		case "system":
			continue
		case "assistant":
			content, _ := js.Lookup(mf, "content")
			blocks := js.Elements(content)
			if len(blocks) == 0 {
				return false
			}
			bf, _ := js.Members(blocks[len(blocks)-1])
			t, _ := js.Lookup(bf, "type")
			s, _ := js.StringValue(t)
			return strings.HasSuffix(s, "_tool_result") && s != "tool_result"
		default:
			return true
		}
	}
	return true
}

// allDeferred reports whether tools is a non-empty list in which every tool
// has defer_loading true (recorded: refused after the sampling fields).
func allDeferred(v []byte) bool {
	tools := js.Elements(v)
	if js.KindOf(v) != js.Array || len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		fs, _ := js.Members(t)
		if d, _ := js.Lookup(fs, "defer_loading"); string(js.TrimSpace(d)) != "true" {
			return false
		}
	}
	return true
}

// anthThinking checks thinking: an object; the adaptive branch has type
// and display only, display "summarized" or "omitted", and "updates" with
// the beta thinking-display-updates (recorded on 2026-11-03).
func anthThinking(name string, v []byte, h driver.Headers) string {
	fs, ok := js.Members(v)
	if !ok {
		return name + ": Input should be an object"
	}
	t, _ := js.Lookup(fs, "type")
	if s, _ := js.StringValue(t); s != "adaptive" {
		return ""
	}
	p := name + ".adaptive."
	for _, f := range fs {
		switch f.Key {
		case "type":
		case "display":
			d, _ := js.StringValue(f.Val)
			updates := hasBeta(h, "thinking-display-updates-")
			if d == "summarized" || d == "omitted" || (d == "updates" && updates) {
				continue
			}
			if updates {
				return p + "display: Input should be 'summarized', 'omitted', 'updates'"
			}
			return p + "display: Input should be 'summarized', 'omitted'"
		default:
			return p + f.Key + ": Extra inputs are not permitted"
		}
	}
	return ""
}

// anthSafeguards checks safeguards: an extra input without the beta
// dangerous-tool-use; with it a list of dangerous_tool_use objects
// (recorded on 2026-11-03; after context_management, before max_tokens).
func anthSafeguards(name string, v []byte, h driver.Headers) string {
	if v == nil {
		return ""
	}
	if !hasBeta(h, "dangerous-tool-use-") {
		return name + ": Extra inputs are not permitted"
	}
	if js.KindOf(v) == js.Null {
		return ""
	}
	if js.KindOf(v) != js.Array {
		return name + ": Input should be a valid array"
	}
	for i, e := range js.Elements(v) {
		fs, _ := js.Members(e)
		t, ok := js.Lookup(fs, "type")
		s, _ := js.StringValue(t)
		switch {
		case !ok:
			return name + "." + strconv.Itoa(i) + ": Unable to extract tag using discriminator 'type'"
		case s != "dangerous_tool_use":
			return name + "." + strconv.Itoa(i) + ": Input tag '" + s + "' found using 'type' does not match any of the expected tags: 'dangerous_tool_use'"
		}
	}
	return ""
}

// anthBlock checks a text content block; pydantic names the union branch
// (the block type) in the path.
func anthBlock(p string, b []byte) string {
	fs, ok := js.Members(b)
	if !ok {
		return ""
	}
	t, _ := js.Lookup(fs, "type")
	if s, _ := js.StringValue(t); s != "text" {
		return ""
	}
	p += ".text"
	if _, ok := js.Lookup(fs, "text"); !ok {
		return p + ".text: Field required"
	}
	for _, f := range fs {
		if _, ok := anthTextBlock[f.Key]; !ok {
			return p + "." + f.Key + ": Extra inputs are not permitted"
		}
	}
	return ""
}

// --- serde_json syntax errors ---------------------------------------------

// serdeError returns serde_json's message for a malformed document ("" when
// it is well formed): "<what> at line L column C". The position follows
// serde: column counts the bytes of the line up to and including the
// offending byte; at end of input it is the length of the last line.
func serdeError(b []byte) string {
	p := &serde{b: b}
	if !p.value() {
		return p.err
	}
	if p.ws() {
		p.fail("trailing characters")
		return p.err
	}
	return ""
}

type serde struct {
	b   []byte
	i   int
	err string
}

// fail records msg at the peeked byte (or at end of input).
func (p *serde) fail(msg string) bool {
	return p.failAt(msg, min(p.i+1, len(p.b)))
}

// failAt records msg at the position of index end (exclusive).
func (p *serde) failAt(msg string, end int) bool {
	line, col := 1, 0
	for _, c := range p.b[:end] {
		col++
		if c == '\n' {
			line, col = line+1, 0
		}
	}
	p.err = msg + " at line " + strconv.Itoa(line) + " column " + strconv.Itoa(col)
	return false
}

// ws skips whitespace and reports whether a byte follows.
func (p *serde) ws() bool {
	p.i = js.SkipSpace(p.b, p.i)
	return p.i < len(p.b)
}

func (p *serde) value() bool {
	if !p.ws() {
		return p.fail("EOF while parsing a value")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		p.i++
		return p.object()
	case c == '[':
		p.i++
		return p.list()
	case c == '"':
		return p.str()
	case c == 't':
		return p.ident("true")
	case c == 'f':
		return p.ident("false")
	case c == 'n':
		return p.ident("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return p.fail("expected value")
}

func (p *serde) ident(word string) bool {
	for k := 0; k < len(word); k++ {
		if p.i >= len(p.b) {
			return p.failAt("EOF while parsing a value", len(p.b))
		}
		if p.b[p.i] != word[k] {
			p.i++
			return p.failAt("expected ident", p.i)
		}
		p.i++
	}
	return true
}

func (p *serde) number() bool {
	if p.b[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i >= len(p.b) {
		return p.failAt("EOF while parsing a value", len(p.b))
	}
	if p.b[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		p.i++
		return p.failAt("invalid number", p.i)
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if digits() == 0 {
			if p.i >= len(p.b) {
				return p.failAt("EOF while parsing a value", len(p.b))
			}
			p.i++
			return p.failAt("invalid number", p.i)
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			if p.i >= len(p.b) {
				return p.failAt("EOF while parsing a value", len(p.b))
			}
			p.i++
			return p.failAt("invalid number", p.i)
		}
	}
	return true
}

func (p *serde) str() bool {
	for p.i++; p.i < len(p.b); p.i++ {
		switch c := p.b[p.i]; {
		case c == '"':
			p.i++
			return true
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return p.failAt("EOF while parsing a string", len(p.b))
			}
			switch p.b[p.i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for k := 0; k < 4; k++ {
					p.i++
					if p.i >= len(p.b) {
						return p.failAt("EOF while parsing a string", len(p.b))
					}
					if !isHex(p.b[p.i]) {
						return p.failAt("invalid escape", p.i+1)
					}
				}
			default:
				return p.failAt("invalid escape", p.i+1)
			}
		case c < 0x20:
			return p.failAt("control character (\\u0000-\\u001F) found while parsing a string", p.i+1)
		}
	}
	return p.failAt("EOF while parsing a string", len(p.b))
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (p *serde) list() bool {
	for first := true; ; first = false {
		if !p.ws() {
			return p.fail("EOF while parsing a list")
		}
		switch c := p.b[p.i]; {
		case c == ']':
			p.i++
			return true
		case !first && c == ',':
			p.i++
			if !p.ws() {
				return p.fail("EOF while parsing a value")
			}
			if p.b[p.i] == ']' {
				return p.fail("trailing comma")
			}
		case !first:
			return p.fail("expected `,` or `]`")
		}
		if !p.value() {
			return false
		}
	}
}

func (p *serde) object() bool {
	for first := true; ; first = false {
		if !p.ws() {
			return p.fail("EOF while parsing an object")
		}
		switch c := p.b[p.i]; {
		case c == '}':
			p.i++
			return true
		case !first && c == ',':
			p.i++
			if !p.ws() {
				return p.fail("EOF while parsing a value")
			}
		case !first:
			return p.fail("expected `,` or `}`")
		}
		switch p.b[p.i] {
		case '"':
		case '}':
			return p.fail("trailing comma")
		default:
			return p.fail("key must be a string")
		}
		if !p.str() {
			return false
		}
		if !p.ws() {
			return p.fail("EOF while parsing an object")
		}
		if p.b[p.i] != ':' {
			return p.fail("expected `:`")
		}
		p.i++
		if !p.value() {
			return false
		}
	}
}
