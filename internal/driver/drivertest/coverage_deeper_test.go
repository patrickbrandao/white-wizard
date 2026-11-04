package drivertest_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// leaves visits the scalar leaves of v (strings, numbers, bools, raw JSON)
// in a fixed order; visit may change the leaf and returns whether to stop.
func leaves(v reflect.Value, visit func(reflect.Value) bool) bool {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return leaves(v.Elem(), visit)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && leaves(v.Field(i), visit) {
				return true
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return visit(v)
		}
		for i := 0; i < v.Len(); i++ {
			if leaves(v.Index(i), visit) {
				return true
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			tmp := reflect.New(v.Type().Elem()).Elem()
			tmp.Set(v.MapIndex(k))
			stop := leaves(tmp, visit)
			v.SetMapIndex(k, tmp)
			if stop {
				return true
			}
		}
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int32, reflect.Int64, reflect.Float32, reflect.Float64:
		return visit(v)
	}
	return false
}

func perturbLeaf(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Slice:
		v.SetBytes([]byte("0"))
	}
}

// requestLeaf perturbs the n-th leaf of the request.
func requestLeaf(n int) func(*core.Request) {
	return func(r *core.Request) {
		i := 0
		leaves(reflect.ValueOf(r).Elem(), func(v reflect.Value) bool {
			if i == n {
				perturbLeaf(v)
				return true
			}
			i++
			return false
		})
	}
}

// requestLeafCount is the most leaves any request of the run has.
func requestLeafCount(name string) int {
	most := 0
	m := &mutation{name: "count", req: func(r *core.Request) {
		n := 0
		leaves(reflect.ValueOf(r).Elem(), func(reflect.Value) bool { n++; return false })
		if n > most {
			most = n
		}
	}}
	runSuite(wrap(mustNew0(name), m))
	return most
}

// setWhere sets the member key of the objects that also have a non-null
// member when.
func setWhere(key, raw, when string) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		var rec func(n *jnode)
		rec = func(n *jnode) {
			if n.kind == 'o' {
				has := false
				for i, k := range n.keys {
					if k == when && string(n.kids[i].raw) != "null" {
						has = true
					}
				}
				if has {
					for i, k := range n.keys {
						if k == key {
							n.kids[i] = &jnode{kind: 's', raw: []byte(raw)}
							changed = true
						}
					}
				}
			}
			for _, k := range n.kids {
				rec(k)
			}
		}
		rec(root)
		return changed
	}
}

// replaceWith changes every scalar written as raw to repl.
func replaceWith(raw, repl string) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		root.walk(func(_ *jnode, _ int, _ string, kid *jnode) {
			if kid.kind == 's' && string(kid.raw) == raw {
				kid.raw = []byte(repl)
				changed = true
			}
		})
		return changed
	}
}

// thinkingToSignature turns thinking deltas into signature deltas.
func thinkingToSignature(root *jnode) bool {
	changed := false
	var rec func(n *jnode)
	rec = func(n *jnode) {
		if n.kind == 'o' {
			for i, k := range n.keys {
				if k == "type" && string(n.kids[i].raw) == `"thinking_delta"` {
					n.kids[i].raw = []byte(`"signature_delta"`)
					for j, kk := range n.keys {
						if kk == "thinking" {
							n.keys[j] = "signature"
						}
					}
					changed = true
				}
			}
		}
		for _, k := range n.kids {
			rec(k)
		}
	}
	rec(root)
	return changed
}

