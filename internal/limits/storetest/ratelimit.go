package storetest

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func rateLimitCases() []testCase {
	return []testCase{
		{"NoLimits", testNoLimits},
		{"RPSTokenBucket", testRPSTokenBucket},
		{"RPSBurst", testRPSBurst},
		{"RPMShortWindow", testRPMShortWindow},
		{"RPMTokenBucket", testRPMTokenBucket},
		{"RPMFixedWindow", testRPMFixedWindow},
		{"TPMReserveAndRefund", testTPMReserveAndRefund},
		{"TPMDeniedDoesNotConsume", testTPMDeniedDoesNotConsume},
		{"TPMFixedWindowRefund", testTPMFixedWindowRefund},
		{"TPMExcludeCacheRead", testTPMExcludeCacheRead},
		{"ITPM", testITPM},
		{"OTPM", testOTPM},
		{"RPMDenialReportsTokens", testRPMDenialReportsTokens},
		{"RPMDenialReportsTokensFixedWindow", testRPMDenialReportsTokensFixed},
		{"TPMDenialReportsInputOutput", testTPMDenialReportsInputOutput},
		{"InputOutputSumAsTokens", testInputOutputSum},
		{"RPDResetsAtUTCMidnight", testRPD},
		{"TPDResetsAtUTCMidnight", testTPD},
		{"ModeOff", testModeOff},
		{"ModeAfterN", testModeAfterN},
		{"ModeWindow", testModeWindow},
		{"Ramp", testRamp},
		{"RequestsCounter", testRequestsCounter},
	}
}

func testNoLimits(t T, s limits.Store) {
	d := checkAt(t, s, baseTime(), 1000, sub(keyEnt, config.Limits{}))
	mustAllow(t, d, "no limits")
	if d.State.Present {
		t.Fatalf("State.Present without limits: %+v", d.State)
	}
}

func testRPSTokenBucket(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPS: 2})
	for range 2 {
		mustAllow(t, checkAt(t, s, now, 0, ks), "rps within burst")
	}
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "rps", "third request in the same instant")
	if d.Kind != core.ErrRateLimit || d.RetryAfter <= 0 || d.RetryAfter > time.Second {
		t.Fatalf("rps denial: kind=%s retry=%v", d.Kind, d.RetryAfter)
	}
	if !d.State.Present || d.State.RequestsLimit != 2 || d.State.RequestsRemaining != 0 {
		t.Fatalf("rps state: %+v", d.State)
	}
	// 500 ms refill one token at 2/s.
	mustAllow(t, checkAt(t, s, now.Add(500*time.Millisecond), 0, ks), "after 500ms refill")
	mustDeny(t, checkAt(t, s, now.Add(500*time.Millisecond), 0, ks), keyEnt, "rps", "bucket empty again")
	// Long idle never exceeds the burst capacity.
	later := now.Add(time.Minute)
	if n := countAllowed(t, s, 5, limits.CheckRequest{Subjects: subs(ks), Now: later}); n != 2 {
		t.Fatalf("after idle allowed %d, want 2 (burst = rps)", n)
	}
}

func testRPSBurst(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPS: 1, Burst: 5})
	if n := countAllowed(t, s, 8, limits.CheckRequest{Subjects: subs(ks), Now: now}); n != 5 {
		t.Fatalf("burst allowed %d, want 5", n)
	}
	mustAllow(t, checkAt(t, s, now.Add(time.Second), 0, ks), "one token after 1s")
}

