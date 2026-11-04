package drivertest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// frame is one SSE frame.
type frame struct {
	Event   string
	Data    string
	Comment string
}

// parseSSE splits a stream into frames; every frame ends with a blank line.
func parseSSE(t *testing.T, b []byte) []frame {
	t.Helper()
	s := string(b)
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("stream does not end with a blank line: %q", tail(s))
	}
	var out []frame
	for _, block := range strings.Split(strings.TrimSuffix(s, "\n\n"), "\n\n") {
		var f frame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, ":"):
				f.Comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			default:
				t.Fatalf("unexpected SSE line %q in frame %q", line, block)
			}
		}
		out = append(out, f)
	}
	return out
}

func tail(s string) string {
	if len(s) > 120 {
		return "…" + s[len(s)-120:]
	}
	return s
}

func decodeData(t *testing.T, f frame) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader([]byte(f.Data)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("data is not a JSON object: %v: %q", err, f.Data)
	}
	return m
}

type toolSpec struct {
	id, name string
	args     []string
}

type streamCase struct {
	name         string
	reasoning    []string
	text         []string
	tools        []toolSpec
	finish       core.FinishReason
	includeUsage bool
	usage        core.Usage
}

func (s *suite) streamCases(k driver.Kind) []streamCase {
	u := core.Usage{PromptTokens: 12, CompletionTokens: 7, ReasoningTokens: 3, CachedTokens: 4, CacheWriteTokens: 1, CostMicroUSD: 2500}
	cases := []streamCase{
		{name: "text", text: []string{"Hel", "lo ", "world."}, finish: core.FinishStop, includeUsage: true, usage: u},
		{name: "text_without_usage", text: []string{"Hi"}, finish: core.FinishLength, usage: u},
	}
	if k == driver.KindCompletions {
		return cases
	}
	if s.surface(k).RefusalDetails {
		// Fixed refusal case: stop_details must be checked on every run.
		cases = append(cases, streamCase{name: "refusal", text: []string{"I can't ", "help."}, finish: core.FinishRefusal, includeUsage: true, usage: u})
	}
	return append(cases,
		streamCase{name: "tool_calls", text: []string{"Checking."}, tools: []toolSpec{
			{id: s.toolCallID(k, "s1"), name: "get_weather", args: []string{`{"ci`, `ty":"Pa`, `ris"}`}},
			{id: s.toolCallID(k, "s2"), name: "get_time", args: []string{`{}`}},
		}, finish: core.FinishToolCalls, includeUsage: true, usage: u},
		streamCase{name: "reasoning", reasoning: []string{"Let me ", "think."}, text: []string{"Answer."}, finish: core.FinishStop, includeUsage: true, usage: u},
	)
}

// drive feeds the abstract events in the order the engine uses.
func drive(t *testing.T, sw core.StreamWriter, resp *core.Response, c streamCase) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(sw.Start(resp))
	for _, r := range c.reasoning {
		must(sw.ReasoningDelta(r))
	}
	for _, x := range c.text {
		must(sw.TextDelta(x))
	}
	for i, tl := range c.tools {
		must(sw.ToolCallStart(i, tl.id, tl.name))
		for _, a := range tl.args {
			must(sw.ToolArgsDelta(i, a))
		}
		must(sw.ToolCallEnd(i))
	}
	must(sw.Finish(c.finish, c.usage, c.includeUsage))
	must(sw.Close())
}

