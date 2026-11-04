package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

const scenarioYAML = "name: s1\ndescription: um passo\nsteps:\n  - id: a\n    expect: { tool_results: 0 }\n    content: { tokens: \"5..10\" }\n"

// anyAction matches the action forms of the task 053 screens.
var anyAction = regexp.MustCompile(`hx-post="/(limits|errors|scenarios)/act|action="/config/policies"`)

// testValkey returns a panel Valkey source under a random prefix (removed at
// the end) on the database of internal/panel/api (WWZ_TEST_VALKEY_DB−3,
// never flushed), or skips.
func testValkey(t *testing.T) (*source.Valkey, valkey.Client, string) {
	t.Helper()
	if os.Getenv("WWZ_TEST_VALKEY") == "" {
		t.Skip("Valkey de teste ausente: rode `make verify-quick`")
	}
	addr := os.Getenv("WWZ_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:16379"
	}
	db := 15
	if s := os.Getenv("WWZ_TEST_VALKEY_DB"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 {
			t.Fatalf("WWZ_TEST_VALKEY_DB=%q: os testes nunca usam o banco 0", s)
		}
		db = n
	}
	if db -= 3; db <= 0 {
		t.Skip("sem banco de teste livre abaixo de WWZ_TEST_VALKEY_DB")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := "wwzweb" + hex.EncodeToString(b)
	raw, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, SelectDB: db, ForceSingleClient: true, DisableCache: true})
	if err != nil {
		t.Fatalf("valkey: %v", err)
	}
	t.Cleanup(func() {
		keys, _ := raw.Do(ctx, raw.B().Keys().Pattern(prefix+":*").Build()).AsStrSlice()
		if len(keys) > 0 {
			_ = raw.Do(ctx, raw.B().Del().Key(keys...).Build()).Error()
		}
		raw.Close()
	})
	src, err := source.NewValkey(panelcfg.Valkey{Addr: []string{addr}, Prefix: prefix}, source.ValkeyOptions{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src, raw, prefix
}

func TestControlPagesViewer(t *testing.T) {
	e := newEnv(t)
	vi := e.login("vi", viPass)
	for _, p := range []string{"/limits?kind=key&id=test-01", "/errors", "/scenarios", "/config"} {
		r := e.do("GET", p, nil, vi)
		if r.code != 200 || anyAction.MatchString(r.body) || strings.Contains(r.body, "<th>Ações</th>") {
			t.Errorf("%s: viewer vê ações: %d %s", p, r.code, r.body)
		}
		if regexp.MustCompile(`<script(?:[^>]*)>[^<]|<style|\son[a-z]+=|style=`).MatchString(r.body) {
			t.Errorf("%s: recurso inline", p)
		}
		has(t, p, r.body, `href="/limits"`, `href="/errors"`, `href="/scenarios"`, `href="/config"`, `aria-current="page"`, `<section id="live" hx-get="`)
	}
	// Viewer posting an action: 403.
	for _, p := range []string{"/limits/act", "/errors/act", "/scenarios/act", "/config/policies"} {
		if r := e.do("POST", p, url.Values{"csrf": {vi.csrf}}, vi, hx); r.code != 403 {
			t.Errorf("%s viewer: %d", p, r.code)
		}
	}
}

func TestLimitsPage(t *testing.T) {
	e := newEnv(t)
	op := e.login("op", opPass)
	if _, err := e.admin.Block(ctx, limits.Entity{Kind: limits.KindKey, ID: "test-01"}, source.BlockRequest{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	r := e.do("GET", "/limits", nil, op)
	has(t, "limites vazio", r.body, "Escolha uma entidade para ver os limites.", `<a class="tag" href="/limits?id=test-01&amp;kind=key">key/test-01</a>`)
	hasNot(t, "limites vazio", r.body, "Alterar tier")
	r = e.do("GET", "/limits?kind=key&id=test-01", nil, op)
	has(t, "limites", r.body, `<p class="big">key/test-01</p>`, `<span class="st st-suspended">suspended</span>`, "Limites ao vivo só com Valkey",
		`hx-confirm="Alterar o tier da conta acme?"`, `hx-confirm="Comprar créditos para a conta acme?"`, "Tier da conta acme", `hx-confirm="Zerar os contadores de key/test-01?"`, `<a href="/config">Configuração</a>`,
		`hx-get="/limits?id=test-01&amp;kind=key"`)

	// Tier via HTMX: live region with the result. The tier is the account's
	// (ADR 0051 §5), so the override shows on account/acct-test.
	tier := url.Values{"csrf": {op.csrf}, "kind": {"account"}, "id": {"acct-test"}, "action": {"tier"}, "tier": {"default"}}
	r = e.do("POST", "/limits/act?kind=account&id=acct-test", tier, op, hx)
	if r.code != 200 || !strings.HasPrefix(r.body, `<section id="live"`) {
		t.Fatalf("tier: %d %s", r.code, r.body)
	}
	has(t, "tier", r.body, "Ação tier aplicada em account/acct-test (auditoria #1).", "sobreposição: default")
	// Purchase: add_usd on the account of the key, shown as purchased.
	buy := url.Values{"csrf": {op.csrf}, "kind": {"account"}, "id": {"acct-test"}, "action": {"credits"}, "add_usd": {"7.5"}}
	r = e.do("POST", "/limits/act?kind=account&id=acct-test", buy, op, hx)
	has(t, "compra", r.body, "Ação credits aplicada em account/acct-test (auditoria #2).", "US$ 7.5", "saldo: US$ 7.5")
	// Reset by a plain form post: 303 with the result.
	reset := url.Values{"csrf": {op.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"reset"}}
	r = e.do("POST", "/limits/act", reset, op)
	loc := r.hdr.Get("Location")
	if r.code != 303 || !strings.HasPrefix(loc, "/limits?done=") || !strings.Contains(loc, "id=test-01&kind=key") {
		t.Fatalf("reset: %d %q", r.code, loc)
	}
	has(t, "flash", e.do("GET", loc, nil, op).body, "Ação reset aplicada em key/test-01 (auditoria #3).")
	if rows, _ := e.st.Audit.Query(ctx, store.AuditFilter{Limit: 1}); rows[0].Action != "entity.reset" {
		t.Fatalf("auditoria: %+v", rows)
	}
	// Invalid input.
	for name, f := range map[string]url.Values{
		"ação":          {"csrf": {op.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"ban"}},
		"kind":          {"csrf": {op.csrf}, "kind": {"x"}, "id": {"a"}, "action": {"tier"}},
		"compra":        {"csrf": {op.csrf}, "kind": {"account"}, "id": {"acct-test"}, "action": {"credits"}, "add_usd": {"x"}},
		"compra zero":   {"csrf": {op.csrf}, "kind": {"account"}, "id": {"acct-test"}, "action": {"credits"}, "add_usd": {"0"}},
		"tier recusado": {"csrf": {op.csrf}, "kind": {"key"}, "id": {"test-01"}, "action": {"tier"}, "tier": {"nao-existe"}},
	} {
		if r := e.do("POST", "/limits/act", f, op, hx); r.code != 200 || !strings.Contains(r.body, `role="alert"`) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	if r := e.do("GET", "/limits?kind=x&id=a", nil, op); r.code != 400 || !strings.Contains(r.body, "kind inválido") {
		t.Fatalf("kind inválido: %d", r.code)
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("GET", "/limits?kind=key&id=k", nil, d.login("op", opPass)); r.code != 502 {
		t.Fatalf("admin fora: %d", r.code)
	}
}

func TestLimitsPageValkey(t *testing.T) {
	src, _, _ := testValkey(t)
	e := newEnv(t, func(o *envOpts) { o.valkey = src })
	op := e.login("op", opPass)
	r := e.do("GET", "/limits?kind=key&id=test-01", nil, op)
	has(t, "chave", r.body, "Fonte</h2><p>valkey", `<p class="big">gold</p>`, "Tier da conta acme", "Esta chave não declara teto próprio")
	hasNot(t, "chave", r.body, "<td>rpm</td>")
	r = e.do("GET", "/limits?kind=account&id=acme", nil, op)
	has(t, "limites ao vivo", r.body, "<td>rpm</td><td>minuto</td><td>1000</td><td>1000</td>", `<progress value="0" max="1000">0</progress>`,
		"<td>rpd</td><td>dia</td><td>5000</td>", "Fonte</h2><p>valkey", `<p class="big">gold</p>`, "Comprado", "US$ 0")
	if strings.Index(r.body, "<td>rpm</td>") > strings.Index(r.body, "<td>rpd</td>") {
		t.Fatal("limites fora da ordem das janelas")
	}
	// A tier with no limits: the empty message.
	r = e.do("GET", "/limits?kind=ip/x&id=1.2.3.4", nil, op)
	if r.code != 400 {
		t.Fatalf("kind com barra: %d", r.code)
	}
	adm := newEnv(t, func(o *envOpts) {
		o.valkey = src
		o.adminURL = fakeAdminURL(t, map[string]string{"/admin/config": `{}`, "/admin/entities": `{"entities":[]}`})
	})
	has(t, "sem limites", adm.do("GET", "/limits?kind=account&id=z", nil, adm.login("op", opPass)).body, "Nenhum limite configurado para o tier efetivo.")
}

func TestErrorsPage(t *testing.T) {
	e := newEnv(t)
	op := e.login("op", opPass)
	e.st.Events.InsertBatch(ctx, []store.Event{{StreamID: "1", At: time.Now(), Type: store.EventError, Driver: "openai", Model: "m", Status: 429, Error: "rate_limit", RequestID: "req-1", Data: json.RawMessage(`{}`)}}) //nolint:errcheck // checked by the page
	// A pending one-shot error of the wwz.
	req, _ := http.NewRequest("POST", e.admin.BaseURL()+"/admin/chaos/next", strings.NewReader(`{"test_id":"t9","type":"overloaded"}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 201 {
		t.Fatalf("chaos/next: %v %v", res, err)
	}
	r := e.do("GET", "/errors", nil, op)
	has(t, "erros", r.body, "fonte admin_stats (totais desde o início da instância)", "Injetados por tipo", "Erros do simulador",
		"bad_request: <b>—</b>", "Nenhum evento de caos ativo.", "One-shot pendente:", "t9", "<td>rate_limit</td><td>req-1</td>",
		`<button type="submit" name="action" value="injection_on">Ligar injeção</button>`, `hx-confirm="Disparar o evento de caos?"`)
	hasNot(t, "erros", r.body, "Encerrar todos")

	act := func(f url.Values, opts ...reqOpt) resp {
		f.Set("csrf", op.csrf)
		return e.do("POST", "/errors/act?window=1h", f, op, opts...)
	}
	r = act(url.Values{"action": {"chaos_start"}, "type": {"overloaded"}, "duration": {"30s"}, "models": {"*, wwz-fast"}, "rate": {"50"}, "test_id": {"t1"}}, hx)
	has(t, "caos", r.body, "Evento de caos disparado (auditoria #1).", "Encerrar todos", `hx-get="/errors?window=1h"`, `value="chaos_stop"`)
	id := regexp.MustCompile(`name="id" value="([^"]+)"`).FindStringSubmatch(r.body)
	if id == nil {
		t.Fatalf("evento sem id: %s", r.body)
	}
	has(t, "parar", act(url.Values{"action": {"chaos_stop"}, "id": {id[1]}}, hx).body, "1 evento(s) de caos encerrado(s) (auditoria #2).")
	has(t, "injeção", act(url.Values{"action": {"injection_on"}, "target": {"model"}, "name": {"wwz-fast"}, "rate": {"30"}, "types": {"overloaded"}}, hx).body,
		"Injeção atualizada em model wwz-fast (auditoria #3).")
	has(t, "perfil", act(url.Values{"action": {"injection_on"}, "target": {"profile"}, "name": {"flaky"}, "rate": {"10"}}, hx).body,
		"Injeção atualizada em profile flaky (auditoria #4).")
	r = act(url.Values{"action": {"injection_off"}, "target": {"model"}, "name": {"wwz-fast"}})
	if r.code != 303 || !strings.HasPrefix(r.hdr.Get("Location"), "/errors?done=") {
		t.Fatalf("desligar (form): %d %v", r.code, r.hdr)
	}
	for name, f := range map[string]url.Values{
		"taxa da injeção": {"action": {"injection_on"}, "name": {"wwz-fast"}, "rate": {"x"}},
		"taxa do caos":    {"action": {"chaos_start"}, "type": {"overloaded"}, "duration": {"1s"}, "rate": {"x"}},
		"caos recusado":   {"action": {"chaos_start"}, "duration": {"1s"}},
		"ação":            {"action": {"nada"}},
	} {
		if r := act(f, hx); r.code != 200 || !strings.Contains(r.body, `role="alert"`) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	if r := e.do("GET", "/errors?window=2h", nil, op); r.code != 400 {
		t.Fatalf("janela: %d", r.code)
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("GET", "/errors", nil, d.login("op", opPass)); r.code != 502 {
		t.Fatalf("admin fora: %d", r.code)
	}
}

func TestScenariosPage(t *testing.T) {
	e := newEnv(t)
	op := e.login("op", opPass)
	r := e.do("GET", "/scenarios", nil, op)
	has(t, "cenários vazio", r.body, "Nenhum cenário carregado.", `<textarea name="yaml" rows="12" required></textarea>`, "Carregar / simular")
	act := func(f url.Values, opts ...reqOpt) resp {
		f.Set("csrf", op.csrf)
		return e.do("POST", "/scenarios/act?window=1h", f, op, opts...)
	}
	r = act(url.Values{"action": {"load"}, "yaml": {scenarioYAML}}, hx)
	has(t, "carregar", r.body, "Cenários válidos e carregados: s1 (auditoria #1).", "<td>s1</td><td>um passo</td><td>1</td>",
		`hx-confirm="Desativar o cenário s1?"`)
	has(t, "substituir", act(url.Values{"action": {"load"}, "yaml": {scenarioYAML}}, hx).body, "(substituídos: s1)")
	// Simulation failing: 400 page with the messages and the YAML kept.
	r = act(url.Values{"action": {"load"}, "yaml": {"name: s2\nsteps: []\n"}})
	has(t, "inválido", r.body, `role="alert"`, "o wwz recusou a requisição", "name: s2")
	if r.code != 400 {
		t.Fatalf("inválido: %d", r.code)
	}
	// Steps served (events, no Prometheus).
	e.st.Events.InsertBatch(ctx, []store.Event{ //nolint:errcheck // checked by the page
		{StreamID: "1", At: time.Now(), Type: store.EventRequest, Data: json.RawMessage(`{"scenario":"s1","step":"a"}`)},
		{StreamID: "2", At: time.Now(), Type: store.EventRequest, Data: json.RawMessage(`{"scenario":"s1","step":"b"}`)},
	})
	has(t, "passos", e.do("GET", "/scenarios", nil, op).body, "<td>a: 1 · b: 1</td><td>0 %</td>")
	has(t, "remover", act(url.Values{"action": {"delete"}, "name": {"s1"}}, hx).body, "Cenário s1 removido (auditoria #4).", "Nenhum cenário carregado.")
	r = act(url.Values{"action": {"delete"}, "name": {"s1"}})
	if r.code != 404 {
		t.Fatalf("remover de novo: %d", r.code)
	}
	r = act(url.Values{"action": {"load"}, "yaml": {scenarioYAML}})
	if r.code != 303 || !strings.HasPrefix(r.hdr.Get("Location"), "/scenarios?done=") {
		t.Fatalf("carregar (form): %d %v", r.code, r.hdr)
	}
	if r := act(url.Values{"action": {"x"}}, hx); !strings.Contains(r.body, `role="alert"`) {
		t.Fatalf("ação: %s", r.body)
	}
	if r := e.do("GET", "/scenarios?window=2h", nil, op); r.code != 400 {
		t.Fatalf("janela: %d", r.code)
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	if r := d.do("GET", "/scenarios", nil, d.login("op", opPass)); r.code != 502 {
		t.Fatalf("admin fora: %d", r.code)
	}
}

func TestConfigPage(t *testing.T) {
	e := newEnv(t)
	op := e.login("op", opPass)
	r := e.do("GET", "/config", nil, op)
	has(t, "config", r.body, "YAML efetivo da instância "+e.admin.BaseURL(), "Configuração efetiva", "tiers:", "rpm: 1000",
		"Fonte config · versão 0 · só leitura", "Nenhuma entidade com tier atribuído.", "&#34;gold&#34;")
	hasNot(t, "config", r.body, `action="/config/policies"`)
	for name, c := range map[string]struct {
		f    url.Values
		code int
		msg  string
	}{
		"sem valkey": {url.Values{"version": {"0"}, "tiers": {"{}"}}, 409, "políticas indisponíveis sem Valkey"},
		"versão":     {url.Values{"version": {"x"}, "tiers": {"{}"}}, 400, "versão inválida"},
		"tiers":      {url.Values{"version": {"0"}, "tiers": {"[1]"}}, 400, "tiers deve ser um objeto JSON"},
	} {
		c.f.Set("csrf", op.csrf)
		r := e.do("POST", "/config/policies", c.f, op)
		if r.code != c.code || !strings.Contains(r.body, c.msg) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	d := newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	r = d.do("GET", "/config", nil, d.login("op", opPass))
	if r.code != 502 || strings.Count(r.body, "fonte de dados indisponível") != 2 {
		t.Fatalf("admin fora: %d %s", r.code, r.body)
	}
}

func TestConfigPageValkey(t *testing.T) {
	src, raw, prefix := testValkey(t)
	e := newEnv(t, func(o *envOpts) { o.valkey = src })
	op := e.login("op", opPass)
	r := e.do("GET", "/config", nil, op)
	has(t, "políticas", r.body, "Fonte valkey · versão 0", `<form class="act stack" method="post" action="/config/policies">`,
		`name="version" value="0"`, "Publicar para o cluster")
	f := url.Values{"csrf": {op.csrf}, "version": {"0"}, "tiers": {`{"tier-4": {"rpm": 10}}`}}
	r = e.do("POST", "/config/policies", f, op)
	loc := r.hdr.Get("Location")
	if r.code != 303 || !strings.HasPrefix(loc, "/config?done=") {
		t.Fatalf("publicar: %d %s", r.code, r.body)
	}
	if err := raw.Do(ctx, raw.B().Hset().Key(prefix+":cfg:policies").FieldValue().FieldValue("ent:key:test-01", "tier-4").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	r = e.do("GET", loc, nil, op)
	has(t, "publicado", r.body, "Políticas publicadas na versão 1 (auditoria #1).", "Fonte valkey · versão 1", "key:test-01 → tier-4", "&#34;tier-4&#34;")
	// Stale version: 409 with the text kept in the form.
	f.Set("tiers", `{"tier-5": {}}`)
	r = e.do("POST", "/config/policies", f, op)
	if r.code != 409 || !strings.Contains(r.body, "tier-5") {
		t.Fatalf("versão velha: %d %s", r.code, r.body)
	}
}

// fakeAdminURL answers each path with a fixed body (404 for the others).
func fakeAdminURL(t *testing.T, bodies map[string]string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","message":"sem rota"}`)
			return
		}
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(s.Close)
	return s.URL
}
