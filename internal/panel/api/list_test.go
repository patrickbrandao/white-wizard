package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

func TestEntitiesAdmin(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"key/test-01", "key/test-02", "ip/10.0.0.1", "user/u1"} {
		if r := e.do("POST", "/api/v1/entities/"+p+"/block", "", operator); r.code != 200 {
			t.Fatalf("block %s: %d %s", p, r.code, r.body)
		}
	}
	var all []string
	cursor := ""
	for range 10 {
		var page EntityList
		r := e.do("GET", "/api/v1/entities?"+q("status", "suspended", "limit", "1", "cursor", cursor), "", operator)
		r.decode(t, &page)
		if r.code != 200 || len(page.Entities) != 1 || page.Source != SourceAdmin {
			t.Fatalf("página: %d %s", r.code, r.body)
		}
		all = append(all, page.Entities[0].Entity.ID)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(all) != 4 {
		t.Fatalf("paginação: %v", all)
	}
	var page EntityList
	r := e.do("GET", "/api/v1/entities?"+q("kind", "key"), "", operator)
	r.decode(t, &page)
	if len(page.Entities) != 2 || page.NextCursor != "" {
		t.Fatalf("filtro kind: %s", r.body)
	}
	r = e.do("GET", "/api/v1/entities?"+q("cursor", encodeCursor("o:99")), "", operator)
	r.decode(t, &page)
	if r.code != 200 || len(page.Entities) != 0 || !strings.Contains(string(r.body), `"entities":[]`) {
		t.Fatalf("cursor além do fim: %s", r.body)
	}
	for _, bad := range []string{
		q("status", "x"), q("kind", "planet"), q("limit", "1001"), q("cursor", "!!"), q("cursor", encodeCursor("e:1")),
		q("cursor", encodeCursor("o:x")), q("cursor", encodeCursor("o:-1")),
	} {
		if r := e.do("GET", "/api/v1/entities?"+bad, "", operator); r.code != 400 {
			t.Errorf("%s: %d %s", bad, r.code, r.body)
		}
	}

	// Detail without Valkey: the admin state, no live limits.
	var d EntityDetail
	r = e.do("GET", "/api/v1/entities/key/test-01", "", operator)
	r.decode(t, &d)
	if r.code != 200 || d.Source != SourceAdmin || d.State.Status != limits.StatusSuspended || d.Limits == nil || len(d.Limits) != 0 {
		t.Fatalf("detalhe: %d %s", r.code, r.body)
	}
	if r := e.do("GET", "/api/v1/entities/planet/x", "", operator); r.code != 400 {
		t.Fatalf("detalhe kind inválido: %d", r.code)
	}
	if _, err := ParseEntity("key", ""); err == nil {
		t.Fatal("id vazio")
	}

	e = newEnv(t, func(o *envOpts) { o.adminURL = deadURL(t) })
	for _, p := range []string{"/api/v1/entities", "/api/v1/entities/key/a"} {
		if r := e.do("GET", p, "", operator); r.code != 502 || !strings.Contains(string(r.body), "admin: ") {
			t.Errorf("%s admin fora: %d %s", p, r.code, r.body)
		}
	}
	e = newEnv(t, func(o *envOpts) { o.adminURL = failingAdmin(t, http.StatusNotFound) })
	if r := e.do("GET", "/api/v1/entities/key/a", "", operator); r.code != 404 {
		t.Fatalf("404 do admin: %d %s", r.code, r.body)
	}
	e = newEnv(t, func(o *envOpts) { o.valkey = deadValkey(t) })
	for _, p := range []string{"/api/v1/entities", "/api/v1/entities/key/a"} {
		if r := e.do("GET", p, "", operator); r.code != 502 || !strings.Contains(string(r.body), "valkey: ") {
			t.Errorf("%s valkey fora: %d %s", p, r.code, r.body)
		}
	}
}

