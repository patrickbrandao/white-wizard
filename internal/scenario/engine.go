package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

// State is derived from the message history of one request.
type State struct {
	Turn              int    // number of user turns
	ToolResults       int    // number of tool results seen
	LastTool          string // last tool called by the assistant
	LastToolResult    string // content of the latest tool result
	LastToolResultErr bool
	LastStep          string // decoded from the last tool call id (<call|toolu>_<scn>_<step>_<n>_<nonce>)
	LastScenario      string
	LatestRole        string
	ToolsAvailable    []string
	ToolResultsFor    map[string]int // tool name -> number of results
	ConversationID    string
	StepsSeen         int // number of assistant turns (approximation of steps taken)
}

// callIDRe decodes an encoded tool call id; the prefix is the dialect of
// the route (toolu on Messages routes, call elsewhere).
var callIDRe = regexp.MustCompile(`^(?:call|toolu)_([A-Za-z0-9-]+)_([A-Za-z0-9-]+)_(\d+)_([a-z0-9]+)$`)

// DeriveState computes the state from a request.
func DeriveState(req *core.Request) State {
	st := State{ToolResultsFor: map[string]int{}}
	callNames := map[string]string{}
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			if len(m.ToolResults) == 0 {
				st.Turn++
			}
			for _, tr := range m.ToolResults {
				st.ToolResults++
				st.LastToolResult = tr.Content
				st.LastToolResultErr = tr.IsError
				if n, ok := callNames[tr.ToolCallID]; ok {
					st.ToolResultsFor[n]++
				}
			}
		case "assistant":
			st.StepsSeen++
			for _, tc := range m.ToolCalls {
				st.LastTool = tc.Name
				callNames[tc.ID] = tc.Name
				if mm := callIDRe.FindStringSubmatch(tc.ID); mm != nil {
					st.LastScenario, st.LastStep = mm[1], mm[2]
				}
			}
		case "tool":
			st.ToolResults++
			st.LastToolResult = m.Content
			if n, ok := callNames[m.ToolCallID]; ok {
				st.ToolResultsFor[n]++
			}
		}
		st.LatestRole = m.Role
	}
	if st.Turn == 0 {
		st.Turn = 1
	}
	for _, t := range req.Tools {
		st.ToolsAvailable = append(st.ToolsAvailable, t.Name)
	}
	return st
}

// ConversationID returns a stable id for the conversation: a header when the
// client sends one, else a hash of the first user message plus the model.
func ConversationID(req *core.Request, headerName string) string {
	if headerName != "" {
		if v := req.Header(headerName); v != "" {
			return v
		}
	}
	for _, k := range []string{"x-wwz-conversation-id", "x-fakellm-conversation-id", "x-opencode-session", "x-session-id"} {
		if v := req.Header(k); v != "" {
			return v
		}
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(req.Model)) // fnv: Write never fails
	for _, m := range req.Messages {
		if m.Role == "user" {
			_, _ = h.Write([]byte(m.Content))
			break
		}
	}
	_, _ = h.Write([]byte(req.SystemPrompt()))
	return "conv-" + strconv.FormatUint(h.Sum64(), 36)
}

// Resolved is the step after templating, ready for the engine.
type Resolved struct {
	Scenario        string
	Step            string
	Attempt         int
	Reasoning       string
	ReasoningTokens int // when > 0 and Reasoning == "", generate this many
	Content         string
	ContentTokens   int
	ToolCalls       []core.ToolCall
	FinishReason    core.FinishReason
	Error           *core.Error
	Latency         *config.LatencyProfile
	Speed           float64
	Stall           *Stall
	CutAfterTokens  int
	Headers         map[string]string
	ToolMisbehavior string
	Moderation      string
	Timeline        []TimelineEvent
	Fallback        bool // no scenario matched; use generation profile
	ExhaustedError  bool
}

// Registry holds loaded scenarios.
type Registry struct {
	mu        sync.RWMutex
	scenarios map[string]*Scenario
	order     []string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{scenarios: map[string]*Scenario{}} }

