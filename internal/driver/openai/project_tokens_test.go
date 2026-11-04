package openai_test

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

var projectTokenHeaders = map[string]string{
	"x-ratelimit-limit-project-tokens":     "60000",
	"x-ratelimit-remaining-project-tokens": "57000",
	"x-ratelimit-reset-project-tokens":     "3s",
}

// TestProjectTokenHeaders (task 234, spec 010): with a project tpm the
// openai driver adds x-ratelimit-{limit,remaining,reset}-project-tokens to
// the body, the stream and the 429, next to the six usual headers; without
// it they are absent (the sample values of the rate-limits guide; not
// conferred on the real, spec 006 "Não conferidos no real").
func TestProjectTokenHeaders(t *testing.T) {
	rl := &core.RateLimitState{
		Present: true, RequestsLimit: 60, RequestsRemaining: 59, RequestsReset: time.Second,
		TokensLimit: 150000, TokensRemaining: 149984, TokensReset: 6 * time.Minute,
		ProjectTokensLimit: 60000, ProjectTokensRemain: 57000, ProjectTokensReset: 3 * time.Second,
	}
	resp := &core.Response{ID: "chatcmpl-x", Model: "gpt-4o-mini", Created: time.Unix(1793449970, 0), Content: "Ok.", FinishReason: core.FinishStop}
	check := func(name string, h map[string]string, want bool) {
		t.Helper()
		for k, v := range projectTokenHeaders {
			switch {
			case want && h[k] != v:
				t.Errorf("%s: %s = %q, want %q", name, k, h[k], v)
			case !want && h[k] != "":
				t.Errorf("%s: %s = %q without a project limit", name, k, h[k])
			}
		}
		if h["x-ratelimit-limit-tokens"] != "150000" && want {
			t.Errorf("%s: tokens headers %v", name, h)
		}
	}

	w := core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, probeReq(false), resp, rl); err != nil {
		t.Fatal(err)
	}
	check("body", w.Headers(), true)

	w = core.NewMemWriter()
	s := newDriver(t).StreamWriter(w, probeReq(true), rl)
	if err := s.Start(resp); err != nil {
		t.Fatal(err)
	}
	check("stream", w.Headers(), true)

	w = core.NewMemWriter()
	newDriver(t).WriteError(w, probeReq(false), &core.Error{Kind: core.ErrRateLimit, Status: 429, Message: "slow down", RetryAfter: 3 * time.Second}, rl)
	check("429", w.Headers(), true)

	noProj := *rl
	noProj.ProjectTokensLimit = 0
	w = core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, probeReq(false), resp, &noProj); err != nil {
		t.Fatal(err)
	}
	check("no project", w.Headers(), false)

	w = core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, probeReq(false), resp, nil); err != nil {
		t.Fatal(err)
	}
	check("no state", w.Headers(), false)

	// Remaining never goes negative.
	neg := *rl
	neg.ProjectTokensRemain = -5
	w = core.NewMemWriter()
	if err := newDriver(t).WriteResponse(w, probeReq(false), resp, &neg); err != nil {
		t.Fatal(err)
	}
	if got := w.Headers()["x-ratelimit-remaining-project-tokens"]; got != "0" {
		t.Errorf("negative remaining = %q, want 0", got)
	}
}
