package generate

import (
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// claudeFamily is the prompt template of a Claude generation: the cost of
// the conversation frame, of the tools system prompt and of the tool-use
// history besides the text the tokenizer counts (spec 007, tasks 259 and
// 264). Derived from /v1/messages/count_tokens on 31/10/2026, one model of
// each generation measured in full (testdata/real/anthropic/template/ and
// tools*/), the others classified by a smaller set; every constant is a
// real token count, exact when the text counts are.
type claudeFamily struct {
	// msgHeader opens each message (user or assistant) and is shared by
	// consecutive messages of the same role, which join with msgJoin; a
	// final assistant message (prefill) has none.
	msgHeader, msgJoin int
	// reply opens the answer (the request's fixed cost).
	reply int
	// sysHeader opens the system prompt (the request's or the tools').
	sysHeader int
	// asciiTools: the tools text escapes non-ASCII (Claude 4.5 and before).
	asciiTools bool
	// toolsTextPct scales the simulated count of the tools text (0 = 100):
	// the tokenizer of Claude 4.7 on splits that JSON into ~26% more tokens
	// than the 4.5 one countToolsText is calibrated on.
	toolsTextPct int
	// textPct scales the simulated count of every other text (0 = 100):
	// Claude 4.7 on count ~32% more tokens in the same text than Claude 4.5,
	// whose counts Simulated approximates (task 301, 12 texts of
	// testdata/real/anthropic/tokenizer/; identical from 4.7 to 5.5).
	textPct int
	// assistantFirst: a conversation that opens with an assistant message
	// costs this much more (task 301; with or without system, any turns
	// after it).
	assistantFirst int
	// sysRun and sysRunTurn: a run of role "system" messages in messages
	// (the form of Claude Code, each message with text costing sysHeader +
	// its text in System, without a turn of its own; task 451) costs sysRun
	// more, and sysRunTurn more when an assistant turn with header follows
	// it (not the prefill nor the reply). Recorded with count_tokens on
	// 2026-11-03 (claude-opus-4-8, claude-fable-5 and the 5.5 models; the
	// 4.5 to 4.7 ones refuse the role).
	sysRun, sysRunTurn int

	// Tools system prompt: fixed text by tool_choice (named adds
	// tokens(name)); noParallelForced with any/tool and
	// disable_parallel_tool_use; sysJoin when the request has a system
	// prompt too.
	toolsAuto, toolsSerial, toolsAny, toolsNamed, noParallelForced, sysJoin int

	// Tool-use history: callPair is the first call of an assistant turn and
	// its result (both turn headers included), callExtra each further call
	// and result in the same turns and callMulti a turn with two or more
	// calls; besides them, each call costs tokens(name) and, per input key,
	// callParam + tokens(key) + tokens(value), and each result its text
	// (resultEmpty when empty) plus resultError when is_error.
	callPair, callExtra, callMulti, callParam, resultEmpty, resultError int
	// callFirst: the first call of the request.
	callFirst int
	// textBeforeCalls and textAfterResults: an assistant text block with
	// the calls and a user text block with the results cost these besides
	// their text; afterResultsText is the first text after results (in the
	// results turn or in a later one).
	textBeforeCalls, textAfterResults, afterResultsText int
}

var (
	// claude45: Claude 4.5 and earlier (claude-haiku-4-5, sonnet-4-5 and
	// opus-4-5 identical), the default.
	claude45 = claudeFamily{
		msgHeader: 3, msgJoin: 1, reply: 4, asciiTools: true, assistantFirst: 12,
		toolsAuto: 489, toolsSerial: 281, toolsAny: 581, toolsNamed: 583, noParallelForced: 1, sysJoin: 1,
		callPair: 45, callFirst: 1, callExtra: 39, callMulti: 24, callParam: 15, resultEmpty: 14, resultError: 8,
		textAfterResults: 5,
	}
	// claude46: Claude 4.6 (sonnet and opus), 4.5 with a system header and
	// the tools text in UTF-8.
	claude46 = func() claudeFamily {
		f := claude45
		f.sysHeader = 1
		f.asciiTools = false
		return f
	}()
	// claude47: claude-opus-4-7 (classified by the smaller set; any/tool
	// with disable_parallel_tool_use and a second call turn not conferred,
	// taken from claude5).
	claude47 = claudeFamily{
		msgHeader: 5, msgJoin: 1, reply: 6, sysHeader: 1, toolsTextPct: 126, textPct: 132, assistantFirst: 24,
		toolsAuto: 665, toolsSerial: 394, toolsAny: 794, toolsNamed: 796, noParallelForced: 3, sysJoin: 1,
		callPair: 52, callExtra: 43, callMulti: 24, callParam: 19, resultEmpty: 18, resultError: 8,
		textAfterResults: 9,
	}
	// claude5: claude-opus-4-8 and the Claude 5 models (claude-fable-5
	// measured in full; opus-5 and sonnet-5 differ in the tools prompt).
	// disable_parallel_tool_use does not change the auto prompt.
	claude5 = claudeFamily{
		msgHeader: 2, msgJoin: 1, reply: 4, sysHeader: 1, toolsTextPct: 126, textPct: 132, assistantFirst: 21,
		toolsAuto: 280, toolsSerial: 280, toolsAny: 400, toolsNamed: 402, noParallelForced: 3, sysJoin: 1,
		callPair: 28, callExtra: 43, callMulti: 24, callParam: 19, resultEmpty: 18, resultError: 6,
		textBeforeCalls: 1, textAfterResults: 2, sysRun: 1,
	}
	claude5Opus   = withTools(claude5, 276)
	claude5Sonnet = withTools(claude5, 344)
	// claude55: claude-haiku-5-5, sonnet-5-5, opus-5-5 and claude-fable-5-1
	// (identical; any and tool measured on haiku-5-5, the others refuse
	// them).
	claude55 = func() claudeFamily {
		f := withTools(claude5, 276)
		f.reply = 6
		f.afterResultsText = 2
		f.sysRun, f.sysRunTurn = 0, 1
		return f
	}()
)

// withTools shifts the tools prompt of f so that auto costs n (serial, any
// and tool keep their distance to it).
func withTools(f claudeFamily, n int) claudeFamily {
	d := n - f.toolsAuto
	f.toolsAuto += d
	f.toolsSerial += d
	f.toolsAny += d
	f.toolsNamed += d
	return f
}

// claudeFamilyOf picks the template by the Claude version in the model id
// ("claude-opus-5-5", "claude-haiku-4-5-20251001"); unknown ids use
// claude45.
func claudeFamilyOf(model string) *claudeFamily {
	line, major, minor := claudeVersion(model)
	switch {
	case major > 5 || major == 5 && minor >= 1:
		return &claude55
	case major == 5 && line == "opus":
		return &claude5Opus
	case major == 5 && line == "sonnet":
		return &claude5Sonnet
	case major == 5, major == 4 && minor >= 8:
		return &claude5
	case major == 4 && minor == 7:
		return &claude47
	case major == 4 && minor == 6:
		return &claude46
	}
	return &claude45
}

// claudeVersion reads "claude-<line>-<major>[-<minor>][-<date>]" without
// allocating; other ids (claude-3-5-sonnet-…) report major 0.
func claudeVersion(model string) (line string, major, minor int) {
	rest, ok := strings.CutPrefix(strings.ToLower(model), "claude-")
	if !ok {
		return "", 0, 0
	}
	line, rest, _ = strings.Cut(rest, "-")
	if line == "" || line[0] >= '0' && line[0] <= '9' {
		return "", 0, 0
	}
	num := func() int {
		var part string
		part, rest, _ = strings.Cut(rest, "-")
		if len(part) == 0 || len(part) > 2 { // missing, or a date
			return -1
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return -1
		}
		return n
	}
	if major = num(); major < 0 {
		return "", 0, 0
	}
	return line, major, max(0, num())
}

// claudeTurn is a run of consecutive messages of one role, which Claude
// reads as one turn.
type claudeTurn struct {
	role    string
	texts   int // text messages or segments
	text    int // their tokens
	calls   []core.ToolCall
	results []core.ToolResult
	images  int
	// afterSys: the turn opens right after a run of system messages.
	afterSys bool
}

// countAnthropicPrompt is CountPrompt for a request Claude counts: the
// system prompt (text, plus the system header), the tools prompt, each turn
// with its header (consecutive messages of a role are one turn, their texts
// joined; a final assistant turn has none), the tool-use history as the
// family renders it and the reply. A role "system" message counts in
// System (sysHeader + its text, plus the run costs) and opens no turn; one
// without text (the directive-only form) costs nothing.
func countAnthropicPrompt(tk Tokenizer, req *core.Request, imageTokens int) PromptBreakdown {
	f := claudeFamilyOf(req.Model)
	var b PromptBreakdown
	b.ToolSchemas = anthropicToolTokens(tk, req)
	if _, ok := tk.(Simulated); ok && f.textPct > 0 {
		tk = scaled{pct: f.textPct}
	}
	if req.System != "" {
		b.System = f.sysHeader + tk.Count(req.System)
	}
	var turns []claudeTurn
	inRun := false
	for _, m := range req.Messages {
		if m.Role == "system" {
			if m.Content != "" {
				if !inRun {
					b.System += f.sysRun
				}
				b.System += f.sysHeader + tk.Count(m.Content)
				inRun = true
			}
			continue
		}
		if len(turns) == 0 || turns[len(turns)-1].role != m.Role {
			turns = append(turns, claudeTurn{role: m.Role, afterSys: inRun})
		}
		inRun = false
		t := &turns[len(turns)-1]
		if m.Content != "" || len(m.ToolCalls)+len(m.ToolResults) == 0 {
			t.texts++
			t.text += tk.Count(m.Content)
		}
		t.calls = append(t.calls, m.ToolCalls...)
		t.results = append(t.results, m.ToolResults...)
		t.images += m.ImageParts
	}
	if len(turns) > 0 && turns[0].role == "assistant" {
		b.User += f.assistantFirst
	}
	firstCall, afterResults := true, false
	for i, t := range turns {
		if t.afterSys && t.role == "assistant" && i < len(turns)-1 {
			b.System += f.sysRunTurn
		}
		text := 0
		if t.texts > 0 {
			text = t.text + (t.texts-1)*f.msgJoin
			if afterResults {
				text += f.afterResultsText
				afterResults = false
			}
		}
		switch {
		case len(t.calls) > 0:
			b.Assistant += text
			b.ToolCalls += anthropicCallTokens(tk, f, t.calls)
			if firstCall {
				b.ToolCalls += f.callFirst
				firstCall = false
			}
			if t.texts > 0 {
				b.ToolCalls += f.textBeforeCalls
			}
			afterResults = false
		case len(t.results) > 0:
			for _, tr := range t.results {
				if tr.Content == "" {
					b.ToolResults += f.resultEmpty
				} else {
					b.ToolResults += tk.Count(tr.Content)
				}
				if tr.IsError {
					b.ToolResults += f.resultError
				}
			}
			b.User += text
			if t.texts > 0 {
				b.ToolResults += f.textAfterResults + f.afterResultsText
			} else {
				afterResults = true
			}
		default:
			if t.role != "assistant" || i < len(turns)-1 {
				text += f.msgHeader
			}
			if t.role == "assistant" {
				b.Assistant += text
			} else {
				b.User += text
			}
		}
		b.Images += t.images * imageTokens
	}
	b.Assistant += f.reply
	b.Total = b.System + b.ToolSchemas + b.User + b.Assistant + b.ToolCalls + b.ToolResults + b.Images
	return b
}

// anthropicCallTokens is the cost of the calls of an assistant turn and
// their results' frame: the pair, the further calls, the names and the
// input, one entry per key (the key and the value: a string as is, any
// other JSON as json.dumps writes it).
func anthropicCallTokens(tk Tokenizer, f *claudeFamily, calls []core.ToolCall) int {
	n := f.callPair + f.callExtra*(len(calls)-1)
	if len(calls) > 1 {
		n += f.callMulti
	}
	for _, c := range calls {
		n += tk.Count(c.Name)
		in := parseJSON([]byte(c.Arguments))
		for i, k := range in.keys {
			n += f.callParam + tk.Count(k)
			if v := in.vals[i]; v.kind == '"' {
				n += tk.Count(v.str)
			} else {
				var vb strings.Builder
				writePyJSON(&vb, v, f.asciiTools)
				n += tk.Count(vb.String())
			}
		}
	}
	return n
}

// scaled is Simulated with its counts multiplied by pct/100 (rounded), the
// tokenizer of a Claude generation that splits text finer than the one
// Simulated approximates.
type scaled struct{ pct int }

func (s scaled) Count(text string) int { return (Simulated{}.Count(text)*s.pct + 50) / 100 }

func (scaled) Split(text string) []string { return Simulated{}.Split(text) }
