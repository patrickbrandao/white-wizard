package claudecode

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrUnknownVersion is wrapped when a version has no [Profile].
var ErrUnknownVersion = errors.New("no Claude Code profile for version")

// Request holds the inputs of one Claude Code request. [Build] turns them
// into the exact bytes; [Extract] recovers them from a captured request.
type Request struct {
	Version    string     // profile version, e.g. "2.1.308"
	Entrypoint Entrypoint // sdk-cli (claude -p) or cli (interactive)
	Type       Type
	Credential Credential
	Prefix     string // path of ANTHROPIC_BASE_URL (the driver mount), "" at the root
	Host       string // Host header

	SessionID   string // X-Claude-Code-Session-Id and metadata session_id (UUID v4)
	DeviceID    string // 64 lowercase hex
	AccountUUID string // "" with CLAUDE_CODE_OAUTH_TOKEN or an API key
	RetryCount  int    // X-Stainless-Retry-Count
	Arch        string // X-Stainless-Arch; "" means arm64
	OS          string // X-Stainless-OS; "" means MacOS

	Model string
	// Messages are sent as given (turn, turn_safeguards, title,
	// count_tokens); the quota probe always sends the profile's message.
	// The caller builds them with the cache_control of the credential
	// ([CacheControl]), the environment block (role "system") and the
	// per-message output_config, as Claude Code does.
	Messages     []Value
	SystemPrompt string  // system[2] of turn, turn_safeguards and title
	Tools        []Value // tools (turn, turn_safeguards, title, count_tokens)
	ToolSearch   bool    // the ToolSearch tool is on: adds the tool-search beta to the turns
	Safeguards   Value   // safeguards of turn_safeguards
	Effort       string  // output_config.effort; "" means the profile default
	OutputFormat Value   // output_config.format of title; null leaves it out
	NonStream    bool    // non-streaming fallback of a turn (max_tokens and timeout of the fallback)
}

// Build returns the head (request line, headers and the empty line) and
// the body Claude Code sends for r, byte for byte.
func Build(r Request) (head, body []byte, err error) {
	p, ok := Lookup(r.Version)
	if !ok {
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownVersion, r.Version)
	}
	if r.Type == TypeHello {
		return buildHello(p, r), nil, nil
	}
	if r.Type == TypeClassifier {
		return nil, nil, fmt.Errorf("profile %s: the %s request is verified, not built", p.Version, r.Type)
	}
	if r.Entrypoint != EntrypointSDKCLI && r.Entrypoint != EntrypointCLI {
		return nil, nil, fmt.Errorf("unknown entrypoint %q", r.Entrypoint)
	}
	if r.Credential.Kind != CredentialOAuth && r.Credential.Kind != CredentialAPIKey {
		return nil, nil, errors.New("unknown credential kind")
	}
	betas, ok := p.BetaList(r.Type, r.Credential.Kind, r.Entrypoint, r.ToolSearch)
	if !ok {
		return nil, nil, fmt.Errorf("profile %s: no %q request from %s with %s",
			p.Version, r.Type, r.Entrypoint, r.Credential.Kind)
	}
	body = Marshal(buildBody(p, r))
	return buildHead(p, r, betas, len(body)), body, nil
}

func buildHello(p *Profile, r Request) []byte {
	h := Head{Method: "HEAD", Target: r.Prefix + PathHello, Proto: "HTTP/1.1"}
	for _, name := range p.HeaderNames(TypeHello, 0) {
		var v string
		switch name {
		case HeaderConnection:
			v = p.Connection
		case HeaderUserAgent:
			v = "Bun/" + p.BunVersion
		case HeaderAccept:
			v = p.HelloAccept
		case HeaderHost:
			v = r.Host
		default: // Accept-Encoding
			v = p.AcceptEncoding
		}
		h.Headers = append(h.Headers, Header{Name: name, Value: v})
	}
	return h.Append(nil)
}

func buildHead(p *Profile, r Request, betas []string, bodyLen int) []byte {
	path := PathMessages
	if r.Type == TypeCountTokens {
		path = PathCountTokens
	}
	h := Head{Method: "POST", Target: r.Prefix + path + "?beta=true", Proto: "HTTP/1.1"}
	for _, name := range p.HeaderNames(r.Type, r.Credential.Kind) {
		h.Headers = append(h.Headers, Header{Name: name, Value: headerValue(p, r, name, betas, bodyLen)})
	}
	return h.Append(nil)
}

