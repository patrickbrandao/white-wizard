package engine

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// A finish_length cut ends with length; the usage drops to what was sent
// and never grows past the planned reply (task 393).
func TestCutUsage(t *testing.T) {
	r := &request{model: &config.ModelConfig{}, resp: &core.Response{FinishReason: core.FinishStop, Usage: core.Usage{CompletionTokens: 10, ReasoningTokens: 6}}}
	cutUsage(r, 4)
	if u := r.resp.Usage; r.resp.FinishReason != core.FinishLength || u.CompletionTokens != 4 || u.ReasoningTokens != 4 {
		t.Fatalf("cut at 4: %s %+v", r.resp.FinishReason, u)
	}
	cutUsage(r, 9)
	if u := r.resp.Usage; u.CompletionTokens != 4 {
		t.Fatalf("sent past the plan: %+v", u)
	}
}
