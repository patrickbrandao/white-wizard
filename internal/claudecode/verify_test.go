package claudecode

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// req is a request under test: a head and a body.
type req struct {
	head Head
	body []byte
}

func fromFixture(f fixture) req {
	return req{head: Head{
		Method: f.head.Method, Target: f.head.Target, Proto: f.head.Proto,
		Headers: slices.Clone(f.head.Headers),
	}, body: slices.Clone(f.body)}
}

func (r req) index(name string) int {
	return slices.IndexFunc(r.head.Headers, func(h Header) bool { return h.Name == name })
}

func (r req) set(name, value string) req {
	r.head.Headers[r.index(name)].Value = value
	return r
}

func (r req) remove(name string) req {
	r.head.Headers = slices.Delete(r.head.Headers, r.index(name), r.index(name)+1)
	return r
}

func (r req) rename(name, to string) req {
	r.head.Headers[r.index(name)].Name = to
	return r
}

func (r req) insert(at int, name, value string) req {
	r.head.Headers = slices.Insert(r.head.Headers, at, Header{Name: name, Value: value})
	return r
}

// withBody replaces the body and recalculates Content-Length, so the
// refusal comes from the intended check.
func (r req) withBody(body []byte) req {
	r.body = body
	if i := r.index(HeaderContentLength); i >= 0 {
		r.head.Headers[i].Value = strconv.Itoa(len(body))
	}
	return r
}

func (r req) replace(t *testing.T, old, new string) req {
	t.Helper()
	if !strings.Contains(string(r.body), old) {
		t.Fatalf("body has no %q", old)
	}
	return r.withBody([]byte(strings.Replace(string(r.body), old, new, 1)))
}

// edit decodes the body, applies fn and encodes it back.
func (r req) edit(t *testing.T, fn func(Value) Value) req {
	t.Helper()
	v, err := Parse(r.body)
	if err != nil {
		t.Fatal(err)
	}
	return r.withBody(Marshal(fn(v)))
}

// setKey returns obj with member key replaced by v.
func setKey(obj Value, key string, v Value) Value {
	ms := slices.Clone(obj.Members())
	for i := range ms {
		if ms[i].Key == key {
			ms[i].Value = v
		}
	}
	return Object(ms...)
}

// editSystem applies fn to the system blocks.
func editSystem(fn func([]Value) []Value) func(Value) Value {
	return func(v Value) Value {
		sys, _ := v.Get("system")
		return setKey(v, "system", Array(fn(slices.Clone(sys.Elems()))...))
	}
}

