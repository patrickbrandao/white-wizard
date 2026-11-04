package api

import (
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

type contractOp struct {
	Role, Audit, Admin, OpID, Method, Path string
}

// contract reads the operations of cmd/wwz-panel/openapi.yaml.
func contract(t *testing.T) []contractOp {
	t.Helper()
	data, err := os.ReadFile("../../../cmd/wwz-panel/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var ops []contractOp
	for path, item := range doc.Paths {
		for method, raw := range item {
			switch method {
			case "get", "post", "put", "delete", "patch":
			default:
				continue
			}
			m, _ := raw.(map[string]any)
			str := func(k string) string { s, _ := m[k].(string); return s }
			op := contractOp{
				Role: str("x-wwz-role"), Audit: str("x-wwz-audit-action"), Admin: str("x-wwz-admin"), OpID: str("operationId"),
				Method: strings.ToUpper(method), Path: path,
			}
			ops = append(ops, op)
		}
	}
	return ops
}

// TestRoutesMatchContract: every route of /api/v1, /auth, /health* and
// /metrics registered by the API is in the openapi.yaml with the same role,
// and vice versa.
func TestRoutesMatchContract(t *testing.T) {
	e := newEnv(t)
	ops := contract(t)
	want := map[string]string{}
	for _, op := range ops {
		if !strings.HasPrefix(op.Path, "/api/v1/") && !strings.HasPrefix(op.Path, "/auth/") &&
			!strings.HasPrefix(op.Path, "/health") && op.Path != "/metrics" {
			continue
		}
		want[op.Method+" "+op.Path] = op.Role
		if op.Role != RoleOperator && op.Audit != "" {
			t.Errorf("%s %s: auditoria sem papel operator", op.Method, op.Path)
		}
	}
	got := map[string]string{}
	for _, r := range e.api.Routes() {
		got[r.Method+" "+r.Path] = r.Role
	}
	var missing, extra []string
	for k, role := range want {
		g, ok := got[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case g != role:
			t.Errorf("%s: papel %q, contrato %q", k, g, role)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("rotas fora do contrato: faltam %v; sobram %v", missing, extra)
	}
	if len(got) != 36 {
		t.Fatalf("%d operações, contrato tem 36", len(got))
	}
}

// TestNoRouteNotImplemented: since task 053 every operation of the contract
// has a handler (no 501 left).
func TestNoRouteNotImplemented(t *testing.T) {
	e := newEnv(t)
	path := func(p string) string {
		return strings.NewReplacer("{kind}", "key", "{id}", "test-01", "{name}", "s1").Replace(p)
	}
	for _, rt := range e.api.Routes() {
		r := e.do(rt.Method, path(rt.Path), "", operator)
		if r.code == http.StatusNotImplemented || r.code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d %s", rt.Method, rt.Path, r.code, r.body)
		}
	}
}

// TestAuthorization: no credential → 401; viewer on any operator
// operation → 403 forbidden; cookie without CSRF → 403 csrf.
func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	viewer := e.login("vi", viPass)
	op := e.login("op", opPass)
	path := func(p string) string {
		return strings.NewReplacer("{kind}", "key", "{id}", "test-01", "{name}", "s1").Replace(p)
	}
	for _, r := range e.api.Routes() {
		if r.Role == RoleNone {
			continue
		}
		p := path(r.Path)
		if res := e.do(r.Method, p, "", cred{}); res.code != http.StatusUnauthorized || res.problem(t).Error != "unauthorized" {
			t.Errorf("%s %s sem sessão: %d %s", r.Method, p, res.code, res.body)
		}
		if r.Role == RoleOperator {
			if res := e.do(r.Method, p, "", viewer); res.code != http.StatusForbidden || res.problem(t).Error != "forbidden" {
				t.Errorf("%s %s viewer: %d %s", r.Method, p, res.code, res.body)
			}
			noCSRF := op
			noCSRF.csrf = ""
			if res := e.do(r.Method, p, "", noCSRF); res.code != http.StatusForbidden || res.problem(t).Error != "csrf" {
				t.Errorf("%s %s sem csrf: %d %s", r.Method, p, res.code, res.body)
			}
		}
	}
	// viewer reads; me returns the csrf token.
	if r := e.do("GET", "/api/v1/me", "", viewer); r.code != http.StatusOK || !strings.Contains(string(r.body), viewer.csrf) {
		t.Fatalf("me: %d %s", r.code, r.body)
	}
	if r := e.do("POST", "/api/v1/logout", "", viewer); r.code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/v1/me", "", viewer); r.code != http.StatusUnauthorized {
		t.Fatalf("me depois do logout: %d", r.code)
	}
	if r := e.do("GET", "/auth/oidc/login", "", cred{}); r.code != http.StatusNotFound {
		t.Fatalf("oidc sem configuração: %d", r.code)
	}
	if r := e.do("GET", "/auth/oidc/callback", "", cred{}); r.code != http.StatusNotFound {
		t.Fatalf("oidc callback sem configuração: %d", r.code)
	}
}

