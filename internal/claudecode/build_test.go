package claudecode

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func baseRequest() Request {
	return Request{
		Version: "2.1.308", Entrypoint: EntrypointSDKCLI, Type: TypeTurn,
		Credential: Credential{Kind: CredentialOAuth, Secret: "sk-ant-oat01-WWZFAKE"},
		Host:       "127.0.0.1:8000", SessionID: "ce90e450-43fc-4765-8110-6aed6d650ca1",
		DeviceID: strings.Repeat("0f", 32), Model: "claude-sonnet-5-5",
		Messages: []Value{Object(M("role", String("user")), M("content", String("say ok")))},
	}
}

func TestBuildErrors(t *testing.T) {
	cases := map[string]func(*Request){
		"unknown version":    func(r *Request) { r.Version = "2.1.309" },
		"unknown entrypoint": func(r *Request) { r.Entrypoint = "sdk-py" },
		"no credential":      func(r *Request) { r.Credential = Credential{} },
		"quota from sdk-cli": func(r *Request) { r.Type = TypeQuota },
		"unknown type":       func(r *Request) { r.Type = "batch" },
	}
	for name, mutate := range cases {
		r := baseRequest()
		mutate(&r)
		if _, _, err := Build(r); err == nil {
			t.Errorf("%s: Build accepted", name)
		}
	}
	r := baseRequest()
	r.Version = "9.9.9"
	if _, _, err := Build(r); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown version: %v", err)
	}
}

func TestBuildTurn(t *testing.T) {
	r := baseRequest()
	r.Prefix = "/anthropic"
	head, body, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(head), "POST /anthropic/v1/messages?beta=true HTTP/1.1\r\nAccept: application/json\r\n") ||
		!strings.Contains(string(head), "\r\nX-Stainless-Arch: arm64\r\n") ||
		!strings.Contains(string(head), "\r\nX-Stainless-OS: MacOS\r\n") ||
		!strings.HasSuffix(string(head), "\r\nContent-Length: "+itoa(len(body))+"\r\n\r\n") {
		t.Fatalf("head:\n%s", head)
	}
	if !strings.Contains(string(body), `"cc_version=2.1.308.7f2; cc_entrypoint=sdk-cli;"`) &&
		!strings.Contains(string(body), "cc_version=2.1.308.7f2; cc_entrypoint=sdk-cli;") {
		t.Fatalf("billing header missing: %s", body)
	}
	if !strings.Contains(string(body), `"max_tokens":128000`) || !strings.HasSuffix(string(body), `"stream":true}`) ||
		!strings.Contains(string(body), `"effort":"medium"`) {
		t.Fatalf("body: %s", body)
	}

	// Non-streaming fallback.
	r.NonStream = true
	head, body, _ = Build(r)
	if !strings.Contains(string(head), "X-Stainless-Timeout: 300\r\n") ||
		!strings.Contains(string(body), `"max_tokens":64000`) || !strings.HasSuffix(string(body), `"stream":false}`) {
		t.Fatalf("fallback:\n%s%s", head, body)
	}

	// Tool search adds advanced-tool-use before effort.
	r = baseRequest()
	r.ToolSearch = true
	head, _, _ = Build(r)
	if !strings.Contains(string(head), "mid-conversation-tool-changes-2026-07-01,advanced-tool-use-2025-11-20,effort-2025-11-24,") {
		t.Fatalf("tool search:\n%s", head)
	}
}

func itoa(n int) string {
	return string(AppendNumber(nil, float64(n)))
}

func TestBuildHello(t *testing.T) {
	r := Request{Version: "2.1.308", Type: TypeHello, Host: "wwz:8000", Prefix: "/a"}
	head, body, err := Build(r)
	if err != nil || body != nil {
		t.Fatal(err)
	}
	want := "HEAD /a/api/hello HTTP/1.1\r\nConnection: keep-alive\r\nUser-Agent: Bun/1.4.4\r\nAccept: */*\r\n" +
		"Host: wwz:8000\r\nAccept-Encoding: gzip, deflate, br, zstd\r\n\r\n"
	if string(head) != want {
		t.Fatalf("hello = %q", head)
	}
}