// sdkHeads are the heads the official anthropic SDKs send for
// messages.create and beta.messages.create (betas: claude-code-20250219),
// captured on 2026-11-03 with tmp/claude-code/capture.py (Python SDK 2.3.0,
// Node SDK 1.3.0, fake key). Neither adds ?beta=true.
var sdkHeads = map[string]string{
	"python messages.create": "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:18610\r\nAccept-Encoding: gzip, deflate\r\nConnection: keep-alive\r\n" +
		"x-stainless-timeout: 600\r\nx-stainless-retry-count: 0\r\nx-stainless-read-timeout: 600\r\nAccept: application/json\r\n" +
		"Content-Type: application/json\r\nUser-Agent: Anthropic/Python 2.3.0\r\nX-Stainless-Lang: python\r\n" +
		"X-Stainless-Package-Version: 2.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: CPython\r\n" +
		"X-Stainless-Runtime-Version: 3.13.18\r\nAuthorization: Bearer sk-ant-api03-WWZFAKE\r\nX-Stainless-Async: false\r\n" +
		"anthropic-version: 2026-10-21\r\nContent-Length: 113\r\n\r\n",
	"python beta.messages.create": "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1:18610\r\nAccept-Encoding: gzip, deflate\r\nConnection: keep-alive\r\n" +
		"x-stainless-timeout: 600\r\nx-stainless-retry-count: 0\r\nx-stainless-read-timeout: 600\r\nAccept: application/json\r\n" +
		"Content-Type: application/json\r\nUser-Agent: Anthropic/Python 2.3.0\r\nX-Stainless-Lang: python\r\n" +
		"X-Stainless-Package-Version: 2.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: CPython\r\n" +
		"X-Stainless-Runtime-Version: 3.13.18\r\nAuthorization: Bearer sk-ant-api03-WWZFAKE\r\nX-Stainless-Async: false\r\n" +
		"anthropic-version: 2026-10-21\r\nanthropic-beta: claude-code-20250219\r\nContent-Length: 113\r\n\r\n",
	"node messages.create": "POST /v1/messages HTTP/1.1\r\nhost: 127.0.0.1:18610\r\nconnection: keep-alive\r\nAccept: application/json\r\n" +
		"User-Agent: Anthropic/JS 1.3.0\r\nX-Stainless-Retry-Count: 0\r\nX-Stainless-Timeout: 600\r\nX-Stainless-Lang: js\r\n" +
		"X-Stainless-Package-Version: 1.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: node\r\n" +
		"X-Stainless-Runtime-Version: v26.8.1\r\nanthropic-version: 2026-10-21\r\nauthorization: Bearer sk-ant-api03-WWZFAKE\r\n" +
		"content-type: application/json\r\naccept-language: *\r\nsec-fetch-mode: cors\r\naccept-encoding: gzip, deflate\r\n" +
		"content-length: 113\r\n\r\n",
	"node beta.messages.create": "POST /v1/messages HTTP/1.1\r\nhost: 127.0.0.1:18610\r\nconnection: keep-alive\r\nAccept: application/json\r\n" +
		"User-Agent: Anthropic/JS 1.3.0\r\nX-Stainless-Retry-Count: 0\r\nX-Stainless-Timeout: 600\r\nX-Stainless-Lang: js\r\n" +
		"X-Stainless-Package-Version: 1.3.0\r\nX-Stainless-OS: MacOS\r\nX-Stainless-Arch: arm64\r\nX-Stainless-Runtime: node\r\n" +
		"X-Stainless-Runtime-Version: v26.8.1\r\nanthropic-version: 2026-10-21\r\nauthorization: Bearer sk-ant-api03-WWZFAKE\r\n" +
		"content-type: application/json\r\nanthropic-beta: claude-code-20250219\r\naccept-language: *\r\nsec-fetch-mode: cors\r\n" +
		"accept-encoding: gzip, deflate\r\ncontent-length: 113\r\n\r\n",
}

