package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// chunkText splits text into chunks of roughly n tokens (or n runes when
// runes > 0), keeping whitespace attached to the preceding token.
func chunkText(tk generate.Tokenizer, text string, tokens, runes int) []string {
	if text == "" {
		return nil
	}
	if runes > 0 && utf8.ValidString(text) {
		// Chunks are substrings of text (task 270); invalid UTF-8 goes
		// through []rune below, which writes U+FFFD for each bad byte.
		out := make([]string, 0, utf8.RuneCountInString(text)/runes+1)
		start, n := 0, 0
		for i := range text {
			if n == runes {
				out = append(out, text[start:i])
				start, n = i, 0
			}
			n++
		}
		return append(out, text[start:])
	}
	if runes > 0 {
		rs := []rune(text)
		out := make([]string, 0, len(rs)/runes+1)
		for i := 0; i < len(rs); i += runes {
			j := i + runes
			if j > len(rs) {
				j = len(rs)
			}
			out = append(out, string(rs[i:j]))
		}
		return out
	}
	if tokens <= 0 {
		tokens = 1
	}
	parts := tk.Split(text)
	out := make([]string, 0, len(parts)/tokens+1)
	if contiguous(text, parts) {
		// The tokens are text cut in order: every chunk is a substring of
		// text, with no copy (task 270).
		start, end := 0, 0
		for i, p := range parts {
			end += len(p)
			if (i+1)%tokens == 0 {
				out = append(out, text[start:end])
				start = end
			}
		}
		if end > start {
			out = append(out, text[start:end])
		}
		return out
	}
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if (i+1)%tokens == 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// contiguous reports whether parts, concatenated, are exactly text.
func contiguous(text string, parts []string) bool {
	off := 0
	for _, p := range parts {
		if len(text)-off < len(p) || text[off:off+len(p)] != p {
			return false
		}
		off += len(p)
	}
	return off == len(text)
}

func chunkBytes(s string, n int) []string {
	if n <= 0 {
		n = 16
	}
	out := make([]string, 0, len(s)/n+1)
	for i := 0; i < len(s); i += n {
		j := i + n
		if j > len(s) {
			j = len(s)
		}
		out = append(out, s[i:j])
	}
	return out
}

// streamState tracks the progress of one stream for faults and stalls.
type streamState struct {
	chunks   int
	total    int
	tokens   int
	stalled  bool
	slowTail bool
	aborted  bool
	// cutLength is set by the finish_length fault: the stream stops early
	// and still finishes normally, with the length reason.
	cutLength bool
	// sent is the text and the tool calls written so far, which a
	// finish_length cut records as the reply (Responses API, ADR 0070).
	sent      []string
	sentTools []core.ToolCall
	done      <-chan struct{}
	lastWrite time.Time
	pingEvery time.Duration
	sw        core.StreamWriter
	plan      interface {
		Wait(time.Duration, <-chan struct{}) bool
	}
}

// wait sleeps d, emitting pings when the gap exceeds the ping interval.
func (st *streamState) wait(d time.Duration) bool {
	if st.slowTail {
		d *= 10
	}
	if st.pingEvery <= 0 || d <= st.pingEvery {
		return st.plan.Wait(d, st.done)
	}
	for d > 0 {
		step := st.pingEvery
		if step > d {
			step = d
		}
		if !st.plan.Wait(step, st.done) {
			return false
		}
		d -= step
		if d > 0 {
			_ = st.sw.Ping()
		}
	}
	return true
}

// stream delivers r.resp over the driver's stream writer with the latency plan.
func (e *Engine) stream(ctx context.Context, r *request, w core.ResponseWriter) {
	drv, req, resp, plan := r.in.Driver, r.req, r.resp, r.plan
	sw := drv.StreamWriter(w, req, &r.rl)
	st := &streamState{done: ctx.Done(), pingEvery: r.cfg.Server.PingInterval, sw: sw, plan: plan}
	// Every wait below is on ctx.Done(): the cancellation fires the plan's
	// timer, so a wait parks on the timer alone (task 341).
	defer plan.BindCancel(ctx)()
	r.status = 200
	timeline := r.scn != nil && len(r.scn.Timeline) > 0

	// Pieces to send.
	chunkTok := plan.ChunkTokens
	argBytes := r.gen.ToolArgsChunkBytes
	type toolPart struct {
		idx  int
		tc   core.ToolCall
		args []string
	}
	tools := make([]toolPart, 0, len(resp.ToolCalls))
	limit := len(resp.Reasoning) + len(resp.Content) + 1
	for i, tc := range resp.ToolCalls {
		tools = append(tools, toolPart{idx: i, tc: tc})
		limit += len(tc.Arguments)
	}
	var reasoning, text []string
	rechunk := func(factor int) {
		reasoning = chunkText(r.tk, resp.Reasoning, chunkTok*factor, 0)
		// Subword splitting only at factor 1, so the frame cap (I22) can
		// still merge; dribble regroups into its exact chunk count (I5).
		text = plan.Shape(chunkText(r.tk, resp.Content, chunkTok*factor, plan.ChunkRunes*factor), factor == 1)
		for i := range tools {
			tools[i].args = chunkBytes(tools[i].tc.Arguments, argBytes*factor)
			if r.rs != nil {
				// The Responses API streams arguments token by token
				// (function_call_stream), whatever tool_args_chunk_bytes says.
				tools[i].args = chunkText(r.tk, tools[i].tc.Arguments, chunkTok*factor, 0)
			}
		}
	}
	count := func() int {
		n := len(reasoning) + len(text)
		for _, t := range tools {
			n += 1 + len(t.args)
		}
		return n
	}
	rechunk(1)
	st.total = count()
	if timeline {
		st.total = timelineFrames(r.scn.Timeline, func(i int) int {
			if i < len(tools) {
				return 1 + len(tools[i].args)
			}
			return 0
		})
	} else if max := r.cfg.Server.MaxChunksPerStream; max > 0 && st.total > max {
		// Cap the number of frames per stream (I22) by merging text,
		// reasoning and tool-argument chunks; only the start frame of each
		// tool call cannot be merged.
		for factor := (st.total + max - 1) / max; ; factor *= 2 {
			rechunk(factor)
			st.total = count()
			if st.total <= max || factor >= limit {
				break
			}
		}
	}
	// Mid-stream fault position.
	mid := r.chaos.Mid
	faultAt := -1
	if mid != nil {
		faultAt = mid.AfterChunks
		if faultAt <= 0 {
			faultAt = int(float64(st.total) * mid.AfterRatio)
			if faultAt <= 0 {
				faultAt = 1
			}
		}
	}
	includeUsage := req.IncludeUsage
	if mid != nil && mid.Kind == core.StreamUsageMiss {
		includeUsage = false
	}

	// fault runs the mid-stream fault; returns true when the stream must stop.
	fault := func() bool {
		e.M.ErrorsInjected.Inc(drv.Name(), string(mid.Kind), "mid")
		switch mid.Kind {
		case core.StreamErrorEvent:
			er := mid.Error
			_ = sw.Error(er)
			_ = sw.Close()
			r.finishErrKind = er.Kind
			return true
		case core.StreamTruncated:
			_ = w.Flush()
			r.finishErrKind = "stream_truncated"
			st.aborted = true
			return true
		case core.StreamReset:
			r.finishErrKind = "stream_reset"
			st.aborted = true
			if !w.Fault(core.TransportReset) {
				_ = w.Flush()
			}
			return true
		case core.StreamHang:
			d := mid.HangFor
			if !plan.Wait(d, st.done) {
				st.aborted = true
				return true
			}
			if !mid.Resume {
				r.finishErrKind = "stream_hang"
				st.aborted = true
				return true
			}
		case core.StreamMalformed:
			_ = sw.Raw(malformedFrame(r.decl))
		case core.StreamDupFrames:
			if lf := sw.LastFrame(); len(lf) > 0 {
				_ = sw.Raw(lf)
			}
		case core.StreamSlowTail:
			st.slowTail = true
		case chaos.StreamFinishLength:
			st.cutLength = true
			return true
		}
		return false
	}
	// emit wraps a frame write with fault/stall/cut checks.
	emit := func(delay time.Duration, tokens int, write func() error) bool {
		if !st.wait(delay) {
			st.aborted = true
			return false
		}
		if err := write(); err != nil {
			st.aborted = true
			return false
		}
		st.chunks++
		st.tokens += tokens
		if r.stall != nil && !st.stalled && st.tokens >= r.stall.AfterTokens {
			st.stalled = true
			if !plan.Wait(stallDuration(r.stall), st.done) {
				st.aborted = true
				return false
			}
		}
		if r.cutAfter > 0 && st.tokens >= r.cutAfter {
			r.finishErrKind = "cut_after_tokens"
			st.aborted = true
			return false
		}
		if mid != nil && st.chunks == faultAt {
			if fault() {
				return false
			}
		}
		return true
	}
	// endCut closes the tool call a finish_length cut stopped (its
	// arguments stay incomplete, as on a real max_tokens cut).
	endCut := func(idx int) bool {
		if st.cutLength {
			_ = sw.ToolCallEnd(idx)
		}
		return false
	}
	// emitTool sends one tool call: start, argument deltas, end.
	emitTool := func(t toolPart) bool {
		if !emit(plan.ToolCallDelay, 1, func() error { return st.sentCall(sw.ToolCallStart(t.idx, t.tc.ID, t.tc.Name), t.tc) }) {
			return endCut(t.idx)
		}
		for _, a := range t.args {
			a := a
			if !emit(plan.ToolArgsITL, 1, func() error { return st.sentArgs(sw.ToolArgsDelta(t.idx, a), a) }) {
				return endCut(t.idx)
			}
		}
		_ = sw.ToolCallEnd(t.idx)
		return true
	}

	// Queue + TTFT, then the stream opens.
	if !st.wait(plan.Queue + plan.TTFT) {
		return
	}
	// The first byte: the prompt-cache entries become valid now.
	e.promptCacheCommit(ctx, r)
	if err := sw.Start(resp); err != nil {
		return
	}
	_ = w.Flush()
	ok := true
	if timeline {
		// I14: replay the step timeline 1:1 through the stream writer.
		toolIdx := 0
	events:
		for _, ev := range r.scn.Timeline {
			switch ev.Type {
			case "text":
				c := renderTemplate(ev.Text, r)
				ok = emit(plan.ITL, r.tk.Count(c), func() error { return st.sentText(sw.TextDelta(c), c) })
			case "reasoning":
				c := renderTemplate(ev.Text, r)
				ok = emit(plan.ReasoningITL, r.tk.Count(c), func() error { return sw.ReasoningDelta(c) })
			case "tool_call":
				if toolIdx < len(tools) {
					ok = emitTool(tools[toolIdx])
				}
				toolIdx++
			case "ping":
				_ = sw.Ping()
				_ = w.Flush()
			case "delay":
				if !st.wait(time.Duration(ev.MS) * time.Millisecond) {
					st.aborted = true
					ok = false
				}
			case "error":
				er := timelineError(ev)
				e.M.ErrorsInjected.Inc(drv.Name(), string(er.Kind), "mid")
				_ = sw.Error(er)
				_ = sw.Close()
				_ = w.Flush()
				r.finishErrKind = er.Kind
				ok = false
			case "finish":
				break events
			}
			// "usage" writes no frame of its own: usage goes out with finish.
			if !ok {
				break
			}
		}
	} else {
		for _, c := range reasoning {
			if ok = emit(plan.ReasoningITL, chunkTok, func() error { return sw.ReasoningDelta(c) }); !ok {
				break
			}
		}
		for i := 0; ok && i < len(text); i++ {
			c := text[i]
			if ok = emit(plan.ChunkDelay(i, len(text), plan.ITL), chunkTok, func() error { return st.sentText(sw.TextDelta(c), c) }); !ok {
				break
			}
		}
		for _, t := range tools {
			if !ok {
				break
			}
			ok = emitTool(t)
		}
	}
	if st.cutLength {
		// finish_length: what was sent is the whole reply, ended by length.
		cutUsage(r, st.tokens)
		r.delivered = &core.Response{ID: resp.ID, Content: strings.Join(st.sent, ""), ToolCalls: st.sentTools}
		ok = true
	}
	if !ok {
		if r.finishErrKind != "" && r.finishErrKind != "client_gone" {
			// The client consumed (part of) a stream: a re-send right after
			// counts in the resilience report (I15).
			e.Resilience.Observe(r.testID, req, 200, nil)
			// A stream cut by a fault, a timeline error or cut_after_tokens
			// still goes to /admin/requests, with mid_stream when a chaos
			// fault cut it (task 105).
			e.record(r)
		}
		if !st.aborted {
			return
		}
		// Abrupt end: close without Finish so the client sees a truncated stream.
		if r.finishErrKind == "" {
			r.finishErrKind = "client_gone"
		}
		return
	}
	if !st.wait(plan.FinishDelay) {
		return
	}
	// The record of a Responses stream is written when it ends (completed
	// or incomplete), never after an abort (ADR 0070, not conferred).
	e.responsesPut(ctx, r)
	_ = sw.Finish(resp.FinishReason, resp.Usage, includeUsage)
	_ = sw.Close()
	_ = w.Flush()
	e.count(drv, r.modelLabel(), r.endpoint(), 200)
	e.M.E2E.Observe(time.Since(r.t0).Seconds(), drv.Name(), r.modelLabel())
	e.record(r)
	e.Resilience.Observe(r.testID, req, 200, nil)
	e.logReq(r, 200, nil)
}

// sentText notes text c as written when err is nil.
func (st *streamState) sentText(err error, c string) error {
	if err == nil {
		st.sent = append(st.sent, c)
	}
	return err
}

// sentCall notes the start of tool call tc (no arguments yet).
func (st *streamState) sentCall(err error, tc core.ToolCall) error {
	if err == nil {
		tc.Arguments = ""
		st.sentTools = append(st.sentTools, tc)
	}
	return err
}

// sentArgs adds argument text a to the last tool call written.
func (st *streamState) sentArgs(err error, a string) error {
	if err == nil {
		st.sentTools[len(st.sentTools)-1].Arguments += a
	}
	return err
}

// cutUsage ends r.resp with the length reason and the usage of the tokens
// sent before a finish_length cut.
func cutUsage(r *request, sent int) {
	resp := r.resp
	resp.FinishReason = core.FinishLength
	if sent >= resp.Usage.CompletionTokens {
		return
	}
	resp.Usage.CompletionTokens = sent
	resp.Usage.ReasoningTokens = min(resp.Usage.ReasoningTokens, sent)
	setCost(r.model.Pricing, &resp.Usage)
}

// stallDuration is the pause of a stall: duration (Go syntax) wins over ms.
func stallDuration(s *scenario.Stall) time.Duration {
	d := time.Duration(s.MS) * time.Millisecond
	if s.Duration != "" {
		if pd, err := time.ParseDuration(s.Duration); err == nil {
			d = pd
		}
	}
	return d
}

// chatMalformedFrame is a chat completion chunk whose JSON is cut.
var chatMalformedFrame = []byte("data: {\"id\":\"chatcmpl-wwz\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\n\n")

// malformedFrame is one SSE frame whose JSON is cut, in the shape the
// driver declares (a chat completion chunk by default).
func malformedFrame(decl driver.EngineDecl) []byte {
	if decl.MalformedFrame != nil {
		return decl.MalformedFrame
	}
	return chatMalformedFrame
}

// timelineFrames counts the frames a timeline writes up to its finish event.
func timelineFrames(tl []scenario.TimelineEvent, toolFrames func(int) int) int {
	n, tool := 0, 0
	for _, ev := range tl {
		switch ev.Type {
		case "text", "reasoning":
			n++
		case "tool_call":
			n += toolFrames(tool)
			tool++
		case "finish":
			return n
		}
	}
	return n
}

// timelineError is the error a timeline "error" event emits.
func timelineError(ev scenario.TimelineEvent) *core.Error {
	er := &core.Error{Kind: core.ErrorKind(ev.Error)}
	if er.Kind == "" {
		er.Kind = core.ErrServerError
	}
	er.Message = ev.Text
	if er.Message == "" {
		er.Message = "scenario-injected " + string(er.Kind)
	}
	return er
}

// foldTimeline turns a step timeline (I14) into the step's content,
// reasoning, tool calls and finish reason, so the response plan (usage,
// non-streaming body) matches what the stream replays. Events after the
// first finish are ignored.
func (e *Engine) foldTimeline(r *request) {
	if r.scn == nil || len(r.scn.Timeline) == 0 {
		return
	}
	var content, reasoning strings.Builder
	var calls []core.ToolCall
	finish := core.FinishReason("")
loop:
	for _, ev := range r.scn.Timeline {
		switch ev.Type {
		case "text":
			content.WriteString(renderTemplate(ev.Text, r))
		case "reasoning":
			reasoning.WriteString(renderTemplate(ev.Text, r))
		case "tool_call":
			args := "{}"
			if ev.Args != nil {
				if b, err := json.Marshal(ev.Args); err == nil {
					args = string(b)
				}
			}
			calls = append(calls, core.ToolCall{ID: r.toolCallID(len(calls)), Name: renderTemplate(ev.Name, r), Arguments: args})
		case "finish":
			finish = core.FinishReason(ev.Reason)
			break loop
		}
	}
	r.scn.Content, r.scn.ContentTokens = content.String(), 0
	r.scn.Reasoning, r.scn.ReasoningTokens = reasoning.String(), 0
	r.scn.ToolCalls = calls
	switch {
	case finish != "":
		r.scn.FinishReason = finish
	case len(calls) > 0:
		r.scn.FinishReason = core.FinishToolCalls
	default:
		r.scn.FinishReason = core.FinishStop
	}
}

// timelineErrorEvent returns the first error event before a finish, if any.
func timelineErrorEvent(r *request) *core.Error {
	if r.scn == nil {
		return nil
	}
	for _, ev := range r.scn.Timeline {
		switch ev.Type {
		case "error":
			return timelineError(ev)
		case "finish":
			return nil
		}
	}
	return nil
}

// nonStream holds the response for the simulated duration, then writes it.
func (e *Engine) nonStream(ctx context.Context, r *request, w core.ResponseWriter) {
	drv, req, resp, plan := r.in.Driver, r.req, r.resp, r.plan
	argBytes := 0
	for _, tc := range resp.ToolCalls {
		argBytes += len(tc.Arguments)
	}
	total := plan.Total(resp.Usage.ReasoningTokens, resp.Usage.CompletionTokens-resp.Usage.ReasoningTokens, argBytes, len(resp.ToolCalls))
	if r.stall != nil {
		total += stallDuration(r.stall)
	}
	if !plan.Wait(total, ctx.Done()) {
		r.status = 499
		r.finishErrKind = "client_gone"
		return
	}
	// A timeline error event (I14) is the reply of a non-streaming request.
	if er := timelineErrorEvent(r); er != nil {
		e.M.ErrorsInjected.Inc(drv.Name(), string(er.Kind), "mid")
		r.finishErrKind = er.Kind
		e.failNoFinish(r, w, er)
		return
	}
	// A pre-planned mid-stream fault on a non-streaming request degrades to
	// the closest transport equivalent.
	if mid := r.chaos.Mid; mid != nil {
		switch mid.Kind {
		case core.StreamTruncated, core.StreamReset:
			e.M.ErrorsInjected.Inc(drv.Name(), string(mid.Kind), "mid")
			r.status = 0
			r.finishErrKind = core.ErrorKind(mid.Kind)
			if w.Fault(core.TransportReset) {
				e.record(r)
				return
			}
		case core.StreamErrorEvent:
			er := mid.Error
			e.M.ErrorsInjected.Inc(drv.Name(), string(mid.Kind), "mid")
			r.finishErrKind = er.Kind
			e.failNoFinish(r, w, er)
			return
		}
	}
	r.status = 200
	e.promptCacheCommit(ctx, r) // the first byte of the body
	e.responsesPut(ctx, r)
	if err := drv.WriteResponse(w, req, resp, &r.rl); err != nil {
		r.finishErrKind = "write_error"
		return
	}
	e.count(drv, r.modelLabel(), r.endpoint(), 200)
	e.M.E2E.Observe(time.Since(r.t0).Seconds(), drv.Name(), r.modelLabel())
	e.record(r)
	e.Resilience.Observe(r.testID, req, 200, nil)
	e.logReq(r, 200, nil)
}