func TestEvents(t *testing.T) {
	e := newEnv(t)
	base := e.clk.now().Add(-time.Hour)
	var evs []store.Event
	for i := range 5 { // same millisecond
		evs = append(evs, store.Event{
			StreamID: fmt.Sprintf("100-%d", i), At: base, Type: "block", EntityKind: "key", EntityID: "k1", Data: []byte(`{"x":1}`),
		})
	}
	evs = append(evs,
		store.Event{StreamID: "200-0", At: base.Add(time.Second), Type: "request", Driver: "openai", Model: "m1", TestID: "t1", Status: 200, Sample: 0.5},
		store.Event{StreamID: "50-0", At: base.Add(-time.Second), Type: "error", Error: "rate_limit", Status: 429},
	)
	e.insertEvents(evs...)

	var seen []string
	cursor := ""
	for range 20 {
		var page EventPage
		r := e.do("GET", "/api/v1/events?"+q("limit", "1", "cursor", cursor), "", operator)
		r.decode(t, &page)
		if r.code != 200 || len(page.Events) != 1 {
			t.Fatalf("página: %d %s", r.code, r.body)
		}
		seen = append(seen, page.Events[0].StreamID)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(seen, ",") != "200-0,100-4,100-3,100-2,100-1,100-0,50-0" {
		t.Fatalf("ordem/paginação: %v", seen)
	}
	var page EventPage
	r := e.do("GET", "/api/v1/events?"+q("type", "request, error", "driver", "openai"), "", operator)
	r.decode(t, &page)
	if len(page.Events) != 1 || page.Events[0].Sample != 0.5 || page.Events[0].Entity != nil || page.Events[0].TestID != "t1" {
		t.Fatalf("filtro: %s", r.body)
	}
	r = e.do("GET", "/api/v1/events?"+q("entity_kind", "key", "entity_id", "k1", "limit", "2",
		"since", base.Format(time.RFC3339Nano), "until", base.Add(time.Millisecond).Format(time.RFC3339Nano)), "", operator)
	r.decode(t, &page)
	if len(page.Events) != 2 || page.Events[0].Entity.ID != "k1" || page.NextCursor == "" || string(page.Events[0].Data) != `{"x":1}` {
		t.Fatalf("filtro por entidade: %s", r.body)
	}
	// until before the cursor: the narrower bound wins.
	r = e.do("GET", "/api/v1/events?"+q("cursor", page.NextCursor, "until", base.Format(time.RFC3339)), "", operator)
	r.decode(t, &page)
	if len(page.Events) != 1 || page.Events[0].StreamID != "50-0" {
		t.Fatalf("until antes do cursor: %s", r.body)
	}
	for _, bad := range []string{
		q("type", "x"), q("entity_kind", "planet"), q("since", "ontem"), q("until", "amanhã"), q("limit", "0"),
		q("cursor", "!!"), q("cursor", encodeCursor("o:1")), q("cursor", encodeCursor("e:x:1")),
	} {
		if r := e.do("GET", "/api/v1/events?"+bad, "", operator); r.code != 400 {
			t.Errorf("%s: %d %s", bad, r.code, r.body)
		}
	}
	_ = e.st.Close()
	if r := e.do("GET", "/api/v1/events", "", operator); r.code != 500 {
		t.Fatalf("events sem banco: %d", r.code)
	}
}

func TestAudit(t *testing.T) {
	e := newEnv(t)
	at := e.clk.now().Add(-time.Hour)
	for i, a := range []string{"entity.block", "entity.unblock", "chaos.start", "entity.ban"} {
		res := store.ResultOK
		if i == 2 {
			res = store.ResultError
		}
		if _, err := e.st.Audit.Record(ctx, store.AuditEntry{
			At: at.Add(time.Duration(i) * time.Second), User: "op", Role: "operator", Action: a, Target: "key/k" + fmt.Sprint(i), Result: res,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var page AuditPage
	r := e.do("GET", "/api/v1/audit?"+q("action", "entity.", "limit", "2"), "", operator)
	r.decode(t, &page)
	if r.code != 200 || len(page.Entries) != 2 || page.Entries[0].Action != "entity.ban" || page.NextCursor == "" {
		t.Fatalf("audit: %d %s", r.code, r.body)
	}
	r = e.do("GET", "/api/v1/audit?"+q("action", "entity.", "limit", "2", "cursor", page.NextCursor), "", operator)
	page = AuditPage{}
	r.decode(t, &page)
	if len(page.Entries) != 1 || page.Entries[0].Action != "entity.block" || page.NextCursor != "" {
		t.Fatalf("audit página 2: %s", r.body)
	}
	r = e.do("GET", "/api/v1/audit?"+q("result", "error", "user", "op", "target", "key/k2",
		"since", at.Format(time.RFC3339), "until", at.Add(time.Hour).Format(time.RFC3339)), "", operator)
	r.decode(t, &page)
	if len(page.Entries) != 1 || page.Entries[0].Action != "chaos.start" {
		t.Fatalf("audit filtros: %s", r.body)
	}
	r = e.do("GET", "/api/v1/audit?"+q("user", "ninguem"), "", operator)
	if !strings.Contains(string(r.body), `"entries":[]`) {
		t.Fatalf("audit vazia: %s", r.body)
	}
	// CSV: every row without limit; limit respected when given.
	r = e.do("GET", "/api/v1/audit?format=csv", "", operator)
	recs, err := csv.NewReader(strings.NewReader(string(r.body))).ReadAll()
	if err != nil || len(recs) != 5 || recs[0][0] != "id" || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/csv") {
		t.Fatalf("csv: %v %s", err, r.body)
	}
	r = e.do("GET", "/api/v1/audit?"+q("format", "csv", "limit", "1"), "", operator)
	if recs, _ := csv.NewReader(strings.NewReader(string(r.body))).ReadAll(); len(recs) != 2 {
		t.Fatalf("csv com limit: %s", r.body)
	}
	for _, bad := range []string{
		q("format", "xml"), q("limit", "x"), q("result", "talvez"), q("since", "x"), q("until", "x"), q("cursor", "!!"),
		q("format", "csv", "result", "talvez"),
	} {
		if r := e.do("GET", "/api/v1/audit?"+bad, "", operator); r.code != 400 {
			t.Errorf("%s: %d %s", bad, r.code, r.body)
		}
	}
	_ = e.st.Close()
	if r := e.do("GET", "/api/v1/audit", "", operator); r.code != 500 {
		t.Fatalf("audit sem banco: %d", r.code)
	}
	if r := e.do("GET", "/api/v1/audit?format=csv", "", operator); r.code != 200 || !strings.Contains(e.logs.String(), "exportar auditoria") {
		t.Fatalf("csv sem banco: %d", r.code)
	}
}

// TestEntityKeyAccountAdmin checks that, without Valkey, a key's detail
// still names its account from /admin/config (ADR 0051), and stays empty
// when the configuration is unavailable, unreadable or lacks the key.
func TestEntityKeyAccountAdmin(t *testing.T) {
	e := newEnv(t)
	if d := detail(t, e, "key/test-01"); d.AccountID != "acme" || d.Source != SourceAdmin {
		t.Fatalf("conta da chave: %+v", d)
	}
	if d := detail(t, e, "key/ninguem"); d.AccountID != "" {
		t.Fatalf("chave fora da configuração: %+v", d)
	}
	ent := `{"entity":{"kind":"key","id":"k"},"status":"active"}`
	for name, cfg := range map[string]map[string]string{
		"sem config":      {"/admin/entities/key/k": ent},
		"config inválida": {"/admin/entities/key/k": ent, "/admin/config": `{"keys": 5}`},
	} {
		f := newEnv(t, func(o *envOpts) { o.adminURL = fakeAdmin(t, cfg) })
		if d := detail(t, f, "key/k"); d.AccountID != "" {
			t.Fatalf("%s: %+v", name, d)
		}
	}
}
