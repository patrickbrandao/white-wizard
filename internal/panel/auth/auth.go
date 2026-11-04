// Package auth is the wwz-panel authentication (spec 016, "Autenticação,
// papéis e sessão"; ADR 0023): argon2id passwords in PHC format, signed
// session cookies backed by the sessions table with sliding expiration, a
// per-session CSRF token, OIDC login (authorization code + PKCE, state and
// nonce, ID token verified by go-oidc), the WWZ_PANEL_API_TOKEN bearer, the
// viewer/operator role middleware and the login/logout/me handlers reused by
// the JSON API (task 049) and the HTML screens (task 050).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Cookie names and well-known values.
const (
	SessionCookie = "wwz_panel_session"
	oidcCookie    = "wwz_panel_oidc"
	CSRFHeader    = "X-CSRF-Token"
	CSRFField     = "csrf"

	// SourceAPIToken is Identity.Source for the WWZ_PANEL_API_TOKEN bearer.
	SourceAPIToken = "api_token"
	AuthSession    = "session"
	AuthBearer     = "bearer"

	// LoginLimit attempts per LoginWindow per client IP (spec 016).
	LoginLimit  = 10
	LoginWindow = time.Minute

	sessionIDLen = 32
	csrfLen      = 32
	// touchEvery limits the sliding-expiration writes to one per session
	// per interval.
	touchEvery = 30 * time.Second
)

// Options configures New.
type Options struct {
	Store  *store.Store   // required
	Config *config.Config // required: Session, PublicURL, APIToken, OIDC
	Logger *slog.Logger   // nil = slog.Default()
	// Now is the clock (nil = time.Now). Keep it equal to the store clock.
	Now func() time.Time
	// Hasher hashes the dummy password used to equalize login timing for
	// unknown users (zero value = spec 016 parameters).
	Hasher Argon2Hasher
	// HTTPClient talks to the OIDC issuer (nil = client with a 10 s timeout).
	HTTPClient *http.Client
	// ClientIP extracts the client address (nil = host of RemoteAddr).
	ClientIP func(*http.Request) string
}

// Service authenticates panel requests.
type Service struct {
	st       *store.Store
	cfg      *config.Config
	log      *slog.Logger
	now      func() time.Time
	hasher   Argon2Hasher
	client   *http.Client
	clientIP func(*http.Request) string
	key      []byte
	limiter  *limiter
	oidc     *oidcState

	dummyOnce sync.Once
	dummyHash string
}

// New builds the service. The HMAC key comes from Config.Session.Key or,
// when empty, from settings (generated and stored on first boot).
func New(ctx context.Context, o Options) (*Service, error) {
	if o.Store == nil || o.Config == nil {
		return nil, errors.New("auth: Store e Config são obrigatórios")
	}
	s := &Service{
		st: o.Store, cfg: o.Config, log: o.Logger, now: o.Now, hasher: o.Hasher,
		client: o.HTTPClient, clientIP: o.ClientIP,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 10 * time.Second}
	}
	if s.clientIP == nil {
		s.clientIP = remoteIP
	}
	s.limiter = newLimiter(LoginLimit, LoginWindow)
	key, err := s.sessionKey(ctx)
	if err != nil {
		return nil, err
	}
	s.key = key
	if o.Config.OIDC.Enabled() {
		s.oidc = &oidcState{}
	}
	return s, nil
}

// sessionKey returns the configured key or the one kept in settings.
func (s *Service) sessionKey(ctx context.Context) ([]byte, error) {
	if k := s.cfg.Session.Key; k != "" {
		return []byte(k), nil
	}
	raw, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	v, err := s.st.Settings.SetIfAbsent(ctx, store.SettingSessionKey, base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		return nil, fmt.Errorf("auth: chave de sessão: %w", err)
	}
	key, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(key) < config.MinSessionKey {
		return nil, fmt.Errorf("auth: chave de sessão inválida em settings.%s", store.SettingSessionKey)
	}
	return key, nil
}

// SyncUsers inserts or refreshes the panel.yaml users (source yaml). Users
// of another source with the same name are left untouched and logged
// (spec 016). Run it after store.Bootstrap.
func (s *Service) SyncUsers(ctx context.Context, users []config.User) error {
	for _, u := range users {
		res, err := s.st.Users.UpsertYAML(ctx, u.Name, u.Role, u.PasswordHash)
		if err != nil {
			return fmt.Errorf("auth: usuário %q do panel.yaml: %w", u.Name, err)
		}
		if res == store.SyncConflict {
			s.log.Warn("painel: usuário do panel.yaml ignorado, nome já usado por outra origem", "user", u.Name)
		}
	}
	return nil
}

// secure reports whether cookies must carry the Secure attribute.
func (s *Service) secure(r *http.Request) bool {
	return strings.HasPrefix(s.cfg.PublicURL, "https://") || r.TLS != nil ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return nil, fmt.Errorf("auth: aleatório: %w", err)
	}
	return b, nil
}

func randomToken(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenEqual compares secrets in constant time (hashing first so lengths
// do not leak).
func tokenEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return hmacEqual(ha[:], hb[:])
}
