package loadgen

import (
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// DefaultMaxTurns is the --max-turns of a scenario without step_limit.
const DefaultMaxTurns = 12

// ConversationSystem is the fixed system message of conversation mode.
const ConversationSystem = "You are an agent driven by wwz-load. Use the tools offered to complete the task."

// Reasons of a conversation that did not complete (besides the error class
// of the request that failed it).
const (
	// ReasonMaxTurns: --max-turns reached with tool calls still pending.
	ReasonMaxTurns = "max_turns"
	// ReasonNoToolCalls: finish reason asks for tools but none came.
	ReasonNoToolCalls = "no_tool_calls"
	// ReasonFinishPrefix + finish reason: the model stopped without tool
	// calls for a reason that is not final (content_filter, refusal, …).
	ReasonFinishPrefix = "finish_reason:"
	// ReasonIncomplete: the final answer fell on a step that declares tool
	// calls, or on a step the scenario does not have (ADR 0041).
	ReasonIncomplete = "incomplete"
)

// LoadScenario parses a scenario file (spec 011) and returns it with the
// default --max-turns: its step_limit, else DefaultMaxTurns.
func LoadScenario(data []byte) (*scenario.Scenario, int, error) {
	s, err := scenario.ParseYAML(data)
	if err != nil {
		return nil, 0, err
	}
	var raw struct {
		StepLimit int `yaml:"step_limit"`
	}
	_ = yaml.Unmarshal(data, &raw) // already parsed strictly above
	turns := DefaultMaxTurns
	if raw.StepLimit > 0 {
		turns = raw.StepLimit
	}
	return s, turns, nil
}

// ConversationPlan describes the conversation unit (spec 015, "Modo
// conversa"): the client plays the agent of Scenario.
type ConversationPlan struct {
	Scenario *scenario.Scenario
	// Template is copied for every turn (URL, key, model, stream, max
	// tokens, headers); its messages and tools are replaced.
	Template Request
	// Prompt is the user message when the scenario has no
	// match.user_message_matches.
	Prompt string
	// MaxTurns ≤ 0: DefaultMaxTurns.
	MaxTurns int
	// Seed ≠ 0 sends x-wwz-seed: Seed + SeedStep·i on every turn of
	// conversation i, so the scenario variables stay the same along the
	// conversation.
	Seed int64
	// SeedStep is the stride of x-wwz-seed between conversations (≤ 0
	// counts as 1; N in a distributed run of N workers, ADR 0033).
	SeedStep int64
	// DrawSeed seeds the invented tool results.
	DrawSeed int64
	// ToolResultErrorRate is the percent (0–100) of tool results sent as
	// errors.
	ToolResultErrorRate float64
	// Interrupt, when closed, stops the conversations before their next
	// turn; they are counted as interrupted. Without it a conversation
	// started before the end of the plan runs to its end.
	Interrupt <-chan struct{}
}

// ConvStats are the conversation measures of a step. Mergeable; the JSON
// form travels from the workers to the controller of --instances (task
// 045), with the E2E histogram serialized as in ADR 0024.
type ConvStats struct {
	Started        int64            `json:"started"`
	Completed      int64            `json:"completed"`
	Failed         int64            `json:"failed"`
	MaxTurns       int64            `json:"max_turns"`
	Interrupted    int64            `json:"interrupted"`
	FailedByReason map[string]int64 `json:"failed_by_reason"`
	// Turns[n] counts the ended (not interrupted) conversations with n
	// turns.
	Turns     []int64 `json:"turns"`
	ToolCalls int64   `json:"tool_calls"`
	// OrderViolations, IDMismatches, ReasoningMissing and
	// ScenarioNotApplied count responses.
	OrderViolations    int64 `json:"order_violations"`
	IDMismatches       int64 `json:"id_mismatches"`
	ReasoningMissing   int64 `json:"reasoning_missing"`
	ScenarioNotApplied int64 `json:"scenario_not_applied"`
	// Scenario is the name of the scenario played (for the text warning).
	Scenario string `json:"scenario,omitempty"`
	// E2E of the completed conversations (first send to last byte).
	E2E *hist.Hist `json:"e2e"`
}

// NewConvStats returns empty ConvStats.
func NewConvStats() *ConvStats {
	return &ConvStats{FailedByReason: map[string]int64{}, E2E: hist.New()}
}

// Merge adds o into s.
func (s *ConvStats) Merge(o *ConvStats) {
	s.Started += o.Started
	s.Completed += o.Completed
	s.Failed += o.Failed
	s.MaxTurns += o.MaxTurns
	s.Interrupted += o.Interrupted
	for k, v := range o.FailedByReason {
		s.FailedByReason[k] += v
	}
	if len(o.Turns) > len(s.Turns) {
		s.Turns = append(s.Turns, make([]int64, len(o.Turns)-len(s.Turns))...)
	}
	for n, c := range o.Turns {
		s.Turns[n] += c
	}
	s.ToolCalls += o.ToolCalls
	s.OrderViolations += o.OrderViolations
	s.IDMismatches += o.IDMismatches
	s.ReasoningMissing += o.ReasoningMissing
	s.ScenarioNotApplied += o.ScenarioNotApplied
	if s.Scenario == "" {
		s.Scenario = o.Scenario
	}
	s.E2E.Merge(o.E2E)
}

// Conversations collects the ConvStats of a conversation unit, one per
// (shard, step) of the runner, written under the shard lock.
type Conversations struct {
	m    sync.Map // *Stats → *ConvStats
	name string   // scenario name
}

func (c *Conversations) of(s *Stats) *ConvStats {
	if v, ok := c.m.Load(s); ok {
		return v.(*ConvStats)
	}
	cs := NewConvStats()
	cs.Scenario = c.name
	v, _ := c.m.LoadOrStore(s, cs)
	return v.(*ConvStats)
}

// Stats merges everything recorded so far.
func (c *Conversations) Stats() *ConvStats {
	out := NewConvStats()
	out.Scenario = c.name
	c.m.Range(func(_, v any) bool {
		out.Merge(v.(*ConvStats))
		return true
	})
	return out
}

// Report is the "conversations" object of the JSON report.
func (c *Conversations) Report() *ConversationReport { return c.Stats().Report() }

// ConversationReport is the "conversations" object of the report. started
// = completed + failed + interrupted; max_turns is part of failed (also in
// failed_by_reason).
type ConversationReport struct {
	Started        int64            `json:"started"`
	Completed      int64            `json:"completed"`
	Failed         int64            `json:"failed"`
	MaxTurns       int64            `json:"max_turns"`
	Interrupted    int64            `json:"interrupted"`
	FailedByReason map[string]int64 `json:"failed_by_reason"`
	// Turns is the distribution of turns per ended conversation (exact
	// values, not ms).
	Turns            hist.Summary `json:"turns"`
	ToolCalls        int64        `json:"tool_calls"`
	E2E              hist.Summary `json:"e2e_ms"`
	OrderViolations  int64        `json:"order_violations"`
	IDMismatches     int64        `json:"id_mismatches"`
	ReasoningMissing int64        `json:"reasoning_missing"`
	// ScenarioNotApplied: 2xx responses without x-wwz-scenario: Scenario
	// (ADR 0041).
	ScenarioNotApplied int64  `json:"scenario_not_applied"`
	Scenario           string `json:"-"`
}

// Report summarizes s.
func (s *ConvStats) Report() *ConversationReport {
	reasons := make(map[string]int64, len(s.FailedByReason))
	for k, v := range s.FailedByReason {
		reasons[k] = v
	}
	return &ConversationReport{
		Started: s.Started, Completed: s.Completed, Failed: s.Failed, MaxTurns: s.MaxTurns,
		Interrupted: s.Interrupted, FailedByReason: reasons, Turns: countSummary(s.Turns),
		ToolCalls: s.ToolCalls, E2E: s.E2E.Summary(), OrderViolations: s.OrderViolations,
		IDMismatches: s.IDMismatches, ReasoningMissing: s.ReasoningMissing,
		ScenarioNotApplied: s.ScenarioNotApplied, Scenario: s.Scenario,
	}
}

// countSummary is the exact summary of a histogram of small integers
// (counts[v] = occurrences of v).
func countSummary(counts []int64) hist.Summary {
	var n, sum, maxV int64
	for v, c := range counts {
		n += c
		sum += int64(v) * c
		if c > 0 {
			maxV = int64(v)
		}
	}
	if n == 0 {
		return hist.Summary{}
	}
	q := func(p float64) float64 {
		rank := int64(math.Ceil(p * float64(n)))
		v, cum := 0, counts[0]
		for cum < rank {
			v++
			cum += counts[v]
		}
		return float64(v)
	}
	return hist.Summary{
		Count: uint64(n), Mean: round3(float64(sum) / float64(n)), //nolint:gosec // G115: n > 0
		P50: q(0.5), P90: q(0.9), P99: q(0.99), Max: float64(maxV),
	}
}

// WriteText writes the conversations block of the text report.
func (r *ConversationReport) WriteText(w io.Writer) error {
	ew := &errWriter{w: w}
	ew.printf("\nconversations: %d started  %d completed  %d failed (%d max_turns)  %d interrupted\n",
		r.Started, r.Completed, r.Failed, r.MaxTurns, r.Interrupted)
	ew.printf("  turns p50 %s  p99 %s  max %s  mean %s;  tool calls %d\n", fnum(r.Turns.P50), fnum(r.Turns.P99), fnum(r.Turns.Max), fnum(r.Turns.Mean), r.ToolCalls)
	ew.printf("  conversation e2e ms p50 %s  p99 %s  max %s\n", fnum(r.E2E.P50), fnum(r.E2E.P99), fnum(r.E2E.Max))
	ew.printf("  order violations %d  id mismatches %d  reasoning missing %d  scenario not applied %d\n",
		r.OrderViolations, r.IDMismatches, r.ReasoningMissing, r.ScenarioNotApplied)
	if r.ScenarioNotApplied > 0 {
		ew.printf("aviso: %d respostas sem x-wwz-scenario: %s (cenário não aplicado ou header removido pelo roteador)\n",
			r.ScenarioNotApplied, r.Scenario)
	}
	for _, k := range sortedErrors(r.FailedByReason) {
		ew.printf("  failed %8d  %s\n", r.FailedByReason[k], k)
	}
	return ew.err
}

// NewConversationUnit returns a Unit that plays one whole conversation per
// call and the collector of its measures. Every turn is a measured request
// (Recorder.Record); the conversation is recorded once, when it ends.
func NewConversationUnit(c *Client, p ConversationPlan) (Unit, *Conversations, error) {
	s := p.Scenario
	if s == nil {
		return nil, nil, fmt.Errorf("conversation: no scenario")
	}
	user := p.Prompt
	if s.Match != nil && s.Match.UserMessageMatches != "" {
		var err error
		if user, err = MessageMatching(s.Match.UserMessageMatches); err != nil {
			return nil, nil, fmt.Errorf("scenario %q: match.user_message_matches: %w", s.Name, err)
		}
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = DefaultMaxTurns
	}
	d := &dialog{
		c: c, p: p, tools: scenarioTools(s), user: user,
		results: ToolResults{Seed: p.DrawSeed, ErrorRate: p.ToolResultErrorRate},
		steps:   map[string]*scenario.Step{}, byID: map[string]*scenario.Step{}, scn: sanitizeID(s.Name),
	}
	for i := range s.Steps {
		d.steps[sanitizeID(s.Steps[i].ID)] = &s.Steps[i]
		d.byID[s.Steps[i].ID] = &s.Steps[i]
	}
	convs := &Conversations{name: s.Name}
	return func(ctx context.Context, i int64, rec *Recorder) {
		o := d.run(ctx, i, rec)
		rec.Update(func(st *Stats) { o.addTo(convs.of(st)) })
	}, convs, nil
}

// scenarioTools declares match.tools_include, then the literal tool names
// of the steps; without any, read_file and write_file so any, any_read and
// any_write have something to pick.
func scenarioTools(s *scenario.Scenario) []Tool {
	var names []string
	if s.Match != nil {
		names = append(names, s.Match.ToolsInclude...)
	}
	for _, st := range s.Steps {
		for _, tc := range st.ToolCalls {
			if literalTool(tc.Name) {
				names = append(names, tc.Name)
			}
		}
	}
	if len(names) == 0 {
		names = []string{"read_file", "write_file"}
	}
	var tools []Tool
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			tools = append(tools, Tool{Name: n})
		}
	}
	return tools
}