// Add registers a scenario (replacing one with the same name).
func (r *Registry) Add(s *Scenario) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.scenarios[s.Name]; !ok {
		r.order = append(r.order, s.Name)
	}
	r.scenarios[s.Name] = s
}

// Remove deletes a scenario.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.scenarios, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Get returns a scenario by name.
func (r *Registry) Get(name string) *Scenario {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scenarios[name]
}

// Names lists scenarios in load order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// LoadDir loads every *.yaml/*.yml in dir. Files that fail validation are
// reported and skipped; previously loaded scenarios are kept.
func (r *Registry) LoadDir(dir string) (loaded []string, errs []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); ext == ".yaml" || ext == ".yml" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, n)))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s, err := ParseYAML(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
			continue
		}
		r.Add(s)
		loaded = append(loaded, s.Name)
	}
	return loaded, errs
}

// Selection is how the scenario was chosen plus per-request overrides.
type Selection struct {
	Scenario *Scenario
	Source   string // header | metadata | marker | model_suffix | match | model
	Seed     int64
	Speed    float64
	// CleanLastMessage is the last user message without the marker.
	CleanLastMessage string
}

var markerRe = regexp.MustCompile(`\[\[(?:wwz|llmsim|sim):([A-Za-z0-9_-]+)((?:\s+[a-z]+=[0-9.]+)*)\s*\]\]`)

// Select picks the scenario for a request following the configured order.
// modelScenarios are the candidates listed on the model.
func (r *Registry) Select(req *core.Request, order []string, modelScenarios []string) (Selection, error) {
	for _, how := range order {
		switch how {
		case "header":
			if v := req.Header("x-wwz-scenario"); v != "" {
				s := r.Get(v)
				if s == nil {
					return Selection{}, fmt.Errorf("unknown scenario %q", v)
				}
				return Selection{Scenario: s, Source: "header"}, nil
			}
		case "metadata":
			if req.Metadata != nil {
				v := req.Metadata["scenario"]
				if v == "" {
					if u := req.Metadata["user_id"]; strings.HasPrefix(u, "scenario:") {
						v = strings.TrimPrefix(u, "scenario:")
					}
				}
				if v != "" {
					s := r.Get(v)
					if s == nil {
						return Selection{}, fmt.Errorf("unknown scenario %q", v)
					}
					return Selection{Scenario: s, Source: "metadata"}, nil
				}
			}
		case "marker":
			last := req.LastUserMessage()
			if mm := markerRe.FindStringSubmatch(last); mm != nil {
				s := r.Get(mm[1])
				if s == nil {
					return Selection{}, fmt.Errorf("unknown scenario %q", mm[1])
				}
				sel := Selection{Scenario: s, Source: "marker", CleanLastMessage: strings.TrimSpace(markerRe.ReplaceAllString(last, ""))}
				for _, kv := range strings.Fields(mm[2]) {
					k, v, _ := strings.Cut(kv, "=")
					switch k {
					case "seed":
						sel.Seed, _ = strconv.ParseInt(v, 10, 64)
					case "speed":
						sel.Speed, _ = strconv.ParseFloat(v, 64)
					}
				}
				return sel, nil
			}
		case "model_suffix":
			// Cut at the last "-scenario-", as the engine does when it
			// resolves the model (spec 011): model ids may contain it.
			if i := strings.LastIndex(req.Model, "-scenario-"); i > 0 {
				name := req.Model[i+len("-scenario-"):]
				s := r.Get(name)
				if s == nil {
					return Selection{}, fmt.Errorf("unknown scenario %q", name)
				}
				return Selection{Scenario: s, Source: "model_suffix"}, nil
			}
		case "match":
			for _, name := range modelScenarios {
				s := r.Get(name)
				if s != nil && s.matches(req) {
					return Selection{Scenario: s, Source: "model"}, nil
				}
			}
			// Also allow the last tool call id to pin the scenario.
			st := DeriveState(req)
			if st.LastScenario != "" {
				if s := r.Get(st.LastScenario); s != nil {
					return Selection{Scenario: s, Source: "tool_call_id"}, nil
				}
			}
		}
	}
	return Selection{}, nil
}

