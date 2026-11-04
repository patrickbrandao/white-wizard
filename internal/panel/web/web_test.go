package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/api"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// htmxSHA256 is the SHA-256 of the vendored htmx 2.0.4 (CHANGELOG).
const htmxSHA256 = "e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447"

func TestStaticAndSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/login", nil, cred{})
	if r.code != 200 || r.hdr.Get("Content-Security-Policy") != CSP || r.hdr.Get("X-Frame-Options") != "DENY" ||
		r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("cabeçalhos: %d %v", r.code, r.hdr)
	}
	if strings.Contains(CSP, "unsafe-inline") || strings.Contains(CSP, "unsafe-eval") || strings.Contains(CSP, "http") {
		t.Fatalf("CSP permissiva: %s", CSP)
	}
	// No inline script, style or handler; nothing loaded from outside.
	if regexp.MustCompile(`<script(?:[^>]*)>[^<]|<style|\son[a-z]+=|style=|https?://`).MatchString(r.body) {
		t.Fatalf("recurso inline ou externo:\n%s", r.body)
	}
	for name, ctype := range map[string]string{"htmx.min.js": "javascript", "panel.css": "text/css"} {
		p := assetPath[name]
		if !regexp.MustCompile(`^/static/[a-z.]+\.[0-9a-f]{12}\.(js|css)$`).MatchString(p) || !strings.Contains(r.body, `"`+p+`"`) {
			t.Fatalf("asset %s: %q", name, p)
		}
		s := e.do("GET", p, nil, cred{})
		if s.code != 200 || !strings.Contains(s.hdr.Get("Content-Type"), ctype) ||
			s.hdr.Get("Cache-Control") != "public, max-age=31536000, immutable" || s.hdr.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("static %s: %d %v", p, s.code, s.hdr)
		}
		if name == "htmx.min.js" {
			sum := sha256.Sum256([]byte(s.body))
			if hex.EncodeToString(sum[:]) != htmxSHA256 {
				t.Fatal("htmx.min.js diferente da versão fixada")
			}
		}
	}
	for _, p := range []string{"/static/htmx.min.js", "/static/nada.css"} {
		if r := e.do("GET", p, nil, cred{}); r.code != 404 {
			t.Errorf("%s: %d", p, r.code)
		}
	}
	if r := e.do("GET", "/nada", nil, cred{}); r.code != 404 {
		t.Fatalf("rota desconhecida: %d", r.code)
	}
}

func TestLoginPage(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/login?next=/audit", nil, cred{})
	has(t, "login", r.body, `<form method="post" action="/api/v1/login">`, `name="next" value="/audit"`,
		`name="username"`, `name="password" type="password"`, `<title>Entrar · wwz-panel</title>`)
	hasNot(t, "login", r.body, "OIDC", "<nav>", "hx-headers")
	// Foreign next is dropped.
	has(t, "next externo", e.do("GET", "/login?next=//mal.example", nil, cred{}).body, `name="next" value="/"`)
	has(t, "erro", e.do("GET", "/login?error=1", nil, cred{}).body, "Usuário ou senha inválidos.")
	has(t, "rate", e.do("GET", "/login?error=rate", nil, cred{}).body, "Muitas tentativas")
	// Wrong password: back to the form with the error.
	r = e.do("POST", "/api/v1/login", url.Values{"username": {"op"}, "password": {"x"}}, cred{})
	if r.code != 303 || r.hdr.Get("Location") != "/login?error=1" {
		t.Fatalf("senha errada: %d %v", r.code, r.hdr)
	}
	// Logged in: /login goes to next; logout by form.
	op := e.login("op", opPass)
	if r := e.do("GET", "/login?next=/blocks", nil, op); r.code != 303 || r.hdr.Get("Location") != "/blocks" {
		t.Fatalf("login com sessão: %d %v", r.code, r.hdr)
	}
	if r := e.do("POST", "/api/v1/logout", url.Values{"csrf": {op.csrf}}, op); r.code != 303 || r.hdr.Get("Location") != "/login" {
		t.Fatalf("logout: %d %v", r.code, r.hdr)
	}
	if r := e.do("GET", "/", nil, op); r.code != 303 {
		t.Fatalf("sessão revogada: %d", r.code)
	}

	e = newEnv(t, func(o *envOpts) { o.oidc = true })
	has(t, "oidc", e.do("GET", "/login?next=/usage", nil, cred{}).body, `href="/auth/oidc/login?next=%2fusage"`, "Entrar com OIDC")
}