func TestBuildTitle(t *testing.T) {
	// The title request (no fixture: its prompt is Claude Code's), by the
	// key order and betas of capture 011.
	r := baseRequest()
	r.Type, r.Entrypoint, r.SystemPrompt = TypeTitle, EntrypointCLI, "Name the session."
	r.OutputFormat = Object(M("type", String("json_schema")))
	head, body, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := Parse(body)
	if got := strings.Join(v.Keys(), ","); got != "model,messages,system,tools,metadata,max_tokens,output_config,stream" {
		t.Fatalf("keys %s", got)
	}
	sys, _ := v.Get("system")
	for i, b := range sys.Elems() {
		if _, ok := b.Get("cache_control"); ok {
			t.Errorf("system[%d] has cache_control", i)
		}
	}
	if !strings.Contains(string(body), `"output_config":{"effort":"medium","format":{"type":"json_schema"}}`) {
		t.Fatalf("output_config: %s", body)
	}
	if !strings.Contains(string(head), "effort-2025-11-24,structured-outputs-2025-12-15\r\n") {
		t.Fatalf("betas:\n%s", head)
	}
	r.OutputFormat = Null()
	_, body, _ = Build(r)
	if !strings.Contains(string(body), `"output_config":{"effort":"medium"},`) {
		t.Fatalf("title without format: %s", body)
	}
}

func TestBuildCountTokens(t *testing.T) {
	r := baseRequest()
	r.Type, r.Credential = TypeCountTokens, Credential{Kind: CredentialAPIKey, Secret: "k"}
	head, body, err := Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(head), HeaderStainlessTimeout) || !strings.HasPrefix(string(head), "POST /v1/messages/count_tokens?beta=true ") ||
		!strings.Contains(string(head), "\r\nx-api-key: k\r\n") || strings.Contains(string(head), HeaderAuthorization) {
		t.Fatalf("head:\n%s", head)
	}
	if string(body) != `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"say ok"}],"tools":[]}` {
		t.Fatalf("body %s", body)
	}
	r.Messages = append(r.Messages,
		Object(M("role", String("user")), M("content", String("no thinking here"))),
		Object(M("role", String("assistant")), M("content", String("plain"))),
		Object(M("role", String("assistant")), M("content", Array(Object(M("type", String("redacted_thinking")))))))
	if _, body, _ = Build(r); !strings.HasSuffix(string(body), `"tools":[],"thinking":{"type":"adaptive"}}`) {
		t.Fatalf("with thinking: %s", body)
	}
}

func TestExtractErrors(t *testing.T) {
	fx := loadFixture(t, "002-sdk-cli-oauth-turn.raw")
	hello := loadFixture(t, "001-api-hello-head.raw")
	cases := map[string]func() req{
		"probe of another Bun": func() req { return fromFixture(hello).set(HeaderUserAgent, "Bun/0.0.1") },
		"other path": func() req {
			r := fromFixture(fx)
			r.head.Target = "/v1/complete"
			return r
		},
		"bad User-Agent":  func() req { return fromFixture(fx).set(HeaderUserAgent, "curl/8") },
		"unknown version": func() req { return fromFixture(fx).set(HeaderUserAgent, "claude-cli/2.1.309 (external, cli)") },
		"invalid JSON":    func() req { return fromFixture(fx).withBody([]byte("{")) },
		"unknown keys":    func() req { return fromFixture(fx).withBody([]byte(`{"model":"x"}`)) },
	}
	for name, mk := range cases {
		r := mk()
		if _, err := Extract(r.head, r.body); err == nil {
			t.Errorf("%s: Extract accepted", name)
		}
	}
	if _, err := Extract(fromFixture(fx).set(HeaderUserAgent, "claude-cli/2.1.309 (external, cli)").head, nil); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown version: %v", err)
	}
}

