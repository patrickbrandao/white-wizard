package engine

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

func TestCoverageChunkText(t *testing.T) {
	tk := tokenizerFor(&config.ModelConfig{ID: "gpt-4o-mini"})
	if chunkText(tk, "", 1, 0) != nil {
		t.Fatal("empty text has no chunks")
	}
	got := chunkText(tk, "héllo wörld", 0, 4)
	if len(got) != 3 || got[0]+got[1]+got[2] != "héllo wörld" || got[2] != "rld" {
		t.Fatalf("runes: %q", got)
	}
	if got := chunkText(tk, "one two three", 0, 0); len(got) == 0 {
		t.Fatal("tokens <= 0 is one token a chunk")
	}
	if got := chunkText(tk, "one two three four five", 2, 0); len(got) < 2 {
		t.Fatalf("2 tokens a chunk: %q", got)
	}
	if got := chunkText(tk, "one two three", 100, 0); len(got) != 1 {
		t.Fatalf("remainder only: %q", got)
	}
	var _ generate.Tokenizer = tk //nolint:staticcheck // asserts the interface
}

func TestCoverageChunkBytes(t *testing.T) {
	if got := chunkBytes("0123456789012345678", 0); len(got) != 2 || got[1] != "678" {
		t.Fatalf("default 16: %q", got)
	}
	if got := chunkBytes("abcde", 2); len(got) != 3 || got[2] != "e" {
		t.Fatalf("%q", got)
	}
}

// fakePlan waits instantly and fails after failAfter calls.
type fakePlan struct {
	calls, failAfter int
	waited           []time.Duration
}

func (p *fakePlan) Wait(d time.Duration, _ <-chan struct{}) bool {
	p.calls++
	p.waited = append(p.waited, d)
	return p.failAfter == 0 || p.calls < p.failAfter
}

type pingCounter struct {
	core.StreamWriter
	pings int
}

func (p *pingCounter) Ping() error { p.pings++; return nil }

func TestCoverageStreamStateWait(t *testing.T) {
	pc := &pingCounter{}
	fp := &fakePlan{}
	st := &streamState{pingEvery: 10 * time.Millisecond, sw: pc, plan: fp}
	if !st.wait(25 * time.Millisecond) {
		t.Fatal("wait")
	}
	if pc.pings != 2 || len(fp.waited) != 3 || fp.waited[2] != 5*time.Millisecond {
		t.Fatalf("pings %d waits %v", pc.pings, fp.waited)
	}
	// slow tail multiplies by ten.
	fp2 := &fakePlan{}
	st = &streamState{slowTail: true, plan: fp2}
	st.wait(time.Millisecond)
	if fp2.waited[0] != 10*time.Millisecond {
		t.Fatalf("slow tail %v", fp2.waited)
	}
	// A wait that fails inside the ping loop reports false.
	fp3 := &fakePlan{failAfter: 2}
	st = &streamState{pingEvery: time.Millisecond, sw: &pingCounter{}, plan: fp3}
	if st.wait(10 * time.Millisecond) {
		t.Fatal("cancelled wait must report false")
	}
}

func TestCoverageTimelineHelpers(t *testing.T) {
	if er := timelineError(scenario.TimelineEvent{Type: "error"}); er.Kind != core.ErrServerError || er.Message != "scenario-injected server_error" {
		t.Fatalf("%+v", er)
	}
	if timelineErrorEvent(&request{}) != nil {
		t.Fatal("no scenario")
	}
}