// testRPMShortWindow: Subject.Window evaluates the rpm over a short
// interval (Azure's 1 s / 10 s, task 295): 6 RPM over 10 s admit one
// request per 10 s; 600 RPM over 1 s admit 10 at once. The tier's rps
// does not apply and the denial reports the rpm.
func testRPMShortWindow(t T, s limits.Store) {
	now := baseTime()
	dep := limits.Entity{Kind: limits.KindDeployment, ID: "acme/gpt-x"}
	ds := sub(dep, config.Limits{RPM: 6, RPS: 100})
	ds.Window = 10 * time.Second
	mustAllow(t, checkAt(t, s, now, 0, ds), "first request of the window")
	d := checkAt(t, s, now.Add(time.Second), 0, ds)
	mustDeny(t, d, dep, "rps", "second request inside 10 s")
	if d.Kind != core.ErrRateLimit || d.RetryAfter < 8*time.Second || d.RetryAfter > 9*time.Second {
		t.Fatalf("window denial: kind=%s retry=%v", d.Kind, d.RetryAfter)
	}
	if d.State.RequestsLimit != 6 || d.Message != "rate limit of 6 requests per minute exceeded (evaluated every 10s)" {
		t.Fatalf("window denial: state %+v message %q", d.State, d.Message)
	}
	mustAllow(t, checkAt(t, s, now.Add(10*time.Second), 0, ds), "after 10 s")

	big := sub(limits.Entity{Kind: limits.KindDeployment, ID: "acme/gpt-y"}, config.Limits{RPM: 600})
	big.Window = time.Second
	if n := countAllowed(t, s, 15, limits.CheckRequest{Subjects: subs(big), Now: now}); n != 10 {
		t.Fatalf("600 RPM over 1 s allowed %d, want 10", n)
	}
}

func testRPMTokenBucket(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 60})
	first := checkAt(t, s, now, 0, ks)
	mustAllow(t, first, "first")
	if !first.State.Present || first.State.RequestsLimit != 60 || first.State.RequestsRemaining != 59 || first.State.RequestsReset <= 0 {
		t.Fatalf("allowed state: %+v", first.State)
	}
	if n := countAllowed(t, s, 70, limits.CheckRequest{Subjects: subs(ks), Now: now}); n != 59 {
		t.Fatalf("allowed %d more, want 59", n)
	}
	d := checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "rpm", "61st")
	if d.Kind != core.ErrRateLimit || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("rpm denial: %+v", d)
	}
	if d.State.RequestsLimit != 60 || d.State.RequestsRemaining != 0 {
		t.Fatalf("rpm denial state: %+v", d.State)
	}
	// Continuous refill: 30 s later about half the bucket is back.
	d = checkAt(t, s, now.Add(30*time.Second), 0, ks)
	mustAllow(t, d, "after 30s")
	if r := d.State.RequestsRemaining; r < 28 || r > 30 {
		t.Fatalf("remaining after 30s = %d, want ~29", r)
	}
}

func testRPMFixedWindow(t T, s limits.Store) {
	now := baseTime() // start of a minute
	ks := sub(keyEnt, config.Limits{RPM: 3, Algorithm: "fixed_window"})
	for want := 2; want >= 0; want-- {
		d := checkAt(t, s, now, 0, ks)
		mustAllow(t, d, "within window")
		if d.State.RequestsLimit != 3 || d.State.RequestsRemaining != want {
			t.Fatalf("remaining = %d, want %d (%+v)", d.State.RequestsRemaining, want, d.State)
		}
	}
	d := checkAt(t, s, now.Add(10*time.Second), 0, ks)
	mustDeny(t, d, keyEnt, "rpm", "4th in window")
	if d.RetryAfter != 50*time.Second {
		t.Fatalf("RetryAfter = %v, want 50s (until window end)", d.RetryAfter)
	}
	if d.State.RequestsRemaining != 0 || d.State.RequestsReset != 50*time.Second {
		t.Fatalf("denial state: %+v", d.State)
	}
	d = checkAt(t, s, now.Add(time.Minute), 0, ks)
	mustAllow(t, d, "next window")
	if d.State.RequestsRemaining != 2 {
		t.Fatalf("next window remaining = %d, want 2", d.State.RequestsRemaining)
	}
}

func testTPMReserveAndRefund(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{TPM: 1000})
	d := check(t, s, limits.CheckRequest{Subjects: subs(ks), EstPromptTokens: 100, EstOutputTokens: 500, Now: now})
	mustAllow(t, d, "reserve prompt + max_tokens")
	if d.State.TokensLimit != 1000 || d.State.TokensRemaining != 400 {
		t.Fatalf("reserve state: %+v", d.State)
	}
	// Only 100 of the 500 reserved output tokens were used: 400 return.
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, EstOutputTokens: 500, ActualOutput: 100, ActualPrompt: 100, Status: 200})
	d = checkAt(t, s, now, 800, ks)
	mustAllow(t, d, "800 fits after refund")
	if d.State.TokensRemaining != 0 {
		t.Fatalf("remaining after refund+800 = %d, want 0", d.State.TokensRemaining)
	}
	d = checkAt(t, s, now, 1, ks)
	mustDeny(t, d, keyEnt, "tpm", "bucket exhausted")
	if d.Kind != core.ErrRateLimit || d.RetryAfter <= 0 {
		t.Fatalf("tpm denial: %+v", d)
	}
}

