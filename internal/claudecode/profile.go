package claudecode

import (
	"slices"
	"strings"
)

// Entrypoint is the cc_entrypoint of a Claude Code request: sdk-cli for
// `claude -p`, cli for the interactive session.
type Entrypoint string

// Entrypoints of the profile.
const (
	EntrypointSDKCLI Entrypoint = "sdk-cli"
	EntrypointCLI    Entrypoint = "cli"
)

// CredentialKind is how a request authenticates.
type CredentialKind uint8

// Credential kinds.
const (
	// CredentialOAuth is a subscription access token (Authorization:
	// Bearer sk-ant-oat01-…), as with CLAUDE_CODE_OAUTH_TOKEN.
	CredentialOAuth CredentialKind = iota + 1
	// CredentialAPIKey is an API key (x-api-key), as with ANTHROPIC_API_KEY.
	CredentialAPIKey
)

// String returns "oauth" or "api-key".
func (k CredentialKind) String() string {
	switch k {
	case CredentialOAuth:
		return "oauth"
	case CredentialAPIKey:
		return "api-key"
	default:
		return "unknown"
	}
}

// Credential is the credential of a request.
type Credential struct {
	Kind   CredentialKind
	Secret string // the access token or the API key
}

// Type is a request type of the profile (spec 020, "Tipos de requisição").
type Type string

// Request types.
const (
	// TypeTurn is a turn of `claude -p` or an interactive turn without the
	// dangerous-tool-use beta.
	TypeTurn Type = "turn"
	// TypeTurnSafeguards is an interactive turn with safeguards.
	TypeTurnSafeguards Type = "turn_safeguards"
	// TypeQuota is the quota probe of the interactive session.
	TypeQuota Type = "quota"
	// TypeTitle is the session title request of the interactive session.
	TypeTitle Type = "title"
	// TypeClassifier is a request of the auto-mode classifier (the
	// "security monitor" side query of the interactive session, task 457).
	// [Verify] accepts it; [Build] does not make it (no fixture: its body
	// carries Claude Code's classifier prompt).
	TypeClassifier Type = "classifier"
	// TypeCountTokens is POST /v1/messages/count_tokens.
	TypeCountTokens Type = "count_tokens"
	// TypeHello is the connectivity probe HEAD /api/hello. It is not an
	// inference request and [Verify] never accepts it.
	TypeHello Type = "hello"
)

// Paths of the profile, after the mount prefix.
const (
	PathMessages    = "/v1/messages"
	PathCountTokens = "/v1/messages/count_tokens" //nolint:gosec // a route, not a credential
	PathHello       = "/api/hello"
)

// Header names, with the spelling Claude Code uses.
const (
	HeaderAccept                       = "Accept"
	HeaderAuthorization                = "Authorization"
	HeaderContentType                  = "Content-Type"
	HeaderUserAgent                    = "User-Agent"
	HeaderSessionID                    = "X-Claude-Code-Session-Id"
	HeaderStainlessArch                = "X-Stainless-Arch"
	HeaderStainlessLang                = "X-Stainless-Lang"
	HeaderStainlessOS                  = "X-Stainless-OS"
	HeaderStainlessPackageVersion      = "X-Stainless-Package-Version"
	HeaderStainlessRetryCount          = "X-Stainless-Retry-Count"
	HeaderStainlessRuntime             = "X-Stainless-Runtime"
	HeaderStainlessRuntimeVersion      = "X-Stainless-Runtime-Version"
	HeaderStainlessTimeout             = "X-Stainless-Timeout"
	HeaderAnthropicBeta                = "anthropic-beta"
	HeaderDangerousDirectBrowserAccess = "anthropic-dangerous-direct-browser-access"
	HeaderAnthropicVersion             = "anthropic-version"
	HeaderAPIKey                       = "x-api-key"
	HeaderApp                          = "x-app"
	HeaderAgentID                      = "x-claude-code-agent-id"
	HeaderParentAgentID                = "x-claude-code-parent-agent-id"
	HeaderConnection                   = "Connection"
	HeaderHost                         = "Host"
	HeaderAcceptEncoding               = "Accept-Encoding"
	HeaderContentLength                = "Content-Length"
	HeaderTransferEncoding             = "Transfer-Encoding"
)