func deeperMutations(name string, routes []driver.Route) []*mutation {
	out := []*mutation{
		{name: "no routes", routes: func([]driver.Route) []driver.Route { return nil }},
		{name: "auth key always set", auth: func(c driver.Credential) driver.Credential { c.Key = "wwz"; return c }},
		{name: "auth missing always", auth: func(c driver.Credential) driver.Credential { c.Missing = []string{"wwz"}; return c }},
		{name: "sniff always", sniff: func(bool) bool { return true }},
		{name: "sniff never", sniff: func(bool) bool { return false }},
		{name: "policy bogus entity", policy: func(p driver.LimitPolicy) driver.LimitPolicy {
			p.Entities = append(append([]string(nil), p.Entities...), "bogus")
			return p
		}},
		{name: "policy repeated entity", policy: func(p driver.LimitPolicy) driver.LimitPolicy {
			p.Entities = append(append([]string(nil), p.Entities...), p.Entities...)
			return p
		}},
		{name: "forced model namer", forceNamer: true},
		{name: "model name changes", modelName: func(n string) string { return n + "x" }},
		{name: "unprefix changes", unprefix: func(n string, ok bool) (string, bool) { return n + "x", ok }},
		{name: "unprefix always", unprefix: func(n string, _ bool) (string, bool) { return n, true }},
		{name: "unprefix never", unprefix: func(n string, _ bool) (string, bool) { return n, false }},
		{name: "body after GetModel", afterGetModel: "x"},
		{name: "reasoning leaks into content", json: replaceWith(`"Answer."`, `"Let me think.Answer."`)},
		{name: "reasoning leaks into stream content", json: replaceWith(`"Answer."`, `"think.Answer."`)},
		{name: "thinking becomes signature", json: thinkingToSignature},
		{name: "usage chunk without choices", json: setWhere("choices", `[]`, "usage")},
		{name: "usage chunk with a choice", json: setWhere("choices", `[{"index":0,"delta":{},"finish_reason":null}]`, "usage")},
		{name: "usage chunk with null choices", json: setWhere("choices", `null`, "usage")},
		{name: "far reset", header: func(k, v string) (string, bool) {
			if strings.HasSuffix(strings.ToLower(k), "-reset") {
				return "2099-01-01T00:00:00Z", false
			}
			return v, false
		}},
		{name: "empty error code", json: setKey("code", `""`)},
		{name: "numbers become strings", json: numbersToStrings},
		{name: "body after Extra", afterExtra: "x"},
		{name: "broken array body", body: func(p []byte) []byte {
			if len(p) > 0 && p[0] == '[' {
				return append([]byte("[x"), p[1:]...)
			}
			return p
		}},
		{name: "no last frame", failStream: "LastFrame"},
		{name: "wrong generation epoch", id: func(_, v string) string {
			if strings.HasPrefix(v, "gen-") {
				return "gen-1-" + strings.SplitN(v, "-", 3)[2]
			}
			return v
		}},
		{name: "only comment frames", frame: func(int, []byte) [][]byte { return [][]byte{[]byte(": wwz\n\n")} }},
		{name: "event done after every frame", frame: func(_ int, p []byte) [][]byte {
			return [][]byte{p, []byte("event: message_delta\ndata: [DONE]\n\n")}
		}},
	}
	for i := range routes {
		for _, k := range []driver.Kind{driver.KindMessages} {
			out = append(out, &mutation{name: fmt.Sprintf("kind %s on route %d", k, i), routes: func(rs []driver.Route) []driver.Route { rs[i].Kind = k; return rs }})
		}
		out = append(out,
			&mutation{name: fmt.Sprintf("method GET on route %d", i), routes: func(rs []driver.Route) []driver.Route { rs[i].Method = "GET"; return rs }},
			&mutation{name: fmt.Sprintf("duplicate route %d", i), routes: func(rs []driver.Route) []driver.Route { return append(rs, rs[i]) }},
		)
	}
	out = append(out,
		&mutation{name: "every route is relative", routes: func(rs []driver.Route) []driver.Route {
			for i := range rs {
				rs[i].Path = strings.TrimPrefix(rs[i].Path, "/")
			}
			return rs
		}},
		&mutation{name: "every route has a double slash", routes: func(rs []driver.Route) []driver.Route {
			for i := range rs {
				rs[i].Path = "//" + strings.TrimPrefix(rs[i].Path, "/")
			}
			return rs
		}},
		&mutation{name: "every route has an inner wildcard", routes: func(rs []driver.Route) []driver.Route {
			for i := range rs {
				rs[i].Path = "/*" + rs[i].Path
			}
			return rs
		}},
	)
	out = append(out, &mutation{name: "every route suffix changes", routes: func(rs []driver.Route) []driver.Route {
		for i := range rs {
			rs[i].Path += "x"
		}
		return rs
	}})
	for n := 0; n < requestLeafCount(name); n++ {
		out = append(out, &mutation{name: fmt.Sprintf("perturb request leaf %d", n), req: requestLeaf(n)})
	}
	return out
}

// setKey sets every member called key to raw.
func setKey(key, raw string) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		root.walk(func(p *jnode, i int, k string, _ *jnode) {
			if k == key {
				p.kids[i] = &jnode{kind: 's', raw: []byte(raw)}
				changed = true
			}
		})
		return changed
	}
}

// numbersToStrings writes every number as a string.
func numbersToStrings(root *jnode) bool {
	changed := false
	root.walk(func(_ *jnode, _ int, _ string, kid *jnode) {
		if kid.kind == 's' && len(kid.raw) > 0 && (kid.raw[0] == '-' || kid.raw[0] >= '0' && kid.raw[0] <= '9') {
			kid.raw = []byte(`"` + string(kid.raw) + `"`)
			changed = true
		}
	})
	return changed
}
