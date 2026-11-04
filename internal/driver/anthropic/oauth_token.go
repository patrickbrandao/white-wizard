package anthropic

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// The subscription (OAuth) of Claude Code (ADR 0074, spec 006 16.2.1).
// Nothing is kept in the store: the access token, the refresh token and the
// authorization code are signed with HMAC-SHA256, the secret being the
// value of the keys[] entry drivers[].options.oauth_key names, like the
// Console of the opencode driver (ADR 0072, item 17). The access token
// stands for that key on inference (ResolveKey). Layout and times are the
// wwz's (not conferred).

// The token prefixes. The client tells an OAuth token by "sk-ant-oat".
const (
	accessPrefix  = "sk-ant-oat01-"
	refreshPrefix = "sk-ant-ort01-"
	oauthMark     = "sk-ant-oat"
	refreshMark   = "sk-ant-ort"
)

// The client id, the beta of OAuth inference and the manual redirect of
// Claude Code 2.1.308 (tmp/claude-code/oauth-shapes.md).
const (
	oauthClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthBeta      = "oauth-2025-04-20"
	manualRedirect = "https://platform.claude.com/oauth/code/callback"
)

// Times the wwz chose (spec 006 16.2.1, not conferred).
const (
	codeTTL    = 600 * time.Second
	accessTTL  = 28_800 * time.Second
	refreshTTL = 30 * 24 * time.Hour
)

// The texts of the OAuth refusals on inference: the invalid token is
// conferred (real-probes h1/b1); the others are not (the client matches
// "^OAuth (?:access )?token has expired\b" to refresh).
const (
	msgTokenInvalid = "OAuth access token is invalid."                                                     //nolint:gosec // G101: an error text, not a credential
	msgTokenExpired = "OAuth token has expired. Please obtain a new token or refresh your existing token." //nolint:gosec // G101: an error text, not a credential
	msgNoBeta       = "OAuth authentication is currently not supported."
	msgScopePrefix  = "OAuth token does not meet scope requirement "
)

// The scopes, one bit each in this order (spec 006 16.2.1).
const (
	scopeCreateAPIKey = "org:create_api_key" //nolint:gosec // G101: a scope name, not a credential
	scopeProfile      = "user:profile"
	scopeInference    = "user:inference"
)

var scopeNames = [...]string{
	scopeCreateAPIKey, scopeProfile, scopeInference, "user:sessions:claude_code",
	"user:mcp_servers", "user:file_upload", "user:plugins",
}

// scopes is a set of scopes, a bit per scopeNames entry.
type scopes uint8

// parseScopes reads a space-separated scope list; false on an unknown
// scope or an empty list.
func parseScopes(list string) (scopes, bool) {
	var s scopes
	for _, name := range strings.Fields(list) {
		bit, ok := scopeBit(name)
		if !ok {
			return 0, false
		}
		s |= bit
	}
	return s, s != 0
}

func scopeBit(name string) (scopes, bool) {
	for i, n := range scopeNames {
		if n == name {
			return 1 << i, true
		}
	}
	return 0, false
}

// has reports whether s holds the scope name (a known one).
func (s scopes) has(name string) bool {
	bit, _ := scopeBit(name)
	return s&bit != 0
}

// String is the space-separated list, in scopeNames order.
func (s scopes) String() string {
	var out []string
	for i, n := range scopeNames {
		if s&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// oauthToken is what a signed token carries: its scopes, when it was
// issued and when it expires (Unix seconds) and, for an authorization
// code, the binding of its code_challenge, redirect_uri and client_id.
type oauthToken struct {
	scopes  scopes
	expires time.Time
	issued  time.Time
	binding []byte
}

const (
	tokenVersion = 1
	macSize      = 16
	bindingSize  = 16
	// tokenSize is version, scopes, expiry, issue, nonce and MAC.
	tokenSize = 1 + 1 + 8 + 8 + 8 + macSize
)

// tokenB64 is the tokens' encoding: base64url without padding.
var tokenB64 = base64.RawURLEncoding

// newOAuthToken is a token with scopes s issued at now that lives ttl.
func newOAuthToken(s scopes, now time.Time, ttl time.Duration) oauthToken {
	return oauthToken{scopes: s, issued: now, expires: now.Add(ttl)}
}

// sign is prefix + base64url(version ‖ scopes ‖ expiry ‖ issue [‖
// binding] ‖ nonce ‖ MAC), MAC = HMAC-SHA256(secret, prefix ‖ the bytes
// before it) truncated to 16 bytes. The code has no prefix.
func (t oauthToken) sign(secret, prefix string) string {
	b := make([]byte, 0, tokenSize+bindingSize)
	b = append(b, tokenVersion, byte(t.scopes))
	b = binary.BigEndian.AppendUint64(b, uint64(t.expires.Unix())) //nolint:gosec // G115: Unix seconds of now are positive
	b = binary.BigEndian.AppendUint64(b, uint64(t.issued.Unix()))  //nolint:gosec // G115: Unix seconds of now are positive
	b = append(b, t.binding...)
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	b = append(b, nonce[:]...)
	b = append(b, tokenMAC(secret, prefix, b)...)
	return prefix + tokenB64.EncodeToString(b)
}

func tokenMAC(secret, prefix string, b []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(prefix))
	m.Write(b)
	return m.Sum(nil)[:macSize]
}

// verifyOAuth checks tok, a token of prefix (a code when withBinding),
// under secret: its layout and its MAC. It does not look at the expiry.
// An empty secret verifies nothing.
func verifyOAuth(secret, prefix, tok string, withBinding bool) (oauthToken, bool) {
	body, ok := strings.CutPrefix(tok, prefix)
	if !ok || secret == "" {
		return oauthToken{}, false
	}
	b, err := tokenB64.DecodeString(body)
	size := tokenSize
	if withBinding {
		size += bindingSize
	}
	if err != nil || len(b) != size || b[0] != tokenVersion {
		return oauthToken{}, false
	}
	if !hmac.Equal(b[size-macSize:], tokenMAC(secret, prefix, b[:size-macSize])) {
		return oauthToken{}, false
	}
	t := oauthToken{
		scopes:  scopes(b[1]),
		expires: time.Unix(int64(binary.BigEndian.Uint64(b[2:10])), 0),  //nolint:gosec // G115: the MAC verified, so wwz wrote it from an int64
		issued:  time.Unix(int64(binary.BigEndian.Uint64(b[10:18])), 0), //nolint:gosec // G115: the MAC verified, so wwz wrote it from an int64
	}
	if withBinding {
		t.binding = b[18 : 18+bindingSize]
	}
	return t, true
}

// codeBinding ties an authorization code to the PKCE challenge, the
// redirect and the client: the first 16 bytes of
// sha256(code_challenge ‖ "\n" ‖ redirect_uri ‖ "\n" ‖ client_id).
func codeBinding(challenge, redirect, client string) []byte {
	sum := sha256.Sum256([]byte(challenge + "\n" + redirect + "\n" + client))
	return sum[:bindingSize]
}

// pkceChallenge is the S256 code_challenge of a code_verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return tokenB64.EncodeToString(sum[:])
}