func (s *suite) testStream(t *testing.T) {
	for _, k := range s.surfaces() {
		s.run(t, string(k), func(t *testing.T) {
			for _, c := range s.streamCases(k) {
				s.run(t, c.name, func(t *testing.T) {
					w := core.NewMemWriter()
					req := s.newReq(k, true)
					req.IncludeUsage = c.includeUsage
					resp := &core.Response{ID: s.responseID(k, "s"), Model: "model-a", Created: created, Usage: core.Usage{PromptTokens: c.usage.PromptTokens, CachedTokens: c.usage.CachedTokens, CacheWriteTokens: c.usage.CacheWriteTokens}}
					drive(t, s.d.StreamWriter(w, req, nil), resp, c)
					if w.StatusCode() != 200 {
						t.Errorf("status %d, want 200", w.StatusCode())
					}
					if ct, _ := header(w.Headers(), "Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
						t.Errorf("Content-Type %q, want text/event-stream", ct)
					}
					s.checkSuccessHeaders(t, k, w.Headers())
					frames := parseSSE(t, w.Body())
					if anthropicWire(k) {
						s.checkMessagesStream(t, frames, w.Headers(), req, resp, c)
					} else {
						s.checkChatStream(t, k, frames, resp, c)
					}
				})
			}
			s.run(t, "midstream_error", func(t *testing.T) { s.streamError(t, k) })
			s.run(t, "ping", func(t *testing.T) {
				w := core.NewMemWriter()
				sw := s.d.StreamWriter(w, s.newReq(k, true), nil)
				resp := &core.Response{ID: s.responseID(k, "p"), Model: "model-a", Created: created}
				if err := sw.Start(resp); err != nil {
					t.Fatal(err)
				}
				if err := sw.Ping(); err != nil {
					t.Fatal(err)
				}
				frames := parseSSE(t, w.Body())
				last := frames[len(frames)-1]
				if anthropicWire(k) {
					if last.Event != "ping" || str(decodeData(t, last), "type") != "ping" {
						t.Errorf("ping frame = %+v, want event: ping", last)
					}
				} else if last.Comment == "" || last.Data != "" {
					t.Errorf("ping frame = %+v, want an SSE comment", last)
				}
			})
		})
	}
}