func testTPMDeniedDoesNotConsume(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{TPM: 100})
	mustDeny(t, checkAt(t, s, now, 150, ks), keyEnt, "tpm", "request larger than the bucket")
	mustAllow(t, checkAt(t, s, now, 100, ks), "denied request must not consume tokens")
}

func testTPMFixedWindowRefund(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{TPM: 1000, Algorithm: "fixed_window"})
	d := check(t, s, limits.CheckRequest{Subjects: subs(ks), EstPromptTokens: 100, EstOutputTokens: 500, Now: now})
	mustAllow(t, d, "reserve")
	if d.State.TokensRemaining != 400 {
		t.Fatalf("reserve state: %+v", d.State)
	}
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, EstOutputTokens: 500, ActualOutput: 100, Status: 200})
	// spec 010: tpm reserves max_tokens and adjusts at the end, for every algorithm.
	mustAllow(t, checkAt(t, s, now, 800, ks), "800 fits after refund in fixed window")
}

func testTPMExcludeCacheRead(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{TPM: 1000, ExcludeCacheRead: true})
	d := check(t, s, limits.CheckRequest{Subjects: subs(ks), EstPromptTokens: 900, CachedTokens: 800, Now: now})
	mustAllow(t, d, "cached tokens excluded")
	if d.State.TokensRemaining != 900 {
		t.Fatalf("remaining = %d, want 900", d.State.TokensRemaining)
	}
}

func testITPM(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{ITPM: 100})
	d := checkAt(t, s, now, 60, ks)
	mustAllow(t, d, "first input")
	if d.State.InputTokensLimit != 100 || d.State.InputTokensRemain != 40 {
		t.Fatalf("itpm state: %+v", d.State)
	}
	d = checkAt(t, s, now, 60, ks)
	mustDeny(t, d, keyEnt, "itpm", "input exceeds")
	if d.Kind != core.ErrRateLimit || d.RetryAfter <= 0 {
		t.Fatalf("itpm denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now.Add(time.Minute), 60, ks), "refilled after a minute")
}

func testOTPM(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{OTPM: 100})
	d := checkAt(t, s, now, 0, ks)
	mustAllow(t, d, "headroom")
	if d.State.OutputTokensLimit != 100 || d.State.OutputTokensRem != 100 {
		t.Fatalf("otpm state: %+v", d.State)
	}
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, ActualOutput: 100, Status: 200})
	d = checkAt(t, s, now, 0, ks)
	mustDeny(t, d, keyEnt, "otpm", "output budget spent in Finish")
	if d.Kind != core.ErrRateLimit || d.RetryAfter <= 0 {
		t.Fatalf("otpm denial: %+v", d)
	}
	mustAllow(t, checkAt(t, s, now.Add(time.Minute), 0, ks), "refilled after a minute")
}

func testRPD(t T, s limits.Store) {
	midnight := nextUTCMidnight()
	before := midnight.Add(-time.Minute)
	ks := sub(keyEnt, config.Limits{RPD: 2})
	mustAllow(t, checkAt(t, s, before, 0, ks), "rpd 1")
	mustAllow(t, checkAt(t, s, before, 0, ks), "rpd 2")
	d := checkAt(t, s, before, 0, ks)
	mustDeny(t, d, keyEnt, "rpd", "rpd 3")
	if d.Kind != core.ErrRateLimit || d.RetryAfter != time.Minute {
		t.Fatalf("rpd denial: kind=%s retry=%v (want 1m until midnight UTC)", d.Kind, d.RetryAfter)
	}
	mustAllow(t, checkAt(t, s, midnight.Add(time.Minute), 0, ks), "new UTC day")
}

