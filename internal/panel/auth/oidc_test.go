package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

var (
	keysOnce          sync.Once
	idpKey, otherKey  *rsa.PrivateKey
	errKeyGeneration  error
	b64               = base64.RawURLEncoding
	defaultTestClaims = map[string]any{"email": "bia@exemplo", "groups": []any{"devs", "ops"}}
)

func testKeys(t *testing.T) {
	t.Helper()
	keysOnce.Do(func() {
		if idpKey, errKeyGeneration = rsa.GenerateKey(rand.Reader, 2048); errKeyGeneration == nil {
			otherKey, errKeyGeneration = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if errKeyGeneration != nil {
		t.Fatal(errKeyGeneration)
	}
}

type grant struct {
	challenge, nonce string
	claims           map[string]any
}

// fakeIdP is a local OpenID provider: discovery, JWKS, and a token endpoint
// that checks PKCE and returns an RS256 ID token signed in the test.
type fakeIdP struct {
	t           *testing.T
	srv         *httptest.Server
	clk         *clock
	mu          sync.Mutex
	codes       map[string]grant
	discoveries int
	tokenStatus int             // != 0: token endpoint answers this status
	omitIDToken bool            // token response without id_token
	signWith    *rsa.PrivateKey // nil = idpKey
	nonce       string          // != "": overrides the nonce in the ID token
}

func newIdP(t *testing.T, clk *clock) *fakeIdP {
	t.Helper()
	testKeys(t)
	p := &fakeIdP{t: t, clk: clk, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.discoveries++
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64.EncodeToString(idpKey.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(idpKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	g, found := p.codes[r.PostForm.Get("code")]
	status, omit, key, nonce := p.tokenStatus, p.omitIDToken, p.signWith, p.nonce
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if status == 0 && (!found || b64.EncodeToString(sum[:]) != g.challenge) {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	if nonce == "" {
		nonce = g.nonce
	}
	now := p.clk.now()
	claims := map[string]any{"iss": p.srv.URL, "aud": "wwz-panel", "sub": "u-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": nonce}
	for k, v := range g.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	if key == nil {
		key = idpKey
	}
	resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
	if !omit {
		resp["id_token"] = signJWT(p.t, key, claims)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64.EncodeToString(sig)
}

func (p *fakeIdP) issue(challenge, nonce string, claims map[string]any) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	code := fmt.Sprintf("code-%d", len(p.codes)+1)
	p.codes[code] = grant{challenge: challenge, nonce: nonce, claims: claims}
	return code
}

// oidcEnv is an env with OIDC pointing at a fake IdP.
func oidcEnv(t *testing.T, mut func(*config.OIDC)) (*env, *fakeIdP) {
	t.Helper()
	idp := newIdP(t, nil)
	e := newEnv(t, func(c *config.Config, o *Options) {
		c.OIDC = config.OIDC{
			Issuer: idp.srv.URL, ClientID: "wwz-panel", ClientSecret: "s3cr3t",
			Scopes: []string{"openid", "email"}, UsernameClaim: "email", RoleClaim: "groups",
			OperatorValues: []string{"ops"}, DefaultRole: config.RoleViewer,
		}
		if mut != nil {
			mut(&c.OIDC)
		}
		o.HTTPClient = idp.srv.Client()
	})
	idp.clk = e.clk
	return e, idp
}

// start runs GET /auth/oidc/login and returns the flow cookie and the
// authorize URL.
func (e *env) start(next string) (*http.Cookie, url.Values) {
	e.t.Helper()
	rec := serve(e.svc.OIDCLoginHandler(), httpGet(OIDCLoginPath+"?next="+url.QueryEscape(next)))
	if rec.Code != http.StatusFound {
		e.t.Fatalf("login OIDC: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	return cookieOf(e.t, rec, oidcCookie), loc.Query()
}

func (e *env) callback(c *http.Cookie, query string) *httptest.ResponseRecorder {
	r := httpGet(OIDCCallbackPath + "?" + query)
	if c != nil {
		r.AddCookie(c)
	}
	return serve(e.svc.OIDCCallbackHandler(), r)
}

// flow runs the whole login with the given claims.
func (e *env) flow(idp *fakeIdP, claims map[string]any, next string) *httptest.ResponseRecorder {
	e.t.Helper()
	c, q := e.start(next)
	code := idp.issue(q.Get("code_challenge"), q.Get("nonce"), claims)
	return e.callback(c, url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
}

func TestOIDCLoginOperator(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	c, q := e.start("/usage")
	if c.Path != "/auth/oidc" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 600 {
		t.Fatalf("cookie do fluxo: %+v", c)
	}
	if q.Get("client_id") != "wwz-panel" || q.Get("redirect_uri") != "http://painel.test/auth/oidc/callback" ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" ||
		q.Get("nonce") == "" || q.Get("scope") != "openid email" {
		t.Fatalf("authorize URL: %v", q)
	}
	code := idp.issue(q.Get("code_challenge"), q.Get("nonce"), defaultTestClaims)
	rec := e.callback(c, url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/usage" {
		t.Fatalf("callback: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if fc := cookieOf(t, rec, oidcCookie); fc.MaxAge >= 0 {
		t.Fatal("cookie do fluxo não apagado")
	}
	sc := cookieOf(t, rec, SessionCookie)
	u, err := e.st.Users.GetByOIDCSubject(ctx, idp.srv.URL+"|u-1")
	if err != nil || u.Name != "bia@exemplo" || u.Role != store.RoleOperator || u.Source != store.SourceOIDC || u.PasswordHash != "" {
		t.Fatalf("usuário OIDC: %+v, %v", u, err)
	}
	r := httpGet("/api/v1/me")
	r.AddCookie(sc)
	if me := problemOrMe(t, serve(e.svc.MeHandler(), r)); me.Role != "operator" || me.Source != "oidc" {
		t.Fatalf("me = %+v", me)
	}

	// Role recomputed at every login; the change revokes the old session.
	rec = e.flow(idp, map[string]any{"email": "bia@exemplo", "groups": "devs"}, "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("segundo login: %d %s", rec.Code, rec.Body)
	}
	if u, _ = e.st.Users.GetByOIDCSubject(ctx, idp.srv.URL+"|u-1"); u.Role != store.RoleViewer {
		t.Fatalf("papel não recalculado: %s", u.Role)
	}
	r = httpGet("/api/v1/me")
	r.AddCookie(sc)
	wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusUnauthorized, "unauthorized")
	if idp.discoveries != 1 {
		t.Fatalf("discovery repetida: %d", idp.discoveries)
	}
}

func TestOIDCRoleMapping(t *testing.T) {
	strict := func(o *config.OIDC) { o.ViewerValues = []string{"wwz-viewers"}; o.DefaultRole = config.RoleNone }
	e, idp := oidcEnv(t, strict)
	rec := e.flow(idp, map[string]any{"email": "a@x", "groups": []any{"wwz-viewers", 7}}, "/")
	if rec.Code != http.StatusFound {
		t.Fatalf("viewer por viewer_values: %d %s", rec.Code, rec.Body)
	}
	if u, _ := e.st.Users.GetByName(ctx, "a@x"); u.Role != store.RoleViewer {
		t.Fatalf("papel = %s", u.Role)
	}
	wantProblem(t, e.flow(idp, map[string]any{"email": "b@x", "sub": "u-2", "groups": "outros"}, "/"), http.StatusForbidden, "forbidden")

	cases := []struct {
		o      config.OIDC
		claims map[string]any
		role   string
		ok     bool
	}{
		{config.OIDC{RoleClaim: "g", OperatorValues: []string{"ops"}, DefaultRole: "viewer"}, map[string]any{"g": "ops"}, "operator", true},
		{config.OIDC{RoleClaim: "g", OperatorValues: []string{"ops"}, DefaultRole: "viewer"}, map[string]any{}, "viewer", true},
		{config.OIDC{RoleClaim: "g", OperatorValues: []string{"ops"}, DefaultRole: "none"}, map[string]any{"g": 1}, "", false},
		{config.OIDC{RoleClaim: "g", ViewerValues: []string{"v"}, DefaultRole: "none"}, map[string]any{"g": []any{"v"}}, "viewer", true},
	}
	for i, tc := range cases {
		if role, found := OIDCRole(tc.o, tc.claims); role != tc.role || found != tc.ok {
			t.Errorf("caso %d: %q %v", i, role, found)
		}
	}
}

func TestOIDCDisabled(t *testing.T) {
	e := newEnv(t, nil)
	wantProblem(t, serve(e.svc.OIDCLoginHandler(), httpGet(OIDCLoginPath)), http.StatusNotFound, "not_found")
	wantProblem(t, serve(e.svc.OIDCCallbackHandler(), httpGet(OIDCCallbackPath)), http.StatusNotFound, "not_found")
}

func TestOIDCDiscoveryFailure(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	e := newEnv(t, func(c *config.Config, _ *Options) {
		c.OIDC = config.OIDC{Issuer: down.URL, ClientID: "wwz-panel", UsernameClaim: "email", RoleClaim: "groups", DefaultRole: "viewer"}
	})
	wantProblem(t, serve(e.svc.OIDCLoginHandler(), httpGet(OIDCLoginPath)), http.StatusBadGateway, "upstream_unavailable")
	flow, _ := json.Marshal(oidcFlow{State: "st", Exp: e.clk.now().Add(time.Minute).Unix(), Next: "/"})
	c := &http.Cookie{Name: oidcCookie, Value: e.svc.encodeSigned(flow)}
	wantProblem(t, e.callback(c, "code=x&state=st"), http.StatusBadGateway, "upstream_unavailable")
}

func TestOIDCLoginRandFailure(t *testing.T) {
	e, _ := oidcEnv(t, nil)
	failRand(t, 0)
	wantProblem(t, serve(e.svc.OIDCLoginHandler(), httpGet(OIDCLoginPath)), http.StatusInternalServerError, "internal")
}

func TestOIDCCallbackBadFlow(t *testing.T) {
	e, _ := oidcEnv(t, nil)
	c, q := e.start("/")
	state := q.Get("state")
	notJSON := &http.Cookie{Name: oidcCookie, Value: e.svc.encodeSigned([]byte("x"))}
	tampered := &http.Cookie{Name: oidcCookie, Value: c.Value + "x"}

	wantProblem(t, e.callback(c, "error=access_denied&error_description=nao"), http.StatusBadRequest, "invalid")
	wantProblem(t, e.callback(nil, "code=x&state="+state), http.StatusBadRequest, "invalid")
	wantProblem(t, e.callback(tampered, "code=x&state="+state), http.StatusBadRequest, "invalid")
	wantProblem(t, e.callback(notJSON, "code=x&state="+state), http.StatusBadRequest, "invalid")
	wantProblem(t, e.callback(c, "code=x&state=outro"), http.StatusBadRequest, "invalid")
	wantProblem(t, e.callback(c, "code=x"), http.StatusBadRequest, "invalid")
	e.clk.add(oidcFlowTTL)
	wantProblem(t, e.callback(c, "code=x&state="+state), http.StatusBadRequest, "invalid")
}

func TestOIDCTokenFailures(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	t.Run("pkce errado", func(t *testing.T) {
		c, q := e.start("/")
		code := idp.issue("outro-challenge", q.Get("nonce"), defaultTestClaims)
		wantProblem(t, e.callback(c, url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()), http.StatusBadGateway, "upstream_unavailable")
	})
	set := func(f func()) { idp.mu.Lock(); f(); idp.mu.Unlock() }
	t.Run("token 500", func(t *testing.T) {
		set(func() { idp.tokenStatus = http.StatusInternalServerError })
		defer set(func() { idp.tokenStatus = 0 })
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusBadGateway, "upstream_unavailable")
	})
	t.Run("sem id_token", func(t *testing.T) {
		set(func() { idp.omitIDToken = true })
		defer set(func() { idp.omitIDToken = false })
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusBadGateway, "upstream_unavailable")
	})
	t.Run("assinatura errada", func(t *testing.T) {
		set(func() { idp.signWith = otherKey })
		defer set(func() { idp.signWith = nil })
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusBadRequest, "invalid")
	})
	t.Run("audience errada", func(t *testing.T) {
		wantProblem(t, e.flow(idp, map[string]any{"email": "a@x", "aud": "outro"}, "/"), http.StatusBadRequest, "invalid")
	})
	t.Run("expirado", func(t *testing.T) {
		wantProblem(t, e.flow(idp, map[string]any{"email": "a@x", "exp": e.clk.now().Add(-time.Minute).Unix()}, "/"), http.StatusBadRequest, "invalid")
	})
	t.Run("nonce errado", func(t *testing.T) {
		set(func() { idp.nonce = "outro" })
		defer set(func() { idp.nonce = "" })
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusBadRequest, "invalid")
	})
	t.Run("sem claim de nome", func(t *testing.T) {
		rec := e.flow(idp, map[string]any{"groups": "ops"}, "/")
		wantProblem(t, rec, http.StatusBadRequest, "invalid")
		if !strings.Contains(rec.Body.String(), "email") {
			t.Fatalf("mensagem sem a claim: %s", rec.Body)
		}
	})
}

func TestOIDCUserConflicts(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	e.user("ana@x", store.RoleOperator, "pw")
	wantProblem(t, e.flow(idp, map[string]any{"email": "ana@x", "groups": "ops"}, "/"), http.StatusForbidden, "forbidden")
	if u, _ := e.st.Users.GetByName(ctx, "ana@x"); u.Source != store.SourceYAML {
		t.Fatal("usuário local sobrescrito")
	}

	u, err := e.st.Users.UpsertOIDC(ctx, idp.srv.URL+"|u-9", "off@x", store.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Users.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.flow(idp, map[string]any{"email": "off@x", "sub": "u-9"}, "/"), http.StatusForbidden, "forbidden")
}

func TestOIDCStoreFailures(t *testing.T) {
	t.Run("upsert", func(t *testing.T) {
		e, idp := oidcEnv(t, nil)
		e.sqlExec(`CREATE TRIGGER boom BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'boom'); END`)
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusInternalServerError, "internal")
	})
	t.Run("sessão", func(t *testing.T) {
		e, idp := oidcEnv(t, nil)
		e.sqlExec(`CREATE TRIGGER boom BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'boom'); END`)
		wantProblem(t, e.flow(idp, defaultTestClaims, "/"), http.StatusInternalServerError, "internal")
	})
}
