package loadgen

import (
	"errors"
	"strings"
	"testing"
)

type ev struct{ name, data string }

func collect(t *testing.T, chunks []string) []ev {
	t.Helper()
	var got []ev
	p := sseParser{onEvent: func(n, d []byte) error {
		got = append(got, ev{string(n), string(d)})
		return nil
	}}
	for _, c := range chunks {
		if err := p.Feed([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSSEParser(t *testing.T) {
	stream := ": ping\r\nevent: a\r\ndata: 1\r\n\r\nid: 7\nretry: 10\ndata:2\ndata: 3\nignored\n\nevent: only-name\n\n\ndata: last"
	want := []ev{{"a", "1"}, {"", "2\n3"}, {"", "last"}}
	// Whole, and one byte at a time (lines split across chunks).
	bytewise := make([]string, len(stream))
	for i := range stream {
		bytewise[i] = stream[i : i+1]
	}
	for _, chunks := range [][]string{{stream}, bytewise} {
		got := collect(t, chunks)
		if len(got) != len(want) {
			t.Fatalf("got %q", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("event %d: %q want %q", i, got[i], want[i])
			}
		}
	}
	if got := collect(t, []string{"data: x\n"}); len(got) != 1 {
		t.Fatalf("pending event at close: %q", got)
	}
}

func TestSSEParserErrors(t *testing.T) {
	boom := errors.New("boom")
	p := sseParser{onEvent: func(_, _ []byte) error { return boom }}
	if err := p.Feed([]byte("data: a")); err != nil {
		t.Fatal(err)
	}
	if err := p.Feed([]byte("\n\n")); !errors.Is(err, boom) {
		t.Fatalf("feed: %v", err)
	}
	p = sseParser{onEvent: func(_, _ []byte) error { return boom }}
	_ = p.Feed([]byte("data: a\n"))
	if err := p.Close(); !errors.Is(err, boom) {
		t.Fatalf("close pending: %v", err)
	}
	p = sseParser{onEvent: func(_, _ []byte) error { return boom }}
	_ = p.Feed([]byte("data: a\n\r"))
	if err := p.Close(); !errors.Is(err, boom) {
		t.Fatalf("close partial line: %v", err)
	}
}

func TestSSEParserNoAllocPerLine(t *testing.T) {
	p := sseParser{onEvent: func(_, _ []byte) error { return nil }}
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"abc\"}}\n\n")
	_ = p.Feed(chunk)
	if n := testing.AllocsPerRun(100, func() { _ = p.Feed(chunk) }); n != 0 && !raceEnabled {
		t.Fatalf("%v allocations per event", n)
	}
}

// decode runs SSE events through a decoder like the client does.
func decode(t *testing.T, proto Protocol, capture bool, stream string) (*Result, error) {
	t.Helper()
	res := &Result{}
	var d decoder
	d.reset(proto, res, capture)
	p := sseParser{onEvent: func(n, data []byte) error {
		if err := d.event(n, data); err != nil {
			return err
		}
		if d.content {
			res.ContentEvents++
		}
		return nil
	}}
	err := p.Feed([]byte(stream))
	if err == nil {
		err = p.Close()
	}
	if err == nil {
		if !d.done {
			t.Fatalf("stream not done")
		}
		d.finish()
	}
	return res, err
}

func TestDecodeOpenAI(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"pensa"}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"ndo"}}]}`,
		`data: {"choices":[{"delta":{"content":"oi "}}]}`,
		`data: {"choices":[{"delta":{"content":""}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"completion_tokens_details":{"reasoning_tokens":5}}}`,
		`data: [DONE]`,
		`data: {not json, ignored after done}`,
	}, "\n\n") + "\n\n"
	res, err := decode(t, OpenAI, true, stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentEvents != 6 || res.InputTokens != 11 || res.OutputTokens != 22 || res.ReasoningTokens != 5 || res.TokensEstimated || res.FinishReason != "tool_calls" {
		t.Fatalf("%+v", res)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0] != (ToolCall{"c1", "f", `{"a":1}`}) || res.ToolCalls[1] != (ToolCall{"c2", "g", "{}"}) {
		t.Fatalf("tool calls %+v", res.ToolCalls)
	}
	if m := res.Message; m.Role != "assistant" || m.Content != "oi " || m.Reasoning != "pensando" || len(m.ToolCalls) != 2 {
		t.Fatalf("message %+v", m)
	}
	// OpenRouter error envelope in the middle of the stream.
	_, err = decode(t, OpenAI, false, `data: {"error":{"code":502,"message":"x"}}`+"\n\n")
	var se *streamErr
	if !errors.As(err, &se) || se.Error() != "stream_error/502" {
		t.Fatalf("%v", err)
	}
}

func TestDecodeAnthropic(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":4,\"cache_creation_input_tokens\":5,\"output_tokens\":1}}}",
		"event: ping\ndata: {\"type\": \"ping\"}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"SIG\"}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"oi\"}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\",\"input\":{}}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"x\"}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t2\",\"name\":\"g\",\"input\":{\"b\":2}}}",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":3}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":30}}",
		"event: message_stop\ndata: {}",
	}, "\n\n") + "\n\n"
	res, err := decode(t, Anthropic, true, stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens != 12 || res.OutputTokens != 30 || res.FinishReason != "tool_use" || res.ContentEvents != 7 {
		t.Fatalf("%+v", res)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0] != (ToolCall{"t1", "f", `{"a":1}`}) || res.ToolCalls[1] != (ToolCall{"t2", "g", `{"b":2}`}) {
		t.Fatalf("tool calls %+v", res.ToolCalls)
	}
	if m := res.Message; m.Content != "oi" || m.Reasoning != "hmm" || m.Signature != "SIG" {
		t.Fatalf("message %+v", m)
	}
	// message_delta with input tokens overrides message_start.
	res, err = decode(t, Anthropic, false, "data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	if err != nil || res.InputTokens != 7 || res.OutputTokens != 2 || res.Message != nil {
		t.Fatalf("%+v %v", res, err)
	}
	// Events without the needed objects are tolerated.
	_, err = decode(t, Anthropic, false, "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_start\"}\n\ndata: {\"type\":\"content_block_delta\"}\n\ndata: {\"type\":\"message_delta\"}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = decode(t, Anthropic, false, "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n")
	if err == nil || err.Error() != "stream_error/overloaded_error" {
		t.Fatalf("%v", err)
	}
	if _, err = decode(t, Anthropic, false, "data: nope\n\n"); !errors.Is(err, errParse) {
		t.Fatalf("%v", err)
	}
}

func TestDecodeWhole(t *testing.T) {
	res := &Result{}
	var d decoder
	d.reset(Anthropic, res, true)
	body := `{"type":"message","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"oi"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`
	if err := d.whole([]byte(body)); err != nil {
		t.Fatal(err)
	}
	d.finish()
	if res.InputTokens != 2 || res.OutputTokens != 3 || res.FinishReason != "tool_use" || !d.content ||
		res.ToolCalls[0] != (ToolCall{"t1", "f", `{"a":1}`}) || res.Message.Reasoning != "t" || res.Message.Signature != "s" || res.Message.Content != "oi" {
		t.Fatalf("%+v %+v", res, res.Message)
	}
	d.reset(OpenAI, res, false)
	if err := d.whole([]byte(`{"error":{}}`)); !errors.Is(err, errParse) {
		t.Fatalf("no choices: %v", err)
	}
	d.reset(OpenAI, res, false)
	if err := d.whole([]byte(`{"choices":[{"message":{"reasoning_content":"r"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)); err != nil || !d.content || res.FinishReason != "length" {
		t.Fatalf("%v %+v", err, res)
	}
}

// TestDecodeOpenAIReusesChunk: the decoder reuses one oaChunk for every
// event (task 378): a content chunk costs no allocation, and no field of
// one event leaks into the next.
func TestDecodeOpenAIReusesChunk(t *testing.T) {
	var d decoder
	res := &Result{}
	d.reset(OpenAI, res, false)
	chunk := []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"wwz-fast","choices":[{"index":0,"delta":{"content":" testes"},"logprobs":null,"finish_reason":null}]}`)
	if err := d.event(nil, chunk); err != nil || !d.content {
		t.Fatalf("content chunk: %v %v", err, d.content)
	}
	if n := testing.AllocsPerRun(100, func() { _ = d.event(nil, chunk) }); n != 0 && !raceEnabled {
		t.Fatalf("%v allocs per content chunk", n)
	}
	for _, ev := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`{"choices":[{"delta":{}}]}`,
		`{"choices":[{"delta":{}},{"delta":{}}]}`,
	} {
		if err := d.event(nil, []byte(ev)); err != nil {
			t.Fatal(err)
		}
	}
	for i, ch := range d.oa.Choices {
		if ch.Delta.Content != "" || ch.Delta.ToolCalls != nil || ch.FinishReason != "" {
			t.Fatalf("choice %d leaked from an earlier event: %+v", i, ch)
		}
	}
	if d.content || d.oa.Usage != nil || len(d.oa.Choices) != 2 {
		t.Fatalf("leaked into the next event: content %v, %+v", d.content, d.oa)
	}
	if res.FinishReason != "tool_calls" || res.OutputTokens != 2 || len(res.ToolCalls) != 1 {
		t.Fatalf("%+v", res)
	}
}

func benchDecode(b *testing.B, proto Protocol, ev []byte) {
	var d decoder
	d.reset(proto, &Result{}, false)
	b.ReportAllocs()
	for b.Loop() {
		if err := d.event(nil, ev); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeOpenAIDelta(b *testing.B) {
	benchDecode(b, OpenAI, []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"wwz-fast","choices":[{"index":0,"delta":{"content":" testes"},"logprobs":null,"finish_reason":null}]}`))
}

func BenchmarkDecodeAnthropicDelta(b *testing.B) {
	benchDecode(b, Anthropic, []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" testes"}}`))
}

// TestDecodeAnthropicReusesEvent: a delta costs no allocation (task 400) and
// no field of one event leaks into the next.
func TestDecodeAnthropicReusesEvent(t *testing.T) {
	var d decoder
	res := &Result{}
	d.reset(Anthropic, res, false)
	delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" testes"}}`)
	if err := d.event(nil, delta); err != nil || !d.content {
		t.Fatalf("delta: %v %v", err, d.content)
	}
	if n := testing.AllocsPerRun(100, func() { _ = d.event(nil, delta) }); n != 0 && !raceEnabled {
		t.Fatalf("%v allocs per delta", n)
	}
	stop := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	if err := d.event(nil, stop); err != nil || res.FinishReason != "end_turn" || res.OutputTokens != 7 {
		t.Fatalf("%v %+v", err, res)
	}
	// A message_delta without stop_reason must not inherit the previous one.
	res.FinishReason = ""
	if err := d.event(nil, []byte(`{"type":"message_delta","usage":{"output_tokens":8}}`)); err != nil || res.FinishReason != "" || res.OutputTokens != 8 {
		t.Fatalf("%v %+v", err, res)
	}
	if err := d.event(nil, []byte(`{"type":"content_block_delta","index":0}`)); err != nil || d.content {
		t.Fatalf("delta without delta object: %v %v", err, d.content)
	}
}