func TestPagesNeedSession(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/usage", "/usage.csv", "/blocks", "/audit?user=x", "/limits", "/errors", "/scenarios", "/config"} {
		r := e.do("GET", p, nil, cred{})
		if r.code != 303 || r.hdr.Get("Location") != "/login?next="+url.QueryEscape(p) {
			t.Errorf("%s sem sessão: %d %v", p, r.code, r.hdr)
		}
	}
}

func TestOverviewPage(t *testing.T) {
	// Without Prometheus: /admin/stats of the in-process wwz.
	e := newEnv(t)
	vi := e.login("vi", viPass)
	r := e.do("GET", "/", nil, vi)
	if r.code != 200 {
		t.Fatalf("visão geral: %d %s", r.code, r.body)
	}
	has(t, "visão geral", r.body, `<a href="/" aria-current="page">Visão geral</a>`, `vi (viewer)`,
		`hx-headers='{"X-CSRF-Token":"`+vi.csrf+`"}'`, `name="csrf" value="`+vi.csrf+`"`,
		`<section id="live" hx-get="/" hx-trigger="every 2s" hx-swap="outerHTML">`, `fonte admin_stats`,
		`<option value="5m" selected>`, `<p class="big">—</p>`, "<td>memory</td>")
	// HTMX poll: only the live region, filter kept.
	r = e.do("GET", "/?window=1h&driver=openai&model=", nil, vi, hx)
	if r.code != 200 || strings.Contains(r.body, "<html") || !strings.HasPrefix(r.body, `<section id="live" hx-get="/?driver=openai&amp;window=1h"`) {
		t.Fatalf("fragmento: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/?window=2m", nil, vi); r.code != 400 || !strings.Contains(r.body, "Janela inválida") {
		t.Fatalf("janela inválida: %d %s", r.code, r.body)
	}

	// With Prometheus.
	e = newEnv(t, func(o *envOpts) {
		o.prom = promHandler(map[string]string{
			"status=~":                  sample(`"status":"429"`, "2"),
			"wwz_requests_total":        sample(``, "12.5"),
			"wwz_tokens_total":          sample(`"kind":"prompt"`, "100") + "," + sample(`"kind":"completion"`, "50"),
			"wwz_requests_in_flight":    sample(``, "3"),
			"wwz_ttft_seconds_bucket":   sample(``, "0.25"),
			"wwz_e2e_seconds_bucket":    sample(``, "1.5"),
			"wwz_errors_injected_total": sample(`"kind":"overloaded"`, "0.5"),
			"wwz_rate_limited_total":    sample(`"limit":"rpm"`, "1"),
		})
	})
	op := e.login("op", opPass)
	r = e.do("GET", "/?window=1m", nil, op)
	has(t, "prometheus", r.body, "fonte prometheus", `<p class="big">12.5</p>`, "prompt: <b>100</b>", "completion: <b>50</b>",
		`<p class="big">3</p>`, "p50 <b>250</b>", "p99 <b>1500</b>", "injetado: overloaded</td><td>0.5",
		"status 429</td><td>2", "rate limit: rpm</td><td>1", `<option value="1m" selected>`)

	// No source at all: 502 page, HTMX keeps polling with 200.
	e = newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	vi = e.login("vi", viPass)
	r = e.do("GET", "/", nil, vi)
	if r.code != 502 || !strings.Contains(r.body, "fonte de dados indisponível") || !strings.Contains(r.body, "<li>admin: ") {
		t.Fatalf("sem fontes: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/", nil, vi, hx); r.code != 200 || !strings.Contains(r.body, `role="alert"`) {
		t.Fatalf("sem fontes (htmx): %d %s", r.code, r.body)
	}
	has(t, "log", e.logs.String(), `"msg":"painel: tela"`)
}

func TestUsagePage(t *testing.T) {
	e := newEnv(t)
	at := time.Now().Add(-time.Minute)
	if _, err := e.st.Events.InsertBatch(ctx, []store.Event{
		{
			StreamID: "1-0", At: at, Type: "request", Driver: "openai", Model: "m1", EntityKind: "key", EntityID: "k1", Sample: 1,
			Data: []byte(`{"key":"k1","account":"acme","ip":"1.1.1.1","fields":{"prompt_tokens":10,"output_tokens":5,"cached_tokens":4,"cost_usd":0.01,"tool_calls":{"search":2}}}`),
		},
		{StreamID: "2-0", At: at, Type: "request", Driver: "anthropic", Model: "m2", Sample: 1, Data: []byte(`{"fields":{"prompt_tokens":1}}`)},
		{
			StreamID: "3-0", At: at, Type: "request", Driver: "openai", Model: "m3", Sample: 0.5,
			Data: []byte(`{"key":"k3","fields":{"cost_usd":0,"input_breakdown":{"system":40,"tool_schemas":30,"user":20,"assistant":10,"tool_calls":0,"tool_results":0,"images":0,"cached":25,"total":100}}}`),
		},
	}); err != nil {
		t.Fatal(err)
	}
	vi := e.login("vi", viPass)
	r := e.do("GET", "/usage?group=key&window=24h", nil, vi)
	if r.code != 200 {
		t.Fatalf("uso: %d %s", r.code, r.body)
	}
	has(t, "uso", r.body, `<option value="key" selected>`, `<option value="24h" selected>`,
		`href="/usage.csv?group=key&amp;window=24h"`, "Agrupado por key · janela 24h",
		"<td>k1</td><td>events</td><td>1</td><td>10</td><td>5</td><td>—</td><td>4</td><td>40 %</td><td>US$ 0.01</td>",
		`<span class="tag">search: 2</span>`, "<h2>Composição da entrada</h2>",
		// k1 has no breakdown: dash and the usage.extended hint.
		`<tr><td>k1</td><td colspan="12">— <span class="muted">sem decomposição: ligue <code>usage.extended</code> no wwz</span></td></tr>`,
		// k3 (0.4.0, no tool_calls): 0 tool calls; 2 requests with breakdown, stacked bar and % of total.
		"<td>US$ 0</td><td>0</td></tr>",
		`<tr><td>k3</td><td>2</td>`, `<rect class="in-0" x="0" y="0" width="40" height="10"><title>Sistema: 40 %</title></rect>`,
		`<rect class="in-3" x="90" y="0" width="10" height="10">`, "<td>80 <span class=\"muted\">(40 %)</span></td>",
		"<td>50 <span class=\"muted\">(25 %)</span></td><td>200 <span class=\"muted\">(100 %)</span></td>")
	if strings.Contains(r.body, `class="in-4"`) || strings.Contains(r.body, `class="in-7"`) {
		t.Fatalf("segmento vazio ou de cache/total na barra: %s", r.body)
	}
	// Default group model; the key-less request has no reasoning tokens nor tools.
	r = e.do("GET", "/usage", nil, vi, hx)
	has(t, "uso por modelo", r.body, "<td>m2</td>", "<td>m1</td>", "<td>—</td></tr>")
	r = e.do("GET", "/usage?group=ip&window=1m&driver=nada", nil, vi)
	has(t, "uso vazio", r.body, "Sem uso na janela.")
	r = e.do("GET", "/usage?group=x", nil, vi)
	if r.code != 400 || !strings.Contains(r.body, "group inválido") {
		t.Fatalf("grupo inválido: %d %s", r.code, r.body)
	}
	r = e.do("GET", "/usage.csv?group=key&window=24h", nil, vi)
	if r.code != 200 || r.hdr.Get("Content-Disposition") != `attachment; filename="usage-key.csv"` ||
		!strings.HasPrefix(r.body, "value,source,requests,") || !strings.Contains(r.body, "k1,events,1,10,5,,4,0.4,0.01,search=2,,,,,,,,,,0\n") ||
		!strings.Contains(r.body, ",input_total,input_breakdown_requests\n") || !strings.Contains(r.body, "k3,events,2,,,,,,0,0,80,60,40,20,0,0,0,50,200,2\n") {
		t.Fatalf("csv: %d %v %s", r.code, r.hdr, r.body)
	}
	if r := e.do("GET", "/usage.csv?group=x", nil, vi); r.code != 400 || !strings.Contains(r.body, `"error":"invalid"`) {
		t.Fatalf("csv inválido: %d %s", r.code, r.body)
	}
}

var actionForm = regexp.MustCompile(`hx-post="/blocks/act`)

// TestBlocksFlow blocks and unblocks a key by the HTMX form against a wwz
// running in process, checking its state, the list and the audit.
func TestBlocksFlow(t *testing.T) {
	e := newEnv(t)
	op, vi := e.login("op", opPass), e.login("vi", viPass)

	r := e.do("GET", "/blocks", nil, op)
	has(t, "bloqueios vazios", r.body, "Nenhuma entidade com este filtro.", `<form class="act" method="post" action="/blocks/act"`,
		`hx-confirm="Aplicar a ação à entidade informada?"`, `<option value="leak">leak</option>`, "Fonte admin")
	r = e.do("GET", "/blocks", nil, vi)
	if r.code != 200 || actionForm.MatchString(r.body) || strings.Contains(r.body, "<th>Ações</th>") {
		t.Fatalf("viewer vê ações: %d %s", r.code, r.body)
	}

	block := url.Values{"csrf": {op.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"block"}, "reason": {"vazou no log"}, "duration": {"10m"}}
	// CSRF and role.
	noCSRF := url.Values{"kind": {"key"}, "id": {"test-01"}, "action": {"block"}}
	if r := e.do("POST", "/blocks/act", noCSRF, op, hx); r.code != 403 || !strings.Contains(r.body, `"error":"csrf"`) {
		t.Fatalf("sem CSRF: %d %s", r.code, r.body)
	}
	viForm := url.Values{"csrf": {vi.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"block"}}
	if r := e.do("POST", "/blocks/act", viForm, vi, hx); r.code != 403 || !strings.Contains(r.body, `"error":"forbidden"`) {
		t.Fatalf("viewer agindo: %d %s", r.code, r.body)
	}

	// Block via HTMX: the live list comes back with the key and the result.
	r = e.do("POST", "/blocks/act?status=suspended", block, op, hx)
	if r.code != 200 || !strings.HasPrefix(r.body, `<section id="live" hx-get="/blocks?status=suspended"`) {
		t.Fatalf("bloquear: %d %s", r.code, r.body)
	}
	has(t, "bloqueada", r.body, "Ação block aplicada em key/test-01 (auditoria #1).", "<td>key/test-01</td>",
		`<span class="st st-suspended">suspended</span>`, "<td>vazou no log</td>", "<td>panel:op</td>",
		`hx-confirm="Desbloquear key/test-01?"`, `hx-confirm="Banir key/test-01?"`, `hx-confirm="Marcar a chave test-01 como vazada?"`,
		`action="/blocks/act?status=suspended"`)
	hasNot(t, "bloqueada", r.body, "Perdoar strikes", "Desbanir")
	st, err := e.admin.Entity(ctx, limits.Entity{Kind: limits.KindKey, ID: "test-01"})
	if err != nil || st.Status != limits.StatusSuspended || st.Until.IsZero() {
		t.Fatalf("estado no wwz: %+v %v", st, err)
	}
	// Viewer sees the blocked key without buttons.
	r = e.do("GET", "/blocks", nil, vi)
	has(t, "viewer", r.body, "<td>key/test-01</td>")
	hasNot(t, "viewer", r.body, "Desbloquear")

	// Unblock by a plain form post: 303 back to the list with the message.
	unblock := url.Values{"csrf": {op.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"unblock"}}
	r = e.do("POST", "/blocks/act?kind=key", unblock, op)
	loc := r.hdr.Get("Location")
	if r.code != 303 || loc != "/blocks?audit=2&done=unblock&kind=key&target=key%2Ftest-01" {
		t.Fatalf("desbloquear: %d %q", r.code, loc)
	}
	r = e.do("GET", loc, nil, op)
	has(t, "desbloqueada", r.body, "Ação unblock aplicada em key/test-01 (auditoria #2).", "Nenhuma entidade com este filtro.")
	if st, _ := e.admin.Entity(ctx, limits.Entity{Kind: limits.KindKey, ID: "test-01"}); st.Status != limits.StatusActive {
		t.Fatalf("estado depois do desbloqueio: %+v", st)
	}

	// Ban an IP, list it (all statuses) and see the unban button.
	ban := url.Values{"csrf": {op.csrf}, "kind": {"ip"}, "id": {"10.0.0.1"}, "action": {"ban"}, "reason": {"abuso"}, "duration": {"x"}}
	r = e.do("POST", "/blocks/act?status=all", ban, op, hx)
	has(t, "banido", r.body, "<td>ip/10.0.0.1</td>", `hx-confirm="Remover o banimento de ip/10.0.0.1?"`, "<td>key/test-01</td>",
		`hx-confirm="Bloquear key/test-01?"`)
	r = e.do("GET", "/blocks?status=banned&kind=ip", nil, op)
	has(t, "filtro", r.body, `<option value="banned" selected>`, `<option value="ip" selected>`, "<td>ip/10.0.0.1</td>")

	// Invalid input: error message, list kept.
	for name, f := range map[string]url.Values{
		"kind inválido":  {"csrf": {op.csrf}, "kind": {"x"}, "id": {"a"}, "action": {"block"}},
		"ação inválida":  {"csrf": {op.csrf}, "kind": {"key"}, "id": {"a"}, "action": {"credits"}},
		"duração":        {"csrf": {op.csrf}, "kind": {"key"}, "id": {"a"}, "action": {"block"}, "duration": {"-1m"}},
		"leak sem chave": {"csrf": {op.csrf}, "kind": {"ip"}, "id": {"a"}, "action": {"leak"}},
	} {
		r := e.do("POST", "/blocks/act", f, op, hx)
		if r.code != 200 || !strings.Contains(r.body, `role="alert"`) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
		if r := e.do("POST", "/blocks/act", f, op); r.code != 400 || !strings.Contains(r.body, "<html") {
			t.Errorf("%s (form): %d", name, r.code)
		}
	}

	// Audit has the block and the unblock.
	r = e.do("GET", "/audit?action=entity.&target=key/test-01", nil, vi)
	has(t, "auditoria", r.body, "<td>entity.block</td><td>key/test-01</td>", "<td>entity.unblock</td><td>key/test-01</td>",
		"<td>op</td><td>operator</td>", "<td>192.0.2.7</td>", "&#34;reason&#34;:&#34;vazou no log&#34;",
		`href="/api/v1/audit?action=entity.&amp;format=csv&amp;target=key%2Ftest-01"`)
	hasNot(t, "auditoria", r.body, "entity.ban")
}

func TestBlocksSourceDown(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	op := e.login("op", opPass)
	r := e.do("GET", "/blocks", nil, op)
	if r.code != 502 || !strings.Contains(r.body, "fonte de dados indisponível") {
		t.Fatalf("admin fora: %d %s", r.code, r.body)
	}
	f := url.Values{"csrf": {op.csrf}, "kind": {"key"}, "id": {"k"}, "action": {"unban"}}
	if r := e.do("POST", "/blocks/act", f, op); r.code != 502 {
		t.Fatalf("ação com admin fora: %d %s", r.code, r.body)
	}
	rows, _ := e.st.Audit.Query(ctx, store.AuditFilter{Limit: 1})
	if len(rows) != 1 || rows[0].Result != store.ResultError {
		t.Fatalf("auditoria do erro: %+v", rows)
	}
}

func TestBlocksStrikesAndBearer(t *testing.T) {
	// A fake admin listing an entity with strikes; the API token acts too.
	adm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"entities":[{"entity":{"kind":"user","id":"u1"},"status":"limited","strikes":2,"since":"2026-01-02T03:04:05Z"}]}`)
	}))
	t.Cleanup(adm.Close)
	e := newEnv(t, func(o *envOpts) { o.adminURL = adm.URL })
	r := e.do("GET", "/blocks?status=limited", nil, cred{bearer: apiToken})
	has(t, "strikes", r.body, "<td>user/u1</td>", "<td>2</td>", `hx-confirm="Perdoar os strikes de user/u1?"`,
		"<td>2026-01-02 03:04:05 UTC</td>", `<form class="who" method="post" action="/api/v1/logout">`)
	hasNot(t, "bearer", r.body, "hx-headers", "vazada")
}

func TestAuditPage(t *testing.T) {
	e := newEnv(t)
	for i := range 101 {
		res := store.ResultOK
		if i == 100 {
			res = store.ResultError
		}
		if _, err := e.st.Audit.Record(ctx, store.AuditEntry{
			User: "op", Role: "operator", Action: "entity.block", Target: "key/k", Diff: []byte(`{"request":{"reason":"<b>x</b>"}}`),
			Result: res, Error: map[bool]string{true: "falhou"}[i == 100],
		}); err != nil {
			t.Fatal(err)
		}
	}
	vi := e.login("vi", viPass)
	r := e.do("GET", "/audit?user=op&result=error", nil, vi)
	has(t, "auditoria", r.body, `<span class="st st-error">error</span><br><small>falhou</small>`, "<td>—</td>",
		"&#34;&lt;b&gt;x&lt;/b&gt;&#34;", `<option value="error" selected>`)
	hasNot(t, "escape", r.body, "<b>x</b>", "Mais antigos")
	// Pagination.
	r = e.do("GET", "/audit?since=2000-01-01&until=2999-01-01T10:00", nil, vi)
	m := regexp.MustCompile(`<a href="(/audit\?cursor=[^"]+)">Mais antigos`).FindStringSubmatch(r.body)
	if r.code != 200 || m == nil || !strings.Contains(r.body, "since=2000-01-01T00%3A00%3A00Z") {
		t.Fatalf("paginação: %d %s", r.code, r.body)
	}
	next := strings.ReplaceAll(m[1], "&amp;", "&")
	if r := e.do("GET", next, nil, vi, hx); r.code != 200 || strings.Count(r.body, "<tr>") != 2 {
		t.Fatalf("segunda página: %d %s", r.code, r.body)
	}
	for q, want := range map[string]string{
		"since=ontem":   "Data inválida",
		"until=x":       "Data inválida",
		"result=talvez": "result inválido",
	} {
		r := e.do("GET", "/audit?"+q, nil, vi)
		if r.code != 400 || !strings.Contains(r.body, want) {
			t.Errorf("%s: %d %s", q, r.code, r.body)
		}
	}
	r = e.do("GET", "/audit?since="+url.QueryEscape("2000-01-01T00:00:00Z"), nil, vi)
	has(t, "rfc3339", r.body, "Mais antigos")
}