// Profile is the shape of the requests of one Claude Code version.
type Profile struct {
	Version string // e.g. "2.1.308"

	BunVersion       string // User-Agent: Bun/<v> of HEAD /api/hello
	PackageVersion   string // X-Stainless-Package-Version
	Lang             string // X-Stainless-Lang
	Runtime          string // X-Stainless-Runtime
	RuntimeVersion   string // X-Stainless-Runtime-Version
	Timeout          string // X-Stainless-Timeout (streaming)
	FallbackTimeout  string // X-Stainless-Timeout of the non-streaming fallback
	AnthropicVersion string // anthropic-version
	Accept           string
	ContentType      string
	DirectBrowser    string // anthropic-dangerous-direct-browser-access
	App              string // x-app
	Connection       string
	AcceptEncoding   string
	HelloAccept      string // Accept of HEAD /api/hello

	Archs []string // X-Stainless-Arch values
	OSes  []string // X-Stainless-OS values

	// Headers lists every header of an inference request in order.
	Headers []HeaderSpec
	// HelloHeaders lists the headers of HEAD /api/hello in order.
	HelloHeaders []string

	// Betas lists the known betas in canonical order.
	Betas []string
	// BetaLists maps a request type, credential and entrypoint to its
	// anthropic-beta list (before the optional tool-search beta).
	BetaLists map[BetaKey][]string
	// ToolSearchBeta is added, in canonical position, to the lists of
	// TypeTurn and TypeTurnSafeguards when the ToolSearch tool is on.
	ToolSearchBeta string
	// FlagBetas are betas the same Claude Code command sends or not, by
	// state [Verify] cannot see (feature flags, cache TTL). [Verify]
	// accepts each of them present (in canonical position) or absent in
	// the lists of its types and credential; BetaLists are as captured.
	FlagBetas []FlagBeta

	// KeyOrders lists, per type, the accepted orders of the top-level body keys.
	KeyOrders map[Type][][]string

	Identities map[Entrypoint]string // system[1]
	// ClassifierIdentity is the start of system[1] of TypeClassifier (its
	// first sentence; the rest is Claude Code's prompt, not in the profile).
	ClassifierIdentity string
	ClassifierTimeout  string                // X-Stainless-Timeout of TypeClassifier
	ThinkingDisplay    map[Entrypoint]string // thinking.display of the turns

	TurnMaxTokens     int64 // max_tokens of a streaming turn
	FallbackMaxTokens int64 // max_tokens of the non-streaming fallback
	QuotaMaxTokens    int64
	QuotaContent      string // the user message of the quota probe
	DefaultEffort     string // output_config.effort
}

// HeaderSpec is one header of the profile.
type HeaderSpec struct {
	Name string
	// Only, when not zero, keeps the header to that credential kind.
	Only CredentialKind
	// Skip lists the request types that do not send the header.
	Skip []Type
	// Optional headers are sent only by some requests (the agent ids of a
	// subagent): [Verify] accepts them absent, or present at their place.
	// [Build] never sends them.
	Optional bool
}

// FlagBeta is a beta of [Profile.FlagBetas].
type FlagBeta struct {
	Beta  string
	Types []Type
	// Only, when not zero, keeps the flag to that credential kind (a beta
	// the other kind never sends stays refused).
	Only CredentialKind
}

// BetaKey indexes [Profile.BetaLists].
type BetaKey struct {
	Type       Type
	Credential CredentialKind
	Entrypoint Entrypoint
}

var profiles = map[string]*Profile{}

func register(p *Profile) { profiles[p.Version] = p }

// Lookup returns the profile of a Claude Code version.
func Lookup(version string) (*Profile, bool) {
	p, ok := profiles[version]
	return p, ok
}

