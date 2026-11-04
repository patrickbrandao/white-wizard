package claudecode

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shapes of the real Claude Code 2.1.308 in normal use (task 457:
// default system prompt and tools, -p, --resume, tools, subagents,
// CLAUDE.md, interactive with the auto-mode classifier, /context and
// !commands), built from the fixtures without the default prompt.

// firstUser returns v with the content of messages[0] replaced.
func firstUser(content Value) func(Value) Value {
	return func(v Value) Value {
		messages, _ := v.Get("messages")
		ms := slices.Clone(messages.Elems())
		ms[0] = setKey(ms[0], "content", content)
		return setKey(v, "messages", Array(ms...))
	}
}

// withBeta inserts beta in the anthropic-beta list at its canonical place.
func (r req) withBeta(beta string) req {
	list, _ := r.head.Get(HeaderAnthropicBeta)
	return r.set(HeaderAnthropicBeta, strings.Join(latest().insertCanonical(strings.Split(list, ","), beta), ","))
}

// withoutBeta removes beta from the anthropic-beta list.
func (r req) withoutBeta(beta string) req {
	list, _ := r.head.Get(HeaderAnthropicBeta)
	return r.set(HeaderAnthropicBeta, strings.Join(slices.DeleteFunc(strings.Split(list, ","), func(b string) bool { return b == beta }), ","))
}

// cacheControl replaces the cache_control of system[1:] by cc.
func cacheControl(cc Value) func(Value) Value {
	return editSystem(func(b []Value) []Value {
		for i := 1; i < len(b); i++ {
			text, _ := b[i].Get("text")
			b[i] = TextBlock(text.Str(), cc)
		}
		return b
	})
}

const (
	testAgentID = "a67195a5d9d363c76"
	reminder    = "<system-reminder>\nAttribution of commits\n</system-reminder>"
)

// subagent makes r a subagent request: x-claude-code-agent-id after x-app,
// cc_is_subagent=true and the 5-minute cache (no extended-cache-ttl).
func subagent(t *testing.T, r req) req {
	t.Helper()
	r = r.insert(r.index(HeaderApp)+1, HeaderAgentID, testAgentID).withoutBeta(betaExtendedCacheTTL)
	return r.replace(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli;"+billingSubagent).
		edit(t, cacheControl(CacheControl(CredentialAPIKey)))
}

// classifier is a request of the auto-mode classifier with credential
// kind, as captured (task 457) but with a stand-in for its prompt.
func classifier(t *testing.T, kind CredentialKind, stopSequences bool) req {
	t.Helper()
	p := latest()
	session := "81750e95-dbe9-47a1-b0c3-680217cdfb76"
	head := Head{Method: "POST", Target: "/v1/messages?beta=true", Proto: "HTTP/1.1"}
	betas, _ := p.BetaList(TypeClassifier, kind, EntrypointCLI, false)
	cred := Credential{Kind: kind, Secret: "sk-ant-oat01-WWZFAKE"}
	if kind == CredentialAPIKey {
		cred.Secret = "sk-ant-api03-WWZFAKE"
	}
	r := Request{Version: p.Version, Entrypoint: EntrypointCLI, Credential: cred, SessionID: session, Host: "wwz:8000"}
	for _, name := range p.HeaderNames(TypeClassifier, kind) {
		v := headerValue(p, r, name, betas, 0)
		if name == HeaderStainlessTimeout {
			v = p.ClassifierTimeout
		}
		head.Headers = append(head.Headers, Header{Name: name, Value: v})
	}
	text := "Here is the transcript"
	cc := CacheControl(kind)
	ms := []Member{
		M("model", String("claude-sonnet-5")),
		M("max_tokens", Int(64)),
		M("system", Array(
			TextBlock(BillingHeader(p.Version, EntrypointCLI, text), Null()),
			TextBlock(p.ClassifierIdentity+"\n\n## Context\n\n(the classifier prompt)", cc),
			TextBlock("\n\n## Session Context\n", Null()),
		)),
		M("messages", Array(Object(M("role", String("user")), M("content", Array(TextBlock(text, Null()), TextBlock("<transcript>", cc)))))),
	}
	if stopSequences {
		ms = append(ms, M("stop_sequences", Array(String("</severity>"))))
	}
	ms = append(ms,
		M("thinking", Object(M("type", String("disabled")))),
		M("metadata", Object(M("user_id", String(UserID(strings.Repeat("0f", 32), "", session))))))
	return req{head: head}.withBody(Marshal(Object(ms...)))
}

func TestVerifyNormalUse(t *testing.T) {
	fx002, fx004 := loadFixture(t, "002-sdk-cli-oauth-turn.raw"), loadFixture(t, "004-sdk-cli-apikey-turn.raw")
	fx012 := loadFixture(t, "012-cli-oauth-turn-safeguards.raw")
	f002 := func() req { return fromFixture(fx002) }
	f004 := func() req { return fromFixture(fx004) }
	f012 := func() req { return fromFixture(fx012) }
	prompt := TextBlock("Hello there, reply with ok", Null())

	type tc struct {
		name   string
		req    func() req
		reason string // "" = accepted
	}
	cases := []tc{
		// Fingerprint: the meta messages (attribution reminder, CLAUDE.md,
		// /context output) are blocks of the same API message before the prompt.
		{"meta blocks before the prompt", func() req {
			return f002().edit(t, firstUser(Array(TextBlock(reminder, Null()), TextBlock("<system-reminder>\nContents of CLAUDE.md\n</system-reminder>", Null()), prompt)))
		}, ""},
		{"/context before the prompt", func() req {
			return f002().edit(t, firstUser(Array(TextBlock(reminder, Null()),
				TextBlock("<command-name>/context</command-name>", Null()),
				TextBlock("<local-command-stdout>Context Usage</local-command-stdout>", Null()),
				TextBlock("## Context Usage\n\n**Model:** claude-sonnet-5-5", Null()), prompt)))
		}, ""},
		{"meta blocks, no block has the fingerprint", func() req {
			return f002().edit(t, firstUser(Array(TextBlock(reminder, Null()), TextBlock("say ok", Null()))))
		}, ReasonFingerprint},
		{"an image only: the fingerprint of an empty text", func() req {
			r := f002().edit(t, firstUser(Array(TextBlock(reminder, Null()), Object(M("type", String("image"))))))
			return r.replace(t, "cc_version=2.1.308.9fe", "cc_version=2.1.308."+Fingerprint("", "2.1.308"))
		}, ""},

		// Flag betas: inline-tools and advisor-tool (feature flags), each
		// present or absent in the turn types and the title.
		{"OAuth turn with inline-tools", func() req { return f002().withBeta(betaInlineTools) }, ""},
		{"OAuth turn with advisor-tool and tool search", func() req {
			return f002().withBeta(betaAdvisorTool).withBeta(betaAdvancedToolUse)
		}, ""},
		{"API key turn without inline-tools and advisor-tool", func() req {
			return f004().withoutBeta(betaInlineTools).withoutBeta(betaAdvisorTool)
		}, ""},
		{"API key turn with tool search, without the flag betas", func() req {
			return f004().withoutBeta(betaInlineTools).withoutBeta(betaAdvisorTool).withBeta(betaAdvancedToolUse)
		}, ""},
		{"inline-tools in count_tokens", func() req {
			return fromFixture(loadFixture(t, "013-sdk-cli-oauth-count-tokens.raw")).withBeta(betaInlineTools)
		}, ReasonBeta},
		{"advisor-tool in the quota probe", func() req {
			return fromFixture(loadFixture(t, "010-cli-oauth-quota.raw")).withBeta(betaAdvisorTool)
		}, ReasonBeta},

		// extended-cache-ttl goes with the 1-hour cache_control.
		{"OAuth turn on the 5-minute cache", func() req {
			return f002().withoutBeta(betaExtendedCacheTTL).edit(t, cacheControl(CacheControl(CredentialAPIKey)))
		}, ""},
		{"OAuth turn without extended-cache-ttl but with the 1-hour identity", func() req {
			return f002().withoutBeta(betaExtendedCacheTTL)
		}, ReasonIdentity},
		{"API key turn with extended-cache-ttl", func() req { return f004().withBeta(betaExtendedCacheTTL) }, ReasonBeta},

		// The turn after a tool result keeps the safeguards betas without
		// the safeguards key.
		{"turn with the safeguards betas", func() req {
			return f012().edit(t, func(v Value) Value {
				ms := slices.DeleteFunc(slices.Clone(v.Members()), func(m Member) bool { return m.Key == "safeguards" })
				return Object(ms...)
			})
		}, ""},
		{"safeguards with the betas of a cli turn", func() req {
			return f012().withoutBeta(betaDangerousTool).withoutBeta(betaAFKMode)
		}, ReasonBeta},

		// Subagents.
		{"subagent", func() req { return subagent(t, f002()) }, ""},
		{"subagent with an API key", func() req {
			r := f004().insert(fromFixture(fx004).index(HeaderApp)+1, HeaderAgentID, "ageneral-purpose-67195a5d9d363c76")
			return r.replace(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli;"+billingSubagent)
		}, ""},
		{"nested subagent", func() req {
			r := subagent(t, f002())
			return r.insert(r.index(HeaderAgentID)+1, HeaderParentAgentID, "a0123456789abcdef")
		}, ""},
		{"parent agent id before the agent id", func() req {
			r := subagent(t, f002())
			return r.insert(r.index(HeaderAgentID), HeaderParentAgentID, "a0123456789abcdef")
		}, ReasonHeaderOrder},
		{"agent id before x-app", func() req {
			r := f002()
			return r.insert(r.index(HeaderApp), HeaderAgentID, testAgentID)
		}, ReasonHeaderOrder},
		{"agent id in another case", func() req {
			r := f002()
			return r.insert(r.index(HeaderApp)+1, "X-Claude-Code-Agent-Id", testAgentID)
		}, ReasonHeaderOrder},
		{"agent id that is not an id", func() req { return subagent(t, f002()).set(HeaderAgentID, "agent-1") }, ReasonHeaderValue},
		{"agent id without cc_is_subagent", func() req {
			r := f002()
			return r.insert(r.index(HeaderApp)+1, HeaderAgentID, testAgentID)
		}, ReasonBillingHeader},
		{"cc_is_subagent without the agent id", func() req {
			return f002().replace(t, "cc_entrypoint=sdk-cli;", "cc_entrypoint=sdk-cli;"+billingSubagent)
		}, ReasonBillingHeader},
		{"cc_is_subagent before cc_entrypoint", func() req {
			r := subagent(t, f002())
			return r.replace(t, "cc_entrypoint=sdk-cli; cc_is_subagent=true;", "cc_is_subagent=true; cc_entrypoint=sdk-cli;")
		}, ReasonBillingHeader},

		// The auto-mode classifier.
		{"classifier, stage 1", func() req { return classifier(t, CredentialOAuth, true) }, ""},
		{"classifier, stage 2", func() req { return classifier(t, CredentialOAuth, false) }, ""},
		{"classifier with an API key", func() req { return classifier(t, CredentialAPIKey, true) }, ""},
		{"classifier with the turn timeout", func() req {
			return classifier(t, CredentialOAuth, true).set(HeaderStainlessTimeout, "600")
		}, ReasonHeaderValue},
		{"turn with the classifier timeout", func() req { return f002().set(HeaderStainlessTimeout, "60") }, ReasonHeaderValue},
		{"classifier with another prompt", func() req {
			return classifier(t, CredentialOAuth, true).replace(t, "You are a security monitor", "You are a monitor")
		}, ReasonIdentity},
		{"classifier prompt without cache_control", func() req {
			return classifier(t, CredentialOAuth, true).edit(t, editSystem(func(b []Value) []Value {
				text, _ := b[1].Get("text")
				b[1] = TextBlock(text.Str(), Null())
				return b
			}))
		}, ReasonIdentity},
		{"classifier with the turn betas", func() req {
			betas, _ := fromFixture(fx012).head.Get(HeaderAnthropicBeta)
			return classifier(t, CredentialOAuth, true).set(HeaderAnthropicBeta, betas)
		}, ReasonBeta},
		{"classifier from sdk-cli", func() req {
			return classifier(t, CredentialOAuth, true).set(HeaderUserAgent, "claude-cli/2.1.308 (external, sdk-cli)")
		}, ReasonBeta},
	}
	for _, c := range cases {
		r := c.req()
		reason, detail, ok := Verify(r.head, r.body)
		if c.reason == "" && !ok {
			t.Errorf("%s: Verify refused: %s: %s", c.name, reason, detail)
		}
		if c.reason != "" && (ok || reason != c.reason) {
			t.Errorf("%s: Verify = %q (%s), want %q", c.name, reason, detail, c.reason)
		}
	}

	// The detail lists every candidate fingerprint.
	r := f002().edit(t, firstUser(Array(TextBlock(reminder, Null()), TextBlock("say ok", Null()))))
	want := "expected " + Fingerprint(reminder, "2.1.308") + " or " + Fingerprint("say ok", "2.1.308") + ", got 9fe"
	if _, detail, _ := Verify(r.head, r.body); detail != want {
		t.Errorf("detail = %s, want %s", detail, want)
	}
}

func TestFingerprintTexts(t *testing.T) {
	user := func(content Value) Value { return Object(M("role", String("user")), M("content", content)) }
	system := Object(M("role", String("system")), M("content", String("env")))
	image := Object(M("type", String("image")))
	cases := []struct {
		name     string
		messages []Value
		want     []string
	}{
		{"none", nil, []string{""}},
		{"string", []Value{system, user(String("say ok"))}, []string{"say ok"}},
		{"blocks", []Value{user(Array(TextBlock("a", Null()), TextBlock("b", Null())))}, []string{"a", "b"}},
		{"text and image", []Value{user(Array(TextBlock("a", Null()), image))}, []string{"a", ""}},
		{"empty list", []Value{user(Array())}, []string{""}},
		{"only the first user message", []Value{user(String("x")), user(String("y"))}, []string{"x"}},
	}
	for _, c := range cases {
		if got := FingerprintTexts(c.messages); !slices.Equal(got, c.want) {
			t.Errorf("%s: FingerprintTexts = %q, want %q", c.name, got, c.want)
		}
	}
	if got := fingerprints([]string{"say ok", "say ok", ""}, "2.1.308"); !slices.Equal(got, []string{"7f2", Fingerprint("", "2.1.308")}) {
		t.Errorf("fingerprints = %v", got)
	}
}

func TestIsAgentID(t *testing.T) {
	for id, want := range map[string]bool{
		"a67195a5d9d363c76":                                 true,
		"ageneral-purpose-67195a5d9d363c76":                 true,
		"ax_Y-1-0123456789abcdef":                           true,
		"b67195a5d9d363c76":                                 false, // no "a"
		"a67195a5d9d363c7":                                  false, // 15 hex
		"a67195a5d9d363C76":                                 false, // upper-case hex
		"ageneral67195a5d9d363c76":                          false, // label without "-"
		"a-67195a5d9d363c76":                                false, // empty label
		"a" + strings.Repeat("x", 64) + "-67195a5d9d363c76": false, // label of 64
		"ag.p-67195a5d9d363c76":                             false, // "." in the label
	} {
		if got := isAgentID(id); got != want {
			t.Errorf("isAgentID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestBuildClassifier(t *testing.T) {
	r := Request{Version: "2.1.308", Type: TypeClassifier, Entrypoint: EntrypointCLI, Credential: Credential{Kind: CredentialOAuth}}
	if _, _, err := Build(r); err == nil || !strings.Contains(err.Error(), "verified, not built") {
		t.Errorf("Build(classifier) = %v", err)
	}
}

// TestVerifyLocalCaptures runs Verify over the raw captures of the real
// Claude Code kept out of git (they carry its default prompt):
// tmp/claude-code/captures/ (full-default-oauth.raw and, in 457/, the
// captures of task 457: -p and interactive with OAuth and API key, flag
// betas forced by environment variables, subagent, classifier, /context,
// !command, the turn after a tool). WWZ_CC_CAPTURES overrides the
// directory. Captures through a proxy (absolute target, Proxy-Connection)
// are turned back into what Claude Code sends to ANTHROPIC_BASE_URL. Skips
// when the directory does not exist.
func TestVerifyLocalCaptures(t *testing.T) {
	dir := os.Getenv("WWZ_CC_CAPTURES")
	if dir == "" {
		dir = filepath.Join("..", "..", "tmp", "claude-code", "captures")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no local captures in %s", dir)
	}
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".raw") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h, body, err := ParseHead(raw)
		if err != nil {
			return nil // not a request head
		}
		if h.Method != "POST" || !strings.Contains(h.Target, PathMessages) {
			return nil // HEAD /api/hello and the account routes
		}
		if rest, ok := strings.CutPrefix(h.Target, "http://"); ok {
			_, h.Target, _ = strings.Cut(rest, "/")
			h.Target = "/" + h.Target
			h.Headers = slices.DeleteFunc(h.Headers, func(x Header) bool { return x.Name == "Proxy-Connection" })
		}
		n++
		if reason, detail, ok := Verify(h, body); !ok {
			t.Errorf("%s: Verify refused: %s: %s", path, reason, detail)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d captures verified", n)
}
