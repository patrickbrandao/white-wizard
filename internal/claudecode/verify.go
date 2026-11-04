package claudecode

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Refusal reasons of [Verify], in check order (spec 020, "Verificação
// estrita"). The list is a contract: a new code is a spec change.
const (
	ReasonQuery            = "query"
	ReasonTransferEncoding = "transfer_encoding"
	ReasonHeaderMissing    = "header_missing"
	ReasonHeaderOrder      = "header_order"
	ReasonUserAgent        = "user_agent"
	ReasonUnknownVersion   = "unknown_version"
	ReasonHeaderValue      = "header_value"
	ReasonBeta             = "beta"
	ReasonBodyEncoding     = "body_encoding"
	ReasonBodyKeyOrder     = "body_key_order"
	ReasonMetadata         = "metadata"
	ReasonSessionMismatch  = "session_mismatch"
	ReasonBillingHeader    = "billing_header"
	ReasonFingerprint      = "fingerprint"
	ReasonIdentity         = "identity"
)

// Reasons lists every refusal reason in check order.
var Reasons = []string{
	ReasonQuery, ReasonTransferEncoding, ReasonHeaderMissing, ReasonHeaderOrder,
	ReasonUserAgent, ReasonUnknownVersion, ReasonHeaderValue, ReasonBeta,
	ReasonBodyEncoding, ReasonBodyKeyOrder, ReasonMetadata, ReasonSessionMismatch,
	ReasonBillingHeader, ReasonFingerprint, ReasonIdentity,
}

// Verify checks that a request to POST …/v1/messages or POST
// …/v1/messages/count_tokens is exactly what a Claude Code version of the
// profile sends: head is the raw head (line and headers as received) and
// body the raw body. It returns ok, or the reason of the first failing
// check and a detail of what diverged.
//
// The checks follow the order of spec 020. The beta list is checked in two
// steps: before the body, it must be the list of some request type for the
// credential and entrypoint; once the body gives the type, it must be the
// list of that type (still reason "beta"). count_tokens carries neither
// metadata nor system, so its checks end at the key order.
func Verify(head Head, body []byte) (reason, detail string, ok bool) {
	count, d := checkLine(head)
	if d != "" {
		return ReasonQuery, d, false
	}
	for _, hd := range head.Headers {
		if strings.EqualFold(hd.Name, HeaderTransferEncoding) {
			return ReasonTransferEncoding, fmt.Sprintf("%s: %s present", hd.Name, hd.Value), false
		}
	}
	kind := CredentialOAuth
	if _, ok := head.Get(HeaderAuthorization); !ok {
		if _, ok := head.Get(HeaderAPIKey); !ok {
			return ReasonHeaderMissing, fmt.Sprintf("missing %q or %q", HeaderAuthorization, HeaderAPIKey), false
		}
		kind = CredentialAPIKey
	}
	ua, _ := head.Get(HeaderUserAgent)
	version, ep, uaOK := parseUserAgent(ua)
	p, known := Lookup(version)
	if !known {
		p = latest()
	}
	route := TypeTurn
	if count {
		route = TypeCountTokens
	}
	expected := p.HeaderNames(route, kind)
	for _, name := range expected {
		if _, ok := head.Get(name); !ok {
			return ReasonHeaderMissing, fmt.Sprintf("missing %q", name), false
		}
	}
	if d := checkOrder(p.headerNames(route, kind, head.Headers), head.Headers); d != "" {
		return ReasonHeaderOrder, d, false
	}
	if !uaOK {
		return ReasonUserAgent, fmt.Sprintf("%q is not claude-cli/<version> (external, cli|sdk-cli)", ua), false
	}
	if !known {
		return ReasonUnknownVersion, fmt.Sprintf("no profile for Claude Code %s (profiles: %s)",
			version, strings.Join(Versions(), ", ")), false
	}
	if d := checkValues(p, head, len(body)); d != "" {
		return ReasonHeaderValue, d, false
	}
	betaHeader, _ := head.Get(HeaderAnthropicBeta)
	betas := strings.Split(betaHeader, ",")
	if d := checkBetas(p, head, kind, ep, route, betas); d != "" {
		return ReasonBeta, d, false
	}

	v, err := Parse(body)
	if err != nil {
		return ReasonBodyEncoding, err.Error(), false
	}
	if re := Marshal(v); !bytes.Equal(re, body) {
		return ReasonBodyEncoding, fmt.Sprintf("body differs from its JSON.stringify at byte %d", firstDiff(re, body)), false
	}
	t, ok := bodyType(p, count, v.Keys())
	if v.Kind() != KindObject || !ok {
		return ReasonBodyKeyOrder, fmt.Sprintf("top-level keys %v are not those of a Claude Code %s request", v.Keys(), p.Version), false
	}
	if !betaListOf(p, t, kind, ep, betas) {
		return ReasonBeta, fmt.Sprintf("%q is not the list of a %s request from %s with %s", betaHeader, t, ep, kind), false
	}
	if timeout, ok := head.Get(HeaderStainlessTimeout); ok && (timeout == p.ClassifierTimeout) != (t == TypeClassifier) {
		return ReasonHeaderValue, fmt.Sprintf("%s: %q in a %s request", HeaderStainlessTimeout, timeout, t), false
	}
	if t == TypeCountTokens {
		return "", "", true
	}

	metadata, _ := v.Get("metadata")
	userID, _ := metadata.Get("user_id")
	if !slices.Equal(metadata.Keys(), []string{"user_id"}) || userID.Kind() != KindString {
		return ReasonMetadata, "metadata is not {\"user_id\": <string>}", false
	}
	_, _, session, err := ParseUserID(userID.Str())
	if err != nil {
		return ReasonMetadata, err.Error(), false
	}
	if header, _ := head.Get(HeaderSessionID); session != header {
		return ReasonSessionMismatch, fmt.Sprintf("metadata session_id %s, %s %s", session, HeaderSessionID, header), false
	}
	if t == TypeQuota {
		return "", "", true
	}

	sys, _ := v.Get("system")
	blocks := sys.Elems()
	if len(blocks) == 0 {
		return ReasonBillingHeader, "system[0] is missing", false
	}
	text, _ := blocks[0].Get("text")
	if !slices.Equal(blocks[0].Keys(), []string{"type", "text"}) || text.Kind() != KindString {
		return ReasonBillingHeader, "system[0] is not {\"type\":\"text\",\"text\":…} without cache_control", false
	}
	if typ, _ := blocks[0].Get("type"); typ.Str() != "text" {
		return ReasonBillingHeader, "system[0].type is not \"text\"", false
	}
	// A subagent appends " cc_is_subagent=true;" whatever the destination
	// (the other optional fields of the 2.1.308 binary go only to the
	// first-party API or a gateway); it goes with x-claude-code-agent-id.
	billing, subagent := strings.CutSuffix(text.Str(), billingSubagent)
	if _, agent := head.Get(HeaderAgentID); subagent != agent {
		return ReasonBillingHeader, fmt.Sprintf("cc_is_subagent=true present: %v, %s present: %v", subagent, HeaderAgentID, agent), false
	}
	b, err := ParseBillingHeader(billing)
	if err != nil {
		return ReasonBillingHeader, err.Error(), false
	}
	if b.Version != version {
		return ReasonBillingHeader, fmt.Sprintf("cc_version %s differs from the User-Agent version %s", b.Version, version), false
	}
	if b.Entrypoint != string(ep) {
		return ReasonBillingHeader, fmt.Sprintf("cc_entrypoint %s differs from the User-Agent entrypoint %s", b.Entrypoint, ep), false
	}
	messages, _ := v.Get("messages")
	if fps := fingerprints(FingerprintTexts(messages.Elems()), version); !slices.Contains(fps, b.Fingerprint) {
		return ReasonFingerprint, fmt.Sprintf("expected %s, got %s", strings.Join(fps, " or "), b.Fingerprint), false
	}
	// The cache_control of the credential, without the 1-hour TTL when the
	// request has no extended-cache-ttl (a subagent, or a subscriber on
	// overage); none in the title.
	cc := CacheControl(kind)
	if kind == CredentialOAuth && !slices.Contains(betas, betaExtendedCacheTTL) {
		cc = CacheControl(CredentialAPIKey)
	}
	if t == TypeTitle {
		cc = Null()
	}
	if len(blocks) < 2 {
		return ReasonIdentity, "system[1] is missing", false
	}
	if t == TypeClassifier {
		return checkClassifierIdentity(p, blocks[1], cc)
	}
	want := Marshal(TextBlock(p.Identities[ep], cc))
	if got := Marshal(blocks[1]); !bytes.Equal(got, want) {
		return ReasonIdentity, fmt.Sprintf("system[1] is %s, expected %s", got, want), false
	}
	return "", "", true
}

// checkClassifierIdentity checks system[1] of the auto-mode classifier: a
// text block with the cache_control cc whose text starts with the
// classifier's first sentence.
func checkClassifierIdentity(p *Profile, block, cc Value) (reason, detail string, ok bool) {
	text, _ := block.Get("text")
	if !bytes.Equal(Marshal(block), Marshal(TextBlock(text.Str(), cc))) || !strings.HasPrefix(text.Str(), p.ClassifierIdentity) {
		return ReasonIdentity, fmt.Sprintf("system[1] of the classifier is not a text block starting with %q with cache_control %s",
			p.ClassifierIdentity, Marshal(cc)), false
	}
	return "", "", true
}

// billingSubagent ends the billing header of a subagent request.
const billingSubagent = " cc_is_subagent=true;"

// checkLine checks "POST <path>?beta=true HTTP/1.1" with path ending in
// /v1/messages or /v1/messages/count_tokens.
func checkLine(h Head) (count bool, detail string) {
	path, query, _ := splitTarget(h.Target)
	count = strings.HasSuffix(path, PathCountTokens)
	if h.Method != "POST" || h.Proto != "HTTP/1.1" || query != "beta=true" ||
		(!count && !strings.HasSuffix(path, PathMessages)) {
		return count, fmt.Sprintf("expected \"POST <path>?beta=true HTTP/1.1\", got %q", h.Method+" "+h.Target+" "+h.Proto)
	}
	return count, ""
}

// checkOrder compares the header names with the profile.
func checkOrder(expected []string, got []Header) string {
	for i, name := range expected {
		if i >= len(got) {
			return fmt.Sprintf("expected %q at position %d, got the end of the head", name, i+1)
		}
		if got[i].Name != name {
			return fmt.Sprintf("expected %q at position %d, got %q", name, i+1, got[i].Name)
		}
	}
	if len(got) > len(expected) {
		return fmt.Sprintf("unexpected %q at position %d", got[len(expected)].Name, len(expected)+1)
	}
	return ""
}

// checkValues checks the fixed and formatted header values.
func checkValues(p *Profile, h Head, bodyLen int) string {
	for _, hd := range h.Headers {
		var allowed []string
		switch hd.Name {
		case HeaderAccept:
			allowed = []string{p.Accept}
		case HeaderContentType:
			allowed = []string{p.ContentType}
		case HeaderStainlessArch:
			allowed = p.Archs
		case HeaderStainlessLang:
			allowed = []string{p.Lang}
		case HeaderStainlessOS:
			allowed = p.OSes
		case HeaderStainlessPackageVersion:
			allowed = []string{p.PackageVersion}
		case HeaderStainlessRuntime:
			allowed = []string{p.Runtime}
		case HeaderStainlessRuntimeVersion:
			allowed = []string{p.RuntimeVersion}
		case HeaderStainlessTimeout:
			allowed = []string{p.Timeout, p.FallbackTimeout, p.ClassifierTimeout}
		case HeaderDangerousDirectBrowserAccess:
			allowed = []string{p.DirectBrowser}
		case HeaderAnthropicVersion:
			allowed = []string{p.AnthropicVersion}
		case HeaderApp:
			allowed = []string{p.App}
		case HeaderConnection:
			allowed = []string{p.Connection}
		case HeaderAcceptEncoding:
			allowed = []string{p.AcceptEncoding}
		case HeaderContentLength:
			allowed = []string{strconv.Itoa(bodyLen)}
		case HeaderStainlessRetryCount:
			if !isDecimal(hd.Value) {
				return fmt.Sprintf("%s: %q is not a decimal integer", hd.Name, hd.Value)
			}
			continue
		case HeaderSessionID:
			if !IsUUIDv4(hd.Value) {
				return fmt.Sprintf("%s: %q is not a lowercase UUID v4", hd.Name, hd.Value)
			}
			continue
		case HeaderAgentID, HeaderParentAgentID:
			if !isAgentID(hd.Value) {
				return fmt.Sprintf("%s: %q is not a Claude Code agent id", hd.Name, hd.Value)
			}
			continue
		default: // Authorization, User-Agent, anthropic-beta, x-api-key, Host
			continue
		}
		if !slices.Contains(allowed, hd.Value) {
			return fmt.Sprintf("%s: expected %q, got %q", hd.Name, strings.Join(allowed, `" or "`), hd.Value)
		}
	}
	return ""
}

// checkBetas checks the beta list before the body is read.
func checkBetas(p *Profile, h Head, kind CredentialKind, ep Entrypoint, route Type, betas []string) string {
	prev := -1
	for _, b := range betas {
		i := p.betaIndex(b)
		if i < 0 {
			return fmt.Sprintf("unknown beta %q", b)
		}
		if i <= prev {
			return fmt.Sprintf("%q is out of the canonical order (or repeated)", b)
		}
		prev = i
	}
	if !slices.Contains(betas, betaClaudeCode) {
		return "missing " + betaClaudeCode
	}
	auth, _ := h.Get(HeaderAuthorization)
	bearer := kind == CredentialOAuth && strings.HasPrefix(auth, "Bearer sk-ant-oat")
	hasOAuth := slices.Contains(betas, betaOAuth)
	if hasOAuth && !bearer {
		return betaOAuth + " without Authorization: Bearer sk-ant-oat…"
	}
	if !hasOAuth && bearer {
		return "Authorization: Bearer sk-ant-oat… without " + betaOAuth
	}
	types := []Type{TypeTurn, TypeTurnSafeguards, TypeQuota, TypeTitle, TypeClassifier}
	if route == TypeCountTokens {
		types = []Type{TypeCountTokens}
	}
	for _, t := range types {
		if betaListOf(p, t, kind, ep, betas) {
			return ""
		}
	}
	return fmt.Sprintf("%q is not the list of any Claude Code %s request from %s with %s",
		strings.Join(betas, ","), p.Version, ep, kind)
}

// betaListOf reports whether betas is the list of type t (with or without
// the tool-search beta), compared without the flag betas of t and the
// credential (their position was checked against the canonical order
// before). A turn without safeguards may carry the list of
// turn_safeguards: the safeguards go only with the request of a user turn,
// the betas stay for the whole session (the turn after a tool result).
func betaListOf(p *Profile, t Type, kind CredentialKind, ep Entrypoint, betas []string) bool {
	types := []Type{t}
	if t == TypeTurn {
		types = append(types, TypeTurnSafeguards)
	}
	for _, lt := range types {
		strip := func(list []string) []string {
			return slices.DeleteFunc(slices.Clone(list), func(b string) bool { return p.flagBeta(b, lt, kind) })
		}
		got := strip(betas)
		for _, toolSearch := range []bool{false, true} {
			if list, ok := p.BetaList(lt, kind, ep, toolSearch); ok && slices.Equal(strip(list), got) {
				return true
			}
		}
	}
	return false
}

// isAgentID reports whether s is a Claude Code agent id: "a", an optional
// label of [A-Za-z0-9_-] (1–63) and "-", then 16 lowercase hex (p_ of the
// 2.1.308 binary: a${label}-${hex} or a${hex}, hex = randomBytes(8)).
func isAgentID(s string) bool {
	rest, ok := strings.CutPrefix(s, "a")
	if !ok || len(rest) < 16 {
		return false
	}
	hex, label := rest[len(rest)-16:], rest[:len(rest)-16]
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	if label == "" {
		return true
	}
	label, ok = strings.CutSuffix(label, "-")
	if !ok || label == "" || len(label) > 63 {
		return false
	}
	for _, c := range label {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func firstDiff(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}
