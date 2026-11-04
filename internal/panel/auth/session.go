package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// ErrUnauthenticated means the request carries no valid credential.
var ErrUnauthenticated = errors.New("auth: não autenticado")

// Identity is the authenticated principal of a request, used for
// authorization and audit (audit.user = Name, audit.role = Role).
type Identity struct {
	UserID    int64  // 0 for the API token
	Name      string // users.name or store.APITokenUser
	Role      string // viewer | operator
	Source    string // bootstrap | yaml | oidc | api_token
	Auth      string // session | bearer
	CSRF      string // session CSRF token ("" with bearer)
	SessionID string // sha256 hex of the cookie id ("" with bearer)
	ExpiresAt time.Time
}

// Me is the JSON body of GET /api/v1/me and POST /api/v1/login
// (cmd/wwz-panel/openapi.yaml, schema Me).
type Me struct {
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Source    string     `json:"source"`
	Auth      string     `json:"auth"`
	CSRF      string     `json:"csrf,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Me returns the contract view of the identity.
func (i *Identity) Me() Me {
	m := Me{Name: i.Name, Role: i.Role, Source: i.Source, Auth: i.Auth, CSRF: i.CSRF}
	if !i.ExpiresAt.IsZero() {
		t := i.ExpiresAt.UTC()
		m.ExpiresAt = &t
	}
	return m
}

// CanAct reports whether the identity may run actions (operator).
func (i *Identity) CanAct() bool { return i.Role == store.RoleOperator }

type ctxKey struct{}

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity put by the middleware.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok && id != nil
}

// sign returns base64url(HMAC-SHA256(key, data)).
func (s *Service) sign(data []byte) string {
	m := hmac.New(sha256.New, s.key)
	m.Write(data)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// encodeSigned returns base64url(data) + "." + base64url(HMAC).
func (s *Service) encodeSigned(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data) + "." + s.sign(data)
}

// decodeSigned checks the HMAC of an encodeSigned value.
func (s *Service) decodeSigned(v string) ([]byte, bool) {
	body, mac, ok := strings.Cut(v, ".")
	if !ok {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	return data, hmacEqual([]byte(s.sign(data)), []byte(mac))
}

func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }

// Authenticate resolves the credential of r: the API token bearer or the
// session cookie (sliding its expiration). No or invalid credential returns
// ErrUnauthenticated; other errors come from the store.
func (s *Service) Authenticate(r *http.Request) (*Identity, error) {
	// Only the Bearer scheme is a panel credential; another scheme (a
	// basic-auth proxy in front of the panel) falls through to the cookie.
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if s.cfg.APIToken == "" || !tokenEqual(strings.TrimSpace(tok), s.cfg.APIToken) {
			return nil, ErrUnauthenticated
		}
		return &Identity{Name: store.APITokenUser, Role: store.RoleOperator, Source: SourceAPIToken, Auth: AuthBearer}, nil
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	raw, ok := s.decodeSigned(c.Value)
	if !ok || len(raw) != sessionIDLen {
		return nil, ErrUnauthenticated
	}
	ctx := r.Context()
	sess, err := s.st.Sessions.GetActive(ctx, store.HashSessionID(raw))
	var u store.User
	if err == nil {
		u, err = s.st.Users.Get(ctx, sess.UserID)
	}
	if err == nil && u.Disabled {
		err = ErrUnauthenticated
	}
	if err != nil {
		return nil, unauth(err)
	}
	// Sliding expiration, written at most once per touchEvery. A failed
	// write does not invalidate a session that was just found active.
	if now := s.now(); now.Sub(sess.LastSeenAt) >= touchEvery &&
		s.st.Sessions.Touch(ctx, sess.ID, s.cfg.Session.TTL) == nil {
		sess.ExpiresAt = now.Add(s.cfg.Session.TTL)
	}
	return &Identity{
		UserID: u.ID, Name: u.Name, Role: u.Role, Source: u.Source, Auth: AuthSession,
		CSRF: sess.CSRF, SessionID: sess.ID, ExpiresAt: sess.ExpiresAt,
	}, nil
}

// unauth maps store.ErrNotFound to ErrUnauthenticated.
func unauth(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnauthenticated
	}
	return err
}

// StartSession opens a session for u, sets the cookie on w and returns the
// identity. Disabled users are refused with ErrUnauthenticated.
func (s *Service) StartSession(w http.ResponseWriter, r *http.Request, u store.User) (*Identity, error) {
	if u.Disabled {
		return nil, ErrUnauthenticated
	}
	raw, err := randomBytes(sessionIDLen)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(csrfLen)
	if err != nil {
		return nil, err
	}
	now := s.now()
	sess, err := s.st.Sessions.Create(r.Context(), store.Session{
		ID: store.HashSessionID(raw), UserID: u.ID, CSRF: csrf,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.cfg.Session.TTL),
		IP: s.clientIP(r), UserAgent: r.UserAgent(),
	})
	if err == nil {
		err = s.st.Users.TouchLogin(r.Context(), u.ID)
	}
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, s.cookie(r, SessionCookie, s.encodeSigned(raw), "/", 0))
	return &Identity{
		UserID: u.ID, Name: u.Name, Role: u.Role, Source: u.Source, Auth: AuthSession,
		CSRF: csrf, SessionID: sess.ID, ExpiresAt: sess.ExpiresAt,
	}, nil
}

// EndSession revokes the session of id (no-op for the bearer) and clears
// the cookie.
func (s *Service) EndSession(w http.ResponseWriter, r *http.Request, id *Identity) error {
	if id.Auth != AuthSession {
		return nil
	}
	if err := s.st.Sessions.Revoke(r.Context(), id.SessionID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	http.SetCookie(w, s.cookie(r, SessionCookie, "", "/", -1))
	return nil
}

// cookie builds a panel cookie: HttpOnly, SameSite=Lax and Secure under
// https (spec 016; plain http is allowed for local panels).
func (s *Service) cookie(r *http.Request, name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124: Secure depends on public_url/TLS/X-Forwarded-Proto (spec 016)
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secure(r),
	}
}
