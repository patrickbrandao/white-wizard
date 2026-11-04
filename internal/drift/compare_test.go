package drift

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func kinds(ds []Divergence) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.Kind + " " + d.Path + " " + d.Real + "|" + d.WWZ
	}
	return strings.Join(parts, "; ")
}

func TestCompareShapes(t *testing.T) {
	cases := []struct{ real, wwz, want string }{
		{`{"a":1,"b":"x"}`, `{"a":2,"b":"y"}`, ""},
		{`{"a":1,"b":null}`, `{"a":null,"b":"y"}`, ""},
		{`{"a":1,"b":"x"}`, `{"a":1}`, "missing_field b string|"},
		{`{"a":1}`, `{"a":1,"c":true}`, "extra_field c |bool"},
		{`{"a":1}`, `{"a":"1"}`, "type_mismatch a number|string"},
		{`{"a":1,"b":2,"c":3}`, `{"b":2,"a":1,"c":3}`, "order_mismatch $ a,b,c|b,a,c"},
		{`{"m":{"x":1,"y":2}}`, `{"m":{"y":2,"x":1}}`, "order_mismatch m x,y|y,x"},
		// order only of common siblings.
		{`{"a":1,"z":0,"b":2}`, `{"a":1,"b":2}`, "missing_field z number|"},
		// descendants of a missing, null, empty or retyped ancestor are not repeated.
		{`{"m":{"x":{"y":1}}}`, `{}`, "missing_field m object|"},
		{`{"m":{"x":1}}`, `{"m":null}`, ""},
		{`{"m":[{"x":1}]}`, `{"m":[]}`, ""},
		{`{"m":{"x":1}}`, `{"m":"s"}`, "type_mismatch m object|string"},
		{`{"m":null}`, `{"m":{"x":1}}`, ""},
		{`{"c":[{"d":{"e":1}}]}`, `{"c":[{"d":{"e":1,"f":2}}]}`, "extra_field c[].d.f |number"},
	}
	for _, c := range cases {
		got := kinds(CompareShapes(mustShape(t, c.real), mustShape(t, c.wwz)))
		if got != c.want {
			t.Errorf("CompareShapes(%s, %s) = %q, want %q", c.real, c.wwz, got, c.want)
		}
	}
	if !Compatible(TypeNull, TypeArray) || !Compatible(TypeBool, TypeNull) || Compatible(TypeBool, TypeString) {
		t.Error("Compatible")
	}
}

