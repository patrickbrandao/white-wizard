package auth

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

func TestNewRequiresStoreAndConfig(t *testing.T) {
	if _, err := New(ctx, Options{}); err == nil {
		t.Fatal("esperado erro sem Store/Config")
	}
}

func TestNewDefaults(t *testing.T) {
	e := newEnv(t, nil)
	s, err := New(ctx, Options{Store: e.st, Config: e.cfg})
	if err != nil {
		t.Fatal(err)
	}
	if s.log == nil || s.now == nil || s.client == nil || s.clientIP == nil || s.oidc != nil {
		t.Fatal("padrões não preenchidos")
	}
}

func TestSessionKeyGeneratedAndPersisted(t *testing.T) {
	e := newEnv(t, nil)
	v, found, err := e.st.Settings.Get(ctx, store.SettingSessionKey)
	if err != nil || !found {
		t.Fatalf("chave não guardada: %v %v", found, err)
	}
	if k, _ := base64.RawURLEncoding.DecodeString(v); len(k) != 32 {
		t.Fatalf("chave com %d bytes", len(k))
	}
	e.user("ana", store.RoleOperator, "pw")
	c, _ := e.login("ana", "pw")
	// A second service on the same database reuses the key: the cookie stays valid.
	s2, err := New(ctx, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	r := jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(c)
	if rec := serve(s2.MeHandler(), r); rec.Code != http.StatusOK {
		t.Fatalf("cookie recusado após reinício: %d", rec.Code)
	}
}

func TestSessionKeyConfigured(t *testing.T) {
	key := strings.Repeat("k", 32)
	e := newEnv(t, func(c *config.Config, _ *Options) { c.Session.Key = key })
	if string(e.svc.key) != key {
		t.Fatal("chave configurada ignorada")
	}
	if _, found, _ := e.st.Settings.Get(ctx, store.SettingSessionKey); found {
		t.Fatal("chave configurada não deve ir para settings")
	}
}

func TestSessionKeyErrors(t *testing.T) {
	e := newEnv(t, nil)
	t.Run("rand", func(t *testing.T) {
		_ = e.st.Settings.Delete(ctx, store.SettingSessionKey)
		failRand(t, 0)
		if _, err := New(ctx, e.opts); err == nil {
			t.Fatal("esperado erro de aleatório")
		}
	})
	t.Run("invalida", func(t *testing.T) {
		_ = e.st.Settings.Set(ctx, store.SettingSessionKey, "curta")
		if _, err := New(ctx, e.opts); err == nil {
			t.Fatal("esperado erro de chave inválida")
		}
	})
	t.Run("store", func(t *testing.T) {
		_ = e.st.Close()
		if _, err := New(ctx, e.opts); err == nil {
			t.Fatal("esperado erro do store")
		}
	})
}

func TestSyncUsers(t *testing.T) {
	e := newEnv(t, nil)
	h, _ := fastHasher.Hash("pw")
	if _, err := e.st.Users.Create(ctx, store.User{Name: "admin", Role: store.RoleOperator, Source: store.SourceBootstrap, PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	users := []config.User{{Name: "ana", Role: store.RoleViewer, PasswordHash: h}, {Name: "admin", Role: store.RoleViewer, PasswordHash: h}}
	if err := e.svc.SyncUsers(ctx, users); err != nil {
		t.Fatal(err)
	}
	ana, err := e.st.Users.GetByName(ctx, "ana")
	if err != nil || ana.Source != store.SourceYAML || ana.Role != store.RoleViewer {
		t.Fatalf("ana = %+v, %v", ana, err)
	}
	admin, _ := e.st.Users.GetByName(ctx, "admin")
	if admin.Source != store.SourceBootstrap || admin.Role != store.RoleOperator {
		t.Fatalf("bootstrap sobrescrito: %+v", admin)
	}
	if !strings.Contains(e.logs.String(), "admin") {
		t.Fatal("conflito não logado")
	}
	users[0].Role = store.RoleOperator
	if err := e.svc.SyncUsers(ctx, users[:1]); err != nil {
		t.Fatal(err)
	}
	if ana, _ = e.st.Users.GetByName(ctx, "ana"); ana.Role != store.RoleOperator {
		t.Fatal("papel do YAML não atualizado")
	}
	if _, me := e.login("ana", "pw"); me.Name != "ana" {
		t.Fatal("login com hash do YAML falhou")
	}
	_ = e.st.Close()
	if err := e.svc.SyncUsers(ctx, users); err == nil {
		t.Fatal("esperado erro do store")
	}
}

func TestLoginJSONAndMe(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleOperator, "pw")
	rec := serve(e.svc.LoginHandler(), jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "pw"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	c := cookieOf(t, rec, SessionCookie)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Fatalf("atributos do cookie: %+v", c)
	}
	body, mac, found := strings.Cut(c.Value, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	if !found || len(raw) != 32 || mac == "" {
		t.Fatalf("formato do cookie: %q", c.Value)
	}
	sess, err := e.st.Sessions.Get(ctx, store.HashSessionID(raw))
	if err != nil || sess.CSRF == "" || sess.IP != "192.0.2.1" || !sess.ExpiresAt.Equal(e.clk.now().Add(time.Hour)) {
		t.Fatalf("sessão no banco: %+v, %v", sess, err)
	}
	if u, _ := e.st.Users.GetByName(ctx, "ana"); u.LastLoginAt.IsZero() {
		t.Fatal("last_login_at não gravado")
	}
	me := problemOrMe(t, rec)
	if me.Name != "ana" || me.Role != "operator" || me.Source != "yaml" || me.Auth != "session" || me.CSRF != sess.CSRF || me.ExpiresAt == nil {
		t.Fatalf("Me = %+v", me)
	}
	r := jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(c)
	if got := problemOrMe(t, serve(e.svc.MeHandler(), r)); got.CSRF != sess.CSRF {
		t.Fatalf("me = %+v", got)
	}
}

func problemOrMe(t *testing.T, rec interface{ Result() *http.Response }) Me {
	t.Helper()
	var me Me
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if err := decodeJSON(res, &me); err != nil {
		t.Fatal(err)
	}
	return me
}

func TestSecureCookie(t *testing.T) {
	cases := map[string]func(*http.Request, *config.Config){
		"public_url https":  func(_ *http.Request, c *config.Config) { c.PublicURL = "https://painel.test" },
		"x-forwarded-proto": func(r *http.Request, _ *config.Config) { r.Header.Set("X-Forwarded-Proto", "HTTPS") },
		"tls":               func(r *http.Request, _ *config.Config) { r.TLS = &tls.ConnectionState{} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.user("ana", store.RoleViewer, "pw")
			r := jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "pw"})
			mut(r, e.cfg)
			if c := cookieOf(t, serve(e.svc.LoginHandler(), r), SessionCookie); !c.Secure {
				t.Fatal("cookie sem Secure")
			}
		})
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleOperator, "pw")
	off := e.user("off", store.RoleOperator, "pw")
	e.sqlExec(fmt.Sprintf("UPDATE users SET disabled = 1 WHERE id = %d", off.ID))
	if _, err := e.st.Users.Create(ctx, store.User{Name: "oidc", Role: store.RoleViewer, Source: store.SourceOIDC, OIDCSubject: "i|s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Users.UpsertYAML(ctx, "torto", store.RoleViewer, "$argon2id$lixo"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, pw string
	}{
		{"senha errada", "ana", "x"},
		{"desconhecido", "zé", "pw"},
		{"desativado", "off", "pw"},
		{"oidc sem senha", "oidc", "pw"},
		{"hash inválido", "torto", "pw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{tc.user, tc.pw})
			r.RemoteAddr = "198.51.100.1:1" + fmt.Sprint(len(tc.name))
			rec := serve(e.svc.LoginHandler(), r)
			wantProblem(t, rec, http.StatusUnauthorized, "unauthorized")
			if len(rec.Result().Cookies()) != 0 {
				t.Fatal("cookie emitido em falha")
			}
		})
	}
	if !strings.Contains(e.logs.String(), "torto") {
		t.Fatal("hash inválido não logado")
	}
	bad := jsonReq(http.MethodPost, "/api/v1/login", nil)
	bad.Body = http.NoBody
	wantProblem(t, serve(e.svc.LoginHandler(), bad), http.StatusBadRequest, "invalid")
	wantProblem(t, serve(e.svc.LoginHandler(), jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", ""})), http.StatusBadRequest, "invalid")
}

func TestLoginForm(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	for _, tc := range []struct {
		name string
		v    url.Values
		loc  string
	}{
		{"sucesso", url.Values{"username": {"ana"}, "password": {"pw"}, "next": {"/usage?w=1h"}}, "/usage?w=1h"},
		{"next externo", url.Values{"username": {"ana"}, "password": {"pw"}, "next": {"//evil.test"}}, "/"},
		{"senha errada", url.Values{"username": {"ana"}, "password": {"x"}}, "/login?error=1"},
		{"campos vazios", url.Values{}, "/login?error=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(e.svc.LoginHandler(), formReq("/api/v1/login", tc.v))
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.loc {
				t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
			}
		})
	}
}

func TestLoginOrigin(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	for origin, want := range map[string]int{
		"http://example.com":  http.StatusOK, // httptest host
		"http://painel.test":  http.StatusOK, // public_url
		"http://evil.test":    http.StatusForbidden,
		"null":                http.StatusForbidden,
		"http://%zz":          http.StatusForbidden,
		"https://evil.test:1": http.StatusForbidden,
	} {
		r := jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "pw"})
		r.Header.Set("Origin", origin)
		rec := serve(e.svc.LoginHandler(), r)
		if rec.Code != want {
			t.Errorf("Origin %q: %d, want %d", origin, rec.Code, want)
		}
		if want == http.StatusForbidden {
			wantProblem(t, rec, want, "forbidden")
		}
	}
	e.cfg.PublicURL = ""
	r := formReq("/api/v1/login", url.Values{"username": {"ana"}, "password": {"pw"}})
	r.Header.Set("Origin", "http://painel.test")
	wantProblem(t, serve(e.svc.LoginHandler(), r), http.StatusForbidden, "forbidden")
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	attempt := func(ip string, form bool) int {
		var r *http.Request
		if form {
			r = formReq("/api/v1/login", url.Values{"username": {"ana"}, "password": {"x"}})
		} else {
			r = jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "x"})
		}
		r.RemoteAddr = ip + ":4000"
		rec := serve(e.svc.LoginHandler(), r)
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") != "60" {
				t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
			}
			wantProblem(t, rec, rec.Code, "too_many_requests")
		}
		if form && rec.Header().Get("Location") == "/login?error=rate" {
			return http.StatusTooManyRequests
		}
		return rec.Code
	}
	for i := 0; i < LoginLimit; i++ {
		if c := attempt("203.0.113.9", false); c != http.StatusUnauthorized {
			t.Fatalf("tentativa %d: %d", i+1, c)
		}
	}
	if c := attempt("203.0.113.9", false); c != http.StatusTooManyRequests {
		t.Fatalf("11ª tentativa: %d", c)
	}
	if c := attempt("203.0.113.9", true); c != http.StatusTooManyRequests {
		t.Fatalf("formulário limitado: %d", c)
	}
	if c := attempt("203.0.113.10", false); c != http.StatusUnauthorized {
		t.Fatalf("outro IP limitado: %d", c)
	}
	e.clk.add(LoginWindow)
	if c := attempt("203.0.113.9", false); c != http.StatusUnauthorized {
		t.Fatalf("janela não reiniciou: %d", c)
	}
}

