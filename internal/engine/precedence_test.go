package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// precedenceCfg makes the model's own error profile ("none", used by
// wwz-fast) fail every request with bad_gateway (502), so tests can tell it
// apart from the scenario step (rate_limit, 429) and admin chaos (500/529).
const precedenceCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
`

const precedenceProfileCfg = precedenceCfg + `  errors:
    none: {rate: 100, types: [bad_gateway]}
`

const stepErrScenario = "precedence-step-error"

func newPrecedenceEngine(t *testing.T, cfg string) *engine.Engine {
	t.Helper()
	e := newEngine(t, cfg)
	e.Scenarios.Add(&scenario.Scenario{
		Name:  stepErrScenario,
		Steps: []scenario.Step{{ID: "boom", Error: "rate_limit"}},
	})
	return e
}

func precedenceCall(t *testing.T, e *engine.Engine, testID string) (int, string, string) {
	t.Helper()
	h := map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-scenario": stepErrScenario, "x-wwz-conversation-id": "prec-" + testID}
	if testID != "" {
		h["X-Test-Id"] = testID
	}
	w := call(t, e, "openai", "/v1/chat/completions", h, `{"model":"wwz-fast","messages":[{"role":"user","content":"hi"}]}`)
	return w.StatusCode(), string(w.Body()), w.Headers()["x-wwz-step"]
}

func TestScenarioStepErrorWithoutChaos(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	st, body, _ := precedenceCall(t, e, "t-none")
	if st != 429 {
		t.Fatalf("scenario step should inject 429, got %d %s", st, body)
	}
}

func TestOneShotBeatsScenario(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	if err := e.Injector.ScheduleNext(chaos.Next{TestID: "t-one", Type: "server_error"}); err != nil {
		t.Fatal(err)
	}
	if st, body, _ := precedenceCall(t, e, "t-one"); st != 500 {
		t.Fatalf("one-shot should win over the scenario: got %d %s", st, body)
	}
	// Consumed: the next request falls back to the scenario step.
	if st, body, _ := precedenceCall(t, e, "t-one"); st != 429 {
		t.Fatalf("after the one-shot the scenario should apply: got %d %s", st, body)
	}
}

func TestTestScopedEventBeatsScenario(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	if _, err := e.Injector.Start(chaos.EventSpec{Type: "overloaded", Duration: time.Minute, TestID: "t-scoped"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, body, _ := precedenceCall(t, e, "t-scoped"); st != 529 && st != 503 {
		t.Fatalf("test-scoped event should win over the scenario: got %d %s", st, body)
	}
	// Another test id is not affected by the scoped event.
	if st, body, _ := precedenceCall(t, e, "t-other"); st != 429 {
		t.Fatalf("other test id should get the scenario error: got %d %s", st, body)
	}
}

func TestSetScopedBeatsScenario(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	e.Injector.SetScoped("t-set", &config.ErrorProfile{Rate: 100, Types: []string{"server_error"}})
	if st, body, _ := precedenceCall(t, e, "t-set"); st != 500 {
		t.Fatalf("scoped profile should win over the scenario: got %d %s", st, body)
	}
}

func TestGlobalEventBeatsScenario(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	if _, err := e.Injector.Start(chaos.EventSpec{Type: "server_error", Duration: time.Minute}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, body, _ := precedenceCall(t, e, ""); st != 500 {
		t.Fatalf("global event should win over the scenario: got %d %s", st, body)
	}
}

func TestGlobalEventThatDoesNotFireLeavesScenario(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceCfg)
	zero := 0.0
	if _, err := e.Injector.Start(chaos.EventSpec{Type: "server_error", Duration: time.Minute, Rate: &zero}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, body, _ := precedenceCall(t, e, ""); st != 429 {
		t.Fatalf("an event that does not fire must not hide the scenario: got %d %s", st, body)
	}
}

func TestScenarioBeatsModelProfile(t *testing.T) {
	e := newPrecedenceEngine(t, precedenceProfileCfg)
	st, body, step := precedenceCall(t, e, "t-prof")
	if st != 429 {
		t.Fatalf("scenario step should win over the model profile: got %d %s", st, body)
	}
	if step != "boom" {
		t.Fatalf("x-wwz-step = %q", step)
	}
	if !strings.Contains(body, "scenario-injected") {
		t.Fatalf("expected the scenario error message: %s", body)
	}
}