func (s *suite) checkChatStream(t *testing.T, k driver.Kind, frames []frame, resp *core.Response, c streamCase) {
	t.Helper()
	sf := s.surface(k)
	if len(frames) < 3 {
		t.Fatalf("only %d frames", len(frames))
	}
	i := 0
	if sf.KeepAlive != "" {
		if frames[0].Comment != sf.KeepAlive {
			t.Errorf("first frame %+v, want keep-alive comment %q", frames[0], sf.KeepAlive)
		}
		i = 1
	}
	if sf.ContentFilter {
		checkPromptFilterFrame(t, frames[i])
		i++
	}
	last := frames[len(frames)-1]
	if last.Data != "[DONE]" {
		t.Fatalf("last frame %+v, want data: [DONE]", last)
	}
	object := "chat.completion.chunk"
	if k == driver.KindCompletions {
		object = "text_completion"
	}
	var (
		chunks                         []map[string]any
		reasoning, text                strings.Builder
		args                           = map[int]*strings.Builder{}
		starts                         = map[int]any{}
		firstText, lastReasoning       = -1, -1
		finishAt, usageAt, finishCount = -1, -1, 0
	)
	for _, f := range frames[i : len(frames)-1] {
		if f.Comment != "" || f.Event != "" {
			t.Fatalf("unexpected frame %+v in a chat stream", f)
		}
		if f.Data == "[DONE]" {
			t.Fatal("[DONE] before the end of the stream")
		}
		chunks = append(chunks, decodeData(t, f))
	}
	for n, ch := range chunks {
		expectPath(t, ch, "id", resp.ID)
		expectPath(t, ch, "object", object)
		expectPath(t, ch, "created", created.Unix())
		expectPath(t, ch, "model", resp.Model)
		if sf.Obfuscation {
			if o, _ := ch["obfuscation"].(string); o == "" {
				t.Errorf("chunk %d without obfuscation: %v", n, ch)
			}
		} else if _, ok := ch["obfuscation"]; ok && sf.Envelope != EnvAny {
			t.Errorf("chunk %d carries obfuscation", n)
		}
		if sf.ServiceTier != "" && k != driver.KindCompletions {
			expectPath(t, ch, "service_tier", sf.ServiceTier)
		}
		if _, ok := ch["usage"]; !ok && sf.ChunkUsageNull && c.includeUsage {
			t.Errorf("chunk %d without usage: null (include_usage)", n)
		}
		if u, ok := ch["usage"]; ok && u == nil {
			// usage: null before the usage chunk (OpenAI with include_usage).
			if (!sf.ChunkUsageNull || !c.includeUsage) && sf.Envelope != EnvAny {
				t.Errorf("chunk %d carries usage: null; only with ChunkUsageNull and include_usage", n)
			}
		} else if ok {
			if usageAt >= 0 {
				t.Errorf("usage appears in more than one chunk")
			}
			usageAt = n
		}
		if fr, ok := path(ch, "choices.0.finish_reason"); ok && fr != nil {
			if finishAt < 0 {
				finishAt = n
			}
			finishCount++
			expectPath(t, ch, "choices.0.finish_reason", openAIFinish(c.finish))
			if sf.NativeFinishReason {
				expectPresent(t, ch, "choices.0.native_finish_reason")
			}
		}
		if k == driver.KindCompletions {
			text.WriteString(str(ch, "choices.0.text"))
			continue
		}
		delta, _ := path(ch, "choices.0.delta")
		if c0, ok := path(ch, "choices.0"); ok && sf.ContentFilter {
			checkChoiceFilter(t, c0, str(delta, "content") != "")
		}
		if n == 0 {
			expectPath(t, delta, "role", "assistant")
		}
		for _, field := range []string{"reasoning_content", "reasoning"} {
			if v, ok := path(delta, field); ok {
				if sf.ReasoningField != "" && field != sf.ReasoningField {
					t.Errorf("reasoning in delta.%s, want delta.%s", field, sf.ReasoningField)
				}
				x, _ := v.(string)
				reasoning.WriteString(x)
				lastReasoning = n
				s.checkReasoningDetails(t, sf, delta, true, x)
			}
		}
		if _, ok := path(delta, "reasoning_details"); ok && lastReasoning != n {
			t.Errorf("reasoning_details in a delta without reasoning text: %v", delta)
		}
		if x := str(delta, "content"); x != "" {
			text.WriteString(x)
			if firstText < 0 {
				firstText = n
			}
		}
		for _, tc := range arr(delta, "tool_calls") {
			idxf, ok := num(tc, "index")
			if !ok {
				t.Fatalf("tool_calls delta without index: %v", tc)
			}
			idx := int(idxf)
			if id := str(tc, "id"); id != "" {
				if _, dup := starts[idx]; dup {
					t.Errorf("tool call %d started twice", idx)
				}
				starts[idx] = tc
				args[idx] = &strings.Builder{}
			}
			if args[idx] == nil {
				t.Fatalf("arguments for tool call %d before its start", idx)
			}
			_, _ = args[idx].WriteString(str(tc, "function.arguments"))
		}
	}
	want := strings.Join(c.reasoning, "")
	if sf.ReasoningField == "hidden" || (sf.ReasoningField == "" && lastReasoning < 0) {
		// ADR 0022: hidden reasoning sends no delta at all, nor leaks into
		// content; its tokens still count in usage (checked below).
		want = ""
		if len(c.reasoning) > 0 && strings.Contains(text.String(), strings.Join(c.reasoning, "")) {
			t.Errorf("hidden reasoning leaked into content %q", text.String())
		}
	}
	if got := reasoning.String(); got != want {
		t.Errorf("reasoning %q, want %q", got, want)
	}
	if sf.ReasoningField == "hidden" {
		// No empty frame stands in for a hidden reasoning delta: role, one
		// chunk per text/tool event, finish and the optional usage chunk.
		n := 1 + len(c.text) + 1
		for _, tl := range c.tools {
			n += 1 + len(tl.args)
		}
		if c.includeUsage {
			n++
		}
		if len(chunks) != n {
			t.Errorf("%d chunks, want %d (hidden reasoning must not emit frames)", len(chunks), n)
		}
	}
	if lastReasoning >= 0 && firstText >= 0 && lastReasoning > firstText {
		t.Error("reasoning deltas after the first content delta (spec 007: reasoning first)")
	}
	if got, want := text.String(), strings.Join(c.text, ""); got != want {
		t.Errorf("content %q, want %q", got, want)
	}
	if len(starts) != len(c.tools) {
		t.Errorf("%d tool calls started, want %d", len(starts), len(c.tools))
	}
	for i, tl := range c.tools {
		st := starts[i]
		expectPath(t, st, "id", tl.id)
		expectPath(t, st, "type", "function")
		expectPath(t, st, "function.name", tl.name)
		if args[i] != nil && args[i].String() != strings.Join(tl.args, "") {
			t.Errorf("tool %d arguments %q, want %q", i, args[i].String(), strings.Join(tl.args, ""))
		}
	}
	if finishAt < 0 {
		t.Fatal("no chunk with finish_reason")
	}
	if sf.UsageInFinish {
		// The closing chunk itself carries usage (OpenCode).
		if usageAt != len(chunks)-1 || finishAt != usageAt {
			t.Fatalf("usage at %d, finish at %d; want both on the last chunk (%d)", usageAt, finishAt, len(chunks)-1)
		}
		s.checkChatUsage(t, sf, chunks[usageAt], "usage", c.usage)
	} else if c.includeUsage || sf.UsageAlways {
		if usageAt != len(chunks)-1 {
			t.Fatalf("usage chunk at %d, want the last chunk (%d)", usageAt, len(chunks)-1)
		}
		uc := chunks[usageAt]
		s.checkChatUsage(t, sf, uc, "usage", c.usage)
		if k != driver.KindCompletions {
			if sf.FinalChunkWithChoice {
				if len(arr(uc, "choices")) != 1 {
					t.Errorf("usage chunk choices %v, want one empty-delta choice", uc["choices"])
				}
			} else if ch := arr(uc, "choices"); ch == nil || len(ch) != 0 {
				t.Errorf("usage chunk choices %v, want []", uc["choices"])
			}
		}
	} else if usageAt >= 0 {
		t.Errorf("usage chunk without stream_options.include_usage")
	}
	if !sf.FinalChunkWithChoice && finishCount != 1 {
		t.Errorf("finish_reason in %d chunks, want 1", finishCount)
	}
}