func TestParseHead(t *testing.T) {
	raw := "POST /v1/messages?beta=true HTTP/1.1\r\nHost: a\r\nX-Empty:\r\nx-dup: 1\r\nX-Dup:  2 \t\r\n\r\nbody"
	h, body, err := ParseHead([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h.Method != "POST" || h.Target != "/v1/messages?beta=true" || h.Proto != "HTTP/1.1" || string(body) != "body" {
		t.Fatalf("%+v %q", h, body)
	}
	want := []Header{{"Host", "a"}, {"X-Empty", ""}, {"x-dup", "1"}, {"X-Dup", "2"}}
	if !slices.Equal(h.Headers, want) {
		t.Fatalf("headers %+v", h.Headers)
	}
	if v, ok := h.Get("X-DUP"); !ok || v != "1" {
		t.Fatalf("Get = %q %v", v, ok)
	}
	if _, ok := h.Get("missing"); ok {
		t.Fatal("Get(missing) found")
	}
	if got := string(h.Append(nil)); got != "POST /v1/messages?beta=true HTTP/1.1\r\nHost: a\r\nX-Empty: \r\nx-dup: 1\r\nX-Dup: 2\r\n\r\n" {
		t.Fatalf("Append = %q", got)
	}
	bad := []string{
		"POST / HTTP/1.1\r\nHost: a\r\n",
		"POST /\r\n\r\n",
		"POST  / HTTP/1.1\r\n\r\n",
		"POST / HTTP/1.1\r\nno colon\r\n\r\n",
		"POST / HTTP/1.1\r\n: empty name\r\n\r\n",
		"POST / HTTP/1.1\r\nName : space\r\n\r\n",
	}
	for _, s := range bad {
		if _, _, err := ParseHead([]byte(s)); err == nil {
			t.Errorf("ParseHead(%q) accepted", s)
		}
	}
	if _, _, err := ParseHead([]byte("x")); !errors.Is(err, ErrNoHeadEnd) {
		t.Errorf("no head end: %v", err)
	}
}

func TestProfile(t *testing.T) {
	if vs := Versions(); !slices.Equal(vs, []string{"2.1.308"}) {
		t.Fatalf("Versions = %v", vs)
	}
	p, ok := Lookup("2.1.308")
	if !ok || p.UserAgent(EntrypointCLI) != "claude-cli/2.1.308 (external, cli)" {
		t.Fatal("Lookup")
	}
	if _, ok := Lookup("2.1.307"); ok {
		t.Fatal("Lookup(2.1.307) found")
	}
	if CredentialOAuth.String() != "oauth" || CredentialAPIKey.String() != "api-key" || CredentialKind(0).String() != "unknown" {
		t.Fatal("CredentialKind.String")
	}
	// Every list is in canonical order, starts with claude-code and has
	// oauth exactly with OAuth.
	for key, list := range p.BetaLists {
		prev := -1
		for _, b := range list {
			i := p.betaIndex(b)
			if i <= prev {
				t.Errorf("%+v: %s out of order", key, b)
			}
			prev = i
		}
		if list[0] != betaClaudeCode || slices.Contains(list, betaOAuth) != (key.Credential == CredentialOAuth) {
			t.Errorf("%+v: %v", key, list)
		}
	}
	// The tool-search beta only changes the turns, and goes last when
	// nothing after it is in the list.
	l1, _ := p.BetaList(TypeQuota, CredentialOAuth, EntrypointCLI, true)
	l2, _ := p.BetaList(TypeQuota, CredentialOAuth, EntrypointCLI, false)
	if !slices.Equal(l1, l2) {
		t.Error("tool search changed the quota list")
	}
	if got := p.insertCanonical([]string{betaClaudeCode}, betaAdvancedToolUse); !slices.Equal(got, []string{betaClaudeCode, betaAdvancedToolUse}) {
		t.Errorf("insertCanonical = %v", got)
	}
	if _, ok := p.BetaList(TypeTitle, CredentialOAuth, EntrypointSDKCLI, false); ok {
		t.Error("title from sdk-cli has a list")
	}
	if got := p.HeaderNames(TypeCountTokens, CredentialAPIKey); slices.Contains(got, HeaderStainlessTimeout) ||
		slices.Contains(got, HeaderAuthorization) || !slices.Contains(got, HeaderAPIKey) {
		t.Errorf("HeaderNames = %v", got)
	}
	if cc := Marshal(CacheControl(CredentialAPIKey)); string(cc) != `{"type":"ephemeral"}` {
		t.Errorf("CacheControl = %s", cc)
	}
	if cc := Marshal(CacheControl(CredentialOAuth)); string(cc) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Errorf("CacheControl = %s", cc)
	}
}