func headerValue(p *Profile, r Request, name string, betas []string, bodyLen int) string {
	switch name {
	case HeaderAccept:
		return p.Accept
	case HeaderAuthorization:
		return "Bearer " + r.Credential.Secret
	case HeaderContentType:
		return p.ContentType
	case HeaderUserAgent:
		return p.UserAgent(r.Entrypoint)
	case HeaderSessionID:
		return r.SessionID
	case HeaderStainlessArch:
		return orDefault(r.Arch, p.Archs[0])
	case HeaderStainlessLang:
		return p.Lang
	case HeaderStainlessOS:
		return orDefault(r.OS, p.OSes[0])
	case HeaderStainlessPackageVersion:
		return p.PackageVersion
	case HeaderStainlessRetryCount:
		return strconv.Itoa(r.RetryCount)
	case HeaderStainlessRuntime:
		return p.Runtime
	case HeaderStainlessRuntimeVersion:
		return p.RuntimeVersion
	case HeaderStainlessTimeout:
		if r.NonStream {
			return p.FallbackTimeout
		}
		return p.Timeout
	case HeaderAnthropicBeta:
		return strings.Join(betas, ",")
	case HeaderDangerousDirectBrowserAccess:
		return p.DirectBrowser
	case HeaderAnthropicVersion:
		return p.AnthropicVersion
	case HeaderAPIKey:
		return r.Credential.Secret
	case HeaderApp:
		return p.App
	case HeaderConnection:
		return p.Connection
	case HeaderHost:
		return r.Host
	case HeaderAcceptEncoding:
		return p.AcceptEncoding
	default: // Content-Length
		return strconv.Itoa(bodyLen)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func buildBody(p *Profile, r Request) Value {
	model := M("model", String(r.Model))
	metadata := M("metadata", Object(M("user_id", String(UserID(r.DeviceID, r.AccountUUID, r.SessionID)))))
	messages := M("messages", Array(r.Messages...))
	tools := M("tools", Array(r.Tools...))
	effort := M("effort", String(orDefault(r.Effort, p.DefaultEffort)))
	switch r.Type {
	case TypeQuota:
		return Object(model,
			M("max_tokens", Int(p.QuotaMaxTokens)),
			M("messages", Array(Object(M("role", String("user")), M("content", String(p.QuotaContent))))),
			metadata)
	case TypeCountTokens:
		ms := []Member{model, messages, tools}
		if hasThinking(r.Messages) {
			ms = append(ms, M("thinking", Object(M("type", String("adaptive")))))
		}
		return Object(ms...)
	case TypeTitle:
		oc := []Member{effort}
		if r.OutputFormat.Kind() != KindNull {
			oc = append(oc, M("format", r.OutputFormat))
		}
		return Object(model, messages,
			M("system", system(p, r, Null())),
			tools, metadata,
			M("max_tokens", Int(p.TurnMaxTokens)),
			M("output_config", Object(oc...)),
			M("stream", Bool(true)))
	}
	maxTokens := p.TurnMaxTokens
	if r.NonStream {
		maxTokens = p.FallbackMaxTokens
	}
	ms := []Member{
		model, messages,
		M("system", system(p, r, CacheControl(r.Credential.Kind))),
		tools, metadata,
		M("max_tokens", Int(maxTokens)),
		M("thinking", Object(M("type", String("adaptive")), M("display", String(p.ThinkingDisplay[r.Entrypoint])))),
		M("context_management", Object(M("edits", Array(Object(
			M("type", String("clear_thinking_20251015")), M("keep", String("all"))))))),
	}
	if r.Type == TypeTurnSafeguards {
		ms = append(ms, M("safeguards", r.Safeguards))
	}
	ms = append(ms, M("output_config", Object(effort)), M("stream", Bool(!r.NonStream)))
	return Object(ms...)
}

// system returns billing header, identity and system prompt blocks.
func system(p *Profile, r Request, cc Value) Value {
	return Array(
		TextBlock(BillingHeader(p.Version, r.Entrypoint, FirstUserText(r.Messages)), Null()),
		TextBlock(p.Identities[r.Entrypoint], cc),
		TextBlock(r.SystemPrompt, cc),
	)
}

// hasThinking reports whether an assistant message carries a thinking or
// redacted_thinking block (count_tokens then sends thinking).
func hasThinking(messages []Value) bool {
	for _, m := range messages {
		if role, _ := m.Get("role"); role.Str() != "assistant" {
			continue
		}
		content, _ := m.Get("content")
		for _, b := range content.Elems() {
			if t, _ := b.Get("type"); t.Str() == "thinking" || t.Str() == "redacted_thinking" {
				return true
			}
		}
	}
	return false
}

// bodyType returns the request type whose key order matches keys; count
// selects the count_tokens route.
func bodyType(p *Profile, count bool, keys []string) (Type, bool) {
	types := []Type{TypeTurn, TypeTurnSafeguards, TypeQuota, TypeTitle, TypeClassifier}
	if count {
		types = []Type{TypeCountTokens}
	}
	for _, t := range types {
		for _, order := range p.KeyOrders[t] {
			if slices.Equal(order, keys) {
				return t, true
			}
		}
	}
	return "", false
}

// parseUserAgent parses "claude-cli/<version> (external, <ep>)".
func parseUserAgent(ua string) (version string, ep Entrypoint, ok bool) {
	rest, ok := strings.CutPrefix(ua, "claude-cli/")
	if !ok {
		return "", "", false
	}
	version, rest, ok = strings.Cut(rest, " (external, ")
	if !ok || !isVersion(version) {
		return "", "", false
	}
	e, ok := strings.CutSuffix(rest, ")")
	ep = Entrypoint(e)
	if !ok || (ep != EntrypointSDKCLI && ep != EntrypointCLI) {
		return "", "", false
	}
	return version, ep, true
}

// isVersion reports whether s is <digits>.<digits>.<digits>.
func isVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !isDecimal(part) {
			return false
		}
	}
	return true
}