func (s *suite) checkMessagesStream(t *testing.T, frames []frame, h map[string]string, req *core.Request, resp *core.Response, c streamCase) {
	t.Helper()
	sf := s.surface(driver.KindMessages)
	if sf.RequestIDHeader {
		expectHeader(t, h, "request-id", req.RequestID)
	}
	var events []map[string]any
	for _, f := range frames {
		if f.Event == "" || f.Data == "" {
			t.Fatalf("frame %+v: Messages streams use event: + data:", f)
		}
		if f.Data == "[DONE]" {
			t.Fatal("data: [DONE] in a Messages stream")
		}
		ev := decodeData(t, f)
		if str(ev, "type") != f.Event {
			t.Errorf("event %q carries type %q", f.Event, str(ev, "type"))
		}
		events = append(events, ev)
	}
	if str(events[0], "type") != "message_start" {
		t.Fatalf("first event %v, want message_start", events[0])
	}
	ms := events[0]
	expectPath(t, ms, "message.id", resp.ID)
	expectPath(t, ms, "message.type", "message")
	expectPath(t, ms, "message.role", "assistant")
	expectPath(t, ms, "message.model", resp.Model)
	expectPath(t, ms, "message.content", []any{})
	expectPath(t, ms, "message.stop_reason", nil)
	wantIn := c.usage.PromptTokens - c.usage.CachedTokens - c.usage.CacheWriteTokens
	expectPath(t, ms, "message.usage.input_tokens", wantIn)
	expectPath(t, ms, "message.usage.cache_read_input_tokens", c.usage.CachedTokens)
	expectPath(t, ms, "message.usage.cache_creation_input_tokens", c.usage.CacheWriteTokens)
	if str(events[len(events)-1], "type") != "message_stop" {
		t.Fatalf("last event %v, want message_stop", events[len(events)-1])
	}
	md := events[len(events)-2]
	if str(md, "type") != "message_delta" {
		t.Fatalf("event before message_stop %v, want message_delta", md)
	}
	expectPath(t, md, "delta.stop_reason", anthropicStop(c.finish))
	expectPath(t, md, "delta.stop_sequence", nil)
	if sf.RefusalDetails && c.finish == core.FinishRefusal {
		expectRefusalDetails(t, md, "delta.stop_details")
	}
	expectPath(t, md, "usage.output_tokens", c.usage.CompletionTokens)
	expectPath(t, md, "usage.input_tokens", wantIn)
	if sf.UsageCost {
		expectPresent(t, md, "usage.cost")
	}

	// Content blocks: start(i) deltas... stop(i), indices 0..n-1 in order.
	var (
		types    []string
		open     = -1
		thinking strings.Builder
		text     strings.Builder
		sig      bool
		toolArgs []*strings.Builder
		toolHead []any
	)
	for _, ev := range events[1 : len(events)-2] {
		idxf, _ := num(ev, "index")
		idx := int(idxf)
		switch str(ev, "type") {
		case "content_block_start":
			if open >= 0 {
				t.Fatalf("block %d starts while %d is open", idx, open)
			}
			if idx != len(types) {
				t.Fatalf("block index %d, want %d", idx, len(types))
			}
			open = idx
			cb, _ := path(ev, "content_block")
			types = append(types, str(cb, "type"))
			if str(cb, "type") == "tool_use" {
				toolHead = append(toolHead, cb)
				toolArgs = append(toolArgs, &strings.Builder{})
				expectPath(t, cb, "input", map[string]any{})
			}
		case "content_block_delta":
			if idx != open {
				t.Fatalf("delta for block %d while %d is open", idx, open)
			}
			d, _ := path(ev, "delta")
			switch str(d, "type") {
			case "thinking_delta":
				thinking.WriteString(str(d, "thinking"))
			case "signature_delta":
				sig = str(d, "signature") != ""
			case "text_delta":
				text.WriteString(str(d, "text"))
			case "input_json_delta":
				if len(toolArgs) == 0 {
					t.Fatalf("input_json_delta sem bloco tool_use aberto")
				}
				_, _ = toolArgs[len(toolArgs)-1].WriteString(str(d, "partial_json"))
			default:
				t.Errorf("unknown delta type %v", d)
			}
		case "content_block_stop":
			if idx != open {
				t.Fatalf("stop for block %d while %d is open", idx, open)
			}
			open = -1
		case "ping":
			// Anthropic sends one after the first content_block_start and
			// may send more anywhere (spec 006 16.2, 16.7).
		default:
			t.Errorf("unexpected event %v between blocks", ev)
		}
	}
	if open >= 0 {
		t.Errorf("block %d never stopped", open)
	}
	var want []string
	if len(c.reasoning) > 0 {
		want = append(want, "thinking")
	}
	if len(c.text) > 0 {
		want = append(want, "text")
	}
	for range c.tools {
		want = append(want, "tool_use")
	}
	if !eqStrings(types, want) {
		t.Fatalf("block types %v, want %v", types, want)
	}
	if got := thinking.String(); got != strings.Join(c.reasoning, "") {
		t.Errorf("thinking %q, want %q", got, strings.Join(c.reasoning, ""))
	}
	switch nosig := s.surface(driver.KindMessages).NoSignature; {
	case len(c.reasoning) > 0 && !sig && !nosig:
		t.Error("thinking block without signature_delta (spec 007)")
	case sig && nosig:
		t.Error("signature_delta on a surface without signatures")
	}
	if got := text.String(); got != strings.Join(c.text, "") {
		t.Errorf("text %q, want %q", got, strings.Join(c.text, ""))
	}
	for i, tl := range c.tools {
		expectPath(t, toolHead[i], "id", tl.id)
		expectPath(t, toolHead[i], "name", tl.name)
		if got := toolArgs[i].String(); got != strings.Join(tl.args, "") {
			t.Errorf("tool %d partial_json %q, want %q", i, got, strings.Join(tl.args, ""))
		}
	}
}