func (s *Scenario) matches(req *core.Request) bool {
	m := s.Match
	if m == nil {
		return true
	}
	if m.Model != "" && !glob(m.Model, req.Model) {
		return false
	}
	if m.Driver != "" && m.Driver != req.Driver {
		return false
	}
	if len(m.ToolsInclude) > 0 {
		have := map[string]bool{}
		for _, t := range req.Tools {
			have[t.Name] = true
		}
		for _, want := range m.ToolsInclude {
			if !have[want] {
				return false
			}
		}
	}
	if re := s.re(m.UserMessageMatches); re != nil && !re.MatchString(req.LastUserMessage()) {
		return false
	}
	if re := s.re(m.SystemMatches); re != nil && !re.MatchString(req.SystemPrompt()) {
		return false
	}
	for k, v := range m.Header {
		if req.Header(strings.ToLower(k)) != v {
			return false
		}
	}
	return true
}

func glob(pattern, s string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(s, strings.TrimPrefix(pattern, "*"))
	}
	return false
}

// stepMatches evaluates a step's expectation against the state.
func (s *Scenario) stepMatches(st *Step, state *State, req *core.Request) bool {
	e := st.Expect
	if e == nil || e.Always {
		return true
	}
	if e.Turn != nil && state.Turn != *e.Turn {
		return false
	}
	if e.TurnGte != nil && state.Turn < *e.TurnGte {
		return false
	}
	if e.ToolResults != nil && state.ToolResults != *e.ToolResults {
		return false
	}
	if e.ToolResultsGte != nil && state.ToolResults < *e.ToolResultsGte {
		return false
	}
	if e.LastTool != "" && state.LastTool != e.LastTool {
		return false
	}
	if e.LastStep != "" && state.LastStep != e.LastStep {
		return false
	}
	if e.ToolResultFor != "" && state.ToolResultsFor[e.ToolResultFor] == 0 {
		return false
	}
	if e.LatestMessageRole != "" && state.LatestRole != e.LatestMessageRole {
		return false
	}
	if e.Model != "" && !glob(e.Model, req.Model) {
		return false
	}
	if re := s.re(e.LastToolResultMatches); re != nil && !re.MatchString(state.LastToolResult) {
		return false
	}
	if re := s.re(e.UserMessageMatches); re != nil && !re.MatchString(req.LastUserMessage()) {
		return false
	}
	if e.SemanticMatch != nil && !e.SemanticMatch.Matches(stripMarker(req.LastUserMessage())) {
		return false
	}
	for k, v := range e.Header {
		if req.Header(strings.ToLower(k)) != v {
			return false
		}
	}
	return true
}

// Resolver turns a selection into a resolved step.
type Resolver struct {
	// Tokenizer counts {{ text n }} and the token ranges (nil = simulated).
	Tokenizer  generate.Tokenizer
	ArgOptions toolargs.Options
	GlobalSeed int64
	// Attempts returns the attempt number for (conversation, step); nil = always 1.
	Attempts func(conversation, step string) int
}

// tokenizer is the model's tokenizer the engine passed (models[].tokenizer,
// ADR 0053); simulated when none was.
func (rv *Resolver) tokenizer() generate.Tokenizer {
	if rv.Tokenizer == nil {
		return generate.Simulated{}
	}
	return rv.Tokenizer
}

// Resolve chooses the step and renders its templates.
func (rv *Resolver) Resolve(sel Selection, req *core.Request, convID string) (*Resolved, error) {
	s := sel.Scenario
	state := DeriveState(req)
	state.ConversationID = convID
	seed := rv.GlobalSeed
	if sel.Seed != 0 {
		seed = sel.Seed
	}
	if seed == 0 {
		seed = int64(fnvHash(convID))
	}
	rng := generate.NewRNG(seed, "scenario", s.Name, convID)
	vars, err := renderVars(s, rng, &state, req, rv.tokenizer())
	if err != nil {
		return nil, err
	}
	step, res := s.chooseStep(&state, req, rng)
	if res != nil {
		return res, nil
	}
	return rv.render(s, step, sel, req, convID, &state, vars, rng)
}

