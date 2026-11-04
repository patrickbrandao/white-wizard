package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

const scenarioYAML = `name: s1
description: um passo
steps:
  - id: a
    expect: { tool_results: 0 }
    content: { tokens: "5..10" }
`

func lastRow(t *testing.T, e *env) store.AuditEntry {
	t.Helper()
	rows, err := e.st.Audit.Query(ctx, store.AuditFilter{Limit: 1})
	if err != nil || len(rows) == 0 {
		t.Fatalf("auditoria: %v %v", rows, err)
	}
	return rows[0]
}

func (e *env) doCT(method, target, ctype, body string, c cred) resp {
	e.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return resp{code: w.Code, hdr: w.Header(), body: w.Body.Bytes()}
}

func TestTierAndReset(t *testing.T) {
	e := newEnv(t)
	var res ActionResult
	r := e.do("POST", "/api/v1/entities/key/test-02/tier", `{"tier":"gold"}`, operator)
	r.decode(t, &res)
	if r.code != 200 || res.Entity == nil || res.Entity.Tier != "gold" || res.AuditID == 0 {
		t.Fatalf("tier: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != "entity.tier" || a.Target != "key/test-02" || !strings.Contains(string(a.Diff), `"tier":"gold"`) {
		t.Fatalf("auditoria do tier: %+v", a)
	}
	res = ActionResult{}
	r = e.do("POST", "/api/v1/entities/key/test-02/tier", `{"tier":null}`, operator)
	r.decode(t, &res)
	if r.code != 200 || res.Entity.Tier != "" {
		t.Fatalf("remover tier: %d %s", r.code, r.body)
	}
	for body, want := range map[string]int{
		`{"tier":"nao-existe"}`:  400, // refused by wwz
		``:                       400, // tier required
		`{"tier":5}`:             400,
		`{"scope":["counters"]}`: 400,
	} {
		if r := e.do("POST", "/api/v1/entities/key/test-02/tier", body, operator); r.code != want {
			t.Errorf("tier %q: %d %s", body, r.code, r.body)
		}
	}
	if r := e.do("POST", "/api/v1/entities/ip/1.2.3.4/tier", `{"tier":"gold"}`, operator); r.code != 400 {
		t.Errorf("tier em ip: %d %s", r.code, r.body)
	}
	for _, body := range []string{``, `{"scope":["strikes","counters"]}`} {
		r := e.do("POST", "/api/v1/entities/key/test-01/reset", body, operator)
		r.decode(t, &res)
		if r.code != 200 || res.Entity == nil {
			t.Fatalf("reset %q: %d %s", body, r.code, r.body)
		}
	}
	if a := lastRow(t, e); a.Action != "entity.reset" || !strings.Contains(string(a.Diff), `"scope":["strikes","counters"]`) {
		t.Fatalf("auditoria do reset: %+v", a)
	}
	for _, body := range []string{`{"scope":[]}`, `{"scope":["x"]}`, `{"scope":["counters","counters"]}`, `{"tier":"gold"}`, `{"scope":"counters"}`} {
		if r := e.do("POST", "/api/v1/entities/key/test-01/reset", body, operator); r.code != 400 {
			t.Errorf("reset %q: %d %s", body, r.code, r.body)
		}
	}
	// Service layer: tier without HasTier.
	if _, err := e.api.Act(ctx, Actor{}, ActionTier, limits.Entity{Kind: limits.KindKey, ID: "test-01"}, ActionRequest{}); err == nil {
		t.Fatal("tier sem valor: want error")
	}
}

func TestErrorsView(t *testing.T) {
	// Without Prometheus: /admin/stats, chaos of the admin, samples of the
	// events table.
	e := newEnv(t)
	now := e.clk.now()
	e.insertEvents(
		store.Event{StreamID: "1", At: now.Add(-2 * time.Second), Type: store.EventError, Driver: "openai", Model: "m", Status: 429, Error: "rate_limit", Data: json.RawMessage(`{}`)},
		store.Event{StreamID: "2", At: now.Add(-time.Second), Type: store.EventError, Driver: "anthropic", Status: 529, Error: "overloaded", Data: json.RawMessage(`{}`)},
		store.Event{StreamID: "3", At: now, Type: store.EventRequest, Driver: "openai", Status: 200, Data: json.RawMessage(`{}`)},
	)
	if r := e.do("POST", "/api/v1/chaos", `{"type":"overloaded","duration":"30s"}`, operator); r.code != 201 {
		t.Fatalf("chaos: %d %s", r.code, r.body)
	}
	var v ErrorsView
	r := e.do("GET", "/api/v1/errors?"+q("samples", "1", "window", "1h"), "", operator)
	r.decode(t, &v)
	if r.code != 200 || v.Source != SourceAdminStats || v.Window != "1h" || len(v.Samples) != 1 || v.Samples[0].StreamID != "2" ||
		!strings.Contains(string(v.Chaos), "overloaded") || v.InjectedByDriver == nil || v.Simulator == nil {
		t.Fatalf("erros: %d %s", r.code, r.body)
	}
	r = e.do("GET", "/api/v1/errors?"+q("driver", "openai"), "", operator)
	r.decode(t, &v)
	if v.Window != "5m" || len(v.Samples) != 1 || v.Samples[0].StreamID != "1" {
		t.Fatalf("filtro: %s", r.body)
	}
	if r := e.do("GET", "/api/v1/errors?samples=0", "", operator); !strings.Contains(string(r.body), `"samples":[]`) {
		t.Fatalf("sem amostras: %s", r.body)
	}
	for _, qs := range []string{"window=2h", "samples=5000"} {
		if r := e.do("GET", "/api/v1/errors?"+qs, "", operator); r.code != 400 {
			t.Errorf("%s: %d", qs, r.code)
		}
	}
	// SQLite closed: 500.
	_ = e.st.Close()
	if r := e.do("GET", "/api/v1/errors", "", operator); r.code != 500 {
		t.Fatalf("banco fechado: %d %s", r.code, r.body)
	}

	// Prometheus answering, admin down: chaos {} (logged).
	p := newEnv(t, func(o *envOpts) {
		o.adminURL = deadURL(t)
		o.prom = promHandler(promRule{match: "wwz_errors_injected_total", body: vector(sample(`"driver":"openai","kind":"overloaded","phase":"pre"`, "4"))})
	})
	r = p.do("GET", "/api/v1/errors", "", operator)
	r.decode(t, &v)
	if r.code != 200 || v.Source != SourcePrometheus || v.InjectedByType["overloaded"] != 4 || string(v.Chaos) != "{}" {
		t.Fatalf("prometheus: %d %s", r.code, r.body)
	}
	if !strings.Contains(p.logs.String(), "caos da tela de erros") {
		t.Fatal("falha do caos não logada")
	}
	// Nothing answers: 502.
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("GET", "/api/v1/errors", "", operator); r.code != 502 {
		t.Fatalf("tudo fora: %d %s", r.code, r.body)
	}
}

func TestInjection(t *testing.T) {
	e := newEnv(t)
	var res InjectionResult
	r := e.do("POST", "/api/v1/injection", `{"model":"wwz-fast","errors":{"rate":30,"types":["overloaded"]}}`, operator)
	r.decode(t, &res)
	if r.code != 200 || !res.Applied || res.AuditID == 0 {
		t.Fatalf("injeção: %d %s", r.code, r.body)
	}
	a := lastRow(t, e)
	if a.Action != AuditInjection || a.Target != "model/wwz-fast" || !strings.Contains(string(a.Diff), `"before":"none"`) ||
		!strings.Contains(string(a.Diff), `"rate":30`) {
		t.Fatalf("auditoria: %+v %s", a, a.Diff)
	}
	if r := e.do("POST", "/api/v1/injection", `{"model":"wwz-fast","errors":null}`, operator); r.code != 200 {
		t.Fatalf("desligar: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/v1/injection", `{"profile":"flaky","errors":{"rate":50}}`, operator); r.code != 200 {
		t.Fatalf("perfil: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Target != "profile/flaky" || !strings.Contains(string(a.Diff), `"rate":50`) {
		t.Fatalf("auditoria do perfil: %s", a.Diff)
	}
	for _, body := range []string{
		`{"errors":{}}`, `{"model":"a","profile":"b","errors":{}}`, `{"model":"a"}`, `{"model":"a","errors":5}`, `{"x":1}`, `nada`,
		`{"model":"wwz-fast","errors":{"rate":"muito"}}`, // refused by wwz
	} {
		if r := e.do("POST", "/api/v1/injection", body, operator); r.code != 400 {
			t.Errorf("%s: %d %s", body, r.code, r.body)
		}
	}
	if a := lastRow(t, e); a.Result != store.ResultError {
		t.Fatalf("recusa do wwz não auditada como erro: %+v", a)
	}
	// Admin down: 502 and no before.
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("POST", "/api/v1/injection", `{"model":"m","errors":null}`, operator); r.code != 502 {
		t.Fatalf("admin fora: %d %s", r.code, r.body)
	}
	if injectionTarget(json.RawMessage(`{}`), InjectionRequest{Model: "x"}) != nil {
		t.Fatal("alvo ausente")
	}
}

func TestChaos(t *testing.T) {
	e := newEnv(t)
	var st ChaosStarted
	r := e.do("POST", "/api/v1/chaos", `{"type":"overloaded","duration":"30s","test_id":"t1"}`, operator)
	r.decode(t, &st)
	if r.code != 201 || st.AuditID == 0 || !strings.Contains(string(st.Event), `"id"`) {
		t.Fatalf("start: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != AuditChaosStart || a.Target != "chaos" {
		t.Fatalf("auditoria: %+v", a)
	}
	r = e.do("GET", "/api/v1/chaos", "", operator)
	if r.code != 200 || !strings.Contains(string(r.body), "overloaded") {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	for _, body := range []string{`[1]`, `{"duration":"30s"}`} {
		if r := e.do("POST", "/api/v1/chaos", body, operator); r.code != 400 {
			t.Errorf("%s: %d %s", body, r.code, r.body)
		}
	}
	var sp ChaosStopped
	r = e.do("DELETE", "/api/v1/chaos?"+q("test_id", "t1", "id", "nao"), "", operator)
	r.decode(t, &sp)
	if r.code != 200 || sp.AuditID == 0 {
		t.Fatalf("stop: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != AuditChaosStop || !strings.Contains(string(a.Diff), `"test_id":"t1"`) {
		t.Fatalf("auditoria do stop: %s", a.Diff)
	}
	r = e.do("DELETE", "/api/v1/chaos", "", operator)
	r.decode(t, &sp)
	if r.code != 200 {
		t.Fatalf("stop all: %d %s", r.code, r.body)
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	for _, m := range []string{"GET", "DELETE"} {
		if r := d.do(m, "/api/v1/chaos", "", operator); r.code != 502 {
			t.Errorf("%s admin fora: %d", m, r.code)
		}
	}
	if r := d.do("POST", "/api/v1/chaos", `{"type":"overloaded","duration":"1s"}`, operator); r.code != 502 {
		t.Errorf("POST admin fora: %d", r.code)
	}
	big := `{"x":"` + strings.Repeat("a", maxActionBody) + `"}`
	if r := e.do("POST", "/api/v1/chaos", big, operator); r.code != 400 {
		t.Errorf("corpo grande: %d", r.code)
	}
}

func TestScenarios(t *testing.T) {
	e := newEnv(t)
	var ld ScenariosLoaded
	r := e.doCT("POST", "/api/v1/scenarios", "application/yaml; charset=utf-8", scenarioYAML, operator)
	r.decode(t, &ld)
	if r.code != 200 || len(ld.Loaded) != 1 || ld.Loaded[0] != "s1" || ld.AuditID == 0 {
		t.Fatalf("load: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != AuditScenarioAdd || a.Target != "scenario/s1" {
		t.Fatalf("auditoria: %+v", a)
	}
	// "simular": invalid YAML → 400 with the messages of wwz, audited.
	r = e.doCT("POST", "/api/v1/scenarios", "application/x-yaml", "name: s2\nsteps: []\n", operator)
	if r.code != 400 || len(r.problem(t).Details) == 0 {
		t.Fatalf("inválido: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Result != store.ResultError || a.Target != "scenarios" {
		t.Fatalf("auditoria do inválido: %+v", a)
	}
	if r := e.doCT("POST", "/api/v1/scenarios", "application/json", "{}", operator); r.code != 415 {
		t.Fatalf("content-type: %d", r.code)
	}
	if r := e.doCT("POST", "/api/v1/scenarios", "application/yaml", "  \n", operator); r.code != 400 {
		t.Fatalf("vazio: %d", r.code)
	}
	if r := e.doCT("POST", "/api/v1/scenarios", "application/yaml", strings.Repeat("a", maxDocBody+1), operator); r.code != 400 {
		t.Fatalf("grande: %d", r.code)
	}

	// Steps from the events (no Prometheus).
	now := e.clk.now()
	ev := func(id, data string, sample float64) store.Event {
		return store.Event{StreamID: id, At: now, Type: store.EventRequest, Sample: sample, Data: json.RawMessage(data)}
	}
	e.insertEvents(
		ev("a", `{"scenario":"s1","step":"a"}`, 0), ev("b", `{"scenario":"s1","step":"a"}`, 0.5), ev("c", `{"scenario":"s1"}`, 0),
		ev("d", `{}`, 0), ev("e", `5`, 0),
	)
	var list ScenarioList
	r = e.do("GET", "/api/v1/scenarios", "", operator)
	r.decode(t, &list)
	if r.code != 200 || len(list.Scenarios) != 1 {
		t.Fatalf("lista: %d %s", r.code, r.body)
	}
	s := list.Scenarios[0]
	if s.Name != "s1" || s.Steps != 1 || s.ConversationsByStep["a"] != 3 || s.OnNoMatchRate == nil || *s.OnNoMatchRate != 0.25 {
		t.Fatalf("cenário: %+v", s)
	}
	if r := e.do("GET", "/api/v1/scenarios?window=2h", "", operator); r.code != 400 {
		t.Fatalf("janela: %d", r.code)
	}

	// Delete: 204, audited with the scenario before; unknown → 404.
	if r := e.do("DELETE", "/api/v1/scenarios/s1", "", operator); r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != AuditScenarioDel || a.Target != "scenario/s1" || !strings.Contains(string(a.Diff), `"steps":1`) {
		t.Fatalf("auditoria do delete: %s", a.Diff)
	}
	if r := e.do("DELETE", "/api/v1/scenarios/s1", "", operator); r.code != 404 {
		t.Fatalf("delete de novo: %d", r.code)
	}

	// Prometheus: steps from wwz_scenario_steps_total; Prometheus failing
	// falls back to the events (logged); SQLite closed leaves no counts.
	p := newEnv(t, func(o *envOpts) {
		o.prom = promHandler(promRule{match: "wwz_scenario_steps_total", body: vector(sample(`"scenario":"s1","step":"a"`, "2"))})
	})
	p.doCT("POST", "/api/v1/scenarios", "application/yaml", scenarioYAML, operator)
	p.do("GET", "/api/v1/scenarios", "", operator).decode(t, &list)
	if s := list.Scenarios[0]; s.ConversationsByStep["a"] != 2 || *s.OnNoMatchRate != 0 {
		t.Fatalf("prometheus: %+v", s)
	}
	f := newEnv(t, func(o *envOpts) {
		o.prom = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") })
	})
	f.doCT("POST", "/api/v1/scenarios", "application/yaml", scenarioYAML, operator)
	_ = f.st.Close()
	f.do("GET", "/api/v1/scenarios", "", operator).decode(t, &list)
	if s := list.Scenarios[0]; s.ConversationsByStep != nil || s.OnNoMatchRate != nil {
		t.Fatalf("sem dados: %+v", s)
	}
	if !strings.Contains(f.logs.String(), "passos de cenário no prometheus") || !strings.Contains(f.logs.String(), "passos de cenário nos eventos") {
		t.Fatalf("logs: %s", f.logs)
	}

	// wwz loading nothing: loaded [] and the generic target.
	n := newEnv(t, func(o *envOpts) { o.adminURL = fakeAdmin(t, map[string]string{"/admin/scenarios": `{}`}) })
	if r := n.doCT("POST", "/api/v1/scenarios", "application/yaml", "---\n", operator); r.code != 200 || !strings.Contains(string(r.body), `"loaded":[]`) {
		t.Fatalf("nada carregado: %d %s", r.code, r.body)
	}

	// Admin down.
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("GET", "/api/v1/scenarios", "", operator); r.code != 502 {
		t.Fatalf("lista admin fora: %d", r.code)
	}
	if r := d.do("DELETE", "/api/v1/scenarios/x", "", operator); r.code != 502 {
		t.Fatalf("delete admin fora: %d", r.code)
	}
}

func TestConfigAndPoliciesWithoutValkey(t *testing.T) {
	e := newEnv(t)
	var c ConfigView
	r := e.do("GET", "/api/v1/config", "", operator)
	r.decode(t, &c)
	if r.code != 200 || c.AdminURL != e.admin.BaseURL() || len(c.Instances) != 1 || !strings.Contains(string(c.Config), `"tiers"`) {
		t.Fatalf("config: %d %s", r.code, r.body)
	}
	var p PoliciesView
	r = e.do("GET", "/api/v1/policies", "", operator)
	r.decode(t, &p)
	if r.code != 200 || p.Source != SourceConfig || p.Writable || p.Version != 0 || len(p.Entities) != 0 ||
		!strings.Contains(string(p.Tiers["gold"]), `"rpm":1000`) {
		t.Fatalf("políticas: %d %s", r.code, r.body)
	}
	if r := e.do("PUT", "/api/v1/policies", `{"version":0,"tiers":{}}`, operator); r.code != 409 || r.problem(t).Error != "conflict" {
		t.Fatalf("put sem valkey: %d %s", r.code, r.body)
	}
	// Tier without limits shows {}.
	f := newEnv(t, func(o *envOpts) {
		o.adminURL = fakeAdmin(t, map[string]string{"/admin/config": `{"tiers":{"t":{}}}`, "/admin/stats": `{}`})
	})
	f.do("GET", "/api/v1/policies", "", operator).decode(t, &p)
	if string(p.Tiers["t"]) != "{}" {
		t.Fatalf("tier sem limites: %s", p.Tiers["t"])
	}
	// Instances failing: empty list, logged.
	f.do("GET", "/api/v1/config", "", operator).decode(t, &c)
	g := newEnv(t, func(o *envOpts) { o.adminURL = fakeAdmin(t, map[string]string{"/admin/config": `{}`}) })
	g.do("GET", "/api/v1/config", "", operator).decode(t, &c)
	if len(c.Instances) != 0 || !strings.Contains(g.logs.String(), "instâncias da configuração") {
		t.Fatalf("instâncias fora: %+v", c)
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	for _, p := range []string{"/api/v1/config", "/api/v1/policies"} {
		if r := d.do("GET", p, "", operator); r.code != 502 {
			t.Errorf("%s admin fora: %d", p, r.code)
		}
	}
}

func TestPoliciesValkey(t *testing.T) {
	v := newVKEnv(t)
	e := v.env
	var p PoliciesView
	r := e.do("GET", "/api/v1/policies", "", operator)
	r.decode(t, &p)
	if r.code != 200 || p.Source != SourceValkey || !p.Writable || p.Version != 0 || len(p.Tiers) != 0 {
		t.Fatalf("políticas vazias: %d %s", r.code, r.body)
	}
	// Publish: version +1, audited, and the in-process wwz applies it.
	r = e.do("PUT", "/api/v1/policies", `{"version":0,"tiers":{"gold":{"rpm":5},"tier-4":{"rpm":10000,"tpm":2000000}}}`, operator)
	r.decode(t, &p)
	if r.code != 200 || p.Version != 1 || p.AuditID == 0 || len(p.Tiers) != 2 {
		t.Fatalf("publicar: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != AuditPolicies || a.Target != "policies" || !strings.Contains(string(a.Diff), `"version":1`) {
		t.Fatalf("auditoria: %+v", a)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var doc struct {
			Tiers map[string]struct {
				Limits struct {
					RPM int `json:"rpm"`
				} `json:"limits"`
			} `json:"tiers"`
		}
		raw, err := e.admin.Config(ctx)
		if err == nil && json.Unmarshal(raw, &doc) == nil && doc.Tiers["gold"].Limits.RPM == 5 {
			if _, ok := doc.Tiers["tier-4"]; ok {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("o wwz não aplicou as políticas: %s %v", raw, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Stale version → 409.
	if r := e.do("PUT", "/api/v1/policies", `{"version":0,"tiers":{}}`, operator); r.code != 409 {
		t.Fatalf("versão velha: %d %s", r.code, r.body)
	}
	// A tier of the policies assigned to an entity cannot be removed.
	if r := e.do("POST", "/api/v1/entities/key/test-02/tier", `{"tier":"tier-4"}`, operator); r.code != 200 {
		t.Fatalf("atribuir: %d %s", r.code, r.body)
	}
	e.do("GET", "/api/v1/policies", "", operator).decode(t, &p)
	// The wwz assigns the tier to the key's account (ADR 0051 §5).
	if p.Entities["account:acct-test-02"] != "tier-4" {
		t.Fatalf("atribuição: %+v", p)
	}
	body := `{"version":` + jsonInt(p.Version) + `,"tiers":{"gold":{"rpm":6}}}`
	r = e.do("PUT", "/api/v1/policies", body, operator)
	if r.code != 400 || !strings.Contains(string(r.body), "tier-4: atribuído a account:acct-test-02") {
		t.Fatalf("remover atribuído: %d %s", r.code, r.body)
	}
	// Validation in the panel.
	for _, b := range []string{
		`{"version":0,"tiers":{"nome ruim":{}}}`, `{"version":0,"tiers":{"a":[1]}}`, `{"version":0,"tiers":{"a":{"rpmm":1}}}`,
		`{"version":0,"tiers":{"a":{"rpm":"x"}}}`, `{"tiers":{}}`, `{"version":-1,"tiers":{}}`, `{"version":0}`, `{"version":0,"x":1}`,
	} {
		if r := e.do("PUT", "/api/v1/policies", b, operator); r.code != 400 {
			t.Errorf("%s: %d %s", b, r.code, r.body)
		}
	}
	// Valkey down: GET and PUT 502.
	d := newEnv(t, func(o *envOpts) { o.valkey = deadValkey(t) })
	if r := d.do("GET", "/api/v1/policies", "", operator); r.code != 502 {
		t.Fatalf("get valkey fora: %d", r.code)
	}
	if r := d.do("PUT", "/api/v1/policies", `{"version":0,"tiers":{}}`, operator); r.code != 502 {
		t.Fatalf("put valkey fora: %d %s", r.code, r.body)
	}
}

// TestKeyTierAndCreditsActOnAccount: tier and add_usd posted on a key land on
// its account and are audited with the key as the target (spec 016).
func TestKeyTierAndCreditsActOnAccount(t *testing.T) {
	e := newEnv(t)
	acct := func() EntityDetail { return detail(t, e, "account/acme") }

	if r := e.do("POST", "/api/v1/entities/key/test-01/tier", `{"tier":"default"}`, operator); r.code != 200 {
		t.Fatalf("tier: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != "entity.tier" || a.Target != "key/test-01" || !strings.Contains(string(a.Diff), `"tier":"default"`) {
		t.Fatalf("auditoria do tier: %+v", a)
	}
	if d := acct(); d.State.Tier != "default" {
		t.Fatalf("tier da conta: %+v", d.State)
	}

	before := acct().State.PurchasedUSD
	if r := e.do("POST", "/api/v1/entities/key/test-01/credits", `{"add_usd":10}`, operator); r.code != 200 {
		t.Fatalf("add_usd: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != "entity.credits" || a.Target != "key/test-01" || !strings.Contains(string(a.Diff), `"add_usd":10`) {
		t.Fatalf("auditoria dos créditos: %+v", a)
	}
	if d := acct(); d.State.PurchasedUSD != before+10 {
		t.Fatalf("purchased_usd da conta: %v -> %+v", before, d.State)
	}

	// A user has no tier, like a project and an ip.
	if d := detail(t, e, "user/alguem"); d.EffectiveTier != "" || d.State.Tier != "" || len(d.Limits) != 0 {
		t.Fatalf("usuário: %+v", d)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
