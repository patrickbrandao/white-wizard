package anth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestRealToolChoiceRefusals replays testdata/real/anthropic/tool-choice/
// (task 264, ADR 0014): keys a tool_choice type does not accept and the
// models that refuse any/tool give the real 400 byte for byte; the
// accepted requests parse.
func TestRealToolChoiceRefusals(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "anthropic", "tool-choice", "*.json"))
	if err != nil || len(files) < 15 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	reqID := regexp.MustCompile(`"request_id":"req_[A-Za-z0-9]+"`)
	h := APIHooks()
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Name    string `json:"name"`
			Request struct {
				Body string `json:"body"`
			} `json:"request"`
			Response struct {
				Status int     `json:"status"`
				Body   *string `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		req, perr := ParseMessages(&h, []byte(fx.Request.Body), driver.MapHeaders{"anthropic-version": "2023-06-01"}, "anthropic")
		if fx.Response.Status == 200 {
			if perr != nil {
				t.Errorf("%s: refused: %s", fx.Name, perr.Message)
			}
			continue
		}
		if perr == nil {
			t.Errorf("%s: accepted, real %d", fx.Name, fx.Response.Status)
			continue
		}
		w := core.NewMemWriter()
		WriteError(&h, w, req, perr, nil)
		got := reqID.ReplaceAllString(string(w.Body()), `"request_id":"<request_id>"`)
		if w.StatusCode() != fx.Response.Status || got != *fx.Response.Body {
			t.Errorf("%s: %d %s\n real %d %s", fx.Name, w.StatusCode(), got, fx.Response.Status, *fx.Response.Body)
		}
	}
}

// TestToolChoiceRules: the rules a gateway sets (tasks 301 and 342). With
// StrictKeys a key the type does not accept is refused, without it ignored;
// IgnoreWithoutTools drops any and a named tool when there are no tools
// (empty list included), after the type is checked; Refuse receives the
// text of Anthropic's API and whether the type was the problem. The
// forced tool of claude-opus-5.5 is read by OpenRouter's id too. An
// unknown type has no key list.
func TestToolChoiceRules(t *testing.T) {
	type refusal struct {
		msg     string
		unknown bool
	}
	var got []refusal
	rec := func(_ *core.Request, msg string, unknown bool) *core.Error {
		got = append(got, refusal{msg, unknown})
		return &core.Error{Kind: core.ErrInvalidRequest, Message: msg}
	}
	strict := DefaultHooks()
	strict.ToolChoice = ToolChoiceRules{StrictKeys: true, Refuse: rec}
	lenient := DefaultHooks()
	lenient.ToolChoice = ToolChoiceRules{IgnoreWithoutTools: true, Refuse: rec}
	const tools = `"tools":[{"name":"a"}],`
	cases := []struct {
		h            *Hooks
		model, extra string
		want         *refusal
	}{
		{&strict, "claude-haiku-5-5", tools + `"tool_choice":{"type":"tool","name":"zzz"}`, &refusal{"Tool 'zzz' not found in provided tools", false}},
		{&strict, "claude-haiku-5-5", `"tool_choice":{"type":"tool","name":"zzz"}`, &refusal{"Tool 'zzz' not found in provided tools", false}},
		{&strict, "claude-haiku-5-5", `"tool_choice":{"type":"any"}`, &refusal{"tool_choice.any may only be specified while providing tools", false}},
		{&strict, "claude-haiku-5-5", tools + `"tool_choice":{"type":"none","disable_parallel_tool_use":true}`, &refusal{"tool_choice.none.disable_parallel_tool_use: Extra inputs are not permitted", false}},
		{&strict, "claude-haiku-5-5", tools + `"tool_choice":{}`, &refusal{"tool_choice.type: Field required", true}},
		{&strict, "claude-haiku-5-5", tools + `"tool_choice":{"type":"function"}`, &refusal{"tool_choice: Input tag 'function' found using 'type' does not match any of the expected tags: 'auto', 'any', 'tool', 'none'", true}},
		{&strict, "claude-haiku-5-5", tools + `"tool_choice":{"type":"any"}`, nil},
		{&lenient, "anthropic/claude-haiku-4.5", tools + `"tool_choice":{"type":"tool","name":"zzz"}`, &refusal{"Tool 'zzz' not found in provided tools", false}},
		{&lenient, "anthropic/claude-opus-5.5", tools + `"tool_choice":{"type":"any"}`, &refusal{errForcedTool, false}},
		{&lenient, "anthropic/claude-opus-5.5", tools + `"tool_choice":{"type":"none","disable_parallel_tool_use":true}`, nil},
		{&lenient, "anthropic/claude-opus-5.5", tools + `"tool_choice":{"type":"auto"}`, nil},
		{&lenient, "anthropic/claude-opus-5.5", `"tool_choice":{"type":"tool","name":"zzz"}`, nil},
		{&lenient, "anthropic/claude-haiku-4.5", `"tool_choice":{"type":"any"}`, nil},
		{&lenient, "anthropic/claude-haiku-4.5", `"tools":[],"tool_choice":{"type":"any"}`, nil},
		{&lenient, "anthropic/claude-haiku-4.5", `"tool_choice":{"type":"function"}`, &refusal{"tool_choice: Input tag 'function' found using 'type' does not match any of the expected tags: 'auto', 'any', 'tool', 'none'", true}},
	}
	for _, c := range cases {
		got = nil
		body := `{"model":"` + c.model + `","max_tokens":1,"messages":[{"role":"user","content":"x"}],` + c.extra + `}`
		req, err := ParseMessages(c.h, []byte(body), driver.MapHeaders{}, "gw")
		switch {
		case c.want == nil && (err != nil || len(got) > 0):
			t.Errorf("%s: refused %v %v", body, err, got)
		case c.want == nil && len(req.Tools) == 0 && req.ToolChoice.Mode != core.ToolChoiceAuto:
			t.Errorf("%s: tool_choice kept without tools: %+v", body, req.ToolChoice)
		case c.want != nil && (err == nil || len(got) != 1 || got[0] != *c.want):
			t.Errorf("%s: %v %v, want %+v", body, err, got, *c.want)
		}
	}
	if e := toolChoiceExtra([]byte(`{"tool_choice":{"type":"auto"}}`), "unknown"); e != nil {
		t.Error(e.Message)
	}
}

// TestToolChoiceUnknownType: Anthropic's API refuses a missing type and a
// type outside the four (an empty one too) before the tools are looked at
// (conferred on 2026-10-31, task 342: tmp/342 probes missing_type,
// empty_type and unknown_type, with and without tools).
func TestToolChoiceUnknownType(t *testing.T) {
	h := APIHooks()
	for tc, want := range map[string]string{
		`{"type":"function"}`: "tool_choice: Input tag 'function' found using 'type' does not match any of the expected tags: 'auto', 'any', 'tool', 'none'",
		`{"type":""}`:         "tool_choice: Input tag '' found using 'type' does not match any of the expected tags: 'auto', 'any', 'tool', 'none'",
		`{}`:                  "tool_choice.type: Field required",
	} {
		body := `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"x"}],"tool_choice":` + tc + `}`
		_, err := ParseMessages(&h, []byte(body), driver.MapHeaders{"anthropic-version": "2023-06-01"}, "anthropic")
		if err == nil || err.Message != want {
			t.Errorf("%s: %+v", tc, err)
		}
	}
}

// TestTextBlocksJoin: text blocks of the system prompt and of a message
// join with nothing between them, as count_tokens reads them (task 301:
// [x, x] costs what "xx" does, "x\nx" one more; other blocks are skipped).
func TestTextBlocksJoin(t *testing.T) {
	h := APIHooks()
	body := `{"model":"claude-haiku-4-5","max_tokens":1,` +
		`"system":[{"type":"text","text":"hello"},{"type":"image"},{"type":"text","text":" world"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image","source":{}},{"type":"text","text":"y"},{"type":"text","text":"z"}]}]}`
	req, err := ParseMessages(&h, []byte(body), driver.MapHeaders{"anthropic-version": "2023-06-01"}, "anthropic")
	if err != nil {
		t.Fatal(err.Message)
	}
	if req.System != "hello world" || req.Messages[0].Content != "xyz" || req.Messages[0].ImageParts != 1 {
		t.Errorf("system %q, content %q, images %d", req.System, req.Messages[0].Content, req.Messages[0].ImageParts)
	}
	for raw, want := range map[string]string{``: "", `"s"`: "s", `42`: "", `[]`: ""} {
		if got := flattenSystem([]byte(raw)); got != want {
			t.Errorf("flattenSystem(%s) = %q, want %q", raw, got, want)
		}
	}
}