// Versions returns the versions with a profile, sorted.
func Versions() []string {
	vs := make([]string, 0, len(profiles))
	for v := range profiles {
		vs = append(vs, v)
	}
	slices.Sort(vs)
	return vs
}

// latest returns the profile of the highest version (used for the header
// checks before the version is known).
func latest() *Profile {
	vs := Versions()
	return profiles[vs[len(vs)-1]]
}

// UserAgent returns the User-Agent of an inference request.
func (p *Profile) UserAgent(ep Entrypoint) string {
	return "claude-cli/" + p.Version + " (external, " + string(ep) + ")"
}

// HeaderNames returns the header names of a request of type t with a
// credential of kind c, in order.
func (p *Profile) HeaderNames(t Type, c CredentialKind) []string {
	if t == TypeHello {
		return p.HelloHeaders
	}
	return p.headerNames(t, c, nil)
}

// headerNames returns the header names of a request in order, with the
// optional headers that got has (exact spelling).
func (p *Profile) headerNames(t Type, c CredentialKind, got []Header) []string {
	names := make([]string, 0, len(p.Headers))
	for _, h := range p.Headers {
		if h.Only != 0 && h.Only != c {
			continue
		}
		if slices.Contains(h.Skip, t) {
			continue
		}
		if h.Optional && !slices.ContainsFunc(got, func(g Header) bool { return g.Name == h.Name }) {
			continue
		}
		names = append(names, h.Name)
	}
	return names
}

// flagBeta reports whether beta is a flag beta of type t for credential c.
func (p *Profile) flagBeta(beta string, t Type, c CredentialKind) bool {
	for _, f := range p.FlagBetas {
		if f.Beta == beta && slices.Contains(f.Types, t) && (f.Only == 0 || f.Only == c) {
			return true
		}
	}
	return false
}

// BetaList returns the anthropic-beta list of a request; toolSearch adds
// the tool-search beta to the turn types. ok is false when the profile has
// no list for the combination.
func (p *Profile) BetaList(t Type, c CredentialKind, ep Entrypoint, toolSearch bool) ([]string, bool) {
	base, ok := p.BetaLists[BetaKey{Type: t, Credential: c, Entrypoint: ep}]
	if !ok {
		return nil, false
	}
	if !toolSearch || (t != TypeTurn && t != TypeTurnSafeguards) {
		return base, true
	}
	return p.insertCanonical(base, p.ToolSearchBeta), true
}

// insertCanonical inserts beta into list at its canonical position.
func (p *Profile) insertCanonical(list []string, beta string) []string {
	at := p.betaIndex(beta)
	out := make([]string, 0, len(list)+1)
	done := false
	for _, b := range list {
		if !done && p.betaIndex(b) > at {
			out = append(out, beta)
			done = true
		}
		out = append(out, b)
	}
	if !done {
		out = append(out, beta)
	}
	return out
}

// betaIndex is the canonical index of a beta, or -1 when it is unknown.
func (p *Profile) betaIndex(beta string) int { return slices.Index(p.Betas, beta) }

// CacheControl returns the cache_control Claude Code puts on cached blocks
// with a credential of kind c: {"type":"ephemeral","ttl":"1h"} with OAuth,
// {"type":"ephemeral"} with an API key.
func CacheControl(c CredentialKind) Value {
	if c == CredentialOAuth {
		return Object(M("type", String("ephemeral")), M("ttl", String("1h")))
	}
	return Object(M("type", String("ephemeral")))
}

// TextBlock returns {"type":"text","text":text}, with cache_control when
// cacheControl is not null.
func TextBlock(text string, cacheControl Value) Value {
	ms := []Member{M("type", String("text")), M("text", String(text))}
	if cacheControl.Kind() != KindNull {
		ms = append(ms, M("cache_control", cacheControl))
	}
	return Object(ms...)
}

// splitTarget splits a request target into path and query.
func splitTarget(target string) (path, query string, hasQuery bool) {
	return strings.Cut(target, "?")
}
