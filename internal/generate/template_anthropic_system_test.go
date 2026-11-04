package generate

import (
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// sysMsgs builds the messages of a probe: u/y/z are user "x"/"y"/"z", a/b
// assistant "a"/"b", s a role "system" "s" and d a system message without
// text (the directive-only form).
func sysMsgs(shape string) []core.Message {
	var ms []core.Message
	for _, c := range shape {
		switch c {
		case 'u':
			ms = append(ms, core.Message{Role: "user", Content: "x"})
		case 'y', 'z':
			ms = append(ms, core.Message{Role: "user", Content: string(c)})
		case 'a', 'b':
			ms = append(ms, core.Message{Role: "assistant", Content: string(c)})
		case 's':
			ms = append(ms, core.Message{Role: "system", Content: "s"})
		case 'd':
			ms = append(ms, core.Message{Role: "system"})
		}
	}
	return ms
}

// TestAnthropicSystemRole: a role "system" message (the form of Claude
// Code 2.1.308) costs sysHeader + its text in System and opens no turn; a
// run of them costs sysRun more, and sysRunTurn more before an assistant
// turn with header. The totals are the real count_tokens of 2026-11-03
// (task 451), exact here because every text is one token.
func TestAnthropicSystemRole(t *testing.T) {
	cases := []struct {
		model, shape string
		top          bool // top-level system "s"
		real         int
	}{
		// claude-haiku-5-5 (the same on sonnet-5-5, opus-5-5 and fable-5-1
		// for u, us, usa, uau and usau).
		{"claude-haiku-5-5", "u", false, 9},
		{"claude-haiku-5-5", "us", false, 11},
		{"claude-haiku-5-5", "u", true, 11},
		{"claude-haiku-5-5", "us", true, 13},
		{"claude-haiku-5-5", "uss", false, 13},
		{"claude-haiku-5-5", "ua", false, 10},
		{"claude-haiku-5-5", "usa", false, 12},
		{"claude-haiku-5-5", "uau", false, 15},
		{"claude-haiku-5-5", "usau", false, 18},
		{"claude-haiku-5-5", "ussau", false, 20},
		{"claude-haiku-5-5", "uau", true, 17},
		{"claude-haiku-5-5", "usau", true, 20},
		{"claude-haiku-5-5", "uaub", false, 16},
		{"claude-haiku-5-5", "usaub", false, 19},
		{"claude-haiku-5-5", "uaus", false, 17},
		{"claude-haiku-5-5", "uaubz", false, 21},
		{"claude-haiku-5-5", "uausbz", false, 24},
		{"claude-haiku-5-5", "ud", false, 9},
		{"claude-haiku-5-5", "udau", false, 15},
		{"claude-sonnet-5-5", "usau", false, 18},
		{"claude-fable-5-1", "usa", false, 12},
		// claude-opus-4-8 and claude-fable-5 (identical).
		{"claude-opus-4-8", "u", false, 7},
		{"claude-opus-4-8", "us", false, 10},
		{"claude-opus-4-8", "u", true, 9},
		{"claude-opus-4-8", "uss", false, 12},
		{"claude-opus-4-8", "ua", false, 8},
		{"claude-opus-4-8", "usa", false, 11},
		{"claude-opus-4-8", "uau", false, 13},
		{"claude-opus-4-8", "usau", false, 16},
		{"claude-fable-5", "ussau", false, 18},
		{"claude-sonnet-5", "usau", false, 16},
	}
	for _, c := range cases {
		req := &core.Request{Driver: "anthropic", Endpoint: core.EndpointMessages, Model: c.model, Messages: sysMsgs(c.shape)}
		if c.top {
			req.System = "s"
		}
		b := CountPrompt(Simulated{}, req, 0)
		if b.Total != c.real {
			t.Errorf("%s %s top=%v: total %d, real %d (%+v)", c.model, c.shape, c.top, b.Total, c.real, b)
		}
		noSys := &core.Request{Driver: "anthropic", Endpoint: core.EndpointMessages, Model: c.model}
		for _, m := range req.Messages {
			if m.Role != "system" {
				noSys.Messages = append(noSys.Messages, m)
			}
		}
		nb := CountPrompt(Simulated{}, noSys, 0)
		if b.User != nb.User || b.Assistant != nb.Assistant {
			t.Errorf("%s %s: the system messages moved user/assistant: %+v vs %+v", c.model, c.shape, b, nb)
		}
	}
}

// TestAnthropicSystemRoleBreakdown: x-wwz-input-breakdown puts the text of
// a role "system" message in system, the user turn whole.
func TestAnthropicSystemRoleBreakdown(t *testing.T) {
	text := "hello world, be brief"
	req := &core.Request{Driver: "anthropic", Endpoint: core.EndpointMessages, Model: "claude-haiku-5-5", Messages: []core.Message{
		{Role: "user", Content: "x"},
		{Role: "system", Content: text},
	}}
	b := CountPrompt(Simulated{}, req, 0)
	tk := scaled{pct: claude55.textPct}
	if want := claude55.sysHeader + tk.Count(text); b.System != want {
		t.Errorf("system %d, want %d", b.System, want)
	}
	if b.User != 1+claude55.msgHeader {
		t.Errorf("user %d", b.User)
	}
	top := &core.Request{Driver: "anthropic", Endpoint: core.EndpointMessages, Model: "claude-haiku-5-5", System: text, Messages: req.Messages[:1]}
	if tb := CountPrompt(Simulated{}, top, 0); tb != b {
		t.Errorf("as the top-level system (real: 17 both): %+v vs %+v", tb, b)
	}
	got := string(b.AppendJSON(nil))
	if !strings.HasPrefix(got, `{"system":`+strconv.Itoa(b.System)+`,`) || !strings.Contains(got, `"user":`+strconv.Itoa(b.User)+`,`) {
		t.Errorf("header %s", got)
	}
}
