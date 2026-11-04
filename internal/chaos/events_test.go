package chaos

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func rng() *generate.RNG { return generate.NewRNG(1, "t") }

func TestEventGlobalAppliesToModelsUntilExpiry(t *testing.T) {
	in := New()
	now := time.Now()
	ev, err := in.Start(EventSpec{Type: "overloaded", Duration: time.Minute, Models: []string{"m-*"}, RetryAfter: 7 * time.Second}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID == "" || ev.Rate != 100 || !ev.Until.Equal(now.Add(time.Minute)) {
		t.Fatalf("event: %+v", ev)
	}
	d := in.Decide(nil, "m-1", "", rng(), now)
	if d.Pre == nil || d.Pre.Kind != core.ErrOverloaded || d.Pre.RetryAfter != 7*time.Second || d.Source != "admin" {
		t.Fatalf("decision: %+v", d)
	}
	if d := in.Decide(nil, "other", "", rng(), now); d.Pre != nil {
		t.Fatalf("model not matched must not fail: %+v", d)
	}
	if d := in.Decide(nil, "m-1", "", rng(), now.Add(2*time.Minute)); d.Pre != nil {
		t.Fatalf("expired event must not apply: %+v", d)
	}
	if got := in.Events(now.Add(2 * time.Minute)); len(got) != 0 {
		t.Fatalf("expired event listed: %+v", got)
	}
}

func TestEventScopedByTestIDWins(t *testing.T) {
	in := New()
	now := time.Now()
	if _, err := in.Start(EventSpec{Type: "rate_limit", Duration: time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Start(EventSpec{Profile: &config.ErrorProfile{Rate: 100, Types: []string{"bad_gateway"}}, Duration: time.Minute, TestID: "t1"}, now); err != nil {
		t.Fatal(err)
	}
	if d := in.Decide(nil, "m", "t1", rng(), now); d.Pre == nil || d.Pre.Kind != core.ErrBadGateway {
		t.Fatalf("scoped: %+v", d.Pre)
	}
	if d := in.Decide(nil, "m", "t2", rng(), now); d.Pre == nil || d.Pre.Kind != core.ErrRateLimit {
		t.Fatalf("global: %+v", d.Pre)
	}
	if n := in.Stop("", "t1"); n != 1 {
		t.Fatalf("stop by test id: %d", n)
	}
	if n := in.Stop("", ""); n != 1 {
		t.Fatalf("stop all: %d", n)
	}
	if d := in.Decide(nil, "m", "t1", rng(), now); d.Pre != nil {
		t.Fatalf("after stop: %+v", d)
	}
}

func TestEventZeroRateAndStopByID(t *testing.T) {
	in := New()
	now := time.Now()
	zero := 0.0
	ev, err := in.Start(EventSpec{Type: "server_error", Duration: time.Minute, Rate: &zero}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d := in.Decide(&config.ErrorProfile{Rate: 100, Types: []string{"quota"}}, "m", "", rng(), now); d.Pre != nil {
		t.Fatalf("rate 0 event overrides profile: %+v", d.Pre)
	}
	if n := in.Stop("nope", ""); n != 0 {
		t.Fatalf("unknown id stopped %d", n)
	}
	if n := in.Stop(ev.ID, ""); n != 1 {
		t.Fatalf("stop id: %d", n)
	}
	if d := in.Decide(&config.ErrorProfile{Rate: 100, Types: []string{"quota"}}, "m", "", rng(), now); d.Pre == nil || d.Pre.Kind != core.ErrQuota || d.Source != "profile" {
		t.Fatalf("profile after stop: %+v", d)
	}
}

func TestStartValidation(t *testing.T) {
	in := New()
	now := time.Now()
	bad := 150.0
	cases := []EventSpec{
		{Duration: time.Second},
		{Type: "overloaded", Profile: &config.ErrorProfile{}, Duration: time.Second},
		{Type: "overloaded"},
		{Type: "nope", Duration: time.Second},
		{Type: "overloaded", Duration: time.Second, Rate: &bad},
		{Type: "overloaded", Duration: time.Second, Models: []string{""}},
		{Type: "overloaded", Duration: time.Second, Models: []string{"["}},
		{Type: "overloaded", Duration: time.Second, RetryAfter: -1},
		{Profile: &config.ErrorProfile{Types: []string{"bogus"}}, Duration: time.Second},
		{Profile: &config.ErrorProfile{Rate: 101}, Duration: time.Second},
		{Profile: &config.ErrorProfile{MidStreamTypes: []string{"bogus"}}, Duration: time.Second},
		{Profile: &config.ErrorProfile{AfterRatio: 2}, Duration: time.Second},
		{Profile: &config.ErrorProfile{AfterN: -1}, Duration: time.Second},
	}
	for i, c := range cases {
		if _, err := in.Start(c, now); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if err := ValidateProfile(nil); err == nil {
		t.Error("nil profile must fail")
	}
	if _, err := in.Start(EventSpec{Profile: &config.ErrorProfile{Rate: 10, Types: []string{"hang"}, MidStreamTypes: []string{"stream_hang"}}, Duration: time.Second, RetryAfter: time.Second}, now); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProfileDuplicateFrames(t *testing.T) {
	if err := ValidateProfile(&config.ErrorProfile{Rate: 10, Types: []string{"duplicate_frames"}}); err != nil {
		t.Fatalf("duplicate_frames must be accepted: %v", err)
	}
	if err := ValidateProfile(&config.ErrorProfile{Rate: 10, Types: []string{"nope"}}); err == nil {
		t.Fatal("unknown type must be refused")
	}
}

func TestScheduleNextBuildsErrorAndIsConsumedOnce(t *testing.T) {
	in := New()
	err := in.ScheduleNext(Next{TestID: "t", Status: 529, Message: "boom", Code: "c1", RetryAfter: 3 * time.Second, Body: "{raw}"})
	if err != nil {
		t.Fatal(err)
	}
	if p := in.PendingNext(); len(p) != 1 || p[0].Status != 529 {
		t.Fatalf("pending: %+v", p)
	}
	d := in.Decide(nil, "m", "t", rng(), time.Now())
	e := d.Pre
	if e == nil || e.Kind != core.ErrOverloaded || e.Status != 529 || e.Message != "boom" || e.Code != "c1" || e.RetryAfter != 3*time.Second || e.Extra[ExtraRawBody] != "{raw}" {
		t.Fatalf("error: %+v", e)
	}
	if d := in.Decide(nil, "m", "t", rng(), time.Now()); d.Pre != nil {
		t.Fatal("one-shot must be consumed")
	}
	if len(in.PendingNext()) != 0 {
		t.Fatal("pending after consume")
	}
}

func TestNextErrorValidationAndInference(t *testing.T) {
	bad := []Next{{}, {TestID: "t"}, {TestID: "t", Status: 99}, {TestID: "t", Type: "nope"}, {TestID: "t", Type: "overloaded", RetryAfter: -1}}
	for i, n := range bad {
		if _, err := NextError(n); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	want := map[int]core.ErrorKind{
		400: core.ErrInvalidRequest, 401: core.ErrInvalidAPIKey, 403: core.ErrPermission, 404: core.ErrModelNotFound,
		413: core.ErrRequestTooLarge, 429: core.ErrRateLimit, 502: core.ErrBadGateway, 503: core.ErrOverloaded,
		504: core.ErrGatewayTimeout, 418: core.ErrInvalidRequest, 500: core.ErrServerError, 507: core.ErrServerError,
	}
	for st, k := range want {
		e, err := NextError(Next{TestID: "t", Status: st})
		if err != nil || e.Kind != k || e.Status != st {
			t.Errorf("status %d: %+v %v", st, e, err)
		}
	}
	e, err := NextError(Next{TestID: "t", Type: "connection_reset"})
	if err != nil || e.Transport != core.TransportReset {
		t.Fatalf("transport: %+v %v", e, err)
	}
}

func TestLegacySetNextErrorAndScopedAndReset(t *testing.T) {
	in := New()
	in.SetNextError("t", &core.Error{Kind: core.ErrQuota, Message: "q"})
	if p := in.PendingNext(); len(p) != 1 || p[0].Type != "quota" {
		t.Fatalf("pending: %+v", p)
	}
	in.SetScoped("t2", &config.ErrorProfile{Rate: 100, Types: []string{"bad_gateway"}})
	if d := in.Decide(nil, "m", "t2", rng(), time.Now()); d.Pre == nil || d.Pre.Kind != core.ErrBadGateway {
		t.Fatalf("scoped: %+v", d)
	}
	in.Reset()
	if len(in.PendingNext()) != 0 || in.Scoped("t2") != nil {
		t.Fatal("reset must drop one-shots and scoped profiles")
	}
	if !KnownType("hang") || !KnownType("account_banned") || KnownType("nope") {
		t.Fatal("KnownType")
	}
}

// TestOneShotIsolatedByTestID: /chaos/next for test A does not touch the next
// request of test B nor one without an id; only A consumes it (task 384, R21).
func TestOneShotIsolatedByTestID(t *testing.T) {
	in := New()
	now := time.Now()
	if err := in.ScheduleNext(Next{TestID: "A", Type: "overloaded"}); err != nil {
		t.Fatal(err)
	}
	if d := in.Decide(nil, "m", "B", rng(), now); d.Pre != nil {
		t.Fatalf("test B must not receive A's one-shot: %+v", d.Pre)
	}
	if d := in.Decide(nil, "m", "", rng(), now); d.Pre != nil {
		t.Fatalf("a request without id must not receive A's one-shot: %+v", d.Pre)
	}
	if p := in.PendingNext(); len(p) != 1 || p[0].TestID != "A" {
		t.Fatalf("A's one-shot must still be pending: %+v", p)
	}
	d := in.Decide(nil, "m", "A", rng(), now)
	if d.Pre == nil || d.Pre.Kind != core.ErrOverloaded || d.Source != "admin" {
		t.Fatalf("A must get its one-shot: %+v", d)
	}
	if d := in.Decide(nil, "m", "A", rng(), now); d.Pre != nil {
		t.Fatalf("one-shot consumed once: %+v", d.Pre)
	}
}
