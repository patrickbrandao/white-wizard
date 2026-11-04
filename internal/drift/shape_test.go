package drift

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func paths(s Shape) string {
	parts := make([]string, len(s))
	for i, f := range s {
		parts[i] = f.Path + ":" + f.Type
		if f.Empty {
			parts[i] += "!"
		}
	}
	return strings.Join(parts, " ")
}

func mustShape(t *testing.T, s string) Shape {
	t.Helper()
	sh, err := JSONShape([]byte(s))
	if err != nil {
		t.Fatalf("JSONShape(%s): %v", s, err)
	}
	return sh
}

func TestJSONShape(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"id":"x","n":1,"ok":true,"z":null}`, "$:object id:string n:number ok:bool z:null"},
		{`{}`, "$:object!"},
		{`[]`, "$:array!"},
		{`"s"`, "$:string"},
		{`[{"a":1},{"b":"x","a":2}]`, "$:array []:object [].a:number [].b:string"},
		{`{"c":[{"m":{"r":"a"}}]}`, "$:object c:array c[]:object c[].m:object c[].m.r:string"},
		// null gives way to the first concrete type; empty gives way to full.
		{`[{"t":null},{"t":{"k":1}}]`, "$:array []:object [].t:object [].t.k:number"},
		{`[{"t":[]},{"t":[1]}]`, "$:array []:object [].t:array [].t[]:number"},
		{`[{"t":null},{"t":{}}]`, "$:array []:object [].t:object!"},
		// a conflicting concrete type keeps the first.
		{`[1,"x"]`, "$:array []:number"},
		{`[[1]]`, "$:array []:array [][]:number"},
	}
	for _, c := range cases {
		if got := paths(mustShape(t, c.in)); got != c.want {
			t.Errorf("JSONShape(%s) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := mustShape(t, `{"a":{"b":1}}`).String(); got != "$:object,a:object,a.b:number" {
		t.Errorf("String = %q", got)
	}
}

func TestJSONShapeInvalid(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a"`, `{"a":`, `{"a":1`, `[1,`, `[`, `{"a":1} x`, `{"a":[1}`, `[:`, `[}`, `[{"a"`} {
		if _, err := JSONShape([]byte(in)); err == nil {
			t.Errorf("JSONShape(%q) accepted", in)
		}
	}
}

func TestSSEShape(t *testing.T) {
	body := ": OPENROUTER PROCESSING\r\n\r\n" +
		"id: 1\ndata: {\"a\":1}\n\n" +
		"data: {\"a\":2}\n\n" +
		"data: {\"a\":null}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" +
		"data: plain\ndata: text\n\n" +
		"retry: 10\n\n" +
		": ping\n: ping\n" +
		"data: [DONE]"
	got := SSEShape([]byte(body))
	want := []Event{
		// The leading comment is dropped.
		{Event: "", Data: "$:object,a:number", Count: 2},
		{Event: "", Data: "$:object,a:null", Count: 1},
		{Event: "message_stop", Data: "$:object,type:string", Count: 1},
		{Event: "", Data: "text", Count: 1},
		{Event: EventComment, Count: 2},
		{Event: "", Data: DoneData, Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	for i := range want {
		if got[i].Event != want[i].Event || got[i].Data != want[i].Data || got[i].Count != want[i].Count {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[0].String() != "data=$:object,a:number" || got[2].String() != "event=message_stop data=$:object,type:string" {
		t.Errorf("String: %q %q", got[0].String(), got[2].String())
	}
	if got[0].Key() != "$:object,a:number" || got[2].Key() != "message_stop" || got[4].Key() != EventComment || got[5].Key() != DoneData {
		t.Errorf("Key: %q %q %q %q", got[0].Key(), got[2].Key(), got[4].Key(), got[5].Key())
	}
	if d := sseData([]byte(body)); len(d) != 6 || string(d[0]) != `{"a":1}` || string(d[4]) != "plain\ntext" {
		t.Errorf("sseData = %q", d)
	}
}

func TestHeaderNamesAndContentType(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{
		"X-Ratelimit-Limit-Requests", "Anthropic-Ratelimit-Tokens-Limit", "Retry-After", "Request-Id",
		"X-Request-Id", "Openai-Version", "Anthropic-Organization-Id", "Cf-Ray", "Server", "Date",
		"Set-Cookie", "Via", "Content-Length", "X-Openai-Proxy-Wasm", "X-Openai-Internal-Caller",
		"X-Should-Retry", "X-Openai-Authorization-Error", "X-Openai-Ide-Error-Code", "X-Error-Json",
		"X-Generation-Id", "X-Opencode-Log-Id", "X-Zen-Model", "X-Opencode-Other", "Anthropic-Workspace-Id",
	} {
		h.Set(k, "v")
	}
	h["x-request-id"] = []string{"dup"} // non-canonical duplicate
	want := []string{
		"anthropic-organization-id", "anthropic-ratelimit-tokens-limit", "anthropic-workspace-id", "openai-version", "request-id",
		"retry-after", "x-error-json", "x-generation-id", "x-openai-authorization-error", "x-openai-ide-error-code",
		"x-opencode-log-id", "x-ratelimit-limit-requests", "x-request-id", "x-should-retry", "x-zen-model",
	}
	if got := HeaderNames(h); !reflect.DeepEqual(got, want) {
		t.Errorf("HeaderNames = %v", got)
	}
	for in, want := range map[string]string{
		"Application/JSON; charset=utf-8": "application/json",
		"text/event-stream":               "text/event-stream",
		"Application/Json; charset":       "application/json",
		"":                                "",
	} {
		if got := ContentType(http.Header{"Content-Type": {in}}); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSign(t *testing.T) {
	sse := http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}
	s, err := Sign(200, sse, []byte("data: {\"a\":1}\n\n"))
	if err != nil || !s.Stream || len(s.Events) != 1 || s.Body != nil {
		t.Fatalf("sse: %+v %v", s, err)
	}
	js := http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"r"}}
	s, err = Sign(201, js, []byte(` {"a":1} `))
	if err != nil || s.Stream || s.Status != 201 || len(s.Body) != 2 || !reflect.DeepEqual(s.Headers, []string{"x-request-id"}) {
		t.Fatalf("json: %+v %v", s, err)
	}
	s, err = Sign(204, http.Header{}, []byte("  "))
	if err != nil || s.Body != nil {
		t.Fatalf("empty: %+v %v", s, err)
	}
	if _, err = Sign(502, http.Header{"Content-Type": {"text/html"}}, []byte("<html>")); err == nil ||
		!strings.Contains(err.Error(), "status 502") {
		t.Fatalf("html: %v", err)
	}
}
