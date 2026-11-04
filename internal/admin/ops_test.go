package admin

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

func TestStatsAndMetricsReset(t *testing.T) {
	s := newServer(t, fastCfg, func(o *Options) { o.PodName = "pod-a" })
	for range 3 {
		if w := chat(t, s.eng, nil); w.StatusCode() != 200 {
			t.Fatalf("chat: %d %s", w.StatusCode(), w.Body())
		}
	}
	r := do(t, s, "GET", "/admin/stats", "", "")
	want(t, r, 200)
	var st Stats
	r.json(t, &st)
	if st.Totals.Requests != 3 || st.Totals.ByStatus["200"] != 3 || st.Store != "memory" || st.Pod != "pod-a" || st.ResetAt != nil {
		t.Fatalf("stats: %+v", st)
	}
	if len(st.ByModel) != 1 || st.ByModel[0].Model != "wwz-fast" || st.ByModel[0].Driver != "openai" || st.ByModel[0].Requests != 3 || st.ByModel[0].Tokens["completion"] == 0 {
		t.Fatalf("by_model: %+v", st.ByModel)
	}
	if st.ByModel[0].E2EP99MS <= 0 {
		t.Fatalf("e2e quantile: %+v", st.ByModel[0])
	}
	if len(st.ByKey) == 0 || st.ByKey[0].Key != "test-01" || st.ByKey[0].Requests != 3 {
		t.Fatalf("by_key: %+v", st.ByKey)
	}

	r = do(t, s, "POST", "/admin/reset", "application/json", `{"scope":["metrics"]}`)
	want(t, r, 200)
	var rr ResetResult
	r.json(t, &rr)
	if len(rr.Reset) != 1 || rr.Reset[0] != "metrics" || rr.ResetAt.IsZero() {
		t.Fatalf("reset: %+v", rr)
	}
	r = do(t, s, "GET", "/admin/stats", "", "")
	r.json(t, &st)
	if st.Totals.Requests != 0 || st.ResetAt == nil || st.ByModel[0].E2EP99MS != 0 {
		t.Fatalf("after reset: %+v", st)
	}
	chat(t, s.eng, nil)
	r = do(t, s, "GET", "/admin/stats", "", "")
	r.json(t, &st)
	if st.Totals.Requests != 1 {
		t.Fatalf("after reset + 1: %+v", st.Totals)
	}
	// /metrics itself is never reset (Prometheus counters are monotonic).
	if !strings.Contains(string(do(t, s, "GET", "/metrics", "", "", "").body), `status="200"} 4`) {
		t.Fatal("metrics exposition must keep counting")
	}
}