func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New sem dependências")
	}
	e := newEnv(t)
	a, err := New(Options{Store: e.st, Auth: e.auth, Admin: e.admin})
	if err != nil || a.metrics == nil || a.now == nil || a.clientIP == nil || a.log == nil {
		t.Fatalf("padrões: %v %+v", err, a)
	}
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "sem-porta"
	if got := a.clientIP(req); got != "sem-porta" {
		t.Fatalf("clientIP = %q", got)
	}
}

func TestProblemOf(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		detail string
	}{
		{invalid("x", "d"), 400, "invalid", "d"},
		{&source.AdminError{Status: 400, Problem: source.Problem{Message: "m", Details: []string{"d"}}}, 400, "invalid", "m"},
		{&source.AdminError{Status: 404}, 404, "not_found", ""},
		{&source.AdminError{Status: 500, Err: errors.New("boom")}, 502, "upstream_unavailable", "admin: "},
		{errors.New("valkey fora"), 502, "upstream_unavailable", "valkey: valkey fora"},
		{dbErr(errors.New("disk")), 500, "internal", "disk"},
	}
	for _, c := range cases {
		st, p := ProblemOf(c.err, "valkey")
		if st != c.status || p.Error != c.code {
			t.Errorf("%v: %d %s", c.err, st, p.Error)
		}
		if c.detail != "" && (len(p.Details) == 0 || !strings.HasPrefix(p.Details[0], c.detail)) {
			t.Errorf("%v: details %v", c.err, p.Details)
		}
	}
	if s := (&Error{Code: "invalid", Message: "m"}).Error(); s != "invalid: m" {
		t.Fatal(s)
	}
}

func TestHealthReadyMetrics(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.poller = true })
	if r := e.do("GET", "/health", "", cred{}); r.code != 200 || !strings.Contains(string(r.body), `"ok"`) {
		t.Fatalf("health: %d %s", r.code, r.body)
	}
	var rd Ready
	r := e.do("GET", "/health/ready", "", cred{})
	r.decode(t, &rd)
	if r.code != 200 || rd.Status != "ready" || rd.Sources != (Sources{StateOff, StateOK, StateOff}) {
		t.Fatalf("ready: %d %+v", r.code, rd)
	}
	if err := e.st.Settings.Set(ctx, store.SettingLastBackupAt, "1700000000500"); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", "/metrics", "", cred{})
	body := string(r.body)
	for _, want := range []string{
		"wwz_panel_events_ingested_total 0", "wwz_panel_last_backup_timestamp_seconds 1700000000.5",
		`wwz_panel_source_up{source="admin"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics sem %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "wwz_panel_stream_lag") || strings.Contains(body, `source="valkey"`) {
		t.Errorf("metrics com fonte desligada:\n%s", body)
	}
	if !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatal(r.hdr)
	}

	// Admin and Prometheus down: still ready, sources down.
	e2 := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t); o.prom = http.NotFoundHandler() })
	r = e2.do("GET", "/health/ready", "", cred{})
	r.decode(t, &rd)
	if r.code != 200 || rd.Sources != (Sources{StateOff, StateDown, StateDown}) {
		t.Fatalf("ready com fontes fora: %d %+v", r.code, rd)
	}
	r = e2.do("GET", "/metrics", "", cred{})
	if !strings.Contains(string(r.body), `wwz_panel_source_up{source="admin"} 0`) ||
		!strings.Contains(string(r.body), `wwz_panel_source_up{source="prometheus"} 0`) ||
		!strings.Contains(string(r.body), "wwz_panel_last_backup_timestamp_seconds 0") {
		t.Fatalf("metrics: %s", r.body)
	}
	// SQLite closed: 503 starting.
	_ = e2.st.Close()
	r = e2.do("GET", "/health/ready", "", cred{})
	r.decode(t, &rd)
	if r.code != http.StatusServiceUnavailable || rd.Status != "starting" {
		t.Fatalf("ready sem SQLite: %d %+v", r.code, rd)
	}
}
