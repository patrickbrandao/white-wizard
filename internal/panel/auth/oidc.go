package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// OIDC routes and the lifetime of the state cookie.
const (
	OIDCLoginPath    = "/auth/oidc/login"
	OIDCCallbackPath = "/auth/oidc/callback"
	oidcCookiePath   = "/auth/oidc"
	oidcFlowTTL      = 10 * time.Minute
)

// oidcState holds the provider, discovered lazily on the first login so the
// panel boots even with the issuer down (the login answers 502).
type oidcState struct {
	mu       sync.Mutex
	conf     *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// oidcFlow is the signed content of the short-lived wwz_panel_oidc cookie.
type oidcFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

// clientCtx makes oauth2 and go-oidc use the configured HTTP client.
func (s *Service) clientCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, s.client)
}

// provider returns the oauth2 config and the ID token verifier, running the
// discovery once it succeeds.
func (s *Service) provider() (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	st := s.oidc
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.conf != nil {
		return st.conf, st.verifier, nil
	}
	o := s.cfg.OIDC
	// Background context: the key set keeps it for later JWKS refreshes.
	p, err := oidc.NewProvider(s.clientCtx(context.Background()), o.Issuer)
	if err != nil {
		return nil, nil, err
	}
	st.conf = &oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  strings.TrimRight(s.cfg.PublicURL, "/") + OIDCCallbackPath,
		Scopes:       o.Scopes,
	}
	st.verifier = p.Verifier(&oidc.Config{ClientID: o.ClientID, Now: s.now})
	return st.conf, st.verifier, nil
}

func (s *Service) oidcDisabled(w http.ResponseWriter) bool {
	if s.oidc == nil {
		WriteProblem(w, http.StatusNotFound, "not_found", "OIDC não configurado")
		return true
	}
	return false
}

// OIDCLoginHandler is GET /auth/oidc/login: stores state, nonce, the PKCE
// verifier and next in a signed cookie and redirects (302) to the issuer.
// 404 without OIDC; 502 when the discovery fails.
func (s *Service) OIDCLoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.oidcDisabled(w) {
			return
		}
		conf, _, err := s.provider()
		if err != nil {
			s.log.Warn("painel: discovery OIDC", "issuer", s.cfg.OIDC.Issuer, "err", err)
			WriteProblem(w, http.StatusBadGateway, "upstream_unavailable", "provedor OIDC indisponível")
			return
		}
		var f oidcFlow
		if f.State, err = randomToken(32); err == nil {
			f.Nonce, err = randomToken(32)
		}
		if err != nil {
			s.internal(w, "OIDC", err)
			return
		}
		f.Verifier = oauth2.GenerateVerifier()
		f.Next = SafeNext(r.URL.Query().Get("next"))
		f.Exp = s.now().Add(oidcFlowTTL).Unix()
		data, _ := json.Marshal(f)
		http.SetCookie(w, s.cookie(r, oidcCookie, s.encodeSigned(data), oidcCookiePath, int(oidcFlowTTL/time.Second)))
		http.Redirect(w, r, conf.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
	})
}

// readFlow validates the state cookie against the state parameter.
func (s *Service) readFlow(r *http.Request) (oidcFlow, bool) {
	var f oidcFlow
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		return f, false
	}
	data, ok := s.decodeSigned(c.Value)
	if !ok || json.Unmarshal(data, &f) != nil {
		return f, false
	}
	state := r.URL.Query().Get("state")
	return f, state != "" && s.now().Unix() < f.Exp && tokenEqual(state, f.State)
}