// literalTool: a step tool name that is neither a wildcard nor a template.
func literalTool(name string) bool {
	switch name {
	case "", "any", "any_read", "any_write":
		return false
	}
	return !strings.Contains(name, "{{")
}

// dialog is the shared, read-only part of the conversation unit.
type dialog struct {
	c       *Client
	p       ConversationPlan
	tools   []Tool
	user    string
	results ToolResults
	steps   map[string]*scenario.Step // by sanitized id
	byID    map[string]*scenario.Step // by id (x-wwz-step)
	scn     string                    // sanitized scenario name
}

// outcome is the result of one conversation.
type outcome struct {
	status                       string // completed | failed | interrupted
	reason                       string
	turns                        int
	toolCalls                    int64
	order, ids, reasoningMissing int64
	notApplied                   int64
	e2e                          time.Duration
}

const (
	statusCompleted   = "completed"
	statusFailed      = "failed"
	statusInterrupted = "interrupted"
)

func (o *outcome) addTo(s *ConvStats) {
	s.Started++
	switch o.status {
	case statusCompleted:
		s.Completed++
		s.E2E.Record(o.e2e)
	case statusFailed:
		s.Failed++
		s.FailedByReason[o.reason]++
		if o.reason == ReasonMaxTurns {
			s.MaxTurns++
		}
	default:
		s.Interrupted++
	}
	if o.status != statusInterrupted {
		if o.turns >= len(s.Turns) {
			s.Turns = append(s.Turns, make([]int64, o.turns+1-len(s.Turns))...)
		}
		s.Turns[o.turns]++
	}
	s.ToolCalls += o.toolCalls
	s.OrderViolations += o.order
	s.IDMismatches += o.ids
	s.ReasoningMissing += o.reasoningMissing
	s.ScenarioNotApplied += o.notApplied
}

