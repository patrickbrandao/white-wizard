package opencode_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// Task 425 (ADR 0072, items 15 and 16; spec 006 16.7 "OpenCode Go"): the
// public catalogs recorded without a key (testdata/real/opencode/catalog/,
// task 416) against one configuration built from both, as an operator
// writes it: one models[] entry per id, catalogs.opencode and
// catalogs.opencode-go with each base's price, protocol and canonical_id,
// served through the engine. Each base lists the real set of ids (79 and
// 28; /zen as /inference) with the real cost, the one native protocol of
// api and the real canonical_id. base_url is left out: its origin is the
// subject of ADR 0073 (task 436).

// catalogBases are the four listings and the fixture of each.
var catalogBases = []struct {
	fixture, catalog string
	n                int
}{
	{"payg_models", "opencode", 79},
	{"zen_models", "opencode", 79},
	{"go_models", "opencode-go", 28},
	{"zen_go_models", "opencode-go", 28},
}

// pricingBlock is the models[].pricing YAML block of a real cost: prices,
// multipliers and overrides (the input and output override with the same
// when are one entry).
func pricingBlock(t *testing.T, id string, c realCost) map[string]any {
	t.Helper()
	in := c.Text.Input
	block := map[string]any{
		"input_per_m": in.Price, "output_per_m": c.Text.Output.Price,
		"cache_read_per_m": in.Cache["read"], "cache_write_per_m": in.Cache["write"],
	}
	var ms []any
	for _, m := range c.Multipliers {
		ms = append(ms, map[string]any{"when": m.When, "factor": m.Factor})
	}
	if ms != nil {
		block["multipliers"] = ms
	}
	if len(in.Over) != len(c.Text.Output.Over) {
		t.Fatalf("%s: %d input overrides, %d output ones", id, len(in.Over), len(c.Text.Output.Over))
	}
	var os []any
	for i, o := range in.Over {
		e := map[string]any{"when": o.When, "output_per_m": c.Text.Output.Over[i].Price}
		if o.Price != nil {
			e["input_per_m"] = *o.Price
		}
		for k, v := range o.Cache {
			e["cache_"+k+"_per_m"] = v
		}
		os = append(os, e)
	}
	if os != nil {
		block["overrides"] = os
	}
	return block
}

// catalogEntry is catalogs.<catalog> of a real item: its price; its
// protocol in extra.api when it is not the one protocols derives (always
// for responses); its canonical_id (null when the real lists none); and,
// in extra.cost, what the pricing block has no field for.
func catalogEntry(t *testing.T, item map[string]json.RawMessage, derived string) (entry map[string]any, proto string) {
	t.Helper()
	var id string
	_ = json.Unmarshal(item["id"], &id)
	var cost realCost
	if err := json.Unmarshal(item["cost"], &cost); err != nil {
		t.Fatalf("%s: cost: %v", id, err)
	}
	var api map[string]json.RawMessage
	if err := json.Unmarshal(item["api"], &api); err != nil || len(api) != 1 {
		t.Fatalf("%s: api %s (%v): the real lists exactly one protocol", id, item["api"], err)
	}
	for p := range api {
		proto = p
	}
	extra := map[string]any{"canonical_id": nil}
	if raw, ok := item["canonical_id"]; ok {
		var cid string
		_ = json.Unmarshal(raw, &cid)
		extra["canonical_id"] = cid
	}
	if proto != derived || proto == "responses" {
		extra["api"] = map[string]any{proto: map[string]any{}}
	}
	text := map[string]any{}
	if w, ok := cost.Text.Input.Cache["write"]; ok && w == 0 {
		text["input"] = map[string]any{"cache": map[string]any{"write": 0}}
	}
	ec := map[string]any{}
	if len(text) > 0 {
		ec["text"] = text
	}
	if cost.Audio != nil {
		ec["audio"] = cost.Audio
	}
	if len(ec) > 0 {
		extra["cost"] = ec
	}
	return map[string]any{"pricing": pricingBlock(t, id, cost), "extra": extra}, proto
}