// isDecimal reports whether s is a non-negative decimal integer without
// leading zeros.
func isDecimal(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Extract recovers the inputs of a captured Claude Code request (head and
// body), so that [Build] can rebuild it. It reads what the request carries
// and does not check the profile; [Verify] does.
func Extract(h Head, body []byte) (Request, error) {
	path, _, _ := splitTarget(h.Target)
	host, _ := h.Get(HeaderHost)
	ua, _ := h.Get(HeaderUserAgent)
	r := Request{Host: host}
	if h.Method == "HEAD" && strings.HasSuffix(path, PathHello) {
		r.Type, r.Prefix = TypeHello, strings.TrimSuffix(path, PathHello)
		for _, v := range Versions() {
			if "Bun/"+profiles[v].BunVersion == ua {
				r.Version = v
			}
		}
		if r.Version == "" {
			return Request{}, fmt.Errorf("no profile for the probe User-Agent %q", ua)
		}
		return r, nil
	}
	count := false
	switch {
	case strings.HasSuffix(path, PathCountTokens):
		count, r.Prefix = true, strings.TrimSuffix(path, PathCountTokens)
	case strings.HasSuffix(path, PathMessages):
		r.Prefix = strings.TrimSuffix(path, PathMessages)
	default:
		return Request{}, fmt.Errorf("path %q is not a Claude Code route", path)
	}
	var ok bool
	if r.Version, r.Entrypoint, ok = parseUserAgent(ua); !ok {
		return Request{}, fmt.Errorf("User-Agent %q is not claude-cli/<version> (external, <entrypoint>)", ua)
	}
	p, ok := Lookup(r.Version)
	if !ok {
		return Request{}, fmt.Errorf("%w %q", ErrUnknownVersion, r.Version)
	}
	if auth, ok := h.Get(HeaderAuthorization); ok {
		r.Credential = Credential{Kind: CredentialOAuth, Secret: strings.TrimPrefix(auth, "Bearer ")}
	} else {
		key, _ := h.Get(HeaderAPIKey)
		r.Credential = Credential{Kind: CredentialAPIKey, Secret: key}
	}
	r.SessionID, _ = h.Get(HeaderSessionID)
	retry, _ := h.Get(HeaderStainlessRetryCount)
	r.RetryCount, _ = strconv.Atoi(retry)
	r.Arch, _ = h.Get(HeaderStainlessArch)
	r.OS, _ = h.Get(HeaderStainlessOS)
	betas, _ := h.Get(HeaderAnthropicBeta)
	r.ToolSearch = slices.Contains(strings.Split(betas, ","), p.ToolSearchBeta)

	v, err := Parse(body)
	if err != nil {
		return Request{}, err
	}
	if r.Type, ok = bodyType(p, count, v.Keys()); !ok {
		return Request{}, fmt.Errorf("body keys %v are not those of a Claude Code %s request", v.Keys(), p.Version)
	}
	model, _ := v.Get("model")
	r.Model = model.Str()
	messages, _ := v.Get("messages")
	r.Messages = messages.Elems()
	tools, _ := v.Get("tools")
	r.Tools = tools.Elems()
	if sys, _ := v.Get("system"); len(sys.Elems()) > 2 {
		text, _ := sys.Elems()[2].Get("text")
		r.SystemPrompt = text.Str()
	}
	metadata, _ := v.Get("metadata")
	userID, _ := metadata.Get("user_id")
	if uid, err := Parse([]byte(userID.Str())); err == nil {
		device, _ := uid.Get("device_id")
		account, _ := uid.Get("account_uuid")
		r.DeviceID, r.AccountUUID = device.Str(), account.Str()
	}
	if stream, ok := v.Get("stream"); ok && !stream.Truth() {
		r.NonStream = true
	}
	oc, _ := v.Get("output_config")
	effort, _ := oc.Get("effort")
	r.Effort = effort.Str()
	r.OutputFormat, _ = oc.Get("format")
	r.Safeguards, _ = v.Get("safeguards")
	return r, nil
}
