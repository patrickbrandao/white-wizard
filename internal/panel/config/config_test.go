package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "panel.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fieldsOf returns the Field of every *FieldError joined in err.
func fieldsOf(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		var fe *FieldError
		if errors.As(e, &fe) && !isJoin(e) {
			out = append(out, fe.Field)
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
		}
	}
	walk(err)
	return out
}

func isJoin(e error) bool {
	_, ok := e.(interface{ Unwrap() []error })
	return ok
}

func hasField(err error, field string) bool {
	for _, f := range fieldsOf(err) {
		if f == field {
			return true
		}
	}
	return false
}

func TestDefaults(t *testing.T) {
	c, err := Load(Flags{}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Listen: DefaultListen, DB: DefaultDB, RetentionDays: 7, LogLevel: "info",
		Backup:  Backup{At: "03:00", Keep: 7},
		Valkey:  Valkey{Prefix: "wwz"},
		WWZ:     WWZ{AdminURL: DefaultAdminURL},
		Session: Session{TTL: 12 * time.Hour},
		Admin:   Admin{User: "admin"},
		OIDC: OIDC{
			Scopes: []string{"openid", "email", "profile"}, UsernameClaim: "email",
			RoleClaim: "groups", DefaultRole: RoleViewer,
		},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", c, want)
	}
	if c.Valkey.Enabled() || c.OIDC.Enabled() {
		t.Fatal("valkey/oidc should be off by default")
	}
	// No secret has a default.
	if c.Admin.Password != "" || c.Session.Key != "" || c.APIToken != "" || c.WWZ.AdminToken != "" ||
		c.Valkey.Password != "" || c.OIDC.ClientSecret != "" {
		t.Fatal("secret with default")
	}
}

const fullYAML = `
listen: ":8200"
public_url: https://painel.exemplo
db: /data/panel.db
retention_days: 3
log_level: debug
backup: {dir: /backup, at: "04:30", keep: 5}
wwz: {admin_url: "http://wwz:9000"}
valkey: {addr: ["valkey:6379"], prefix: px, cluster: true}
prometheus: {url: "http://prometheus:9090"}
session: {ttl: 2h}
users:
  - {name: ana, role: operator, password_hash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"}
oidc:
  issuer: https://login.exemplo
  client_id: wwz-panel
  scopes: [openid]
  username_claim: preferred_username
  role_claim: roles
  operator_values: [ops]
  viewer_values: [view]
  default_role: none
`

