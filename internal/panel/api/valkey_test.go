package api

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	panelcfg "github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
)

// testValkey returns the test Valkey of scripts/verify.sh or skips. go test
// runs packages in parallel: internal/limits uses WWZ_TEST_VALKEY_DB, cmd/wwz
// DB−1, internal/panel/source DB−2 (with FLUSHDB) and this package DB−3,
// under a random prefix and without FLUSHDB.
func testValkey(t *testing.T) (string, int) {
	t.Helper()
	if os.Getenv("WWZ_TEST_VALKEY") == "" {
		t.Skip("Valkey de teste ausente: rode `make verify-quick` (ou exporte WWZ_TEST_VALKEY=local WWZ_TEST_VALKEY_ADDR=127.0.0.1:16379)")
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
	db -= 3
	if db <= 0 {
		t.Skip("sem banco de teste livre abaixo de WWZ_TEST_VALKEY_DB")
	}
	return addr, db
}

type vkEnv struct {
	*env
	raw    valkey.Client
	prefix string
	src    *source.Valkey
}

func (v *vkEnv) cmd(t *testing.T, args ...string) {
	t.Helper()
	if err := v.raw.Do(ctx, v.raw.B().Arbitrary(args[0]).Keys(args[1]).Args(args[2:]...).Build()).Error(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
}

// newVKEnv runs a wwz whose store is Valkey (heartbeat included) and a panel
// reading the same keys, under a random prefix removed at the end.
func newVKEnv(t *testing.T, mods ...func(*envOpts)) *vkEnv {
	t.Helper()
	addr, db := testValkey(t)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := "wwzapi" + hex.EncodeToString(b)
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
	st, err := limits.NewValkey(ctx, config.StoreConfig{Addr: []string{addr}, Prefix: prefix},
		limits.ValkeyOptions{DB: db, Instance: "wwz-1", Version: "v9", Addr: "10.0.0.9:9000", Heartbeat: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	src, err := source.NewValkey(panelcfg.Valkey{Addr: []string{addr}, Prefix: prefix}, source.ValkeyOptions{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	all := append([]func(*envOpts){func(o *envOpts) { o.wwzStore = st; o.valkey = src; o.consumer = true }}, mods...)
	return &vkEnv{env: newEnv(t, all...), raw: raw, prefix: prefix, src: src}
}

func detail(t *testing.T, e *env, path string) EntityDetail {
	t.Helper()
	var d EntityDetail
	r := e.do("GET", "/api/v1/entities/"+path, "", operator)
	if r.code != 200 {
		t.Fatalf("%s: %d %s", path, r.code, r.body)
	}
	r.decode(t, &d)
	return d
}

func limitOf(d EntityDetail, name string) *LiveLimit {
	for i := range d.Limits {
		if d.Limits[i].Name == name {
			return &d.Limits[i]
		}
	}
	return nil
}

func TestValkeyMode(t *testing.T) {
	v := newVKEnv(t)
	e := v.env

	// Block through the panel: wwz writes Valkey, the panel reads it back.
	if r := e.do("POST", "/api/v1/entities/key/test-01/block", `{"reason":"cluster"}`, operator); r.code != 200 {
		t.Fatalf("block: %d %s", r.code, r.body)
	}
	var list EntityList
	r := e.do("GET", "/api/v1/entities?"+q("status", "suspended", "kind", "key"), "", operator)
	r.decode(t, &list)
	if r.code != 200 || list.Source != SourceValkey || len(list.Entities) != 1 || list.Entities[0].Entity.ID != "test-01" {
		t.Fatalf("lista no valkey: %d %s", r.code, r.body)
	}
	d := detail(t, e, "key/test-01")
	// A key shows the tier of its account and only counts its own ceiling.
	if d.Source != SourceValkey || d.State.Status != limits.StatusSuspended || d.EffectiveTier != "gold" || d.AccountID != "acme" || len(d.Limits) != 0 {
		t.Fatalf("detalhe: %+v", d)
	}
	if d := detail(t, e, "account/acme"); d.EffectiveTier != "gold" || d.AccountID != "" || limitOf(d, "rpm") == nil || limitOf(d, "rpm").Limit != 1000 || limitOf(d, "rpm").Window == "" {
		t.Fatalf("rpm de gold na conta: %+v", d)
	}
	// Purchases are counted on the account and shown in its state.
	if r := e.do("POST", "/api/v1/entities/account/acme/credits", `{"add_usd":12.5}`, operator); r.code != 200 {
		t.Fatalf("add_usd: %d %s", r.code, r.body)
	}
	if d := detail(t, e, "account/acme"); d.State.PurchasedUSD != 12.5 {
		t.Fatalf("purchased_usd: %+v", d.State)
	}

	// cfg:policies: tier:gold replaced, invalid tier ignored, test-02
	// assigned to gold by ent:*.
	v.cmd(t, "HSET", v.prefix+":cfg:policies", "version", "1", "tier:gold", `{"rpm": 5, "concurrency": 2}`, "tier:ruim", `[1]`, "ent:key:test-02", "gold")
	v.cmd(t, "HSET", v.prefix+":tb:{account:acme}", "rpm:ts", strconv.FormatInt(time.Now().UnixMilli(), 10), "rpm:t", "1", "rpm:cap", "5", "rpm:rate", "0.1")
	// The legacy ent:key:test-02 is the tier of its account; the key keeps
	// its own ceiling of 7.
	if d := detail(t, e, "key/test-02"); d.EffectiveTier != "gold" || d.AccountID != "acct-test-02" || limitOf(d, "rpm") == nil || limitOf(d, "rpm").Limit != 7 {
		t.Fatalf("test-02 (gold herdado + teto da chave): %+v", d)
	}
	if d := detail(t, e, "account/acme"); limitOf(d, "rpm").Limit != 5 || limitOf(d, "rpm").Reset == nil ||
		limitOf(d, "rpm").Remaining >= 5 || limitOf(d, "concurrency").Window != "none" {
		t.Fatalf("tier:gold das políticas: %+v", d.Limits)
	}
	// Override on the entity wins (the wwz puts the tier on the key's
	// account, ADR 0051 §5).
	if _, err := e.admin.SetTier(ctx, limits.Entity{Kind: limits.KindKey, ID: "test-01"}, "default"); err != nil {
		t.Fatal(err)
	}
	if d := detail(t, e, "account/acme"); d.EffectiveTier != "default" || limitOf(d, "rpm").Limit != 100 {
		t.Fatalf("sobreposição: %+v", d)
	}
	if d := detail(t, e, "key/test-01"); d.EffectiveTier != "default" {
		t.Fatalf("chave herda a sobreposição da conta: %+v", d)
	}
	// ent:account wins over the legacy ent:key.
	if d := detail(t, e, "key/test-02"); d.EffectiveTier != "gold" {
		t.Fatalf("test-02: %+v", d)
	}
	v.cmd(t, "HSET", v.prefix+":cfg:policies", "ent:account:acct-test-02", "default")
	if d := detail(t, e, "key/test-02"); d.EffectiveTier != "default" {
		t.Fatalf("ent:account vence ent:key: %+v", d)
	}
	// A project counts only projects[]; an ip has no tier.
	if d := detail(t, e, "project/proj-test"); d.EffectiveTier != "" || len(d.Limits) != 0 {
		t.Fatalf("projeto: %+v", d)
	}
	if d := detail(t, e, "key/desconhecida"); d.EffectiveTier != "" || d.AccountID != "" || len(d.Limits) != 0 {
		t.Fatalf("chave fora da configuração: %+v", d)
	}
	if d := detail(t, e, "ip/10.9.9.9"); d.EffectiveTier != "" || d.State.Status != limits.StatusActive {
		t.Fatalf("ip sem estado: %+v", d)
	}
	// Instances from the heartbeat.
	var insts struct {
		Instances []Instance `json:"instances"`
		Source    string     `json:"source"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(insts.Instances) == 0 && time.Now().Before(deadline) {
		r = e.do("GET", "/api/v1/instances", "", operator)
		r.decode(t, &insts)
		time.Sleep(20 * time.Millisecond)
	}
	if insts.Source != SourceValkey || len(insts.Instances) != 1 || insts.Instances[0].ID != "wwz-1" ||
		insts.Instances[0].Version != "v9" || insts.Instances[0].Beat == nil || insts.Instances[0].RPS == nil {
		t.Fatalf("instâncias: %s", r.body)
	}
	var ov Overview
	e.do("GET", "/api/v1/overview", "", operator).decode(t, &ov)
	if len(ov.Instances) != 1 {
		t.Fatalf("visão geral sem instâncias do valkey: %+v", ov.Instances)
	}

	// Ready and metrics with Valkey.
	var rd Ready
	e.do("GET", "/health/ready", "", cred{}).decode(t, &rd)
	if rd.Sources.Valkey != StateOK {
		t.Fatalf("ready: %+v", rd)
	}
	e.api.consumer.RefreshLag(ctx)
	body := string(e.do("GET", "/metrics", "", cred{}).body)
	if !strings.Contains(body, "wwz_panel_stream_lag ") || !strings.Contains(body, `wwz_panel_source_up{source="valkey"} 1`) {
		t.Fatalf("metrics: %s", body)
	}

	// Live limits unavailable: the state still comes, limits empty.
	v.cmd(t, "SET", v.prefix+":tb:{account:zz}", "x")
	if d := detail(t, e, "account/zz"); len(d.Limits) != 0 || d.EffectiveTier != "default" {
		t.Fatalf("tb: com tipo errado: %+v", d)
	}
	v.cmd(t, "DEL", v.prefix+":cfg:policies")
	v.cmd(t, "SET", v.prefix+":cfg:policies", "x")
	if d := detail(t, e, "key/test-01"); len(d.Limits) != 0 || d.EffectiveTier != "" {
		t.Fatalf("cfg:policies com tipo errado: %+v", d)
	}
	if !strings.Contains(e.logs.String(), "limites ao vivo") {
		t.Fatal("falha dos limites não logada")
	}
}

// TestValkeyKeyTierAndCreditsActOnAccount: tier and add_usd posted on a key
// land on its account in Valkey and are audited.
func TestValkeyKeyTierAndCreditsActOnAccount(t *testing.T) {
	v := newVKEnv(t)
	e := v.env
	if d := detail(t, e, "account/acme"); d.EffectiveTier != "gold" {
		t.Fatalf("partida: %+v", d)
	}
	if r := e.do("POST", "/api/v1/entities/key/test-01/tier", `{"tier":"default"}`, operator); r.code != 200 {
		t.Fatalf("tier: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != "entity.tier" || a.Target != "key/test-01" || !strings.Contains(string(a.Diff), `"tier":"default"`) {
		t.Fatalf("auditoria do tier: %+v", a)
	}
	if d := detail(t, e, "account/acme"); d.EffectiveTier != "default" {
		t.Fatalf("effective_tier da conta: %+v", d)
	}
	if r := e.do("POST", "/api/v1/entities/key/test-01/credits", `{"add_usd":10}`, operator); r.code != 200 {
		t.Fatalf("add_usd: %d %s", r.code, r.body)
	}
	if a := lastRow(t, e); a.Action != "entity.credits" || a.Target != "key/test-01" {
		t.Fatalf("auditoria dos créditos: %+v", a)
	}
	if d := detail(t, e, "account/acme"); d.State.PurchasedUSD != 10 {
		t.Fatalf("purchased_usd da conta: %+v", d.State)
	}
	if d := detail(t, e, "user/alguem"); d.EffectiveTier != "" || len(d.Limits) != 0 {
		t.Fatalf("usuário: %+v", d)
	}
}

func TestValkeyEffectiveLimitsAdminFailures(t *testing.T) {
	for name, url := range map[string]func(*testing.T) string{
		"admin fora": deadURL,
		"config inválida": func(t *testing.T) string {
			return fakeAdmin(t, map[string]string{"/admin/config": `{"tiers": 5}`})
		},
	} {
		v := newVKEnv(t, func(o *envOpts) { o.adminURL = url(t) })
		if d := detail(t, v.env, "key/test-01"); d.Source != SourceValkey || len(d.Limits) != 0 || d.EffectiveTier != "" {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	// Configuration without tiers: default tier, no limits.
	v := newVKEnv(t, func(o *envOpts) { o.adminURL = fakeAdmin(t, map[string]string{"/admin/config": `{}`}) })
	if d := detail(t, v.env, "account/acme"); d.EffectiveTier != "default" || len(d.Limits) != 0 {
		t.Fatalf("sem tiers: %+v", d)
	}
}

// TestValkeyStreamLagMetric checks that /metrics publishes the real backlog of
// the panel group on ev:stream (wwz_panel_stream_lag, base of the "painel
// atrasado" alert of spec 016) and that it drops to 0 once the consumer
// ingests the entries (task 055).
func TestValkeyStreamLagMetric(t *testing.T) {
	v := newVKEnv(t)
	e := v.env
	key := v.prefix + ":ev:stream"
	v.cmd(t, "XGROUP", "CREATE", key, source.Group, "$", "MKSTREAM")
	const n = 7
	for i := range n {
		ev := `{"t":"2026-10-31T12:00:00Z","type":"request","instance":"wwz-1","request_id":"lag-` + strconv.Itoa(i) + `"}`
		v.cmd(t, "XADD", key, "*", "d", ev)
	}
	metrics := func() string { return string(e.do("GET", "/metrics", "", cred{}).body) }
	e.api.consumer.RefreshLag(ctx)
	if body := metrics(); !strings.Contains(body, "\nwwz_panel_stream_lag "+strconv.Itoa(n)+"\n") {
		t.Fatalf("lag com %d pendentes: %s", n, body)
	}
	got := 0
	for deadline := time.Now().Add(5 * time.Second); got < n && time.Now().Before(deadline); {
		k, err := e.api.consumer.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got += k
	}
	if got != n {
		t.Fatalf("consumidor gravou %d de %d", got, n)
	}
	e.api.consumer.RefreshLag(ctx)
	body := metrics()
	if !strings.Contains(body, "\nwwz_panel_stream_lag 0\n") ||
		!strings.Contains(body, "\nwwz_panel_events_ingested_total "+strconv.Itoa(n)+"\n") {
		t.Fatalf("depois da ingestão: %s", body)
	}
}