// chooseStep applies the order of spec 011: on_exhausted once the
// conversation has step_limit assistant turns, else the first matching step
// (or a weighted draw), else on_no_match. A non-nil Resolved ends the
// resolution (error or fallback).
func (s *Scenario) chooseStep(state *State, req *core.Request, rng *generate.RNG) (*Step, *Resolved) {
	last := &s.Steps[len(s.Steps)-1]
	if s.StepLimit > 0 && state.StepsSeen >= s.StepLimit {
		switch s.OnExhausted {
		case "error":
			return nil, &Resolved{Scenario: s.Name, ExhaustedError: true, Error: &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("scenario %q: step limit %d reached", s.Name, s.StepLimit)}}
		case "fallback":
			return nil, &Resolved{Scenario: s.Name, Fallback: true}
		default: // repeat_last: the last step declared in the file
			return last, nil
		}
	}
	// Step choice: first match, or weighted among matches when weights are set.
	var cands []*Step
	for i := range s.Steps {
		st := &s.Steps[i]
		if s.stepMatches(st, state, req) {
			cands = append(cands, st)
		}
	}
	var step *Step
	if len(cands) > 0 {
		weighted := false
		for _, c := range cands {
			if c.Weight > 0 {
				weighted = true
				break
			}
		}
		if weighted {
			total := 0.0
			for _, c := range cands {
				w := c.Weight
				if w <= 0 {
					w = 1
				}
				total += w
			}
			x := rng.Float64() * total
			// The last candidate is the default: float rounding may leave
			// x > 0 after the subtractions, so it has no branch of its own.
			step = cands[len(cands)-1]
			for _, c := range cands[:len(cands)-1] {
				w := c.Weight
				if w <= 0 {
					w = 1
				}
				x -= w
				if x <= 0 {
					step = c
					break
				}
			}
		} else {
			step = cands[0]
		}
	}
	if step != nil {
		return step, nil
	}
	switch s.OnNoMatch {
	case "repeat_last":
		return last, nil
	case "error":
		return nil, &Resolved{Scenario: s.Name, ExhaustedError: true, Error: &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("scenario %q: no step matches the conversation state", s.Name)}}
	default:
		return nil, &Resolved{Scenario: s.Name, Fallback: true}
	}
}

// render resolves the templates of the chosen step.
func (rv *Resolver) render(s *Scenario, step *Step, sel Selection, req *core.Request, convID string, state *State, vars map[string]string, rng *generate.RNG) (*Resolved, error) {
	var err error
	res := &Resolved{Scenario: s.Name, Step: step.ID, Latency: step.Latency, Speed: step.Speed, Stall: step.Stall, CutAfterTokens: step.CutAfterTokens, Headers: step.Headers, ToolMisbehavior: step.ToolMisbehavior, Moderation: step.Moderation, Timeline: step.Timeline, Attempt: 1}
	if sel.Speed > 0 {
		res.Speed = sel.Speed
	}
	if rv.Attempts != nil {
		res.Attempt = rv.Attempts(convID, step.ID)
	}
	if ff := step.FailFirst; ff != nil && ff.Times > 0 && res.Attempt <= ff.Times {
		kind := core.ErrorKind(ff.Error)
		if kind == "" {
			kind = core.ErrRateLimit
		}
		res.Error = &core.Error{Kind: kind, Message: ff.Message}
		if res.Error.Message == "" {
			res.Error.Message = fmt.Sprintf("scenario %q step %q: injected failure (attempt %d of %d)", s.Name, step.ID, res.Attempt, ff.Times)
		}
		if ff.RetryAfterMS > 0 {
			res.Error.RetryAfter = time.Duration(ff.RetryAfterMS) * time.Millisecond
		}
		return res, nil
	}
	if step.Error != "" {
		res.Error = &core.Error{Kind: core.ErrorKind(step.Error), Message: step.ErrorMessage}
		if res.Error.Message == "" {
			res.Error.Message = "scenario-injected " + step.Error
		}
		return res, nil
	}
	tctx := &tplContext{Vars: vars, State: state, Req: req, rng: rng, tk: rv.tokenizer()}
	// Reasoning.
	rr, _ := parseTokenRange(step.Reasoning)
	if rr.IsLiteral {
		res.Reasoning, err = tctx.render(rr.Literal)
		if err != nil {
			return nil, err
		}
	} else if rr.Max > 0 {
		res.ReasoningTokens = rng.Between(rr.Min, rr.Max)
	}
	// Content.
	cr, _ := parseTokenRange(step.Content)
	if cr.IsLiteral {
		res.Content, err = tctx.render(cr.Literal)
		if err != nil {
			return nil, err
		}
	} else if cr.Max > 0 {
		res.ContentTokens = rng.Between(cr.Min, cr.Max)
	}
	// Tool calls.
	for i, spec := range step.ToolCalls {
		tc, err := rv.resolveToolCall(spec, i, s, step, req, tctx, rng)
		if err != nil {
			return nil, err
		}
		if tc != nil {
			res.ToolCalls = append(res.ToolCalls, *tc)
			if step.Parallel != nil && !*step.Parallel {
				break
			}
		}
	}
	if step.FinishReason != "" {
		res.FinishReason = core.FinishReason(step.FinishReason)
	} else if len(res.ToolCalls) > 0 {
		res.FinishReason = core.FinishToolCalls
	} else {
		res.FinishReason = core.FinishStop
	}
	return res, nil
}

