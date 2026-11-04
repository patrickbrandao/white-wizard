package oai_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// TestObjOrder: keys keep insertion order; Set replaces in place, Delete
// removes, Merge appends sorted, Reorder moves the listed keys first.
func TestObjOrder(t *testing.T) {
	o := oai.O("z", 1, "a", "x", "m", nil)
	o.Set("a", "y")
	o.Set("b", true)
	o.Delete("m")
	o.Delete("absent")
	o.Merge(map[string]any{"d": int64(4), "c": []any{1, "s"}, "z": 2})
	o.Merge(nil)
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":2,"a":"y","b":true,"c":[1,"s"],"d":4}`; string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
	if v, ok := o.Get("b"); !ok || v != true {
		t.Fatalf("Get(b) = %v %v", v, ok)
	}
	if _, ok := o.Get("m"); ok {
		t.Fatal("deleted key still there")
	}
	r := o.Reorder("d", "absent", "a")
	if b, _ = json.Marshal(r); string(b) != `{"d":4,"a":"y","z":2,"b":true,"c":[1,"s"]}` {
		t.Fatalf("Reorder: %s", b)
	}
	nested := oai.O("list", []oai.Obj{oai.O("k", 1), oai.O("k", 2)}, "map", map[string]any{"y": 1, "x": 2}, "f", 0.5)
	if b, _ = json.Marshal(nested); string(b) != `{"list":[{"k":1},{"k":2}],"map":{"x":2,"y":1},"f":0.5}` {
		t.Fatalf("nested: %s", b)
	}
	for _, bad := range []any{oai.O("c", make(chan int)), oai.O("o", oai.O("c", make(chan int))), oai.O("l", []any{make(chan int)}), oai.O("l", []oai.Obj{oai.O("c", make(chan int))})} {
		if _, err := json.Marshal(bad); err == nil {
			t.Fatalf("%v: want an error", bad)
		}
	}
}

// TestObjStringsLikeEncodingJSON: strings are quoted byte for byte as
// encoding/json quotes them.
func TestObjStringsLikeEncodingJSON(t *testing.T) {
	for _, s := range []string{
		"", "plain", `quote " and \ back`, "<tag> & amp", "\n\r\t\b\f", "\x00\x01\x1f\x7f",
		"só ação 日本 😀", "  and  ", "bad \xff utf8 \xc3", strings.Repeat("ab", 100),
	} {
		want, _ := json.Marshal(map[string]string{"k": s})
		got, err := json.Marshal(oai.O("k", s))
		if err != nil || string(got) != string(want) {
			t.Errorf("%q: got %s, want %s", s, got, want)
		}
	}
}

// TestFormat: indentation and the final newline.
func TestFormat(t *testing.T) {
	o := oai.O("a", oai.O("b", 1), "c", []any{})
	for _, c := range []struct {
		f    oai.Format
		want string
	}{
		{oai.Format{}, `{"a":{"b":1},"c":[]}`},
		{oai.Format{Newline: true}, "{\"a\":{\"b\":1},\"c\":[]}\n"},
		{oai.Format{Indent: "  "}, "{\n  \"a\": {\n    \"b\": 1\n  },\n  \"c\": []\n}"},
		{oai.Format{Indent: "    ", Newline: true}, "{\n    \"a\": {\n        \"b\": 1\n    },\n    \"c\": []\n}\n"},
	} {
		b, err := c.f.Marshal(o)
		if err != nil || string(b) != c.want {
			t.Errorf("%+v: %q, want %q", c.f, b, c.want)
		}
	}
	if _, err := (oai.Format{Indent: " "}).Marshal(make(chan int)); err == nil {
		t.Fatal("unencodable value: want an error")
	}
}

// TestShapeHook: a gateway reorders and drops fields of every part.
func TestShapeHook(t *testing.T) {
	seen := map[oai.Part]bool{}
	h := oai.Hooks{Shape: func(p oai.Part, o oai.Obj) oai.Obj {
		seen[p] = true
		switch p {
		case oai.PartChoice, oai.PartChunkChoice:
			return o.Reorder("finish_reason")
		case oai.PartMessage:
			o.Delete("refusal")
		}
		return o
	}}
	r := newRec()
	if err := oai.WriteChat(&h, r, chatReq(), resp(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.body.String(), `"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hi"},`) {
		t.Fatalf("shaped body: %s", r.body.String())
	}
	w := newRec()
	req := chatReq()
	req.IncludeUsage = true
	s := newStream(h, w, req)
	must(t, s.Start(resp()))
	must(t, s.Finish(core.FinishStop, core.Usage{}, true))
	for _, p := range []oai.Part{oai.PartChat, oai.PartChoice, oai.PartMessage, oai.PartUsage, oai.PartChunk, oai.PartChunkChoice, oai.PartDelta} {
		if !seen[p] {
			t.Errorf("Shape never saw %s", p)
		}
	}
}
