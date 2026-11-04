package scenario

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Report is the result of Check. Errors make the scenario unusable or
// partly dead; Warnings point at steps that fail for some requests.
type Report struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// OK reports whether the scenario has no errors.
func (r Report) OK() bool { return len(r.Errors) == 0 }

// CheckOptions tunes Check.
type CheckOptions struct {
	// Tools offered by the client under test. When set, a step naming a
	// literal tool outside this list is an error. When empty, literal tools
	// are compared with match.tools_include and reported as warnings.
	Tools []string
}

// Allowed values of the scenario enums (spec 011).
var (
	onNoMatchValues   = []string{"fallback", "error", "repeat_last"}
	onExhaustedValues = []string{"repeat_last", "error", "fallback"}
	idFormatValues    = []string{IDFormatEncoded, IDFormatNative}
	fallbackValues    = []string{"", "random", "echo"}
	moderationValues  = []string{"", "finish_reason", "http_403", "refusal"}
	timelineTypes     = []string{"text", "reasoning", "tool_call", "usage", "ping", "error", "finish", "delay"}
	genericTools      = []string{"any", "any_read", "any_write"}
)

// Check runs Validate plus the static checks of `wwz validate` (spec 011):
// enum values, templates that do not parse, steps no conversation can reach
// (an earlier step matches whenever they do) and literal tools the client
// does not offer.
func (s *Scenario) Check(opts CheckOptions) Report {
	var rep Report
	if err := s.Validate(); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	errf := func(format string, a ...any) {
		rep.Errors = append(rep.Errors, fmt.Sprintf("scenario %q: ", s.Name)+fmt.Sprintf(format, a...))
	}
	warnf := func(format string, a ...any) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("scenario %q: ", s.Name)+fmt.Sprintf(format, a...))
	}
	oneOf := func(field, v string, allowed []string) {
		if !contains(allowed, v) {
			errf("%s must be one of %s, got %q", field, strings.Join(nonEmpty(allowed), " | "), v)
		}
	}
	oneOf("on_no_match", s.OnNoMatch, onNoMatchValues)
	oneOf("on_exhausted", s.OnExhausted, onExhaustedValues)
	oneOf("fallback", s.Fallback, fallbackValues)
	oneOf("tool_call_id_format", s.ToolCallIDFormat, idFormatValues)
	if s.StepLimit < 0 {
		errf("step_limit must not be negative, got %d", s.StepLimit)
	}

	// Templates.
	funcs := (&tplContext{tk: generate.Simulated{}}).funcs()
	checkTpl := func(where, src string) {
		if !strings.Contains(src, "{{") {
			return
		}
		if _, err := template.New("s").Funcs(funcs).Parse(src); err != nil {
			errf("%s: invalid template %q: %v", where, src, err)
		}
	}
	keys := make([]string, 0, len(s.Vars))
	for k := range s.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		checkTpl("vars."+k, s.Vars[k])
	}

	offered := opts.Tools
	declared := false
	if len(offered) == 0 && s.Match != nil && len(s.Match.ToolsInclude) > 0 {
		offered, declared = s.Match.ToolsInclude, true
	}
	checkTool := func(where, name string) {
		if name == "" || contains(genericTools, name) || strings.Contains(name, "{{") || len(offered) == 0 || contains(offered, name) {
			return
		}
		if declared {
			warnf("%s: tool %q is not in match.tools_include; requests that do not offer it get an error", where, name)
			return
		}
		errf("%s: tool %q is not offered (offered: %s)", where, name, strings.Join(offered, ", "))
	}

	for i := range s.Steps {
		st := &s.Steps[i]
		at := fmt.Sprintf("step %q", st.ID)
		oneOf(at+" moderation", st.Moderation, moderationValues)
		if st.Expect != nil && st.Expect.LastStep != "" && s.ToolCallIDFormat == IDFormatNative {
			errf("%s expect.last_step: not available with tool_call_id_format: native (native ids do not carry the step)", at)
		}
		if lit, ok := st.Reasoning.(string); ok {
			checkTpl(at+" reasoning", lit)
		}
		if lit, ok := st.Content.(string); ok {
			checkTpl(at+" content", lit)
		}
		for j, tc := range st.ToolCalls {
			where := fmt.Sprintf("%s tool_calls[%d]", at, j)
			checkTpl(where+".name", tc.Name)
			checkTool(where, tc.Name)
			walkStrings(tc.Args, where+".args", checkTpl)
			walkStrings(map[string]any(tc.Overlay), where+".overlay", checkTpl)
		}
		for j, ev := range st.Timeline {
			where := fmt.Sprintf("%s timeline[%d]", at, j)
			oneOf(where+".type", ev.Type, timelineTypes)
			checkTpl(where+".text", ev.Text)
			if ev.Type == "tool_call" {
				checkTool(where, ev.Name)
			}
			walkStrings(ev.Args, where+".args", checkTpl)
		}
	}

	// Unreachable steps. The first matching step wins unless a candidate has
	// a weight, so skip the check when weights are in play.
	weighted := false
	for _, st := range s.Steps {
		if st.Weight > 0 {
			weighted = true
		}
	}
	if !weighted {
		conds := make([]map[string]string, len(s.Steps))
		for i := range s.Steps {
			conds[i] = conditions(s.Steps[i].Expect)
		}
		for i := range s.Steps {
			for j := 0; j < i; j++ {
				if subset(conds[j], conds[i]) {
					errf("step %q is unreachable: the earlier step %q matches whenever it does", s.Steps[i].ID, s.Steps[j].ID)
					break
				}
			}
		}
	}
	return rep
}

// conditions flattens an expect block into comparable key=value pairs; an
// absent or `always` block has none (it matches every state).
func conditions(e *Expect) map[string]string {
	c := map[string]string{}
	if e == nil || e.Always {
		return c
	}
	ints := map[string]*int{"turn": e.Turn, "turn_gte": e.TurnGte, "tool_results": e.ToolResults, "tool_results_gte": e.ToolResultsGte}
	for k, v := range ints {
		if v != nil {
			c[k] = strconv.Itoa(*v)
		}
	}
	strs := map[string]string{
		"last_tool": e.LastTool, "last_step": e.LastStep, "last_tool_result_matches": e.LastToolResultMatches,
		"tool_result_for": e.ToolResultFor, "user_message_matches": e.UserMessageMatches,
		"latest_message_role": e.LatestMessageRole, "model": e.Model,
	}
	for k, v := range strs {
		if v != "" {
			c[k] = v
		}
	}
	for k, v := range e.Header {
		c["header:"+strings.ToLower(k)] = v
	}
	if sm := e.SemanticMatch; sm != nil {
		c["semantic_match"] = strconv.FormatFloat(sm.Threshold, 'g', -1, 64) + "|" + sm.Text
	}
	return c
}

// subset reports whether every condition of a is also in b: then a matches
// whenever b does.
func subset(a, b map[string]string) bool {
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// walkStrings calls fn for every string inside v (maps and lists included).
func walkStrings(v any, where string, fn func(where, s string)) {
	switch x := v.(type) {
	case string:
		fn(where, x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkStrings(x[k], where+"."+k, fn)
		}
	case []any:
		for i, e := range x {
			walkStrings(e, fmt.Sprintf("%s[%d]", where, i), fn)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func nonEmpty(list []string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
