package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Problem is the error envelope of cmd/wwz-panel/openapi.yaml.
type Problem struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// WriteProblem writes a Problem with status.
func WriteProblem(w http.ResponseWriter, status int, code, msg string, details ...string) {
	writeJSON(w, status, Problem{Error: code, Message: msg, Details: details})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// safeMethod reports methods that never need CSRF.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// CheckCSRF reports whether r carries the CSRF token of id: always true for
// safe methods and the bearer; otherwise the X-CSRF-Token header or the
// form field csrf must equal the session token.
func CheckCSRF(r *http.Request, id *Identity) bool {
	if safeMethod(r.Method) || id.Auth != AuthSession {
		return true
	}
	tok := r.Header.Get(CSRFHeader)
	if tok == "" && isForm(r) {
		tok = r.PostFormValue(CSRFField)
	}
	return tok != "" && tokenEqual(tok, id.CSRF)
}

func isForm(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data")
}

// RequireRole is the middleware of the JSON API: it authenticates the
// request, enforces CSRF on non-GET cookie requests and the minimum role
// (viewer or operator), and puts the Identity in the context. Failures:
// 401 unauthorized, 403 csrf, 403 forbidden, 500 internal (Problem JSON).
func (s *Service) RequireRole(role string) func(http.Handler) http.Handler {
	return s.require(role, func(w http.ResponseWriter, _ *http.Request) {
		WriteProblem(w, http.StatusUnauthorized, "unauthorized", "autenticação necessária")
	})
}

// RequirePageRole is RequireRole for HTML pages: without a session it
// redirects (303) to loginPath?next=<path of the request>.
func (s *Service) RequirePageRole(role, loginPath string) func(http.Handler) http.Handler {
	return s.require(role, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

func (s *Service) require(role string, unauthorized http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := s.Authenticate(r)
			switch {
			case errors.Is(err, ErrUnauthenticated):
				unauthorized(w, r)
				return
			case err != nil:
				s.log.Error("painel: autenticação", "err", err)
				WriteProblem(w, http.StatusInternalServerError, "internal", "falha ao validar a sessão")
				return
			}
			if !CheckCSRF(r, id) {
				WriteProblem(w, http.StatusForbidden, "csrf", "token CSRF ausente ou inválido")
				return
			}
			if role == store.RoleOperator && !id.CanAct() {
				WriteProblem(w, http.StatusForbidden, "forbidden", "ação exige o papel operator")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

// Optional authenticates when a credential is present and puts the Identity
// in the context, never rejecting (pages that render for anyone, like the
// login screen). Invalid credentials are treated as absent.
func (s *Service) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, err := s.Authenticate(r); err == nil {
			r = r.WithContext(WithIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}