func TestYAML(t *testing.T) {
	p := writeYAML(t, fullYAML)
	c, err := Load(Flags{}, env(map[string]string{"WWZ_PANEL_CONFIG": p}))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		ConfigPath: p, Listen: ":8200", PublicURL: "https://painel.exemplo", DB: "/data/panel.db",
		RetentionDays: 3, LogLevel: "debug",
		Backup:     Backup{Dir: "/backup", At: "04:30", Keep: 5},
		Valkey:     Valkey{Addr: []string{"valkey:6379"}, Prefix: "px", Cluster: true},
		WWZ:        WWZ{AdminURL: "http://wwz:9000"},
		Prometheus: Prometheus{URL: "http://prometheus:9090"},
		Session:    Session{TTL: 2 * time.Hour},
		Admin:      Admin{User: "admin"},
		OIDC: OIDC{
			Issuer: "https://login.exemplo", ClientID: "wwz-panel", Scopes: []string{"openid"},
			UsernameClaim: "preferred_username", RoleClaim: "roles", OperatorValues: []string{"ops"},
			ViewerValues: []string{"view"}, DefaultRole: RoleNone,
		},
		Users: []User{{Name: "ana", Role: RoleOperator, PasswordHash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"}},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("yaml:\n got %+v\nwant %+v", c, want)
	}
	if !c.Valkey.Enabled() || !c.OIDC.Enabled() {
		t.Fatal("valkey/oidc should be on")
	}
}

func TestPrecedence(t *testing.T) {
	p := writeYAML(t, fullYAML)
	other := writeYAML(t, "listen: \":1\"\n")
	e := map[string]string{
		"WWZ_PANEL_CONFIG":             other, // flag wins
		"WWZ_PANEL_LISTEN":             ":9100",
		"WWZ_PANEL_PUBLIC_URL":         "http://p.local",
		"WWZ_PANEL_DB":                 "/tmp/x.db",
		"WWZ_PANEL_BACKUP_DIR":         "/b",
		"WWZ_PANEL_BACKUP_AT":          "23:59",
		"WWZ_PANEL_BACKUP_KEEP":        "9",
		"WWZ_PANEL_RETENTION_DAYS":     "30",
		"WWZ_PANEL_LOG_LEVEL":          "WARN",
		"WWZ_PANEL_VALKEY_ADDR":        "a:1, b:2,,",
		"WWZ_VALKEY_ADDR":              "ignored:1",
		"WWZ_VALKEY_PASSWORD":          "vk",
		"WWZ_PANEL_VALKEY_PREFIX":      "envp",
		"WWZ_PANEL_VALKEY_CLUSTER":     "false",
		"WWZ_PANEL_WWZ_ADMIN_URL":      "https://admin.local:9000",
		"WWZ_ADMIN_TOKEN":              "tok",
		"WWZ_PANEL_PROMETHEUS_URL":     "https://prom.local",
		"WWZ_PANEL_SESSION_KEY":        strings.Repeat("k", 32),
		"WWZ_PANEL_SESSION_TTL":        "30m",
		"WWZ_PANEL_ADMIN_USER":         "root",
		"WWZ_PANEL_ADMIN_PASSWORD":     "s3cret",
		"WWZ_PANEL_API_TOKEN":          "api",
		"WWZ_PANEL_OIDC_CLIENT_SECRET": "cs",
	}
	c, err := Load(Flags{Config: p, Listen: ":7000"}, env(e))
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigPath != p || c.Listen != ":7000" {
		t.Fatalf("flags lost: %q %q", c.ConfigPath, c.Listen)
	}
	got := []any{
		c.PublicURL, c.DB, c.Backup, c.RetentionDays, c.LogLevel, c.Valkey, c.WWZ, c.Prometheus.URL,
		c.Session.TTL, c.Admin, c.APIToken, c.OIDC.ClientSecret,
	}
	want := []any{
		"http://p.local", "/tmp/x.db",
		Backup{Dir: "/b", At: "23:59", Keep: 9},
		30, "warn",
		Valkey{Addr: []string{"a:1", "b:2"}, Password: "vk", Prefix: "envp"},
		WWZ{AdminURL: "https://admin.local:9000", AdminToken: "tok"},
		"https://prom.local",
		30 * time.Minute,
		Admin{User: "root", Password: "s3cret"},
		"api", "cs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("precedence:\n got %v\nwant %v", got, want)
	}

	// Specific variable over the shared WWZ_VALKEY_* fallback.
	c, err = Load(Flags{}, env(map[string]string{
		"WWZ_VALKEY_ADDR": "v:6379", "WWZ_VALKEY_PASSWORD": "a", "WWZ_PANEL_VALKEY_PASSWORD": "b",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Valkey.Addr, []string{"v:6379"}) || c.Valkey.Password != "b" {
		t.Fatalf("valkey fallback: %+v", c.Valkey)
	}
}

func TestGetenvNilUsesProcessEnv(t *testing.T) {
	t.Setenv("WWZ_PANEL_LISTEN", ":8111")
	c, err := Load(Flags{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8111" {
		t.Fatalf("listen %q", c.Listen)
	}
}

func TestSecretsRefused(t *testing.T) {
	p := writeYAML(t, `
wwz: {admin_url: "http://x:1", token: abc}
valkey: {password: pw}
users:
  - {name: a, role: viewer, password: plain}
oidc: {client_secret: cs}
session_key: k
api_token: t
admin_token: t
`)
	_, err := Load(Flags{Config: p}, env(nil))
	want := []string{"admin_token", "api_token", "oidc.client_secret", "session_key", "users[0].password", "valkey.password", "wwz.token"}
	if got := fieldsOf(err); !reflect.DeepEqual(got, want) {
		t.Fatalf("secret fields: got %v want %v (%v)", got, want, err)
	}
	if !strings.Contains(err.Error(), "WWZ_PANEL_OIDC_CLIENT_SECRET") {
		t.Fatalf("message should name the variable: %v", err)
	}
}

func TestFileErrors(t *testing.T) {
	if _, err := Load(Flags{Config: filepath.Join(t.TempDir(), "nope.yaml")}, env(nil)); err == nil ||
		!strings.Contains(err.Error(), "read") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := Load(Flags{Config: writeYAML(t, "listen: [\n")}, env(nil)); err == nil {
		t.Fatal("bad yaml accepted")
	}
	if _, err := Load(Flags{Config: writeYAML(t, "unknown_field: 1\n")}, env(nil)); err == nil ||
		!strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestEnvParseErrors(t *testing.T) {
	_, err := Load(Flags{}, env(map[string]string{
		"WWZ_PANEL_BACKUP_KEEP":    "x",
		"WWZ_PANEL_RETENTION_DAYS": "y",
		"WWZ_PANEL_VALKEY_CLUSTER": "talvez",
		"WWZ_PANEL_SESSION_TTL":    "sempre",
	}))
	for _, f := range []string{"WWZ_PANEL_BACKUP_KEEP", "WWZ_PANEL_RETENTION_DAYS", "WWZ_PANEL_VALKEY_CLUSTER", "WWZ_PANEL_SESSION_TTL"} {
		if !hasField(err, f) {
			t.Errorf("missing %s in %v", f, err)
		}
	}
	_, err = Load(Flags{Config: writeYAML(t, "session: {ttl: nunca}\n")}, env(nil))
	if !hasField(err, "session.ttl") {
		t.Fatalf("yaml ttl: %v", err)
	}
}

func TestValidate(t *testing.T) {
	base := func() *Config {
		c, err := Load(Flags{}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := []struct {
		field string
		mut   func(*Config)
	}{
		{"listen", func(c *Config) { c.Listen = "8100" }},
		{"public_url", func(c *Config) { c.PublicURL = "ftp://x" }},
		{"public_url", func(c *Config) { c.PublicURL = "http://" }},
		{"public_url", func(c *Config) { c.PublicURL = "http://[::1" }},
		{"db", func(c *Config) { c.DB = " " }},
		{"backup.at", func(c *Config) { c.Backup.At = "24:00" }},
		{"backup.keep", func(c *Config) { c.Backup.Keep = 0 }},
		{"retention_days", func(c *Config) { c.RetentionDays = 0 }},
		{"log_level", func(c *Config) { c.LogLevel = "trace" }},
		{"valkey.addr[1]", func(c *Config) { c.Valkey.Addr = []string{"a:1", "semporta"} }},
		{"valkey.prefix", func(c *Config) { c.Valkey.Prefix = "" }},
		{"wwz.admin_url", func(c *Config) { c.WWZ.AdminURL = "" }},
		{"prometheus.url", func(c *Config) { c.Prometheus.URL = "prom:9090" }},
		{"WWZ_PANEL_SESSION_KEY", func(c *Config) { c.Session.Key = "curta" }},
		{"session.ttl", func(c *Config) { c.Session.TTL = 0 }},
		{"WWZ_PANEL_ADMIN_USER", func(c *Config) { c.Admin.User = "" }},
		{"users[0].name", func(c *Config) { c.Users = []User{{Role: RoleViewer, PasswordHash: "$argon2id$x"}} }},
		{"users[1].name", func(c *Config) {
			c.Users = []User{{Name: "a", Role: RoleViewer, PasswordHash: "$argon2id$x"}, {Name: "a", Role: RoleViewer, PasswordHash: "$argon2id$x"}}
		}},
		{"users[0].role", func(c *Config) { c.Users = []User{{Name: "a", Role: "admin", PasswordHash: "$argon2id$x"}} }},
		{"users[0].password_hash", func(c *Config) { c.Users = []User{{Name: "a", Role: RoleOperator, PasswordHash: "$2a$bcrypt"}} }},
		{"oidc.issuer", func(c *Config) { c.OIDC.Issuer = "login"; c.OIDC.ClientID = "x"; c.PublicURL = "https://p" }},
		{"oidc.client_id", func(c *Config) { c.OIDC.Issuer = "https://login"; c.PublicURL = "https://p" }},
		{"public_url", func(c *Config) { c.OIDC.Issuer = "https://login"; c.OIDC.ClientID = "x" }},
		{"oidc.default_role", func(c *Config) {
			c.OIDC.Issuer = "https://login"
			c.OIDC.ClientID = "x"
			c.PublicURL = "https://p"
			c.OIDC.DefaultRole = "operator"
		}},
		{"oidc.issuer", func(c *Config) { c.OIDC.ClientID = "x" }},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(c)
		err := c.Validate()
		if !hasField(err, tc.field) {
			t.Errorf("%s: got %v", tc.field, err)
		}
		if err != nil && !strings.Contains(err.Error(), tc.field+": ") {
			t.Errorf("%s: message should cite the field: %v", tc.field, err)
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}