func TestResetScopes(t *testing.T) {
	s := newServer(t, fastCfg)
	ctx := context.Background()
	acct := limits.Entity{Kind: limits.KindAccount, ID: "acct-test"}
	if err := s.eng.Store.Block(ctx, acct, "x", 0, "admin"); err != nil {
		t.Fatal(err)
	}
	chat(t, s.eng, map[string]string{"x-test-id": "t1"})
	if _, err := s.eng.Store.Attempts(ctx, "conv", "step", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.eng.Injector.ScheduleNext(chaos.Next{TestID: "t9", Type: "overloaded"}); err != nil {
		t.Fatal(err)
	}

	want(t, do(t, s, "POST", "/admin/reset", "application/json", `{"scope":["bogus"]}`), 400)
	want(t, do(t, s, "POST", "/admin/reset", "application/json", `{"scope":["limits","limits"]}`), 400)
	want(t, do(t, s, "POST", "/admin/reset", "application/json", `{"other":1}`), 400)

	r := do(t, s, "POST", "/admin/reset", "", "")
	want(t, r, 200)
	var rr ResetResult
	r.json(t, &rr)
	if strings.Join(rr.Reset, ",") != "limits,metrics" {
		t.Fatalf("default scope: %v", rr.Reset)
	}
	st, _ := s.eng.Store.Get(ctx, acct)
	if st.Status != limits.StatusSuspended {
		t.Fatalf("reset must keep blocks: %+v", st)
	}
	ks, _ := s.eng.Store.Get(ctx, limits.Entity{Kind: limits.KindKey, ID: "test-01"})
	if ks.Requests != 0 {
		t.Fatalf("reset must zero counters: %+v", ks)
	}

	want(t, do(t, s, "POST", "/admin/reset", "application/json", `{"scope":["requests","resilience","conversations","chaos"]}`), 200)
	if len(s.eng.Recorder.Last(10, nil)) != 0 || s.eng.Resilience.Report("t1") != nil || len(s.eng.Injector.PendingNext()) != 0 {
		t.Fatal("scoped reset did not clear")
	}
	if n, _ := s.eng.Store.Attempts(ctx, "conv", "step", time.Hour); n != 1 {
		t.Fatalf("conversations not reset: %d", n)
	}
}

func TestRequests(t *testing.T) {
	s := newServer(t, fastCfg)
	chat(t, s.eng, map[string]string{"x-test-id": "t1"})
	chat(t, s.eng, nil)
	r := do(t, s, "GET", "/admin/requests?last=10", "", "")
	want(t, r, 200)
	var rl RequestList
	r.json(t, &rl)
	if !rl.Enabled || len(rl.Requests) != 2 || rl.Requests[0].Model != "wwz-fast" {
		t.Fatalf("requests: %+v", rl)
	}
	r = do(t, s, "GET", "/admin/requests?test_id=t1&key=test-01", "", "")
	r.json(t, &rl)
	if len(rl.Requests) != 1 || rl.Requests[0].TestID != "t1" {
		t.Fatalf("filtered: %+v", rl.Requests)
	}
	r = do(t, s, "GET", "/admin/requests?scenario=none", "", "")
	r.json(t, &rl)
	if len(rl.Requests) != 0 {
		t.Fatalf("scenario filter: %+v", rl.Requests)
	}
	for _, q := range []string{"last=0", "last=10001", "last=x", "test_id="} {
		want(t, do(t, s, "GET", "/admin/requests?"+q, "", ""), 400)
	}
	// Without admin.debug the buffer is off.
	s = newServer(t, "server: {seed: 1}\n")
	r = do(t, s, "GET", "/admin/requests", "", "")
	want(t, r, 200)
	r.json(t, &rl)
	if rl.Enabled || rl.Requests == nil || len(rl.Requests) != 0 {
		t.Fatalf("disabled: %+v (%s)", rl, r.body)
	}
}

const scnA = `name: a
description: first
steps:
  - id: s1
    content: hello
`

const scnB = `name: b
steps:
  - id: s1
    content: bye
`

func TestScenarios(t *testing.T) {
	s := newServer(t, fastCfg)
	r := do(t, s, "GET", "/admin/scenarios", "", "")
	want(t, r, 200)
	if strings.TrimSpace(string(r.body)) != `{"scenarios":[]}` {
		t.Fatalf("empty list: %s", r.body)
	}
	r = do(t, s, "POST", "/admin/scenarios", "application/yaml", scnA+"---\n"+scnB)
	want(t, r, 200)
	var loaded struct{ Loaded, Replaced []string }
	r.json(t, &loaded)
	if strings.Join(loaded.Loaded, ",") != "a,b" || len(loaded.Replaced) != 0 {
		t.Fatalf("loaded: %+v", loaded)
	}
	r = do(t, s, "POST", "/admin/scenarios", "application/x-yaml", "---\n"+scnA)
	want(t, r, 200)
	r.json(t, &loaded)
	if strings.Join(loaded.Replaced, ",") != "a" {
		t.Fatalf("replaced: %+v", loaded)
	}
	r = do(t, s, "GET", "/admin/scenarios", "", "")
	var list struct{ Scenarios []ScenarioSummary }
	r.json(t, &list)
	if len(list.Scenarios) != 2 || list.Scenarios[0].Name != "a" || list.Scenarios[0].Steps != 1 || list.Scenarios[0].Description != "first" || list.Scenarios[0].OnNoMatch == "" {
		t.Fatalf("list: %+v", list)
	}
	// Any invalid document: nothing applied.
	r = do(t, s, "POST", "/admin/scenarios", "application/yaml", "name: c\nsteps: [{id: x, content: y}]\n---\nname: d\n")
	want(t, r, 400)
	if s.eng.Scenarios.Get("c") != nil {
		t.Fatal("partial load applied")
	}
	want(t, do(t, s, "POST", "/admin/scenarios", "application/yaml", scnA+"---\n"+scnA), 400)
	want(t, do(t, s, "POST", "/admin/scenarios", "application/yaml", "---\n"), 400)
	want(t, do(t, s, "POST", "/admin/scenarios", "application/json", scnA), 415)

	want(t, do(t, s, "DELETE", "/admin/scenarios?name=a", "", ""), 204)
	want(t, do(t, s, "DELETE", "/admin/scenarios?name=a", "", ""), 404)
	want(t, do(t, s, "DELETE", "/admin/scenarios", "", ""), 400)
	if names := s.eng.Scenarios.Names(); len(names) != 1 || names[0] != "b" {
		t.Fatalf("names: %v", names)
	}
}

func TestChaosEventsAndNext(t *testing.T) {
	s := newServer(t, fastCfg)
	r := do(t, s, "POST", "/admin/chaos", "application/json", `{"type":"overloaded","duration":"30s","models":["wwz-*"],"retry_after":"3s"}`)
	want(t, r, 201)
	var ev ChaosEvent
	r.json(t, &ev)
	if ev.ID == "" || ev.Type != "overloaded" || ev.Rate != 100 || ev.Until.Sub(ev.Started) != 30*time.Second || ev.Models[0] != "wwz-*" {
		t.Fatalf("event: %+v", ev)
	}
	if w := chat(t, s.eng, nil); w.StatusCode() != 503 {
		t.Fatalf("chaos event not applied: %d", w.StatusCode())
	}
	r = do(t, s, "POST", "/admin/chaos", "application/json", `{"profile":{"rate":100,"types":["bad_gateway"],"retry_after":"2s"},"duration":"1m","test_id":"t1"}`)
	want(t, r, 201)
	r.json(t, &ev)
	prof, _ := ev.Profile.(map[string]any)
	if ev.TestID != "t1" || prof["retry_after"] != "2s" || prof["rate"] == nil {
		t.Fatalf("profile event: %+v", ev)
	}
	if w := chat(t, s.eng, map[string]string{"x-test-id": "t1"}); w.StatusCode() != 502 {
		t.Fatalf("scoped event: %d", w.StatusCode())
	}

	r = do(t, s, "POST", "/admin/chaos/next", "application/json", `{"test_id":"t2","status":529,"type":"overloaded","message":"busy","retry_after":"4s","body":"{}","code":"c"}`)
	want(t, r, 201)
	var nx ChaosNextRequest
	r.json(t, &nx)
	if nx.TestID != "t2" || nx.Status != 529 || nx.RetryAfter == nil || time.Duration(*nx.RetryAfter) != 4*time.Second || nx.Body != "{}" {
		t.Fatalf("next: %+v", nx)
	}

	r = do(t, s, "GET", "/admin/chaos", "", "")
	want(t, r, 200)
	var list struct {
		Events      []ChaosEvent
		PendingNext []ChaosNextRequest `json:"pending_next"`
	}
	r.json(t, &list)
	if len(list.Events) != 2 || len(list.PendingNext) != 1 || list.PendingNext[0].TestID != "t2" {
		t.Fatalf("list: %s", r.body)
	}
	if w := chat(t, s.eng, map[string]string{"x-test-id": "t2"}); w.StatusCode() != 529 {
		t.Fatalf("one-shot: %d", w.StatusCode())
	}

	bad := []string{
		``,
		`{"type":"overloaded"}`,
		`{"duration":"30s"}`,
		`{"type":"overloaded","profile":{"rate":1},"duration":"30s"}`,
		`{"type":"nope","duration":"30s"}`,
		`{"type":"overloaded","duration":"forever"}`,
		`{"type":"overloaded","duration":30}`,
		`{"type":"overloaded","duration":"30s","rate":101}`,
		`{"type":"overloaded","duration":"30s","test_id":""}`,
		`{"type":"overloaded","duration":"30s","extra":1}`,
		`{"profile":{"rate":"x"},"duration":"30s"}`,
		`{"profile":{"nope":1},"duration":"30s"}`,
		`{"profile":[1],"duration":"30s"}`,
	}
	for _, b := range bad {
		want(t, do(t, s, "POST", "/admin/chaos", "application/json", b), 400)
	}
	for _, b := range []string{``, `{}`, `{"test_id":"t"}`, `{"test_id":"t","status":42}`, `{"test_id":"t","type":"nope"}`, `{"test_id":"t","status":500,"x":1}`} {
		want(t, do(t, s, "POST", "/admin/chaos/next", "application/json", b), 400)
	}

	want(t, do(t, s, "DELETE", "/admin/chaos?test_id=", "", ""), 400)
	r = do(t, s, "DELETE", "/admin/chaos?id="+list.Events[0].ID, "", "")
	want(t, r, 200)
	var stopped struct{ Stopped int }
	r.json(t, &stopped)
	if stopped.Stopped != 1 {
		t.Fatalf("stopped by id: %+v", stopped)
	}
	r = do(t, s, "DELETE", "/admin/chaos", "", "")
	r.json(t, &stopped)
	if stopped.Stopped != 1 {
		t.Fatalf("stopped all: %+v", stopped)
	}
	if w := chat(t, s.eng, nil); w.StatusCode() != 200 {
		t.Fatalf("after stop: %d", w.StatusCode())
	}
}

func TestEntities(t *testing.T) {
	s := newServer(t, fastCfg)
	r := do(t, s, "GET", "/admin/entities/key/test-01", "", "")
	want(t, r, 200)
	if !strings.Contains(string(r.body), `"entity":{"kind":"key","id":"test-01"}`) || strings.Contains(string(r.body), `"until"`) {
		t.Fatalf("entity json: %s", r.body)
	}
	r = do(t, s, "POST", "/admin/entities/ip/10.1.2.3/block", "application/json", `{"reason":"abuse","duration":"1m"}`)
	want(t, r, 200)
	var st limits.EntityState
	r.json(t, &st)
	if st.Status != limits.StatusSuspended || st.Reason != "abuse" || st.By != "admin" || st.Until.IsZero() {
		t.Fatalf("block: %+v", st)
	}
	if w := chat(t, s.eng, nil); w.StatusCode() < 400 {
		t.Fatalf("blocked ip must be refused: %d", w.StatusCode())
	}
	r = do(t, s, "GET", "/admin/entities?status=suspended", "", "")
	want(t, r, 200)
	var list struct{ Entities []limits.EntityState }
	r.json(t, &list)
	if len(list.Entities) != 1 || list.Entities[0].Entity.ID != "10.1.2.3" {
		t.Fatalf("list: %s", r.body)
	}
	r = do(t, s, "POST", "/admin/entities/ip/10.1.2.3/unblock", "", "")
	r.json(t, &st)
	if st.Status != limits.StatusActive {
		t.Fatalf("unblock: %+v", st)
	}
	r = do(t, s, "POST", "/admin/entities/user/u%2F1/ban", "application/json", `{"reason":"tos","by":"panel:ana"}`)
	want(t, r, 200)
	r.json(t, &st)
	if st.Status != limits.StatusBanned || st.By != "panel:ana" || st.Entity.ID != "u/1" {
		t.Fatalf("ban: %+v", st)
	}
	r = do(t, s, "POST", "/admin/entities/user/u%2F1/unban", "", "")
	r.json(t, &st)
	if st.Status != limits.StatusActive {
		t.Fatalf("unban: %+v", st)
	}
	r = do(t, s, "POST", "/admin/entities/account/acct-test/credits", "application/json", `{"usd":-2.5}`)
	want(t, r, 200)
	r.json(t, &st)
	if st.Credits == nil || *st.Credits != -2.5 {
		t.Fatalf("credits: %+v", st)
	}

	r = do(t, s, "GET", "/admin/entities?limit=1", "", "")
	r.json(t, &list)
	if len(list.Entities) != 1 {
		t.Fatalf("limit: %s", r.body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/entities/bogus/x", ""},
		{"GET", "/admin/entities?status=weird", ""},
		{"GET", "/admin/entities?limit=0", ""},
		{"POST", "/admin/entities/ip/1.2.3.4/block", `{"duration":"-1s"}`},
		{"POST", "/admin/entities/ip/1.2.3.4/block", `{"duration":"x"}`},
		{"POST", "/admin/entities/ip/1.2.3.4/ban", `{"what":1}`},
		{"POST", "/admin/entities/ip/1.2.3.4/credits", `{}`},
		{"POST", "/admin/entities/ip/1.2.3.4/credits", `{"usd":"x"}`},
	} {
		want(t, do(t, s, c.method, c.path, "application/json", c.body), 400)
	}
	want(t, do(t, s, "PUT", "/admin/entities/ip/1.2.3.4/block", "", ""), 405)
	r = do(t, s, "GET", "/admin/entities", "", "")
	if !strings.Contains(string(r.body), `"entities":[`) {
		t.Fatalf("entities must be an array: %s", r.body)
	}
}

const tierCfg = fastCfg + `
tiers:
  tight: {limits: {rpm: 1}}
`

// policyServer wires a limits.PolicyApplier like cmd/wwz serve does.
func policyServer(t *testing.T) (*Server, *limits.PolicyApplier) {
	t.Helper()
	pol := &limits.PolicyApplier{Log: quiet}
	s := newServer(t, tierCfg, func(o *Options) { o.Override = pol.Override })
	s.OnReload(pol.Committed)
	pol.Current, pol.Apply = s.Config, s.Apply
	s.eng.EnablePromotion(pol)
	s.eng.Store.(limits.PolicySource).WatchPolicies(pol.Offer)
	return s, pol
}

// TestEntityTierAndReset covers POST /admin/entities/{tipo}/{id}/tier and
// /reset (ADR 0025): auth, validation, the tier changing the limit applied
// and a reset touching only its entity.
func TestEntityTierAndReset(t *testing.T) {
	s, pol := policyServer(t)
	for _, p := range []string{"/admin/entities/key/test-01/tier", "/admin/entities/key/test-01/reset"} {
		want(t, do(t, s, "POST", p, "application/json", `{"tier":"tight"}`, "wrong"), 401)
		want(t, do(t, s, "POST", p, "application/json", `{"tier":"tight"}`, ""), 401)
	}
	for _, c := range []struct{ path, body string }{
		{"/admin/entities/ip/1.2.3.4/tier", `{"tier":"tight"}`},
		{"/admin/entities/project/p1/tier", `{"tier":"tight"}`},
		{"/admin/entities/key/nobody/tier", `{"tier":"tight"}`},
		{"/admin/entities/bogus/x/tier", `{"tier":"tight"}`},
		{"/admin/entities/key/test-01/tier", ``},
		{"/admin/entities/key/test-01/tier", `{}`},
		{"/admin/entities/key/test-01/tier", `{"tier":1}`},
		{"/admin/entities/key/test-01/tier", `{"tier":"nope"}`},
		{"/admin/entities/key/test-01/tier", `{"tier":"tight","x":1}`},
		{"/admin/entities/bogus/x/reset", ``},
		{"/admin/entities/key/test-01/reset", `{"scope":[]}`},
		{"/admin/entities/key/test-01/reset", `{"scope":["all"]}`},
		{"/admin/entities/key/test-01/reset", `{"scope":["counters","counters"]}`},
		{"/admin/entities/key/test-01/reset", `{"scope":"counters"}`},
	} {
		r := do(t, s, "POST", c.path, "application/json", c.body)
		if r.code != 400 {
			t.Errorf("POST %s %s = %d %s, want 400", c.path, c.body, r.code, r.body)
		}
	}

	twice := func() int {
		t.Helper()
		chat(t, s.eng, nil)
		return chat(t, s.eng, nil).StatusCode()
	}
	if code := twice(); code != 200 {
		t.Fatalf("default tier has no rpm: %d", code)
	}
	r := do(t, s, "POST", "/admin/entities/key/test-01/tier", "application/json", `{"tier":"tight"}`)
	want(t, r, 200)
	var st limits.EntityState
	r.json(t, &st)
	// tipo key is resolved to the key's account (ADR 0051 §5).
	if st.Tier != "tight" || st.Entity != (limits.Entity{Kind: limits.KindAccount, ID: "acct-test"}) || pol.Version() != 1 {
		t.Fatalf("tier: %+v (policies v%d)", st, pol.Version())
	}
	if pol.AccountTier("acct-test") != "tight" || s.Config().Keys[0].Tier != "default" {
		t.Fatalf("effective account tier %q, key %+v", pol.AccountTier("acct-test"), s.Config().Keys[0])
	}
	if code := twice(); code != 429 {
		t.Fatalf("tight tier must limit to 1 rpm: %d", code)
	}

	// Reset of another entity changes nothing; reset of the key frees it.
	r = do(t, s, "POST", "/admin/entities/key/test-02/reset", "", "")
	want(t, r, 200)
	if w := chat(t, s.eng, nil); w.StatusCode() != 429 {
		t.Fatalf("reset of test-02 freed test-01: %d", w.StatusCode())
	}
	if _, err := s.eng.Store.Strike(context.Background(), limits.Entity{Kind: limits.KindKey, ID: "test-01"}, "x", time.Hour); err != nil {
		t.Fatal(err)
	}
	r = do(t, s, "POST", "/admin/entities/key/test-01/reset", "application/json", `{"scope":["strikes"]}`)
	want(t, r, 200)
	r.json(t, &st)
	if st.Strikes != 0 || st.Tier != "tight" || st.Requests == 0 {
		t.Fatalf("strikes reset: %+v", st)
	}
	if w := chat(t, s.eng, nil); w.StatusCode() != 429 {
		t.Fatalf("strikes reset must keep counters: %d", w.StatusCode())
	}
	r = do(t, s, "POST", "/admin/entities/key/test-01/reset", "application/json", `{"scope":["counters","strikes"]}`)
	want(t, r, 200)
	r.json(t, &st)
	if st.Requests != 0 || st.Tier != "tight" {
		t.Fatalf("counters reset: %+v", st)
	}
	// The rpm of the tier counts on the account (ADR 0051, task 189): the
	// key's reset alone does not free it.
	if w := chat(t, s.eng, nil); w.StatusCode() != 429 {
		t.Fatalf("key reset freed the account's rpm: %d", w.StatusCode())
	}
	want(t, do(t, s, "POST", "/admin/entities/account/acct-test/reset", "application/json", `{"scope":["counters"]}`), 200)
	if w := chat(t, s.eng, nil); w.StatusCode() != 200 {
		t.Fatalf("after reset: %d %s", w.StatusCode(), w.Body())
	}

	// Patches and reloads keep the policies on top; null removes the tier.
	want(t, do(t, s, "POST", "/admin/config", "application/json", `{"server":{"seed":7}}`), 200)
	if pol.AccountTier("acct-test") != "tight" || s.Config().Server.Seed != 7 {
		t.Fatalf("patch lost the entity tier: %q", pol.AccountTier("acct-test"))
	}
	if code := twice(); code != 429 {
		t.Fatalf("patch lost the tight tier: %d", code)
	}
	want(t, do(t, s, "POST", "/admin/entities/account/acct-test/reset", "", ""), 200)
	r = do(t, s, "POST", "/admin/entities/key/test-01/tier", "application/json", `{"tier":null}`)
	want(t, r, 200)
	st = limits.EntityState{}
	r.json(t, &st)
	if st.Tier != "" || pol.AccountTier("acct-test") != "" || pol.Version() != 2 {
		t.Fatalf("remove: %+v account tier %q", st, pol.AccountTier("acct-test"))
	}
	if code := twice(); code != 200 {
		t.Fatalf("default tier again: %d", code)
	}
	want(t, do(t, s, "POST", "/admin/entities/key/test-01/tier", "application/json", `{"tier":""}`), 200)

	// Stores without the operations answer 503.
	s.eng.Store = failStore{Store: limits.NewMemory()}
	for _, c := range []struct{ path, body string }{
		{"/admin/entities/key/test-01/tier", `{"tier":"tight"}`},
		{"/admin/entities/key/test-01/reset", ``},
	} {
		r := do(t, s, "POST", c.path, "application/json", c.body)
		want(t, r, 503)
		var p Problem
		r.json(t, &p)
		if p.Error != "store_unavailable" {
			t.Fatalf("unsupported: %+v", p)
		}
	}
}

func TestLeak(t *testing.T) {
	s := newServer(t, fastCfg)
	r := do(t, s, "POST", "/admin/leak", "application/json", `{"label":"test-02","reason":"found in public repository"}`)
	want(t, r, 200)
	var lr LeakResult
	r.json(t, &lr)
	if lr.Key.Status != limits.StatusBanned || lr.Key.Entity.ID != "test-02" || lr.Account == nil || lr.Account.Status != limits.StatusSuspended || lr.Account.Entity.ID != "acct-test" {
		t.Fatalf("leak: %s", r.body)
	}
	r = do(t, s, "POST", "/admin/leak", "application/json", `{"key":"wwz-key-03"}`)
	want(t, r, 200)
	r.json(t, &lr)
	if lr.Key.Entity.ID != "test-03" || lr.Key.Reason != "leaked" {
		t.Fatalf("leak by secret: %+v", lr.Key)
	}
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"label":"nope"}`), 404)
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"reason":"x"}`), 400)
	want(t, do(t, s, "POST", "/admin/leak", "application/json", ``), 400)
	want(t, do(t, s, "POST", "/admin/leak", "application/json", `{"label":1}`), 400)

	// A key without an account only gets banned.
	s = newServer(t, "keys:\n  - {key: k1, label: l1}\n")
	s.eng.Config().Keys[0].Account = ""
	r = do(t, s, "POST", "/admin/leak", "application/json", `{"label":"l1"}`)
	want(t, r, 200)
	if strings.Contains(string(r.body), `"account"`) {
		t.Fatalf("account must be absent: %s", r.body)
	}
}