// realCatalogConfig is the configuration of both catalogs: models[] in the
// order of the pay-as-you-go listing, then the ids only Go lists. The
// model's own pricing is a decoy: each catalog has its own.
func realCatalogConfig(t *testing.T) string {
	t.Helper()
	_, payg := catalogItems(t, "payg_models")
	_, gol := catalogItems(t, "go_models")
	type entry struct {
		id       string
		dialect  string
		derived  string
		catalogs map[string]any
	}
	var order []*entry
	byID := map[string]*entry{}
	for _, c := range []struct {
		name  string
		items []map[string]json.RawMessage
	}{{"opencode", payg}, {"opencode-go", gol}} {
		for _, item := range c.items {
			var id string
			_ = json.Unmarshal(item["id"], &id)
			e := byID[id]
			if e == nil {
				// The protocol of the first catalog that lists the
				// model gives protocols; responses is OpenAI's dialect,
				// whose derived protocol is chat-completions.
				var api map[string]json.RawMessage
				_ = json.Unmarshal(item["api"], &api)
				for p := range api {
					e = &entry{id: id, dialect: dialectOf[p], derived: p, catalogs: map[string]any{}}
				}
				if e.derived == "responses" {
					e.derived = "chat-completions"
				}
				byID[id] = e
				order = append(order, e)
			}
			ce, _ := catalogEntry(t, item, e.derived)
			e.catalogs[c.name] = ce
		}
	}
	var models []any
	for _, e := range order {
		models = append(models, map[string]any{
			"id": e.id, "protocols": []string{e.dialect}, "drivers": []string{"opencode"},
			"pricing":  map[string]any{"input_per_m": 99, "output_per_m": 99},
			"catalogs": e.catalogs,
		})
	}
	cfg := map[string]any{"server": map[string]any{"auth": "keys"}, "models": models}
	return mustJSON(t, cfg)
}

// TestRealFixturesCatalogs: through the engine, each base lists exactly
// the real ids, and for each the real cost, protocol and canonical_id.
func TestRealFixturesCatalogs(t *testing.T) {
	b := newBillEngine(t, realCatalogConfig(t), nil)
	shared := 0
	_, payg := catalogItems(t, "payg_models")
	_, gol := catalogItems(t, "go_models")
	inPayg := map[string]bool{}
	for _, it := range payg {
		var id string
		_ = json.Unmarshal(it["id"], &id)
		inPayg[id] = true
	}
	for _, it := range gol {
		var id string
		_ = json.Unmarshal(it["id"], &id)
		if inPayg[id] {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("no id in both catalogs: the test would not show one models[] entry per id")
	}
	for _, tc := range catalogBases {
		t.Run(tc.fixture, func(t *testing.T) {
			path, real := catalogItems(t, tc.fixture)
			if len(real) != tc.n {
				t.Fatalf("%s lists %d models, want %d", tc.fixture, len(real), tc.n)
			}
			w := b.do(t, "GET", path, nil, "")
			if w.StatusCode() != 200 {
				t.Fatalf("%s: %d %s", path, w.StatusCode(), w.Body())
			}
			var list struct {
				Data []map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(w.Body(), &list); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			got := map[string]map[string]json.RawMessage{}
			for _, it := range list.Data {
				var id string
				_ = json.Unmarshal(it["id"], &id)
				got[id] = it
			}
			want := map[string]map[string]json.RawMessage{}
			for _, it := range real {
				var id string
				_ = json.Unmarshal(it["id"], &id)
				want[id] = it
			}
			if g, r := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(g, r) || len(list.Data) != tc.n {
				t.Fatalf("%s: %d items %v\nreal %d %v", path, len(list.Data), g, len(r), r)
			}
			for id, r := range want {
				g := got[id]
				if a, b := compact(t, g["cost"]), compact(t, r["cost"]); a != b {
					t.Errorf("%s %s: cost\n wwz  %s\n real %s", path, id, a, b)
				}
				var ga, ra map[string]json.RawMessage
				_ = json.Unmarshal(g["api"], &ga)
				_ = json.Unmarshal(r["api"], &ra)
				if a, b := slices.Sorted(maps.Keys(ga)), slices.Sorted(maps.Keys(ra)); !slices.Equal(a, b) {
					t.Errorf("%s %s: api protocols %v, real %v", path, id, a, b)
				}
				if a, b := compact(t, g["canonical_id"]), compact(t, r["canonical_id"]); a != b {
					t.Errorf("%s %s: canonical_id %q, real %q", path, id, a, b)
				}
			}
		})
	}
}
