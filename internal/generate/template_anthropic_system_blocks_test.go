package generate_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// TestAnthropicSystemBlocksBreakdown: each text block of a role "system"
// message costs as a system message of its own, as the real counts it
// (count_tokens of claude-haiku-5-5 on 2026-11-03, task 453:
// [user x, system [s, t]] = 13 = [user x, system s, system t], while
// system "st" = 11), and x-wwz-input-breakdown puts it in system.
func TestAnthropicSystemBlocksBreakdown(t *testing.T) {
	d, err := driver.New("anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}
	count := func(messages string) generate.PromptBreakdown {
		t.Helper()
		body := `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"x"}` + messages + `]}`
		req, e := d.ParseRequest(driver.KindCountTokens, []byte(body), driver.MapHeaders{"anthropic-version": "2023-06-01"})
		if e != nil {
			t.Fatal(e.Message)
		}
		return generate.CountPrompt(generate.Simulated{}, req, 0)
	}
	blocks := count(`,{"role":"system","content":[{"type":"text","text":"s"},{"type":"text","text":"t"}]}`)
	if blocks.Total != 13 {
		t.Errorf("total %d, real 13 (%+v)", blocks.Total, blocks)
	}
	if two := count(`,{"role":"system","content":"s"},{"role":"system","content":"t"}`); two != blocks {
		t.Errorf("[s, t] %+v, two messages %+v", blocks, two)
	}
	if one := count(`,{"role":"system","content":"st"}`); one.Total != 11 {
		t.Errorf("system \"st\": %d, real 11", one.Total)
	}
	if alone := count(``); alone.Total != 9 || blocks.Total-alone.Total != blocks.System {
		t.Errorf("user x alone %+v, with [s, t] %+v", alone, blocks)
	}
	// The real 13 less the 9 of user "x" alone is the system part.
	got := string(blocks.AppendJSON(nil))
	if !strings.HasPrefix(got, `{"system":`+strconv.Itoa(blocks.System)+`,`) || blocks.System != 13-9 {
		t.Errorf("x-wwz-input-breakdown %s: system %d, real 4 (13 - the 9 of user x)", got, blocks.System)
	}
}
