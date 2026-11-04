package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

func lastAudit(t *testing.T, e *env) (store.AuditEntry, store.Diff) {
	t.Helper()
	rows, err := e.st.Audit.Query(ctx, store.AuditFilter{Limit: 1})
	if err != nil || len(rows) == 0 {
		t.Fatalf("auditoria: %v %v", err, rows)
	}
	var d store.Diff
	if err := json.Unmarshal(rows[0].Diff, &d); err != nil {
		t.Fatal(err)
	}
	return rows[0], d
}

// wwzState reads the entity straight from the wwz admin.
func wwzState(t *testing.T, e *env, kind, id string) limits.EntityState {
	t.Helper()
	st, err := e.admin.Entity(ctx, limits.Entity{Kind: limits.EntityKind(kind), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestActionsAgainstWWZ blocks, unblocks, bans, unbans, forgives, credits
// and leaks on a wwz running in process, checking its state and the audit.
func TestActionsAgainstWWZ(t *testing.T) {
	e := newEnv(t)
	op := e.login("op", opPass)

	var res ActionResult
	r := e.do("POST", "/api/v1/entities/key/test-01/block", `{"reason":"teste","duration":"10m"}`, op)
	r.decode(t, &res)
	if r.code != 200 || res.AuditID == 0 || res.Entity.Status != limits.StatusSuspended || res.Key != nil {
		t.Fatalf("block: %d %s", r.code, r.body)
	}
	if st := wwzState(t, e, "key", "test-01"); st.Status != limits.StatusSuspended || st.By != "panel:op" || st.Reason != "teste" {
		t.Fatalf("estado no wwz depois do block: %+v", st)
	}
	a, d := lastAudit(t, e)
	if a.ID != res.AuditID || a.User != "op" || a.Role != store.RoleOperator || a.Action != "entity.block" ||
		a.Target != "key/test-01" || a.Result != store.ResultOK || a.IP != "192.0.2.7" {
		t.Fatalf("auditoria do block: %+v", a)
	}
	if req := d.Request.(map[string]any); req["by"] != "panel:op" || req["duration"] != "10m0s" ||
		d.Before.(map[string]any)["status"] != "active" || d.After.(map[string]any)["status"] != "suspended" {
		t.Fatalf("diff do block: %+v", d)
	}
	// Usage shows the blocked key via the API (entities list).
	r = e.do("GET", "/api/v1/entities?status=suspended", "", op)
	if r.code != 200 || !strings.Contains(string(r.body), `"id":"test-01"`) || !strings.Contains(string(r.body), `"source":"admin"`) {
		t.Fatalf("lista de suspensos: %d %s", r.code, r.body)
	}

	steps := []struct {
		path, body, status, audit string
	}{
		{"key/test-01/unblock", "", "active", "entity.unblock"},
		{"ip/10.0.0.1/ban", `{"reason":"abuso"}`, "banned", "entity.ban"},
		{"ip/10.0.0.1/unban", "", "active", "entity.unban"},
		{"user/u1/forgive", "", "active", "entity.forgive"},
		{"account/acme/credits", `{"usd": 25}`, "active", "entity.credits"},
		{"account/acme/credits", `{"add_usd": 10}`, "active", "entity.credits"},
		{"key/test-02/block", ``, "suspended", "entity.block"},
		{"key/test-02/ban", ``, "banned", "entity.ban"},
	}
	for _, s := range steps {
		res = ActionResult{}
		r := e.do("POST", "/api/v1/entities/"+s.path, s.body, operator)
		r.decode(t, &res)
		if r.code != 200 || string(res.Entity.Status) != s.status {
			t.Fatalf("%s: %d %s", s.path, r.code, r.body)
		}
		if a, _ := lastAudit(t, e); a.Action != s.audit || a.User != store.APITokenUser || a.ID != res.AuditID {
			t.Fatalf("%s: auditoria %+v", s.path, a)
		}
	}
	if _, d := lastAudit(t, e); d.Request.(map[string]any)["by"] != "panel:api-token" {
		t.Fatalf("ban sem by: %+v", d)
	}
	if st := wwzState(t, e, "account", "acme"); st.Credits == nil || *st.Credits != 35 || st.PurchasedUSD != 10 {
		t.Fatalf("créditos no wwz: %+v", st)
	}
	_, d = func() (store.AuditEntry, store.Diff) {
		e.do("POST", "/api/v1/entities/user/u1/forgive", "", operator)
		return lastAudit(t, e)
	}()
	if !strings.Contains(mustJSON(d.Request), `"scope":["strikes"]`) {
		t.Fatalf("forgive: %+v", d)
	}

	// Leak: key banned, account blocked.
	res = ActionResult{}
	r = e.do("POST", "/api/v1/entities/key/test-01/leak", `{"reason":"vazou"}`, operator)
	r.decode(t, &res)
	if r.code != 200 || res.Entity != nil || res.Key.Status != limits.StatusBanned || res.Account == nil ||
		res.Account.Status != limits.StatusSuspended {
		t.Fatalf("leak: %d %s", r.code, r.body)
	}
	if a, d := lastAudit(t, e); a.Action != "entity.leak" || !strings.Contains(mustJSON(d.After), `"account"`) ||
		!strings.Contains(mustJSON(d.Request), `"label":"test-01"`) {
		t.Fatalf("auditoria do leak: %+v %+v", a, d)
	}
	r = e.do("POST", "/api/v1/entities/key/test-02/leak", "", operator)
	if r.code != 200 {
		t.Fatalf("leak sem conta: %d %s", r.code, r.body)
	}
	if _, d := lastAudit(t, e); !strings.Contains(mustJSON(d.Request), `"reason":"leaked"`) {
		t.Fatalf("leak sem motivo: %+v", d)
	}

	// Unknown label: the 404 of wwz is passed through and audited as error.
	r = e.do("POST", "/api/v1/entities/key/nao-existe/leak", "", operator)
	if p := r.problem(t); r.code != 404 || p.Error != "not_found" || len(p.Details) == 0 {
		t.Fatalf("leak desconhecido: %d %s", r.code, r.body)
	}
	if a, _ := lastAudit(t, e); a.Result != store.ResultError || a.Error == "" {
		t.Fatalf("auditoria do erro: %+v", a)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestActionValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct{ path, body, why string }{
		{"/api/v1/entities/planet/x/block", "", "kind inválido"},
		{"/api/v1/entities/ip/1.2.3.4/leak", "", "leak fora de key"},
		{"/api/v1/entities/key/test-01/credits", "", "credits sem usd"},
		{"/api/v1/entities/key/test-01/credits", `{"usd":"x"}`, "usd não numérico"},
		{"/api/v1/entities/key/test-01/credits", `{"usd":1,"add_usd":1}`, "usd e add_usd juntos"},
		{"/api/v1/entities/key/test-01/credits", `{"add_usd":0}`, "add_usd zero"},
		{"/api/v1/entities/key/test-01/credits", `{"add_usd":-5}`, "add_usd negativo"},
		{"/api/v1/entities/key/test-01/block", `{"add_usd":5}`, "add_usd em outra ação"},
		{"/api/v1/entities/key/test-01/block", `{"motivo":"x"}`, "campo desconhecido"},
		{"/api/v1/entities/key/test-01/ban", `{"duration":"1m"}`, "campo de outra ação"},
		{"/api/v1/entities/key/test-01/unblock", `{"reason":"x"}`, "unblock sem corpo"},
		{"/api/v1/entities/key/test-01/block", `{"duration":"amanhã"}`, "duration inválida"},
		{"/api/v1/entities/key/test-01/block", `{"duration":"-1m"}`, "duration negativa"},
		{"/api/v1/entities/key/test-01/block", `{`, "JSON quebrado"},
		{"/api/v1/entities/key/test-01/block", `{"reason":"` + strings.Repeat("x", maxActionBody) + `"}`, "corpo grande"},
	}
	for _, c := range cases {
		if r := e.do("POST", c.path, c.body, operator); r.code != 400 || r.problem(t).Error != "invalid" {
			t.Errorf("%s: %d %s", c.why, r.code, r.body)
		}
	}
	if r := e.do("POST", "/api/v1/entities/key/test-01/block", `{"duration":""}`, operator); r.code != 200 {
		t.Fatalf("duration vazia: %d %s", r.code, r.body)
	}
	if _, err := e.api.Act(ctx, Actor{}, "explode", limits.Entity{Kind: limits.KindKey, ID: "x"}, ActionRequest{}); err == nil {
		t.Fatal("ação desconhecida")
	}
	if n, _ := e.st.Audit.Query(ctx, store.AuditFilter{Result: store.ResultError}); len(n) != 0 {
		t.Fatalf("validação gravou auditoria: %+v", n)
	}
}

func TestActionUpstreamAndAuditFailures(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	r := e.do("POST", "/api/v1/entities/key/test-01/unban", "", operator)
	if p := r.problem(t); r.code != 502 || p.Error != "upstream_unavailable" || !strings.HasPrefix(p.Details[0], "admin: ") {
		t.Fatalf("admin fora: %d %s", r.code, r.body)
	}
	a, d := lastAudit(t, e)
	if a.Result != store.ResultError || d.Before != nil || d.After != nil {
		t.Fatalf("auditoria com admin fora: %+v %+v", a, d)
	}
	// 400 of wwz passed through with its message in details.
	e = newEnv(t, func(o *envOpts) { o.adminURL = failingAdmin(t, http.StatusBadRequest) })
	r = e.do("POST", "/api/v1/entities/key/test-01/credits", `{"usd":1}`, operator)
	if p := r.problem(t); r.code != 400 || p.Error != "invalid" || strings.Join(p.Details, ",") != "falhou,d1" {
		t.Fatalf("400 do admin: %d %s", r.code, r.body)
	}

	// Audit cannot be recorded: the action ran, the API answers 500; with
	// the admin failing too, the admin error wins.
	e = newEnv(t)
	_ = e.st.Close()
	r = e.do("POST", "/api/v1/entities/key/test-01/block", "", operator)
	if r.code != 500 || r.problem(t).Error != "internal" {
		t.Fatalf("auditoria falhando: %d %s", r.code, r.body)
	}
	if st := wwzState(t, e, "key", "test-01"); st.Status != limits.StatusSuspended {
		t.Fatalf("ação não aplicada: %+v", st)
	}
	r = e.do("POST", "/api/v1/entities/key/nao-existe/leak", "", operator)
	if r.code != 404 {
		t.Fatalf("erro do admin e da auditoria: %d %s", r.code, r.body)
	}
}
