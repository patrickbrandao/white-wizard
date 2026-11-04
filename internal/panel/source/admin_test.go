package source

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// fakeAdmin is a wwz control plane: it records each request and answers
// from routes ("METHOD /path?query" → status and body).
type fakeAdmin struct {
	t      *testing.T
	routes map[string]fakeResp
	last   struct {
		method, uri, ctype, auth, body string
	}
}

type fakeResp struct {
	status int
	body   string
}

func newFakeAdmin(t *testing.T, routes map[string]fakeResp) (*fakeAdmin, *Admin) {
	f := &fakeAdmin{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewAdmin(srv.URL+"/", "tok", nil)
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.last.method, f.last.uri, f.last.ctype, f.last.auth, f.last.body = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)
	resp, ok := f.routes[r.Method+" "+r.URL.RequestURI()]
	if !ok {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"no route ` + r.URL.RequestURI() + `"}`))
		return
	}
	if resp.status == 0 {
		resp.status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

const entityJSON = `{"entity":{"kind":"key","id":"a/b c"},"status":"suspended","reason":"r","strikes":0,"requests":3,"spend_usd":0}`

func TestAdminEntities(t *testing.T) {
	const base = "/admin/entities/key/a%2Fb%20c"
	f, a := newFakeAdmin(t, map[string]fakeResp{
		"GET /admin/entities?limit=5&status=banned": {body: `{"entities":[` + entityJSON + `]}`},
		"GET /admin/entities":                       {body: `{}`},
		"GET " + base:                               {body: entityJSON},
		"POST " + base + "/block":                   {body: entityJSON},
		"POST " + base + "/unblock":                 {body: entityJSON},
		"POST " + base + "/ban":                     {body: entityJSON},
		"POST " + base + "/unban":                   {body: entityJSON},
		"POST " + base + "/credits":                 {body: entityJSON},
		"POST " + base + "/tier":                    {body: entityJSON},
		"POST " + base + "/reset":                   {body: entityJSON},
		"POST /admin/leak":                          {body: `{"key":` + entityJSON + `,"account":` + entityJSON + `}`},
	})
	ctx := context.Background()
	e := limits.Entity{Kind: limits.KindKey, ID: "a/b c"}
	list, err := a.Entities(ctx, limits.StatusBanned, 5)
	if err != nil || len(list) != 1 || list[0].Entity != e || list[0].Status != limits.StatusSuspended {
		t.Fatalf("Entities = %+v %v", list, err)
	}
	if f.last.auth != "Bearer tok" || f.last.method != "GET" {
		t.Fatalf("request = %+v", f.last)
	}
	if list, err := a.Entities(ctx, "", 0); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty = %v %v", list, err)
	}
	if st, err := a.Entity(ctx, e); err != nil || st.Requests != 3 {
		t.Fatalf("Entity = %+v %v", st, err)
	}
	checks := []struct {
		name string
		call func() (limits.EntityState, error)
		body string
	}{
		{"block", func() (limits.EntityState, error) {
			return a.Block(ctx, e, BlockRequest{Reason: "r", Duration: 90 * time.Second, By: "panel:ana"})
		}, `{"by":"panel:ana","duration":"1m30s","reason":"r"}`},
		{"block-empty", func() (limits.EntityState, error) { return a.Block(ctx, e, BlockRequest{}) }, `{}`},
		{"unblock", func() (limits.EntityState, error) { return a.Unblock(ctx, e) }, ``},
		{"ban", func() (limits.EntityState, error) { return a.Ban(ctx, e, "abuse", "panel:ana") }, `{"by":"panel:ana","reason":"abuse"}`},
		{"ban-empty", func() (limits.EntityState, error) { return a.Ban(ctx, e, "", "") }, `{}`},
		{"unban", func() (limits.EntityState, error) { return a.Unban(ctx, e) }, ``},
		{"credits", func() (limits.EntityState, error) { return a.SetCredits(ctx, e, -2.5) }, `{"usd":-2.5}`},
		{"add-credits", func() (limits.EntityState, error) { return a.AddCredits(ctx, e, 5) }, `{"add_usd":5}`},
		{"tier", func() (limits.EntityState, error) { return a.SetTier(ctx, e, "") }, `{"tier":""}`},
		{"reset", func() (limits.EntityState, error) { return a.Reset(ctx, e) }, ``},
		{"forgive", func() (limits.EntityState, error) { return a.Reset(ctx, e, ResetStrikes) }, `{"scope":["strikes"]}`},
	}
	for _, c := range checks {
		st, err := c.call()
		if err != nil || st.Entity != e {
			t.Errorf("%s = %+v %v", c.name, st, err)
		}
		if f.last.body != c.body {
			t.Errorf("%s body = %q, want %q", c.name, f.last.body, c.body)
		}
		if c.body != "" && f.last.ctype != "application/json" {
			t.Errorf("%s content-type = %q", c.name, f.last.ctype)
		}
	}
	res, err := a.Leak(ctx, LeakRequest{Label: "ci-key", Reason: "found"})
	if err != nil || res.Account == nil || f.last.body != `{"label":"ci-key","reason":"found"}` {
		t.Fatalf("Leak = %+v %v %q", res, err, f.last.body)
	}
}

func TestAdminOthers(t *testing.T) {
	f, a := newFakeAdmin(t, map[string]fakeResp{
		"GET /health":                         {body: `{"status":"ok"}`},
		"GET /admin/stats":                    {body: `{"started":"2026-10-30T12:00:00Z","uptime_s":1,"store":"memory","totals":{"requests":5,"in_flight":0,"streams_open":1,"errors_injected":0,"rate_limited":0,"blocked":0},"by_model":[{"driver":"openai","model":"m","requests":5,"ttft_p50_ms":12.5}]}`},
		"POST /admin/chaos":                   {status: 201, body: `{"id":"c1","until":"2026-10-30T12:00:30Z","models":["*"]}`},
		"GET /admin/chaos":                    {body: `{"events":[],"pending_next":[]}`},
		"DELETE /admin/chaos":                 {body: `{"stopped":2}`},
		"DELETE /admin/chaos?id=c1&test_id=t": {body: `{"stopped":1}`},
		"GET /admin/scenarios":                {body: `{"scenarios":[{"name":"s","steps":2}]}`},
		"POST /admin/scenarios":               {body: `{"loaded":["s"],"replaced":["s"]}`},
		"DELETE /admin/scenarios?name=a+b":    {status: 204},
		"GET /admin/config":                   {body: `{"server":{}}`},
		"POST /admin/config":                  {body: `{"applied":true,"source":"patch","config":{"models":{}}}`},
		"GET /admin/requests?last=7":          {body: `{"enabled":true,"requests":[{"time":"2026-10-30T12:00:00Z","request_id":"r","driver":"d","model":"m","endpoint":"/e","status":200,"duration_ms":1,"stream":true,"tool_calls":["x"]}]}`},
		"GET /admin/requests":                 {body: `{"enabled":false,"requests":[]}`},
		"GET /admin/entities/key/x":           {body: `not json`},
		"GET /admin/entities/key/err":         {status: 503, body: `{"error":"store_unavailable","message":"limits: valkey store unavailable"}`},
		"GET /admin/entities/key/plain":       {status: 500, body: `oops`},
		"GET /admin/entities/key/bad-request": {status: 400, body: `{"error":"invalid","message":"tipo","details":["a"]}`},
		"POST /admin/entities/key/x/unblock":  {status: 200},
	})
	ctx := context.Background()
	if err := a.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if a.BaseURL() == "" || strings.HasSuffix(a.BaseURL(), "/") {
		t.Fatalf("base = %q", a.BaseURL())
	}
	st, err := a.Stats(ctx)
	if err != nil || st.Totals.Requests != 5 || *st.ByModel[0].TTFTP50MS != 12.5 || st.ByModel[0].TTFTP99MS != nil {
		t.Fatalf("Stats = %+v %v", st, err)
	}
	ev, err := a.StartChaos(ctx, map[string]any{"type": "overloaded", "duration": "30s"})
	if err != nil || !strings.Contains(string(ev), `"c1"`) || f.last.body != `{"duration":"30s","type":"overloaded"}` {
		t.Fatalf("StartChaos = %s %v", ev, err)
	}
	if l, err := a.ListChaos(ctx); err != nil || !json.Valid(l) {
		t.Fatalf("ListChaos = %s %v", l, err)
	}
	if n, err := a.StopChaos(ctx, "", ""); err != nil || n != 2 {
		t.Fatalf("StopChaos all = %d %v", n, err)
	}
	if n, err := a.StopChaos(ctx, "c1", "t"); err != nil || n != 1 {
		t.Fatalf("StopChaos = %d %v", n, err)
	}
	if s, err := a.Scenarios(ctx); err != nil || len(s) != 1 || s[0].Steps != 2 {
		t.Fatalf("Scenarios = %+v %v", s, err)
	}
	loaded, err := a.LoadScenarios(ctx, []byte("name: s\n"))
	if err != nil || loaded.Replaced[0] != "s" || f.last.ctype != "application/yaml" || f.last.body != "name: s\n" {
		t.Fatalf("LoadScenarios = %+v %v %+v", loaded, err, f.last)
	}
	if err := a.DeleteScenario(ctx, "a b"); err != nil {
		t.Fatalf("DeleteScenario = %v", err)
	}
	if c, err := a.Config(ctx); err != nil || string(c) != `{"server":{}}` {
		t.Fatalf("Config = %s %v", c, err)
	}
	if c, err := a.PatchConfig(ctx, map[string]any{"models": map[string]any{}}); err != nil || string(c) != `{"models":{}}` {
		t.Fatalf("PatchConfig = %s %v", c, err)
	}
	rl, err := a.Requests(ctx, 7)
	if err != nil || !rl.Enabled || rl.Requests[0].ToolCalls[0] != "x" || !rl.Requests[0].Stream {
		t.Fatalf("Requests = %+v %v", rl, err)
	}
	if rl, err := a.Requests(ctx, 0); err != nil || rl.Enabled {
		t.Fatalf("Requests disabled = %+v %v", rl, err)
	}
	if _, err := a.Unblock(ctx, limits.Entity{Kind: limits.KindKey, ID: "x"}); err != nil {
		t.Fatalf("empty 200 body = %v", err)
	}
	// Errors.
	cases := []struct {
		id     string
		status int
		panel  int
		msg    string
	}{
		{"x", 0, 502, "resposta inválida"},
		{"err", 503, 502, "store_unavailable"},
		{"plain", 500, 502, "HTTP 500"},
		{"bad-request", 400, 400, "invalid"},
		{"missing", 404, 404, "not_found"},
	}
	for _, c := range cases {
		_, err := a.Entity(ctx, limits.Entity{Kind: limits.KindKey, ID: c.id})
		var ae *AdminError
		if !errors.As(err, &ae) || ae.Status != c.status || ae.HTTPStatus() != c.panel || HTTPStatus(err) != c.panel || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v (%+v)", c.id, err, ae)
		}
	}
	// Errors of the list and YAML helpers.
	_, down := newFakeAdmin(t, map[string]fakeResp{
		"GET /admin/scenarios": {status: 500, body: `{}`},
		"GET /admin/entities":  {status: 503, body: `{}`},
	})
	if _, err := down.Scenarios(ctx); err == nil {
		t.Error("Scenarios 500: want error")
	}
	if _, err := down.Entities(ctx, "", 0); err == nil {
		t.Error("Entities 503: want error")
	}
	if _, err := down.LoadScenarios(ctx, []byte("x")); HTTPStatus(err) != 404 {
		t.Errorf("LoadScenarios 404: %v", err)
	}
	_, empty := newFakeAdmin(t, map[string]fakeResp{"GET /admin/scenarios": {body: `{}`}})
	if s, err := empty.Scenarios(ctx); err != nil || s == nil || len(s) != 0 {
		t.Errorf("Scenarios empty = %v %v", s, err)
	}
	_, err = NewAdmin("http://127.0.0.1:1", "", &http.Client{Timeout: time.Second}).Stats(ctx)
	var ae *AdminError
	if !errors.As(err, &ae) || ae.Status != 0 || errors.Unwrap(err) == nil || HTTPStatus(err) != 502 {
		t.Fatalf("connection refused = %v", err)
	}
	if _, err := NewAdmin("http://a b", "", nil).Stats(ctx); err == nil {
		t.Fatal("bad URL: want error")
	}
	if HTTPStatus(errors.New("x")) != 502 {
		t.Fatal("generic error must be 502")
	}
}

// TestAdminLoadScenariosBadJSON covers a 2xx answer that is not JSON.
func TestAdminLoadScenariosBadJSON(t *testing.T) {
	_, a := newFakeAdmin(t, map[string]fakeResp{"POST /admin/scenarios": {body: "nope"}})
	if _, err := a.LoadScenarios(context.Background(), []byte("x")); err == nil || !strings.Contains(err.Error(), "resposta inválida") {
		t.Fatalf("err = %v", err)
	}
}

// TestAdminReadBodyFails covers a body cut in the middle.
func TestAdminReadBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("{"))
	}))
	defer srv.Close()
	if _, err := NewAdmin(srv.URL, "", nil).Stats(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