func TestRenderError(t *testing.T) {
	// A template failing at execution is logged (headers already sent).
	var logs bytes.Buffer
	w := New(Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	rec := httptest.NewRecorder()
	w.render(rec, httptest.NewRequest("GET", "/", nil), "overview", 200, view{Data: 1})
	if !strings.Contains(logs.String(), "painel: renderizar") {
		t.Fatalf("erro de template não logado: %s", logs.String())
	}
	if New(Options{}).log == nil {
		t.Fatal("logger padrão")
	}
}

func TestFormat(t *testing.T) {
	f, i := 1.5, int64(7)
	tm := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	for _, c := range [][2]string{
		{num(0), "0"},
		{num(12.5), "12.5"},
		{num(100), "100"},
		{num(0.123456), "0.1235"},
		{optFloat(nil), "—"},
		{optFloat(&f), "1.5"},
		{optInt(nil), "—"},
		{optInt(&i), "7"},
		{pct(nil), "—"},
		{pct(&f), "150 %"},
		{usd(nil), "—"},
		{usd(&f), "US$ 1.5"},
		{when(time.Time{}), "—"},
		{when(tm), "2026-01-02 02:04:05 UTC"},
		{whenPtr(nil), "—"},
		{whenPtr(&tm), "2026-01-02 02:04:05 UTC"},
	} {
		if c[0] != c[1] {
			t.Errorf("%q != %q", c[0], c[1])
		}
	}
	if (view{}).CSRF() != "" {
		t.Fatal("CSRF sem identidade")
	}
	if _, ok := parseTime("x"); ok {
		t.Fatal("parseTime")
	}
}

// TestBreakdownZeroTotal: with total 0 (or missing) the cells keep the
// tokens but there is no share nor bar segment.
func TestBreakdownZeroTotal(t *testing.T) {
	zero, five := 0.0, 5.0
	for _, b := range []api.UsageInputBreakdown{{User: &five, Total: &zero}, {User: &five}} {
		cells := breakdown(b)
		if len(cells) != 9 || cells[2].Label != "Usuário" || *cells[2].Value != 5 || cells[2].Share != nil || cells[2].W != 0 || cells[0].Value != nil {
			t.Fatalf("cells %+v", cells)
		}
	}
}