func TestResilienceAndConversations(t *testing.T) {
	s := newServer(t, fastCfg)
	chat(t, s.eng, map[string]string{"x-test-id": "rt"})
	r := do(t, s, "GET", "/admin/resilience?test_id=rt", "", "")
	want(t, r, 200)
	var rep engine.TestReport
	r.json(t, &rep)
	if rep.TestID != "rt" || rep.Requests != 1 || rep.Statuses[200] != 1 {
		t.Fatalf("report: %s", r.body)
	}
	want(t, do(t, s, "GET", "/admin/resilience?test_id=unknown", "", ""), 404)
	want(t, do(t, s, "GET", "/admin/resilience?test_id=", "", ""), 400)
	want(t, do(t, s, "GET", "/admin/resilience?limit=0", "", ""), 400)
	r = do(t, s, "GET", "/admin/resilience?limit=5", "", "")
	want(t, r, 200)
	var list struct{ Tests []engine.TestReport }
	r.json(t, &list)
	if len(list.Tests) != 1 {
		t.Fatalf("list: %s", r.body)
	}

	s = newServer(t, fastCfg+"resilience: {track: false}\n")
	chat(t, s.eng, map[string]string{"x-test-id": "rt"})
	r = do(t, s, "GET", "/admin/resilience", "", "")
	want(t, r, 200)
	if strings.TrimSpace(string(r.body)) != `{"tests":[]}` {
		t.Fatalf("track off list: %s", r.body)
	}
	want(t, do(t, s, "GET", "/admin/resilience?test_id=rt", "", ""), 404)

	ctx := context.Background()
	for _, c := range []string{"c1", "c2"} {
		if _, err := s.eng.Store.Attempts(ctx, c, "s", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	r = do(t, s, "POST", "/admin/conversations/reset", "application/json", `{"conversation_id":"c1"}`)
	want(t, r, 200)
	if strings.TrimSpace(string(r.body)) != `{"reset":1}` {
		t.Fatalf("reset one: %s", r.body)
	}
	if n, _ := s.eng.Store.Attempts(ctx, "c1", "s", time.Hour); n != 1 {
		t.Fatalf("c1 not reset: %d", n)
	}
	r = do(t, s, "POST", "/admin/conversations/reset", "", "")
	want(t, r, 200)
	if strings.TrimSpace(string(r.body)) != `{"reset":2}` {
		t.Fatalf("reset all: %s", r.body)
	}
	want(t, do(t, s, "POST", "/admin/conversations/reset", "application/json", `{"conversation_id":""}`), 400)
	want(t, do(t, s, "POST", "/admin/conversations/reset", "application/json", `{"x":1}`), 400)
}

// TestOpenAPIRoutesRegistered checks that every path and method of
// cmd/wwz/openapi.yaml is routed by the mux (and nothing answers 404/405).
func TestOpenAPIRoutesRegistered(t *testing.T) {
	b, err := os.ReadFile("../../cmd/wwz/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) < 15 {
		t.Fatalf("contract has only %d paths", len(doc.Paths))
	}
	s := newServer(t, fastCfg)
	param := regexp.MustCompile(`\{([^}]+)\}`)
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}
	n := 0
	for p, ops := range doc.Paths {
		target := param.ReplaceAllStringFunc(p, func(m string) string {
			if m == "{tipo}" {
				return "key"
			}
			return "sample"
		})
		for m := range ops {
			if !methods[m] {
				continue
			}
			n++
			req := httptest.NewRequest(strings.ToUpper(m), target, nil)
			_, pattern := s.mux.Handler(req)
			if pattern == "" || pattern == "/" {
				t.Errorf("%s %s is in the contract but not routed", strings.ToUpper(m), p)
				continue
			}
			if !strings.HasPrefix(pattern, strings.ToUpper(m)+" ") {
				t.Errorf("%s %s routed to %q", strings.ToUpper(m), p, pattern)
			}
		}
	}
	if n < 25 {
		t.Fatalf("only %d operations checked", n)
	}
	// And the other way round: every pattern registered on the mux (read
	// from routes() in admin.go) is in the contract with its method.
	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatal(err)
	}
	reg := regexp.MustCompile(`m\.Handle(?:Func)?\("([A-Z]+) ([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(reg) < 25 {
		t.Fatalf("only %d registered patterns found", len(reg))
	}
	for _, r := range reg {
		ops, ok := doc.Paths[r[2]]
		if !ok {
			t.Errorf("%s %s routed but not in the contract", r[1], r[2])
			continue
		}
		if _, ok := ops[strings.ToLower(r[1])]; !ok {
			t.Errorf("%s %s routed but the contract has no such method", r[1], r[2])
		}
	}
}

func TestParseExposition(t *testing.T) {
	text := "# HELP a x\n# TYPE a counter\na{k=\"v\\\"q\\\\\\n\",z=\"1\"} 3\n# TYPE g gauge\ng 5\nbad line\nb{k=\"x\"} nan?\nc{k=\"x\" 1\n\nd{k=x} 1\n"
	got := parseExposition([]byte(text))
	if len(got) != 2 {
		t.Fatalf("samples: %+v", got)
	}
	if got[0].labels["k"] != "v\"q\\\n" || got[0].labels["z"] != "1" || got[0].value != 3 || got[0].gauge {
		t.Fatalf("a: %+v", got[0])
	}
	if got[1].name != "g" || !got[1].gauge || got[1].value != 5 {
		t.Fatalf("g: %+v", got[1])
	}
	pts := []bucketPoint{{0.1, 0}, {1, 10}, {math.Inf(1), 10}}
	if q := quantileMS(pts, math.NaN(), 0.5); q < 549 || q > 551 {
		t.Fatalf("quantile: %v", q)
	}
	if q := quantileMS([]bucketPoint{{1, 0}, {math.Inf(1), 4}}, math.NaN(), 0.5); q != 1000 {
		t.Fatalf("inf bucket: %v", q)
	}
	if quantileMS(nil, 1, 0.5) != 0 || quantileMS([]bucketPoint{{1, 0}}, 0, 0.5) != 0 {
		t.Fatal("empty quantile")
	}
}

func TestDurationJSON(t *testing.T) {
	d := Duration(1500 * time.Millisecond)
	b, _ := d.MarshalJSON()
	if string(b) != `"1.5s"` {
		t.Fatalf("marshal: %s", b)
	}
	if err := d.UnmarshalJSON([]byte(`5`)); err == nil {
		t.Fatal("number must fail")
	}
}
