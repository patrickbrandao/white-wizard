package auth

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// LoginPage is where form logins are sent back on failure.
const LoginPage = "/login"

const maxLoginBody = 64 << 10

// LoginRequest is the body of POST /api/v1/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SafeNext returns next when it is a relative path of the panel ("/..."
// but not "//" or "/\"), otherwise "/".
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

// originAllowed checks the Origin of a sessionless POST: absent (non-browser
// client) or equal to the request host or the public_url host.
func (s *Service) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	p, err := url.Parse(s.cfg.PublicURL)
	return err == nil && p.Host != "" && strings.EqualFold(u.Host, p.Host)
}

// LoginHandler is POST /api/v1/login (local login, argon2id). JSON body:
// 200 with Me and the session cookie; 400, 401, 403 (Origin) and 429 as
// Problem. Form body (page /login, fields username, password, next): 303 to
// next (or /) on success and to /login?error=1 on failure (error=rate when
// limited); a foreign Origin is always 403.
func (s *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		form := isForm(r)
		fail := func(status int, code, msg string) {
			if form {
				q := "1"
				if status == http.StatusTooManyRequests {
					q = "rate"
				}
				http.Redirect(w, r, LoginPage+"?error="+q, http.StatusSeeOther)
				return
			}
			WriteProblem(w, status, code, msg)
		}
		if !s.originAllowed(r) {
			WriteProblem(w, http.StatusForbidden, "forbidden", "Origin diferente do host do painel")
			return
		}
		if ok, wait := s.limiter.allow(s.clientIP(r), s.now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			fail(http.StatusTooManyRequests, "too_many_requests", "muitas tentativas de login; aguarde")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
		var req LoginRequest
		next := "/"
		if form {
			req.Username, req.Password = r.PostFormValue("username"), r.PostFormValue("password")
			next = SafeNext(r.PostFormValue("next"))
		} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "invalid", "corpo JSON inválido")
			return
		}
		if req.Username == "" || req.Password == "" {
			fail(http.StatusBadRequest, "invalid", "username e password são obrigatórios")
			return
		}
		u, err := s.checkPassword(r, req.Username, req.Password)
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				fail(http.StatusUnauthorized, "unauthorized", "usuário ou senha inválidos")
				return
			}
			s.internal(w, "login", err)
			return
		}
		id, err := s.StartSession(w, r, u)
		if err != nil {
			s.internal(w, "sessão", err)
			return
		}
		if form {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		writeJSON(w, http.StatusOK, id.Me())
	})
}

// checkPassword returns the user when name/password are valid. Unknown,
// disabled or passwordless users cost one argon2id run too, so timing does
// not reveal which names exist.
func (s *Service) checkPassword(r *http.Request, name, password string) (store.User, error) {
	u, err := s.st.Users.GetByName(r.Context(), name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}
	hash := u.PasswordHash
	if err != nil || hash == "" || u.Disabled {
		_, _ = Verify(password, s.dummy())
		return store.User{}, ErrUnauthenticated
	}
	ok, verr := Verify(password, hash)
	if verr != nil {
		s.log.Warn("painel: hash de senha inválido", "user", name)
	}
	if !ok {
		return store.User{}, ErrUnauthenticated
	}
	return u, nil
}

// dummy is a hash with the configured parameters used for unknown users.
func (s *Service) dummy() string {
	s.dummyOnce.Do(func() {
		s.dummyHash, _ = s.hasher.Hash("wwz-panel-dummy")
	})
	return s.dummyHash
}

func (s *Service) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error("painel: "+what, "err", err)
	WriteProblem(w, http.StatusInternalServerError, "internal", "erro interno")
}

// LogoutHandler is POST /api/v1/logout, already wrapped by
// RequireRole(viewer) (CSRF included): revokes the session and clears the
// cookie; 204 (303 to /login for a form post). With the bearer it does
// nothing (204).
func (s *Service) LogoutHandler() http.Handler {
	return s.RequireRole(store.RoleViewer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		if err := s.EndSession(w, r, id); err != nil {
			s.internal(w, "logout", err)
			return
		}
		if isForm(r) {
			http.Redirect(w, r, LoginPage, http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// MeHandler is GET /api/v1/me, wrapped by RequireRole(viewer): 200 with Me
// (including the CSRF token of the session).
func (s *Service) MeHandler() http.Handler {
	return s.RequireRole(store.RoleViewer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		writeJSON(w, http.StatusOK, id.Me())
	}))
}