func testTPD(t T, s limits.Store) {
	midnight := nextUTCMidnight()
	before := midnight.Add(-time.Minute)
	ks := sub(keyEnt, config.Limits{TPD: 1000})
	mustAllow(t, checkAt(t, s, before, 600, ks), "tpd 600")
	d := checkAt(t, s, before, 600, ks)
	mustDeny(t, d, keyEnt, "tpd", "tpd 1200")
	if d.Kind != core.ErrRateLimit || d.RetryAfter != time.Minute {
		t.Fatalf("tpd denial: kind=%s retry=%v", d.Kind, d.RetryAfter)
	}
	mustAllow(t, checkAt(t, s, before, 400, ks), "denied request must not count; 1000 fits")
	mustAllow(t, checkAt(t, s, midnight, 1000, ks), "new UTC day")
}

func testModeOff(t T, s limits.Store) {
	ks := sub(keyEnt, config.Limits{RPM: 1, Mode: "off"})
	if n := countAllowed(t, s, 5, limits.CheckRequest{Subjects: subs(ks), Now: baseTime()}); n != 5 {
		t.Fatalf("mode off allowed %d, want 5", n)
	}
}

func testModeAfterN(t T, s limits.Store) {
	ks := sub(keyEnt, config.Limits{RPM: 1, Mode: "after_n", AfterN: 3})
	if n := countAllowed(t, s, 6, limits.CheckRequest{Subjects: subs(ks), Now: baseTime()}); n != 4 {
		t.Fatalf("after_n=3 with rpm=1 allowed %d, want 4 (3 free + 1)", n)
	}
}

// testModeWindow: with window {every: 1m, duration: 10s} the limits apply
// only in the first 10 s of each minute (phase from the Unix epoch).
func testModeWindow(t T, s limits.Store) {
	start := baseTime() // a whole minute, so a window start
	ks := sub(keyEnt, config.Limits{RPM: 1, Mode: "window", Window: &config.Window{Every: time.Minute, Duration: 10 * time.Second}})
	at := func(d time.Duration) limits.CheckRequest {
		return limits.CheckRequest{Subjects: subs(ks), Now: start.Add(d)}
	}
	if n := countAllowed(t, s, 3, at(5*time.Second)); n != 1 {
		t.Fatalf("inside the window allowed %d, want 1 (rpm=1)", n)
	}
	d := checkAt(t, s, start.Add(9*time.Second), 0, ks)
	mustDeny(t, d, keyEnt, "rpm", "still inside the window")
	if n := countAllowed(t, s, 5, at(30*time.Second)); n != 5 {
		t.Fatalf("outside the window allowed %d, want 5 (limits off)", n)
	}
	// Next window: the rpm token taken at +5s is back by +65s, and the
	// requests made outside the window consumed nothing.
	if n := countAllowed(t, s, 3, at(65*time.Second)); n != 1 {
		t.Fatalf("next window allowed %d, want 1", n)
	}
	// Without a window block (config validation rejects it) the mode
	// degrades to always.
	bare := sub(key2Ent, config.Limits{RPM: 1, Mode: "window"})
	if n := countAllowed(t, s, 3, limits.CheckRequest{Subjects: subs(bare), Now: start.Add(30 * time.Second)}); n != 1 {
		t.Fatalf("window mode without window allowed %d, want 1 (always)", n)
	}
}

func testRamp(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{Ramp: &config.Ramp{ThresholdTPM: 100, MaxIncreasePct: 50, Window: time.Minute}})
	mustAllow(t, checkAt(t, s, now, 100, ks), "baseline traffic")
	d := checkAt(t, s, now.Add(time.Second), 100, ks)
	mustDeny(t, d, keyEnt, "ramp", "doubling above threshold")
	if d.Kind != core.ErrSlowDown || d.RetryAfter <= 0 {
		t.Fatalf("ramp denial: %+v", d)
	}
}

func testRequestsCounter(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 3, Algorithm: "fixed_window"})
	countAllowed(t, s, 5, limits.CheckRequest{Subjects: subs(ks), Now: now})
	if st := get(t, s, keyEnt); st.Requests != 3 {
		t.Fatalf("Requests = %d, want 3 (denied requests do not count)", st.Requests)
	}
}