// oauthEntry is the keys[] entry of oauth_key; nil without the option or
// the entry.
func (d *Driver) oauthEntry(keys driver.KeyLookup) *config.KeyConfig {
	if d.oauthKey == "" || keys == nil {
		return nil
	}
	return keys(d.oauthKey)
}

// oauthSecret is the value of the oauth_key entry; "" without it, which
// verifies no token.
func (d *Driver) oauthSecret(keys driver.KeyLookup) string {
	if k := d.oauthEntry(keys); k != nil {
		return k.Key
	}
	return ""
}

// accessToken verifies tok as an access token: the token, whether it
// verified and whether it is still valid at now.
func (d *Driver) accessToken(tok string, keys driver.KeyLookup, now time.Time) (t oauthToken, ok, live bool) {
	t, ok = verifyOAuth(d.oauthSecret(keys), accessPrefix, tok, false)
	return t, ok, ok && now.Before(t.expires)
}

var (
	_ driver.KeyResolver          = (*Driver)(nil)
	_ driver.TokenChecker         = (*Driver)(nil)
	_ driver.SubscriptionReporter = (*Driver)(nil)
)

// ResolveKey: an access token signed with the oauth_key's value and not
// expired stands for that key.
func (d *Driver) ResolveKey(token string, keys driver.KeyLookup, now time.Time) (string, bool) {
	if _, _, live := d.accessToken(token, keys, now); live {
		return d.oauthKey, true
	}
	return "", false
}

// CheckToken: the refusals of an OAuth credential (spec 006 16.2.1). A
// Bearer sk-ant-oat… (or a refresh token) that is no keys[] entry and
// resolved to no key is the invalid token (with or without oauth_key), or
// the expired one when it verifies; an accepted token in x-api-key is the
// invalid API key; on the inference routes an accepted Bearer token wants
// the beta oauth-2025-04-20 and the scope user:inference. Any other
// credential is left to the engine.
func (d *Driver) CheckToken(kind driver.Kind, cred driver.Credential, h driver.Headers, keys driver.KeyLookup, now time.Time) *core.Error {
	if cred.KeyLabel == "" {
		if cred.Scheme != "bearer" || !oauthLike(cred.Key) {
			return nil
		}
		if _, ok, _ := d.accessToken(cred.Key, keys, now); ok {
			return &core.Error{Kind: core.ErrInvalidAPIKey, Message: msgTokenExpired}
		}
		return &core.Error{Kind: core.ErrInvalidAPIKey, Message: msgTokenInvalid}
	}
	if cred.Scheme != "bearer" {
		return &core.Error{Kind: core.ErrInvalidAPIKey}
	}
	switch kind {
	case driver.KindMessages, driver.KindCountTokens, driver.KindModels, driver.KindModel:
	default:
		return nil
	}
	if !hasBeta(h, oauthBeta) {
		return &core.Error{Kind: core.ErrInvalidAPIKey, Message: msgNoBeta}
	}
	if t, _, _ := d.accessToken(cred.Key, keys, now); !t.scopes.has(scopeInference) {
		return &core.Error{Kind: core.ErrPermission, Message: msgScopePrefix + scopeInference}
	}
	return nil
}

// Subscription: the Messages answers to a Bearer sk-ant-oat… carry the
// anthropic-ratelimit-unified-* (with server.auth: none too).
func (d *Driver) Subscription(kind driver.Kind, cred driver.Credential) bool {
	return kind == driver.KindMessages && isOAuthBearer(cred)
}

// oauthLike reports whether key looks like an OAuth token, an access
// (sk-ant-oat…) or a refresh token (sk-ant-ort…): in a Bearer both are
// the invalid token's 401 unless they verify.
func oauthLike(key string) bool {
	return strings.HasPrefix(key, oauthMark) || strings.HasPrefix(key, refreshMark)
}

// isOAuthBearer reports whether cred is a Bearer OAuth access token.
func isOAuthBearer(cred driver.Credential) bool {
	return cred.Scheme == "bearer" && strings.HasPrefix(cred.Key, oauthMark)
}