func (rv *Resolver) resolveToolCall(spec ToolCallSpec, idx int, s *Scenario, step *Step, req *core.Request, tctx *tplContext, rng *generate.RNG) (*core.ToolCall, error) {
	// Choose the tool.
	var tool *core.ToolDef
	switch spec.Name {
	case "any", "any_read", "any_write":
		var pool []*core.ToolDef
		for i := range req.Tools {
			t := &req.Tools[i]
			name := strings.ToLower(t.Name)
			switch spec.Name {
			case "any_read":
				if strings.Contains(name, "read") || strings.Contains(name, "get") || strings.Contains(name, "list") || strings.Contains(name, "search") || strings.Contains(name, "fetch") {
					pool = append(pool, t)
				}
			case "any_write":
				if strings.Contains(name, "write") || strings.Contains(name, "create") || strings.Contains(name, "edit") || strings.Contains(name, "update") || strings.Contains(name, "delete") || strings.Contains(name, "put") || strings.Contains(name, "set") {
					pool = append(pool, t)
				}
			default:
				pool = append(pool, t)
			}
		}
		if len(pool) == 0 {
			if len(req.Tools) == 0 {
				return nil, nil // nothing to call; skip silently
			}
			pool = make([]*core.ToolDef, 0, len(req.Tools))
			for i := range req.Tools {
				pool = append(pool, &req.Tools[i])
			}
		}
		tool = pool[rng.IntN(len(pool))]
	default:
		name, err := tctx.render(spec.Name)
		if err != nil {
			return nil, err
		}
		for i := range req.Tools {
			if req.Tools[i].Name == name {
				tool = &req.Tools[i]
				break
			}
		}
		if tool == nil {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: fmt.Sprintf("scenario %q step %q names tool %q which the request does not offer", s.Name, step.ID, name)}
		}
	}
	// Arguments.
	var args []byte
	switch a := spec.Args.(type) {
	case nil:
		args = []byte("{}")
		if len(tool.Parameters) > 0 {
			sch, err := toolargs.Parse(tool.Parameters)
			if err == nil {
				args, _ = toolargs.Generate(sch, rng, requiredOnly(rv.ArgOptions))
			}
		}
	case string:
		if a == "schema" || a == "schema+" {
			sch, err := toolargs.Parse(tool.Parameters)
			if err != nil {
				return nil, err
			}
			m := toolargs.GenerateMap(sch, rng, rv.ArgOptions)
			if a == "schema+" {
				for k, v := range spec.Overlay {
					if sv, ok := v.(string); ok {
						rendered, err := tctx.render(sv)
						if err != nil {
							return nil, err
						}
						m[k] = rendered
					} else {
						m[k] = v
					}
				}
			}
			args, _ = json.Marshal(m)
		} else {
			rendered, err := tctx.render(a)
			if err != nil {
				return nil, err
			}
			args = []byte(rendered)
		}
	case map[string]any:
		m := map[string]any{}
		for k, v := range a {
			if sv, ok := v.(string); ok {
				rendered, err := tctx.render(sv)
				if err != nil {
					return nil, err
				}
				m[k] = rendered
			} else {
				m[k] = v
			}
		}
		args, _ = json.Marshal(m)
	default:
		args, _ = json.Marshal(a)
	}
	// A literal id wins; native leaves the id empty for the engine to mint
	// with the driver's IDMinter; encoded carries scenario and step.
	id := spec.ID
	if id == "" && s.ToolCallIDFormat != IDFormatNative {
		prefix := "call"
		if req.Endpoint == core.EndpointMessages {
			prefix = "toolu"
		}
		id = fmt.Sprintf("%s_%s_%s_%d_%s", prefix, sanitize(s.Name), sanitize(step.ID), idx, strconv.FormatUint(uint64(rng.Uint32()), 36))
	}
	return &core.ToolCall{ID: id, Name: tool.Name, Arguments: string(args)}, nil
}