// finalFinish are the finish reasons that end a conversation normally.
var finalFinish = map[string]bool{"stop": true, "end_turn": true, "length": true, "max_tokens": true}

func (d *dialog) interrupted() bool {
	if d.p.Interrupt == nil {
		return false
	}
	select {
	case <-d.p.Interrupt:
		return true
	default:
		return false
	}
}

// run plays conversation i.
func (d *dialog) run(ctx context.Context, i int64, rec *Recorder) outcome {
	msgs := []Message{{Role: "system", Content: ConversationSystem}, {Role: "user", Content: d.user}}
	st := convState{resultsFor: map[string]int{}}
	var o outcome
	var start time.Time
	res := new(Result)
	for turn := range d.p.MaxTurns {
		if d.interrupted() {
			o.status = statusInterrupted
			return o
		}
		req := d.p.Template
		req.Messages, req.Tools, req.Capture, req.Body = msgs, d.tools, true, nil
		if d.p.Seed != 0 {
			req.Seed = SeedOf(d.p.Seed, d.p.SeedStep, i)
		}
		d.c.DoInto(ctx, &req, res)
		rec.Record(res)
		if turn == 0 {
			start = res.Start
		}
		if res.Err == ErrCanceled {
			o.status = statusInterrupted
			return o
		}
		o.turns = turn + 1
		if res.Status >= 200 && res.Status < 300 && res.Scenario != d.p.Scenario.Name {
			o.notApplied++
		}
		if !res.OK() {
			o.status, o.reason = statusFailed, res.Err
			return o
		}
		step, fromHeader := d.check(&st, res, &o)
		o.toolCalls += int64(len(res.ToolCalls))
		if len(res.ToolCalls) == 0 {
			switch {
			case finalFinish[res.FinishReason] && !terminal(step, fromHeader):
				o.status, o.reason = statusFailed, ReasonIncomplete
			case finalFinish[res.FinishReason]:
				o.status, o.e2e = statusCompleted, res.Start.Add(res.E2E).Sub(start)
			case res.FinishReason == "tool_calls" || res.FinishReason == "tool_use":
				o.status, o.reason = statusFailed, ReasonNoToolCalls
			default:
				o.status, o.reason = statusFailed, ReasonFinishPrefix+res.FinishReason
			}
			return o
		}
		msgs = append(msgs, *res.Message)
		for k, tc := range res.ToolCalls {
			content, isErr := d.results.For(i, turn, k, tc.Name)
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: content, IsError: isErr})
			st.toolResult(tc.Name, content, isErr, d.p.Template.Protocol)
		}
		st.steps++
	}
	o.status, o.reason = statusFailed, ReasonMaxTurns
	return o
}