// testRPMDenialReportsTokens (task 064): a 429 by rpm still carries the token
// units of the denying entity (x-ratelimit-*-tokens, anthropic-ratelimit-
// {tokens,input-tokens,output-tokens}-*), read without consuming.
func testRPMDenialReportsTokens(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 1, TPM: 1000, ITPM: 500, OTPM: 300})
	d := checkAt(t, s, now, 100, ks)
	mustAllow(t, d, "first request")
	d = checkAt(t, s, now, 100, ks)
	mustDeny(t, d, keyEnt, "rpm", "second request in the same instant")
	st := d.State
	if !st.Present || st.RequestsLimit != 1 || st.RequestsRemaining != 0 {
		t.Fatalf("requests state: %+v", st)
	}
	if st.TokensLimit != 1000 || st.TokensRemaining != 900 || st.TokensReset <= 0 {
		t.Fatalf("tokens state on rpm denial: %+v", st)
	}
	if st.InputTokensLimit != 500 || st.InputTokensRemain != 400 || st.OutputTokensLimit != 300 || st.OutputTokensRem != 300 {
		t.Fatalf("input/output state on rpm denial: %+v", st)
	}
	// Peeking consumed nothing: 900 tokens are still there.
	d = checkAt(t, s, now.Add(time.Minute), 0, ks)
	mustAllow(t, d, "after a minute")
}

func testRPMDenialReportsTokensFixed(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{RPM: 1, TPM: 1000, Algorithm: "fixed_window"})
	mustAllow(t, checkAt(t, s, now, 300, ks), "first request")
	d := checkAt(t, s, now.Add(10*time.Second), 300, ks)
	mustDeny(t, d, keyEnt, "rpm", "second request in the window")
	if st := d.State; st.TokensLimit != 1000 || st.TokensRemaining != 700 || st.TokensReset != 50*time.Second {
		t.Fatalf("tokens state on fixed-window rpm denial: %+v", st)
	}
}

// testTPMDenialReportsInputOutput: a tpm denial still reports itpm and otpm.
func testTPMDenialReportsInputOutput(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{TPM: 100, ITPM: 500, OTPM: 300})
	d := checkAt(t, s, now, 150, ks)
	mustDeny(t, d, keyEnt, "tpm", "larger than tpm")
	if st := d.State; st.TokensLimit != 100 || st.InputTokensLimit != 500 || st.InputTokensRemain != 500 || st.OutputTokensLimit != 300 || st.OutputTokensRem != 300 {
		t.Fatalf("state on tpm denial: %+v", st)
	}
}

// testInputOutputSum: with only itpm/otpm, the tokens unit is input + output
// (anthropic-ratelimit-tokens-*, spec 006), on success and on denial, and
// the reset is the later of the two buckets.
func testInputOutputSum(t T, s limits.Store) {
	now := baseTime()
	ks := sub(keyEnt, config.Limits{ITPM: 600, OTPM: 300})
	d := checkAt(t, s, now, 60, ks)
	mustAllow(t, d, "first")
	if st := d.State; st.TokensLimit != 900 || st.TokensRemaining != 840 || st.TokensReset < 5900*time.Millisecond || st.TokensReset > 6*time.Second {
		t.Fatalf("summed state: %+v", st)
	}
	finish(t, s, limits.FinishRequest{Subjects: subs(ks), Now: now, ActualOutput: 150, Status: 200})
	d = checkAt(t, s, now, 0, ks)
	mustAllow(t, d, "second")
	// otpm: 150 of 300 left, full again in 30s (5 tokens/s); itpm full in 6s.
	if st := d.State; st.TokensLimit != 900 || st.TokensRemaining != 540+150 || st.TokensReset < 29900*time.Millisecond || st.TokensReset > 30*time.Second {
		t.Fatalf("summed state after output: %+v", st)
	}
	d = checkAt(t, s, now, 700, ks)
	mustDeny(t, d, keyEnt, "itpm", "input larger than itpm")
	if st := d.State; st.TokensLimit != 900 || st.OutputTokensLimit != 300 || st.OutputTokensRem != 150 {
		t.Fatalf("summed state on itpm denial: %+v", st)
	}
}