func sig(t *testing.T, status int, ct string, headers []string, body string) Signature {
	t.Helper()
	h := http.Header{"Content-Type": {ct}}
	for _, k := range headers {
		h.Set(k, "v")
	}
	s, err := Sign(status, h, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompare(t *testing.T) {
	js := "application/json"
	sse := "text/event-stream"
	got := Compare(sig(t, 200, js, nil, `{}`), sig(t, 401, js, nil, `{"x":1}`))
	if kinds(got) != "status_mismatch status 200|401" {
		t.Errorf("status: %s", kinds(got))
	}
	got = Compare(sig(t, 200, js, []string{"openai-version", "x-request-id"}, `{"a":1}`),
		sig(t, 200, "application/x-ndjson", []string{"x-request-id", "request-id"}, `{"a":1}`))
	want := "type_mismatch content-type application/json|application/x-ndjson; missing_header openai-version present|; extra_header request-id |present"
	if kinds(got) != want {
		t.Errorf("headers: %s", kinds(got))
	}
	got = Compare(sig(t, 200, sse, nil, "data: {}\n\n"), sig(t, 200, js, nil, `{}`))
	if kinds(got) != "type_mismatch content-type text/event-stream|application/json; event_sequence events sse|json" {
		t.Errorf("stream vs json: %s", kinds(got))
	}
	ref := "event: a\ndata: {\"x\":1,\"y\":null}\n\n: c\n\ndata: [DONE]\n\n"
	got = Compare(sig(t, 200, sse, nil, ref), sig(t, 200, sse, nil, "event: a\ndata: {\"x\":null,\"y\":\"s\"}\n\n: c\ndata: [DONE]\n\n"))
	if len(got) != 0 {
		t.Errorf("equal streams: %s", kinds(got))
	}
	for _, c := range []struct{ ref, wwz, want string }{
		{ref, "event: b\ndata: {\"x\":1}\n\n", "event_sequence events[0] event=a data=$:object,x:number,y:null|; " +
			"event_sequence events[1] event=comment data=|; event_sequence events[2] data=[DONE]|; event_sequence events[0] |event=b data=$:object,x:number"},
		// Every difference is listed, not only the first.
		{ref, "event: a\ndata: {\"x\":\"s\",\"y\":null}\n\n", "type_mismatch events[0:a].x number|string; " +
			"event_sequence events[1] event=comment data=|; event_sequence events[2] data=[DONE]|"},
		{ref, "event: a\ndata: {\"x\":1,\"y\":null}\n\n: c\n\n", "event_sequence events[2] data=[DONE]|"},
		{ref, "event: a\ndata: {\"x\":1,\"y\":null}\n\n: c\n\ndata: [DONE]\n\ndata: more\n\n", "event_sequence events[3] |data=text"},
		{ref, "event: a\ndata: {\"x\":1,\"y\":null}\n\n: c\n\ndata: done\n\n", "event_sequence events[2] data=[DONE]|; event_sequence events[2] |data=text"},
		// Comments before the first event are ignored on both sides.
		{": PROCESSING\n\n" + ref, ": x\n: y\n\n" + ref, ""},
		// Alignment by key: the extra event is found anywhere.
		{"event: a\ndata: {}\n\nevent: b\ndata: {}\n\n", "event: b\ndata: {}\n\nevent: a\ndata: {}\n\nevent: b\ndata: {}\n\n", "event_sequence events[0] |event=b data=$:object"},
		// Without "event:", the key is the data type, else its object.
		{"data: {\"type\":\"t\",\"a\":1}\n\n", "data: {\"a\":1,\"type\":\"t\"}\n\n", "order_mismatch events[0:t] type,a|a,type"},
		{"data: {\"object\":\"chunk\",\"a\":1}\n\n", "data: {\"object\":\"chunk\"}\n\n", "missing_field events[0:chunk].a number|"},
		{"event: message_start\ndata: {\"type\":\"message_start\"}\n\n", "data: {\"type\":\"message_start\"}\n\n", "event_sequence events[0:message_start] event=message_start|event="},
		{"event: x\ndata: [DONE]\n\n", "event: x\ndata: plain\n\n", "type_mismatch events[0:x] [DONE]|text"},
		{"data: [1]\n\n", "data: [1]\n\n", ""},
	} {
		got = Compare(sig(t, 200, sse, nil, c.ref), sig(t, 200, sse, nil, c.wwz))
		if kinds(got) != c.want {
			t.Errorf("stream %q × %q:\n got %s\nwant %s", c.ref, c.wwz, kinds(got), c.want)
		}
	}
}

func TestSignModels(t *testing.T) {
	js := http.Header{"Content-Type": {"application/json"}}
	list := `{"object":"list","data":[{"id":"a","x":1},{"id":"m","y":"s"},{"id":"m","z":true}],"has_more":false}`
	s, err := SignModels(200, js, []byte(list), "m")
	if err != nil || s.Item != "m" || !s.ItemFound || paths(s.Body) != "$:object object:string data:array data[]:object data[].id:string data[].y:string has_more:bool" {
		t.Fatalf("list: %s %+v %v", paths(s.Body), s, err)
	}
	none, err := SignModels(200, js, []byte(`{"data":[{"id":"a","x":1}],"n":1}`), "m")
	if err != nil || none.ItemFound || paths(none.Body) != "$:object data:array! n:number" {
		t.Fatalf("without the item: %s %+v %v", paths(none.Body), none, err)
	}
	if got := kinds(Compare(s, none)); got != "missing_field data[id=m] object|; missing_field object string|; missing_field has_more bool|; extra_field n |number" {
		t.Errorf("item only in real: %s", got)
	}
	if got := kinds(Compare(none, s)); !strings.HasPrefix(got, "extra_field data[id=m] |object") {
		t.Errorf("item only in wwz: %s", got)
	}
	// Not a model list: plain Sign.
	for _, body := range []string{`{"error":{"message":"x"}}`, `[{"id":"m"}]`, `{"data":null}`, `{"data":{"id":"m"}}`} {
		s, err := SignModels(401, js, []byte(body), "m")
		if err != nil || s.Item != "" || len(s.Body) == 0 {
			t.Errorf("%s: %+v %v", body, s, err)
		}
	}
	if _, err := SignModels(200, js, []byte(`{"data":[`), "m"); err == nil {
		t.Error("truncated list accepted")
	}
}

func TestIgnore(t *testing.T) {
	d := Divergence{Kind: KindMissingField, Path: "choices[].message.refusal"}
	for _, c := range []struct {
		ig   Ignore
		want bool
	}{
		{Ignore{}, true},
		{Ignore{Provider: "*", Probe: "*", Path: "*"}, true},
		{Ignore{Provider: "openai", Probe: "chat", Kind: KindMissingField, Path: "choices[].message.refusal"}, true},
		{Ignore{Path: "choices[]*"}, true},
		{Ignore{Path: "usage*"}, false},
		{Ignore{Provider: "anthropic"}, false},
		{Ignore{Probe: "models"}, false},
		{Ignore{Kind: KindExtraField}, false},
	} {
		if got := c.ig.Matches("openai", "chat", d); got != c.want {
			t.Errorf("%+v.Matches = %v", c.ig, got)
		}
	}
	list := []Ignore{{Provider: "openai", Kind: KindMissingHeader, Path: "openai-organization", Reason: "r"}}
	ds := []Divergence{{Kind: KindMissingHeader, Path: "openai-organization"}, {Kind: KindExtraField, Path: "x"}}
	if !markIgnored(list, "openai", "chat", ds) || !ds[0].Ignored || ds[1].Ignored {
		t.Errorf("markIgnored: %+v", ds)
	}
	if markIgnored(list, "openai", "chat", ds[:1]) {
		t.Error("only ignored divergences must not count")
	}
	// The account headers are compared: wwz sends them (ADR 0049).
	if len(DefaultIgnores()) != 0 {
		t.Errorf("default ignores: %+v", DefaultIgnores())
	}
}

func TestParseIgnores(t *testing.T) {
	list, err := ParseIgnores([]byte("- {provider: openai, probe: chat, kind: missing_field, path: service_tier, reason: campo de conta}\n- {path: 'usage*', reason: tudo}\n"))
	if err != nil || len(list) != 2 || list[0].Path != "service_tier" || list[1].Reason != "tudo" {
		t.Fatalf("ParseIgnores = %+v, %v", list, err)
	}
	for in, want := range map[string]string{
		"- {path: a}":                      "reason é obrigatório",
		"- {kind: nope, reason: r}":        "kind \"nope\" desconhecido",
		"- {provider: bedrock, reason: r}": "provider \"bedrock\" desconhecido",
		"- {probe: nope, reason: r}":       "probe \"nope\" desconhecida",
		"- {reason: r, unknown: 1}":        "unknown",
		"{provider: openai}":               "drift: ignore:",
		"- {provider: '*', reason: '   '}": "reason é obrigatório",
	} {
		if _, err := ParseIgnores([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseIgnores(%q) = %v, want %q", in, err, want)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ignore.yaml")
	if err := os.WriteFile(path, []byte("- {kind: extra_header, path: x-foo, reason: r}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, err := LoadIgnores(path); err != nil || !reflect.DeepEqual(list, []Ignore{{Kind: KindExtraHeader, Path: "x-foo", Reason: "r"}}) {
		t.Errorf("LoadIgnores = %+v, %v", list, err)
	}
	if _, err := LoadIgnores(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("LoadIgnores of a missing file")
	}
}