// convState is what the client knows of its own conversation, to predict
// the step the server will pick (scenario.DeriveState from the client
// side) and to check ids.
type convState struct {
	toolResults int
	lastTool    string
	lastResult  string // as the server sees it
	resultsFor  map[string]int
	steps       int
}

func (st *convState) toolResult(name, content string, isErr bool, p Protocol) {
	st.toolResults++
	st.resultsFor[name]++
	st.lastTool = name
	st.lastResult = content
	if isErr && p != Anthropic {
		st.lastResult = "error: " + content
	}
}

// wwzCallID is the tool call id the wwz scenarios emit:
// <call|toolu>_<scenario>_<step>_<index>_<nonce> (toolu on Messages routes,
// task 081).
var wwzCallID = regexp.MustCompile(`^(?:call|toolu)_([A-Za-z0-9-]+)_([A-Za-z0-9-]+)_(\d+)_([a-z0-9]+)$`)

// terminal reports whether a final answer on step ends the conversation
// (ADR 0041): a step without declared tool calls, or an unknown step (no
// x-wwz-step and no prediction). fromHeader with a nil step is an
// x-wwz-step the scenario does not have.
func terminal(step *scenario.Step, fromHeader bool) bool {
	if step == nil {
		return !fromHeader
	}
	return len(step.ToolCalls) == 0
}