// streamError: an error after the 200 is an event in the dialect.
func (s *suite) streamError(t *testing.T, k driver.Kind) {
	sf := s.surface(k)
	w := core.NewMemWriter()
	sw := s.d.StreamWriter(w, s.newReq(k, true), nil)
	resp := &core.Response{ID: s.responseID(k, "e"), Model: "model-a", Created: created}
	if err := sw.Start(resp); err != nil {
		t.Fatal(err)
	}
	if err := sw.TextDelta("partial"); err != nil {
		t.Fatal(err)
	}
	if err := sw.Error(&core.Error{Kind: core.ErrOverloaded, Message: "Overloaded"}); err != nil {
		t.Fatal(err)
	}
	_ = sw.Close()
	if w.StatusCode() != 200 {
		t.Errorf("status %d: a mid-stream error keeps the 200", w.StatusCode())
	}
	frames := parseSSE(t, w.Body())
	if anthropicWire(k) {
		last := frames[len(frames)-1]
		if last.Event != "error" {
			t.Fatalf("last frame %+v, want event: error", last)
		}
		ev := decodeData(t, last)
		expectPath(t, ev, "type", "error")
		if sf.Envelope == EnvAnthropic {
			expectPath(t, ev, "error.type", "overloaded_error")
		}
		if str(ev, "error.message") == "" {
			t.Errorf("error event without message: %v", ev)
		}
		return
	}
	// Chat: a data chunk with "error" at the top level (spec 006).
	var errChunk map[string]any
	for _, f := range frames {
		if f.Data == "" || f.Data == "[DONE]" {
			continue
		}
		if ch := decodeData(t, f); ch["error"] != nil {
			errChunk = ch
		}
	}
	if errChunk == nil {
		t.Fatalf("no chunk with a top-level error: %s", w.Body())
	}
	if str(errChunk, "error.message") == "" {
		t.Errorf("error chunk without message: %v", errChunk)
	}
	if sf.StreamErrorFinishReason {
		expectPath(t, errChunk, "choices.0.finish_reason", "error")
	}
}