func TestLimiterSweep(t *testing.T) {
	l := newLimiter(1, time.Minute)
	now := time.Unix(0, 0)
	for i := 0; i < maxBuckets; i++ {
		l.allow(fmt.Sprint(i), now)
	}
	l.allow("novo", now.Add(time.Minute))
	if len(l.hits) != 1 {
		t.Fatalf("varredura deixou %d entradas", len(l.hits))
	}
}

func TestLoginInternalErrors(t *testing.T) {
	t.Run("start session", func(t *testing.T) {
		e := newEnv(t, nil)
		e.user("ana", store.RoleViewer, "pw")
		failRand(t, 0)
		wantProblem(t, serve(e.svc.LoginHandler(), jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "pw"})), http.StatusInternalServerError, "internal")
	})
	t.Run("store", func(t *testing.T) {
		e := newEnv(t, nil)
		_ = e.st.Close()
		wantProblem(t, serve(e.svc.LoginHandler(), jsonReq(http.MethodPost, "/api/v1/login", LoginRequest{"ana", "pw"})), http.StatusInternalServerError, "internal")
	})
}

func TestSessionExpiryAndSliding(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	c, _ := e.login("ana", "pw")
	me := func() int {
		r := jsonReq(http.MethodGet, "/api/v1/me", nil)
		r.AddCookie(c)
		return serve(e.svc.MeHandler(), r).Code
	}
	e.clk.add(10 * time.Second) // below touchEvery: no write
	if me() != http.StatusOK {
		t.Fatal("sessão recusada")
	}
	raw, _ := e.svc.decodeSigned(c.Value)
	if s, _ := e.st.Sessions.Get(ctx, store.HashSessionID(raw)); !s.ExpiresAt.Equal(e.clk.now().Add(-10 * time.Second).Add(time.Hour)) {
		t.Fatalf("expiração mudou antes de touchEvery: %v", s.ExpiresAt)
	}
	e.clk.add(40 * time.Minute)
	if me() != http.StatusOK {
		t.Fatal("sessão recusada aos 40 min")
	}
	e.clk.add(40 * time.Minute) // 80 min after login, 40 after the slide
	if me() != http.StatusOK {
		t.Fatal("expiração não deslizou")
	}
	e.clk.add(61 * time.Minute)
	wantProblem(t, func() *httpRec {
		r := jsonReq(http.MethodGet, "/api/v1/me", nil)
		r.AddCookie(c)
		return serve(e.svc.MeHandler(), r)
	}(), http.StatusUnauthorized, "unauthorized")
}