func TestVerifySDKHeads(t *testing.T) {
	bodies := map[string]string{
		"python": `{"max_tokens":16,"messages":[{"role":"user","content":"Hello there, reply with ok"}],"model":"claude-sonnet-5-5"}`,
		"node":   `{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"Hello there, reply with ok"}]}`,
	}
	for name, raw := range sdkHeads {
		sdk, _, _ := strings.Cut(name, " ")
		h, _, err := ParseHead([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(bodies[sdk])
		if reason, detail, ok := Verify(h, body); ok || reason != ReasonQuery {
			t.Errorf("%s: Verify = %q (%s), want query", name, reason, detail)
		}
		// An SDK that adds ?beta=true (as older SDKs did on the beta
		// resource) still lacks X-Claude-Code-Session-Id.
		h.Target += "?beta=true"
		if reason, detail, ok := Verify(h, body); ok || reason != ReasonHeaderMissing {
			t.Errorf("%s with ?beta=true: Verify = %q (%s), want header_missing", name, reason, detail)
		}
		// With the missing headers added, the order is still not the profile's.
		for _, name := range latest().HeaderNames(TypeTurn, CredentialOAuth) {
			if _, ok := h.Get(name); !ok {
				h.Headers = append(h.Headers, Header{Name: name, Value: "x"})
			}
		}
		if reason, detail, ok := Verify(h, body); ok || reason != ReasonHeaderOrder {
			t.Errorf("%s with the profile headers: Verify = %q (%s), want header_order", name, reason, detail)
		}
	}
}

func TestVerifyMutations(t *testing.T) {
	fx002, fx010, fx013 := loadFixture(t, "002-sdk-cli-oauth-turn.raw"), loadFixture(t, "010-cli-oauth-quota.raw"), loadFixture(t, "013-sdk-cli-oauth-count-tokens.raw")
	f002 := func() req { return fromFixture(fx002) }
	f010 := func() req { return fromFixture(fx010) }
	f013 := func() req { return fromFixture(fx013) }
	betas, _ := fx002.head.Get(HeaderAnthropicBeta)
	device := "68159782ff84a80342f7e86ed7cddfaaf32e783f7a2a3e29e09814638ebb1f79"
	session := "ce90e450-43fc-4765-8110-6aed6d650ca1"
	other := "ce90e450-43fc-4765-8110-6aed6d650ca2"

	type mutation struct {
		name   string
		req    func() req
		reason string
	}
	ms := []mutation{
		// The table "Mutações de teste" of spec 020.
		{"without ?beta=true", func() req { r := f002(); r.head.Target = "/v1/messages"; return r }, ReasonQuery},
		{"Transfer-Encoding instead of Content-Length", func() req {
			return f002().rename(HeaderContentLength, HeaderTransferEncoding).set(HeaderTransferEncoding, "chunked")
		}, ReasonTransferEncoding},
		{"without X-Stainless-Timeout", func() req { return f002().remove(HeaderStainlessTimeout) }, ReasonHeaderMissing},
		{"X-Stainless-Arch swapped with X-Stainless-Lang", func() req {
			r := f002()
			i, j := r.index(HeaderStainlessArch), r.index(HeaderStainlessLang)
			r.head.Headers[i], r.head.Headers[j] = r.head.Headers[j], r.head.Headers[i]
			return r
		}, ReasonHeaderOrder},
		{"user-agent in lowercase", func() req { return f002().rename(HeaderUserAgent, "user-agent") }, ReasonHeaderOrder},
		{"extra X-Forwarded-For", func() req {
			return f002().insert(19, "X-Forwarded-For", "10.0.0.1")
		}, ReasonHeaderOrder},
		{"User-Agent sdk-py", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.308 (external, sdk-py)")
		}, ReasonUserAgent},
		{"User-Agent 2.1.309", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.309 (external, sdk-cli)")
		}, ReasonUnknownVersion},
		{"X-Stainless-Package-Version 0.129.0", func() req {
			return f002().set(HeaderStainlessPackageVersion, "0.129.0")
		}, ReasonHeaderValue},
		{"betas out of order", func() req {
			return f002().set(HeaderAnthropicBeta,
				strings.Replace(betas, "claude-code-20250219,oauth-2025-04-20", "oauth-2025-04-20,claude-code-20250219", 1))
		}, ReasonBeta},
		{"invented beta", func() req {
			return f002().set(HeaderAnthropicBeta, betas+",wwz-invented-2026-01-01")
		}, ReasonBeta},
		{"oauth beta with x-api-key", func() req {
			r := f002().remove(HeaderAuthorization)
			return r.insert(r.index(HeaderAnthropicVersion)+1, HeaderAPIKey, "sk-ant-api03-WWZFAKE")
		}, ReasonBeta},
		{"space after : in the body", func() req { return f002().replace(t, `"model":`, `"model": `) }, ReasonBodyEncoding},
		{"< escaped", func() req { return f002().replace(t, "<total_tokens>", "\\"+"u003ctotal_tokens>") }, ReasonBodyEncoding},
		{"stream before model", func() req {
			return f002().
				replace(t, `,"stream":true}`, `}`).replace(t, `{"model":`, `{"stream":true,"model":`)
		}, ReasonBodyKeyOrder},
		{"device_id with 63 hex", func() req { return f002().replace(t, device, device[:63]) }, ReasonMetadata},
		{"session_id differs from the header", func() req {
			return f002().replace(t, `\"session_id\":\"`+session, `\"session_id\":\"`+other)
		}, ReasonSessionMismatch},
		{"cch in the billing header", func() req {
			return f002().replace(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli; cch=00000;")
		}, ReasonBillingHeader},
		{"cc_version 2.1.307", func() req { return f002().replace(t, "cc_version=2.1.308.9fe", "cc_version=2.1.307.9fe") }, ReasonBillingHeader},
		{"fingerprint 000", func() req { return f002().replace(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308.000") }, ReasonFingerprint},
		{"cli identity in sdk-cli", func() req {
			return f002().replace(t,
				"You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are Claude Code, Anthropic's official CLI for Claude.")
		}, ReasonIdentity},

		// More of each check.
		{"GET", func() req { r := f002(); r.head.Method = "GET"; return r }, ReasonQuery},
		{"HTTP/1.0", func() req { r := f002(); r.head.Proto = "HTTP/1.0"; return r }, ReasonQuery},
		{"other query parameter", func() req {
			r := f002()
			r.head.Target = "/v1/messages?beta=true&x=1"
			return r
		}, ReasonQuery},
		{"other path", func() req { r := f002(); r.head.Target = "/v1/complete?beta=true"; return r }, ReasonQuery},
		{"no credential", func() req { return f002().remove(HeaderAuthorization) }, ReasonHeaderMissing},
		{"Authorization and x-api-key", func() req {
			return f002().insert(17, HeaderAPIKey, "k")
		}, ReasonHeaderOrder},
		{"repeated Accept", func() req { return f002().insert(1, HeaderAccept, "application/json") }, ReasonHeaderOrder},
		{"X-Stainless-Timeout on count_tokens", func() req {
			r := f013()
			return r.insert(r.index(HeaderAnthropicBeta), HeaderStainlessTimeout, "600")
		}, ReasonHeaderOrder},
		{"User-Agent of another client", func() req { return f002().set(HeaderUserAgent, "Anthropic/JS 1.3.0") }, ReasonUserAgent},
		{"User-Agent without the closing parenthesis", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.308 (external, sdk-cli")
		}, ReasonUserAgent},
		{"User-Agent with a bad version", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.0308 (external, sdk-cli)")
		}, ReasonUserAgent},
		{"User-Agent version with 2 parts", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1 (external, sdk-cli)")
		}, ReasonUserAgent},
		{"User-Agent version with letters", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.30x (external, sdk-cli)")
		}, ReasonUserAgent},
		{"User-Agent without external", func() req {
			return f002().set(HeaderUserAgent, "claude-cli/2.1.308 (sdk-cli)")
		}, ReasonUserAgent},
		{"wrong Content-Length", func() req { return f002().set(HeaderContentLength, "1") }, ReasonHeaderValue},
		{"session not UUID v4", func() req { return f002().set(HeaderSessionID, strings.ToUpper(session)) }, ReasonHeaderValue},
		{"retry count with a leading zero", func() req { return f002().set(HeaderStainlessRetryCount, "01") }, ReasonHeaderValue},
		{"unknown arch", func() req { return f002().set(HeaderStainlessArch, "x86_64") }, ReasonHeaderValue},
		{"Accept-Encoding without zstd", func() req {
			return f002().set(HeaderAcceptEncoding, "gzip, deflate, br")
		}, ReasonHeaderValue},
		{"without claude-code beta", func() req {
			return f002().set(HeaderAnthropicBeta, strings.TrimPrefix(betas, "claude-code-20250219,"))
		}, ReasonBeta},
		{"repeated beta", func() req {
			return f002().set(HeaderAnthropicBeta, strings.Replace(betas, "oauth-2025-04-20", "oauth-2025-04-20,oauth-2025-04-20", 1))
		}, ReasonBeta},
		{"Bearer oat without the oauth beta", func() req {
			return f002().set(HeaderAnthropicBeta, strings.Replace(betas, "oauth-2025-04-20,", "", 1))
		}, ReasonBeta},
		{"Authorization that is not an oat token", func() req {
			return f002().set(HeaderAuthorization, "Bearer sk-ant-api03-WWZFAKE")
		}, ReasonBeta},
		{"list of no request", func() req {
			return f002().set(HeaderAnthropicBeta, strings.Replace(betas, ",effort-2025-11-24", "", 1))
		}, ReasonBeta},
		{"list of another type than the body", func() req {
			list, _ := latest().BetaList(TypeTurn, CredentialOAuth, EntrypointCLI, false)
			return f010().set(HeaderAnthropicBeta, strings.Join(list, ","))
		}, ReasonBeta},
		{"invalid JSON", func() req { return f002().withBody([]byte(`{"model":`)) }, ReasonBodyEncoding},
		{"duplicate key", func() req { return f002().replace(t, `{"model":`, `{"model":"x","model":`) }, ReasonBodyEncoding},
		{"body that is not an object", func() req { return f002().withBody([]byte(`[]`)) }, ReasonBodyKeyOrder},
		{"metadata with another key", func() req {
			return f002().replace(t, `"metadata":{"user_id":`, `"metadata":{"user":`)
		}, ReasonMetadata},
		{"system missing", func() req {
			return f002().edit(t, editSystem(func([]Value) []Value { return nil }))
		}, ReasonBillingHeader},
		{"billing header with cache_control", func() req {
			return f002().edit(t, editSystem(func(b []Value) []Value {
				text, _ := b[0].Get("text")
				b[0] = TextBlock(text.Str(), CacheControl(CredentialOAuth))
				return b
			}))
		}, ReasonBillingHeader},
		{"billing header block of another type", func() req {
			return f002().edit(t, editSystem(func(b []Value) []Value {
				b[0] = setKey(b[0], "type", String("txt"))
				return b
			}))
		}, ReasonBillingHeader},
		{"system[0] is the system prompt", func() req {
			return f002().edit(t, editSystem(func(b []Value) []Value {
				return []Value{TextBlock("You are a terse test assistant.", Null())}
			}))
		}, ReasonBillingHeader},
		{"cc_entrypoint cli in sdk-cli", func() req {
			return f002().replace(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=cli;")
		}, ReasonBillingHeader},
		{"identity missing", func() req {
			return f002().edit(t, editSystem(func(b []Value) []Value { return b[:1] }))
		}, ReasonIdentity},
		{"identity with the API key cache_control", func() req {
			return f002().edit(t, editSystem(func(b []Value) []Value {
				b[1] = TextBlock(latest().Identities[EntrypointSDKCLI], CacheControl(CredentialAPIKey))
				return b
			}))
		}, ReasonIdentity},
		{"quota with system", func() req {
			return f010().replace(t, `,"metadata":`, `,"system":[],"metadata":`)
		}, ReasonBodyKeyOrder},
		{"quota session mismatch", func() req {
			return f010().set(HeaderSessionID, other)
		}, ReasonSessionMismatch},
		{"count_tokens with turn keys", func() req {
			return f013().replace(t, `,"tools":[]}`, `,"tools":[],"stream":true}`)
		}, ReasonBodyKeyOrder},
		{"count_tokens with the turn betas", func() req {
			return f013().set(HeaderAnthropicBeta, betas)
		}, ReasonBeta},
	}
	for _, m := range ms {
		r := m.req()
		reason, detail, ok := Verify(r.head, r.body)
		if ok || reason != m.reason {
			t.Errorf("%s: Verify = %q (%s), want %q", m.name, reason, detail, m.reason)
			continue
		}
		if detail == "" || !slices.Contains(Reasons, reason) {
			t.Errorf("%s: empty detail or unknown reason %q", m.name, reason)
		}
	}
}

func TestVerifyDetail(t *testing.T) {
	fx002 := loadFixture(t, "002-sdk-cli-oauth-turn.raw")
	f002 := func() req { return fromFixture(fx002) }
	r := f002().rename(HeaderStainlessArch, "x-stainless-arch")
	_, detail, _ := Verify(r.head, r.body)
	if want := `expected "X-Stainless-Arch" at position 6, got "x-stainless-arch"`; detail != want {
		t.Errorf("detail = %s, want %s", detail, want)
	}
	r = f002().replace(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308.000")
	if _, detail, _ := Verify(r.head, r.body); detail != "expected 9fe, got 000" {
		t.Errorf("detail = %s", detail)
	}
	r = f002().insert(21, "X-Forwarded-For", "10.0.0.1")
	if _, detail, _ := Verify(r.head, r.body); detail != `unexpected "X-Forwarded-For" at position 22` {
		t.Errorf("detail = %s", detail)
	}
	if d := checkOrder([]string{"A", "B"}, []Header{{Name: "A"}}); d != `expected "B" at position 2, got the end of the head` {
		t.Errorf("checkOrder = %s", d)
	}
	if len(Reasons) != 15 {
		t.Errorf("%d reasons", len(Reasons))
	}
}

func TestVerifyAcceptsBuilt(t *testing.T) {
	// Every request Build makes is accepted, captured or inferred.
	p := latest()
	device := strings.Repeat("0f", 32)
	user := Object(M("role", String("user")), M("content", Array(TextBlock("Hello there, reply with ok", Null()))))
	thinking := Object(M("role", String("assistant")), M("content", Array(Object(M("type", String("thinking")), M("thinking", String("…"))))))
	for key := range p.BetaLists {
		if key.Type == TypeClassifier {
			continue // verified, not built (TestVerifyClassifier)
		}
		for _, toolSearch := range []bool{false, true} {
			for _, nonStream := range []bool{false, true} {
				r := Request{
					Version: p.Version, Entrypoint: key.Entrypoint, Type: key.Type,
					Credential: Credential{Kind: key.Credential, Secret: "sk-ant-oat01-WWZFAKE"},
					Prefix:     "/anthropic", Host: "wwz:8000",
					SessionID: "d559555d-99d8-4d64-92ca-d81b6c0f2022", DeviceID: device,
					AccountUUID: "0b7c2f3e-1d2a-4b5c-9d8e-7f6a5b4c3d2e", RetryCount: 2, Arch: "x64", OS: "Linux",
					Model: "claude-sonnet-5-5", Messages: []Value{user, thinking}, SystemPrompt: "sys",
					Tools: []Value{Object(M("name", String("t")))}, ToolSearch: toolSearch,
					Safeguards: Array(), Effort: "high", NonStream: nonStream && key.Type == TypeTurn,
					OutputFormat: Object(M("type", String("json_schema"))),
				}
				head, body, err := Build(r)
				if err != nil {
					t.Fatalf("%+v: %v", key, err)
				}
				h, rest, err := ParseHead(append(head, body...))
				if err != nil {
					t.Fatal(err)
				}
				if reason, detail, ok := Verify(h, rest); !ok {
					t.Errorf("%+v toolSearch=%v nonStream=%v: Verify refused: %s: %s", key, toolSearch, nonStream, reason, detail)
				}
				back, err := Extract(h, rest)
				if err != nil {
					t.Fatal(err)
				}
				head2, body2, _ := Build(back)
				if string(head2) != string(head) || string(body2) != string(body) {
					t.Errorf("%+v: Extract + Build is not the identity", key)
				}
			}
		}
	}
}