// OIDCCallbackHandler is GET /auth/oidc/callback: checks state, exchanges
// the code with the PKCE verifier, verifies the ID token and its nonce,
// computes the role from the claims, creates or updates the user (source
// oidc, subject iss|sub) and opens the session, redirecting (302) to next.
// 400 invalid flow or token, 403 no role/disabled/name taken, 404 without
// OIDC, 502 issuer failure.
func (s *Service) OIDCCallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.oidcDisabled(w) {
			return
		}
		http.SetCookie(w, s.cookie(r, oidcCookie, "", oidcCookiePath, -1))
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			WriteProblem(w, http.StatusBadRequest, "invalid", "login OIDC recusado pelo provedor", e, q.Get("error_description"))
			return
		}
		flow, ok := s.readFlow(r)
		if !ok {
			WriteProblem(w, http.StatusBadRequest, "invalid", "state OIDC ausente, expirado ou diferente")
			return
		}
		conf, verifier, err := s.provider()
		if err != nil {
			s.log.Warn("painel: discovery OIDC", "issuer", s.cfg.OIDC.Issuer, "err", err)
			WriteProblem(w, http.StatusBadGateway, "upstream_unavailable", "provedor OIDC indisponível")
			return
		}
		ctx := s.clientCtx(r.Context())
		tok, err := conf.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(flow.Verifier))
		if err != nil {
			s.log.Warn("painel: troca do code OIDC", "err", err)
			WriteProblem(w, http.StatusBadGateway, "upstream_unavailable", "falha ao trocar o code no provedor OIDC")
			return
		}
		raw, _ := tok.Extra("id_token").(string)
		if raw == "" {
			WriteProblem(w, http.StatusBadGateway, "upstream_unavailable", "provedor OIDC não devolveu id_token")
			return
		}
		idt, err := verifier.Verify(ctx, raw)
		if err != nil {
			WriteProblem(w, http.StatusBadRequest, "invalid", "ID token inválido", err.Error())
			return
		}
		if !tokenEqual(idt.Nonce, flow.Nonce) {
			WriteProblem(w, http.StatusBadRequest, "invalid", "nonce do ID token diferente")
			return
		}
		var claims map[string]any
		_ = idt.Claims(&claims) // the payload was already parsed by Verify
		name, _ := claims[s.cfg.OIDC.UsernameClaim].(string)
		if name == "" {
			WriteProblem(w, http.StatusBadRequest, "invalid", "claim "+s.cfg.OIDC.UsernameClaim+" ausente no ID token")
			return
		}
		role, ok := OIDCRole(s.cfg.OIDC, claims)
		if !ok {
			WriteProblem(w, http.StatusForbidden, "forbidden", "usuário OIDC sem papel no painel")
			return
		}
		u, err := s.st.Users.UpsertOIDC(r.Context(), idt.Issuer+"|"+idt.Subject, name, role)
		if errors.Is(err, store.ErrConflict) {
			WriteProblem(w, http.StatusForbidden, "forbidden", "nome "+name+" já usado por outro usuário do painel")
			return
		}
		if err != nil {
			s.internal(w, "usuário OIDC", err)
			return
		}
		if u.Disabled {
			WriteProblem(w, http.StatusForbidden, "forbidden", "usuário desativado")
			return
		}
		if _, err := s.StartSession(w, r, u); err != nil {
			s.internal(w, "sessão", err)
			return
		}
		http.Redirect(w, r, flow.Next, http.StatusFound)
	})
}

// OIDCRole maps the role claim (string or list of strings) to a panel role
// (spec 016): any operator_values match → operator; any viewer_values match
// → viewer; otherwise default_role (viewer, or none = refuse, ok false).
// With viewer_values empty and default_role viewer every authenticated user
// that is not operator becomes viewer.
func OIDCRole(o config.OIDC, claims map[string]any) (string, bool) {
	var values []string
	switch v := claims[o.RoleClaim].(type) {
	case string:
		values = []string{v}
	case []any:
		for _, e := range v {
			if str, ok := e.(string); ok {
				values = append(values, str)
			}
		}
	}
	has := func(set []string) bool {
		for _, v := range values {
			for _, w := range set {
				if v == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has(o.OperatorValues):
		return store.RoleOperator, true
	case has(o.ViewerValues), o.DefaultRole != config.RoleNone:
		return store.RoleViewer, true
	}
	return "", false
}