func requiredOnly(o toolargs.Options) toolargs.Options {
	o.OptionalProbability = 0
	return o
}

// stripMarker removes a [[wwz:...]] selection marker from a message.
func stripMarker(msg string) string {
	if !strings.Contains(msg, "[[") {
		return msg
	}
	return strings.TrimSpace(markerRe.ReplaceAllString(msg, ""))
}

func sanitize(s string) string {
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

func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s)) // fnv: Write never fails
	return h.Sum32()
}

// --- templating ---

type tplContext struct {
	Vars  map[string]string
	State *State
	Req   *core.Request
	rng   *generate.RNG
	tk    generate.Tokenizer
}

var tplCache sync.Map

func (c *tplContext) funcs() template.FuncMap {
	g := generate.NewGenerator(c.tk, nil)
	return template.FuncMap{
		"rand_word":  func() string { return randWord(c.rng) },
		"rand_int":   func(lo, hi int) int { return c.rng.Between(lo, hi) },
		"rand_float": func(lo, hi float64) float64 { return lo + c.rng.Float64()*(hi-lo) },
		"lorem":      func(n int) string { return g.Lorem(c.rng, n) },
		"text":       func(n int) string { return g.Text(c.rng, n) },
		"uuid": func() string {
			return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", c.rng.Uint32(), c.rng.IntN(0xffff), c.rng.IntN(0xfff), 0x8000|c.rng.IntN(0x3fff), c.rng.Int64N(1<<48))
		},
		"now":               func() string { return time.Now().UTC().Format(time.RFC3339) },
		"pick":              func(opts ...string) string { return opts[c.rng.IntN(len(opts))] },
		"last_user_message": func() string { return c.Req.LastUserMessage() },
		"json":              func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"upper":             strings.ToUpper,
		"lower":             strings.ToLower,
	}
}

func (c *tplContext) render(src string) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	t, err := template.New("s").Funcs(c.funcs()).Parse(src)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", src, err)
	}
	var buf bytes.Buffer
	data := map[string]any{"vars": c.Vars, "state": c.State, "req": map[string]any{"model": c.Req.Model, "last_user_message": c.Req.LastUserMessage(), "tools": c.State.ToolsAvailable, "driver": c.Req.Driver}}
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template %q: %w", src, err)
	}
	return buf.String(), nil
}

func renderVars(s *Scenario, rng *generate.RNG, state *State, req *core.Request, tk generate.Tokenizer) (map[string]string, error) {
	out := map[string]string{}
	if len(s.Vars) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(s.Vars))
	for k := range s.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ctx := &tplContext{Vars: out, State: state, Req: req, rng: rng, tk: tk}
	for _, k := range keys {
		v, err := ctx.render(s.Vars[k])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

var wordList = []string{"report", "notes", "draft", "summary", "config", "backup", "index", "readme", "todo", "ledger", "manifest", "journal", "sketch", "outline", "budget", "roster"}

func randWord(r *generate.RNG) string { return wordList[r.IntN(len(wordList))] }