// headerStep resolves the x-wwz-step of res: ok is false without the
// header; step is nil when the scenario has no such step or another
// scenario answered.
func (d *dialog) headerStep(res *Result) (step *scenario.Step, ok bool) {
	if res.Step == "" {
		return nil, false
	}
	if res.Scenario != "" && res.Scenario != d.p.Scenario.Name {
		return nil, true
	}
	return d.byID[res.Step], true
}

// check validates a successful response against the scenario (methodology
// item 6): ids present and unique in the response (the wwz repeats the ids
// of a repeated step, so uniqueness across turns is not required), tool
// calls in the order of the step, reasoning present when the step asks for
// it. It returns the step the response was checked against: the
// x-wwz-step of the response (fromHeader), else the step in the tool call
// ids, else the prediction; nil when unknown.
func (d *dialog) check(st *convState, res *Result, o *outcome) (step *scenario.Step, fromHeader bool) {
	step, fromHeader = d.headerStep(res)
	ids, order := false, false
	seen := make(map[string]bool, len(res.ToolCalls))
	for k, tc := range res.ToolCalls {
		if tc.ID == "" || seen[tc.ID] {
			ids = true
		}
		seen[tc.ID] = true
		m := wwzCallID.FindStringSubmatch(tc.ID)
		if m == nil || m[1] != d.scn {
			continue
		}
		if m[3] != strconv.Itoa(k) {
			order = true
		}
		if s := d.steps[m[2]]; s != nil && step == nil && !fromHeader {
			step = s
		}
	}
	if step == nil && !fromHeader {
		step = d.predict(st)
	}
	if step != nil && step.ToolMisbehavior == "" && len(step.Timeline) == 0 {
		specs := step.ToolCalls
		if step.Parallel != nil && !*step.Parallel && len(specs) > 1 {
			specs = specs[:1]
		}
		if len(specs) != len(res.ToolCalls) {
			order = true
		}
		for k := range min(len(specs), len(res.ToolCalls)) {
			if literalTool(specs[k].Name) && specs[k].Name != res.ToolCalls[k].Name {
				order = true
			}
		}
		if step.Reasoning != nil && !reasoningPresent(res) {
			o.reasoningMissing++
		}
	}
	if ids {
		o.ids++
	}
	if order {
		o.order++
	}
	return step, fromHeader
}