func TestLogoutRevokes(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	c, me := e.login("ana", "pw")

	r := jsonReq(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(c)
	wantProblem(t, serve(e.svc.LogoutHandler(), r), http.StatusForbidden, "csrf")

	r = jsonReq(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(c)
	r.Header.Set(CSRFHeader, "outro")
	wantProblem(t, serve(e.svc.LogoutHandler(), r), http.StatusForbidden, "csrf")

	r = jsonReq(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(c)
	r.Header.Set(CSRFHeader, me.CSRF)
	rec := serve(e.svc.LogoutHandler(), r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if cl := cookieOf(t, rec, SessionCookie); cl.MaxAge >= 0 || cl.Value != "" {
		t.Fatalf("cookie não apagado: %+v", cl)
	}
	r = jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(c)
	wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusUnauthorized, "unauthorized")
}

func TestLogoutFormAndBearer(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	c, me := e.login("ana", "pw")
	r := formReq("/logout", url.Values{CSRFField: {me.CSRF}})
	r.AddCookie(c)
	if rec := serve(e.svc.LogoutHandler(), r); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != LoginPage {
		t.Fatalf("logout por formulário: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	r = jsonReq(http.MethodPost, "/api/v1/logout", nil)
	r.Header.Set("Authorization", "Bearer tok-123")
	if rec := serve(e.svc.LogoutHandler(), r); rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("logout com bearer: %d", rec.Code)
	}
}

func TestLogoutRevokeFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	c, me := e.login("ana", "pw")
	e.sqlExec(`CREATE TRIGGER boom BEFORE UPDATE OF revoked ON sessions BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	r := jsonReq(http.MethodPost, "/api/v1/logout", nil)
	r.AddCookie(c)
	r.Header.Set(CSRFHeader, me.CSRF)
	wantProblem(t, serve(e.svc.LogoutHandler(), r), http.StatusInternalServerError, "internal")
}

func TestBearer(t *testing.T) {
	e := newEnv(t, nil)
	r := jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer tok-123")
	me := problemOrMe(t, serve(e.svc.MeHandler(), r))
	if me.Name != store.APITokenUser || me.Role != "operator" || me.Source != "api_token" || me.Auth != "bearer" || me.CSRF != "" || me.ExpiresAt != nil {
		t.Fatalf("Me = %+v", me)
	}
	// Bearer skips CSRF and acts as operator.
	r = jsonReq(http.MethodPost, "/api/v1/entities/key/x/block", nil)
	r.Header.Set("Authorization", "Bearer tok-123")
	if rec := serve(e.svc.RequireRole(store.RoleOperator)(ok), r); rec.Code != http.StatusOK {
		t.Fatalf("ação com bearer: %d", rec.Code)
	}
	for _, h := range []string{"Bearer errado", "Basic dG9rLTEyMw==", "Bearer "} {
		r := jsonReq(http.MethodGet, "/api/v1/me", nil)
		r.Header.Set("Authorization", h)
		wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusUnauthorized, "unauthorized")
	}
	// Another scheme does not hide the session cookie.
	e.user("ana", store.RoleViewer, "pw")
	c, _ := e.login("ana", "pw")
	r = jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Basic dG9rLTEyMw==")
	r.AddCookie(c)
	if got := problemOrMe(t, serve(e.svc.MeHandler(), r)); got.Name != "ana" {
		t.Fatalf("cookie ignorado com Authorization Basic: %+v", got)
	}
	e.cfg.APIToken = ""
	r = jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer ")
	wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusUnauthorized, "unauthorized")
}

func TestBadCookies(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	good, _ := e.login("ana", "pw")
	body, _, _ := strings.Cut(good.Value, ".")
	for name, v := range map[string]string{
		"sem ponto":       body,
		"hmac errado":     body + ".AAAA",
		"base64 inválido": "!!!." + e.svc.sign([]byte("x")),
		"tamanho errado":  e.svc.encodeSigned([]byte("curto")),
		"desconhecida":    e.svc.encodeSigned(make([]byte, 32)),
		"outra chave":     (&Service{key: []byte(strings.Repeat("z", 32))}).encodeSigned(make([]byte, 32)),
	} {
		r := jsonReq(http.MethodGet, "/api/v1/me", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: v})
		if rec := serve(e.svc.MeHandler(), r); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestDisabledUserSessionRejected(t *testing.T) {
	e := newEnv(t, nil)
	u := e.user("ana", store.RoleViewer, "pw")
	c, _ := e.login("ana", "pw")
	e.sqlExec(fmt.Sprintf("UPDATE users SET disabled = 1 WHERE id = %d", u.ID))
	r := jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(c)
	wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusUnauthorized, "unauthorized")
}

func TestAuthenticateStoreError(t *testing.T) {
	e := newEnv(t, nil)
	e.user("ana", store.RoleViewer, "pw")
	c, _ := e.login("ana", "pw")
	_ = e.st.Close()
	r := jsonReq(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(c)
	wantProblem(t, serve(e.svc.MeHandler(), r), http.StatusInternalServerError, "internal")
}

func TestRequireRole(t *testing.T) {
	e := newEnv(t, nil)
	e.user("vi", store.RoleViewer, "pw")
	e.user("op", store.RoleOperator, "pw")
	vc, vme := e.login("vi", "pw")
	oc, ome := e.login("op", "pw")
	action := e.svc.RequireRole(store.RoleOperator)(ok)
	read := e.svc.RequireRole(store.RoleViewer)(ok)

	req := func(method string, c *http.Cookie, csrf string) *http.Request {
		r := jsonReq(method, "/api/v1/x", nil)
		r.AddCookie(c)
		if csrf != "" {
			r.Header.Set(CSRFHeader, csrf)
		}
		return r
	}
	wantProblem(t, serve(action, req(http.MethodPost, vc, vme.CSRF)), http.StatusForbidden, "forbidden")
	wantProblem(t, serve(action, req(http.MethodGet, vc, "")), http.StatusForbidden, "forbidden")
	wantProblem(t, serve(action, req(http.MethodPost, vc, "")), http.StatusForbidden, "csrf")
	wantProblem(t, serve(action, req(http.MethodDelete, oc, "")), http.StatusForbidden, "csrf")
	if rec := serve(action, req(http.MethodPost, oc, ome.CSRF)); rec.Code != http.StatusOK {
		t.Fatalf("operator com CSRF: %d", rec.Code)
	}
	if rec := serve(read, req(http.MethodGet, vc, "")); rec.Code != http.StatusOK {
		t.Fatalf("viewer lendo: %d", rec.Code)
	}
	if rec := serve(read, req(http.MethodHead, vc, "")); rec.Code != http.StatusOK {
		t.Fatalf("HEAD sem CSRF: %d", rec.Code)
	}
	wantProblem(t, serve(read, jsonReq(http.MethodGet, "/api/v1/x", nil)), http.StatusUnauthorized, "unauthorized")

	// CSRF in a multipart form field.
	r := httpMultipart(t, "/x", map[string]string{CSRFField: ome.CSRF})
	r.AddCookie(oc)
	if rec := serve(action, r); rec.Code != http.StatusOK {
		t.Fatalf("CSRF em multipart: %d", rec.Code)
	}
}

func TestRequirePageRoleAndOptional(t *testing.T) {
	e := newEnv(t, nil)
	e.user("vi", store.RoleViewer, "pw")
	c, _ := e.login("vi", "pw")
	page := e.svc.RequirePageRole(store.RoleViewer, "/login")(ok)
	rec := serve(page, httpGet("/usage?w=1h"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2Fusage%3Fw%3D1h" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	r := httpGet("/usage")
	r.AddCookie(c)
	if rec := serve(page, r); rec.Code != http.StatusOK {
		t.Fatalf("página com sessão: %d", rec.Code)
	}
	opt := e.svc.Optional(ok)
	if rec := serve(opt, httpGet("/login")); rec.Code != http.StatusTeapot {
		t.Fatalf("Optional sem sessão: %d", rec.Code)
	}
	r = httpGet("/login")
	r.AddCookie(c)
	if rec := serve(opt, r); rec.Code != http.StatusOK {
		t.Fatalf("Optional com sessão: %d", rec.Code)
	}
}

func TestStartAndEndSessionEdges(t *testing.T) {
	e := newEnv(t, nil)
	u := e.user("ana", store.RoleViewer, "pw")
	off := u
	off.Disabled = true
	if _, err := e.svc.StartSession(newRec(), httpGet("/"), off); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("usuário desativado: %v", err)
	}
	t.Run("csrf rand", func(t *testing.T) {
		failRand(t, 1)
		if _, err := e.svc.StartSession(newRec(), httpGet("/"), u); err == nil {
			t.Fatal("esperado erro no token CSRF")
		}
	})
	e.sqlExec(`CREATE TRIGGER boom BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if _, err := e.svc.StartSession(newRec(), httpGet("/"), u); err == nil {
		t.Fatal("esperado erro do store")
	}
	if err := e.svc.EndSession(newRec(), httpGet("/"), &Identity{Auth: AuthSession, SessionID: "inexistente"}); err != nil {
		t.Fatalf("sessão inexistente: %v", err)
	}
	if err := e.svc.EndSession(newRec(), httpGet("/"), &Identity{Auth: AuthBearer}); err != nil {
		t.Fatal(err)
	}
}

func TestSmallHelpers(t *testing.T) {
	r := httpGet("/")
	r.RemoteAddr = "sem-porta"
	if remoteIP(r) != "sem-porta" {
		t.Fatal("remoteIP sem porta")
	}
	if _, found := FromContext(ctx); found {
		t.Fatal("identidade em contexto vazio")
	}
	if _, found := FromContext(WithIdentity(ctx, nil)); found {
		t.Fatal("identidade nil aceita")
	}
	for in, want := range map[string]string{
		"/usage": "/usage", "": "/", "usage": "/", "//evil": "/", "/\\evil": "/", "/a\r\nb": "/", "https://x": "/",
	} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
	failRand(t, 0)
	if _, err := randomToken(8); err == nil {
		t.Fatal("randomToken sem erro")
	}
}