// reasoningPresent (ADR 0041): reasoning text (reasoning_content,
// reasoning, thinking), a redacted_thinking block or reasoning_tokens > 0
// in the usage (reasoning_field: hidden, OpenRouter reasoning.exclude).
func reasoningPresent(res *Result) bool {
	return res.Message.Reasoning != "" || res.ReasoningRedacted || res.ReasoningTokens > 0
}

// predict returns the step the server should pick from the client's view of
// the conversation, or nil when it cannot tell (conditions the client does
// not see, weighted ties, no match or step limit).
func (d *dialog) predict(st *convState) *scenario.Step {
	s := d.p.Scenario
	if st.steps >= s.StepLimit && s.OnExhausted != "repeat_last" {
		return nil
	}
	var cands []*scenario.Step
	for k := range s.Steps {
		step := &s.Steps[k]
		match, known := stepMatches(step.Expect, st)
		if !known {
			return nil
		}
		if match {
			cands = append(cands, step)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	if len(cands) > 1 && slices.ContainsFunc(cands, func(c *scenario.Step) bool { return c.Weight > 0 }) {
		return nil
	}
	return cands[0]
}

// stepMatches evaluates e like the server does; known is false when e uses
// a condition the client cannot evaluate.
func stepMatches(e *scenario.Expect, st *convState) (match, known bool) {
	if e == nil || e.Always {
		return true, true
	}
	if e.Model != "" || len(e.Header) > 0 || e.LastStep != "" || e.UserMessageMatches != "" || e.LatestMessageRole != "" {
		return false, false
	}
	match = (e.Turn == nil || *e.Turn == 1) && (e.TurnGte == nil || *e.TurnGte <= 1) &&
		(e.ToolResults == nil || *e.ToolResults == st.toolResults) &&
		(e.ToolResultsGte == nil || st.toolResults >= *e.ToolResultsGte) &&
		(e.LastTool == "" || e.LastTool == st.lastTool) &&
		(e.ToolResultFor == "" || st.resultsFor[e.ToolResultFor] > 0)
	if e.LastToolResultMatches != "" {
		re, err := regexp.Compile(e.LastToolResultMatches)
		if err != nil {
			return false, false
		}
		match = match && re.MatchString(st.lastResult)
	}
	return match, true
}

// sanitizeID is the scenario/step segment of a wwz tool call id.
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// MessageMatching returns a literal text matched by the regular expression
// pat (the shortest choices: first alternative, minimum repetitions).
func MessageMatching(pat string) (string, error) {
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", err
	}
	tree, _ := syntax.Parse(pat, syntax.Perl) // compiled above
	var b strings.Builder
	literal(&b, tree.Simplify())
	out := strings.TrimSpace(b.String())
	if !re.MatchString(out) {
		return "", fmt.Errorf("cannot derive a message matching %q", pat)
	}
	return out, nil
}

func literal(b *strings.Builder, r *syntax.Regexp) {
	switch r.Op {
	case syntax.OpLiteral:
		b.WriteString(string(r.Rune))
	case syntax.OpCharClass:
		b.WriteRune(classRune(r.Rune))
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		b.WriteByte('x')
	case syntax.OpCapture, syntax.OpPlus:
		literal(b, r.Sub[0])
	case syntax.OpConcat:
		for _, s := range r.Sub {
			literal(b, s)
		}
	case syntax.OpAlternate:
		literal(b, r.Sub[0])
	case syntax.OpWordBoundary:
		b.WriteByte(' ')
	}
	// OpEmptyMatch, OpStar, OpQuest, anchors: nothing. Simplify already
	// turned OpRepeat into concatenations.
}

// classRune picks a printable rune of a character class (pairs lo, hi).
func classRune(ranges []rune) rune {
	for i := 0; i+1 < len(ranges); i += 2 {
		for _, c := range []rune{'a', 'A', '0', ' '} {
			if c >= ranges[i] && c <= ranges[i+1] {
				return c
			}
		}
	}
	for i := 0; i+1 < len(ranges); i += 2 {
		if lo := max(ranges[i], '!'); lo <= ranges[i+1] {
			return lo
		}
	}
	return ranges[0]
}
