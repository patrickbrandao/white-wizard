// Package config loads the wwz-panel configuration (spec 016, "Configuração"):
// WWZ_PANEL_* environment variables and the optional panel.yaml, with the
// precedence flag > environment > panel.yaml > default. Secrets come only from
// the environment; a panel.yaml carrying a secret field is rejected.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Defaults from spec 016.
const (
	DefaultListen        = ":8100"
	DefaultDB            = "panel.db"
	DefaultBackupAt      = "03:00"
	DefaultBackupKeep    = 7
	DefaultRetentionDays = 7
	DefaultValkeyPrefix  = "wwz"
	DefaultAdminURL      = "http://127.0.0.1:9000"
	DefaultSessionTTL    = 12 * time.Hour
	DefaultAdminUser     = "admin"
	DefaultLogLevel      = "info"
	// MinSessionKey is the minimum length, in bytes, of WWZ_PANEL_SESSION_KEY.
	MinSessionKey = 32
)

// Roles accepted by the panel.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	// RoleNone is valid only as oidc.default_role: refuse the login.
	RoleNone = "none"
)

// Config is the effective panel configuration.
type Config struct {
	// ConfigPath is the panel.yaml in use ("" = none).
	ConfigPath    string
	Listen        string
	PublicURL     string
	DB            string
	RetentionDays int
	LogLevel      string
	Backup        Backup
	Valkey        Valkey
	WWZ           WWZ
	Prometheus    Prometheus
	Session       Session
	Admin         Admin
	// APIToken enables "Authorization: Bearer" automation on /api/v1 ("" = off).
	APIToken string
	OIDC     OIDC
	Users    []User
}

// Backup is the scheduled snapshot (ADR 0009).
type Backup struct {
	Dir  string // "" disables the scheduled backup
	At   string // HH:MM UTC
	Keep int
}

// Valkey is the optional cluster store ("" Addr = mode without Valkey).
type Valkey struct {
	Addr     []string
	Password string
	Prefix   string
	Cluster  bool
}

// Enabled reports whether the panel talks to Valkey.
func (v Valkey) Enabled() bool { return len(v.Addr) > 0 }

// WWZ is the control plane of the simulator.
type WWZ struct {
	AdminURL   string
	AdminToken string
}

// Prometheus is the optional metrics source ("" URL = /admin/stats).
type Prometheus struct {
	URL string
}

// Session configures the login cookie.
type Session struct {
	// Key is the HMAC key; "" = generated on first boot and kept in settings.
	Key string
	TTL time.Duration
}

// Admin is the first-operator bootstrap (ADR 0005).
type Admin struct {
	User     string
	Password string // "" = generated and logged once
}

// OIDC is the optional OpenID Connect login.
type OIDC struct {
	Issuer         string
	ClientID       string
	ClientSecret   string
	Scopes         []string
	UsernameClaim  string
	RoleClaim      string
	OperatorValues []string
	ViewerValues   []string
	DefaultRole    string
}

// Enabled reports whether OIDC login is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

// User is a local login declared in panel.yaml.
type User struct {
	Name         string
	Role         string
	PasswordHash string
}

// Flags are the command-line overrides (-config, -listen); "" = not given.
type Flags struct {
	Config string
	Listen string
}

// file mirrors panel.yaml. Pointers distinguish "absent" from zero.
type file struct {
	Listen        string        `yaml:"listen"`
	PublicURL     string        `yaml:"public_url"`
	DB            string        `yaml:"db"`
	RetentionDays *int          `yaml:"retention_days"`
	LogLevel      string        `yaml:"log_level"`
	Backup        fileBackup    `yaml:"backup"`
	WWZ           fileWWZ       `yaml:"wwz"`
	Valkey        fileValkey    `yaml:"valkey"`
	Prometheus    filePrometheu `yaml:"prometheus"`
	Session       fileSession   `yaml:"session"`
	Users         []fileUser    `yaml:"users"`
	OIDC          fileOIDC      `yaml:"oidc"`
}

type fileBackup struct {
	Dir  string `yaml:"dir"`
	At   string `yaml:"at"`
	Keep *int   `yaml:"keep"`
}

type fileWWZ struct {
	AdminURL string `yaml:"admin_url"`
}

type fileValkey struct {
	Addr    []string `yaml:"addr"`
	Prefix  string   `yaml:"prefix"`
	Cluster *bool    `yaml:"cluster"`
}

type filePrometheu struct {
	URL string `yaml:"url"`
}

type fileSession struct {
	TTL string `yaml:"ttl"`
}

type fileUser struct {
	Name         string `yaml:"name"`
	Role         string `yaml:"role"`
	PasswordHash string `yaml:"password_hash"`
}

type fileOIDC struct {
	Issuer         string   `yaml:"issuer"`
	ClientID       string   `yaml:"client_id"`
	Scopes         []string `yaml:"scopes"`
	UsernameClaim  string   `yaml:"username_claim"`
	RoleClaim      string   `yaml:"role_claim"`
	OperatorValues []string `yaml:"operator_values"`
	ViewerValues   []string `yaml:"viewer_values"`
	DefaultRole    string   `yaml:"default_role"`
}

// secretKeys are the panel.yaml keys refused anywhere in the document.
// Values are variable names, not credentials.
var secretKeys = map[string]string{ //nolint:gosec // G101: maps YAML keys to env var names
	"password":      "WWZ_PANEL_ADMIN_PASSWORD / WWZ_PANEL_VALKEY_PASSWORD",
	"client_secret": "WWZ_PANEL_OIDC_CLIENT_SECRET",
	"token":         "WWZ_ADMIN_TOKEN / WWZ_PANEL_API_TOKEN",
	"admin_token":   "WWZ_ADMIN_TOKEN",
	"api_token":     "WWZ_PANEL_API_TOKEN",
	"session_key":   "WWZ_PANEL_SESSION_KEY",
}

// FieldError is one validation failure, naming the field.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Load builds the configuration from flags, the environment (getenv; nil =
// os.Getenv) and the panel.yaml named by flags.Config or WWZ_PANEL_CONFIG.
// Validation failures are joined *FieldError values.
func Load(f Flags, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := &Config{ConfigPath: first(f.Config, getenv("WWZ_PANEL_CONFIG"))}
	var y file
	if c.ConfigPath != "" {
		data, err := os.ReadFile(filepath.Clean(c.ConfigPath))
		if err != nil {
			return nil, fmt.Errorf("panel config: read %s: %w", c.ConfigPath, err)
		}
		if y, err = parseFile(data); err != nil {
			return nil, err
		}
	}
	var errs []error
	fail := func(field, format string, a ...any) {
		errs = append(errs, &FieldError{Field: field, Msg: fmt.Sprintf(format, a...)})
	}
	envInt := func(name string, fromFile *int, def int) int {
		if v := getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				fail(name, "inteiro inválido %q", v)
			}
			return n
		}
		if fromFile != nil {
			return *fromFile
		}
		return def
	}

	c.Listen = first(f.Listen, getenv("WWZ_PANEL_LISTEN"), y.Listen, DefaultListen)
	c.PublicURL = first(getenv("WWZ_PANEL_PUBLIC_URL"), y.PublicURL)
	c.DB = first(getenv("WWZ_PANEL_DB"), y.DB, DefaultDB)
	c.Backup.Dir = first(getenv("WWZ_PANEL_BACKUP_DIR"), y.Backup.Dir)
	c.Backup.At = first(getenv("WWZ_PANEL_BACKUP_AT"), y.Backup.At, DefaultBackupAt)
	c.Backup.Keep = envInt("WWZ_PANEL_BACKUP_KEEP", y.Backup.Keep, DefaultBackupKeep)
	c.RetentionDays = envInt("WWZ_PANEL_RETENTION_DAYS", y.RetentionDays, DefaultRetentionDays)
	c.LogLevel = strings.ToLower(first(getenv("WWZ_PANEL_LOG_LEVEL"), y.LogLevel, DefaultLogLevel))

	if v := first(getenv("WWZ_PANEL_VALKEY_ADDR"), getenv("WWZ_VALKEY_ADDR")); v != "" {
		c.Valkey.Addr = splitList(v)
	} else {
		c.Valkey.Addr = y.Valkey.Addr
	}
	c.Valkey.Password = first(getenv("WWZ_PANEL_VALKEY_PASSWORD"), getenv("WWZ_VALKEY_PASSWORD"))
	c.Valkey.Prefix = first(getenv("WWZ_PANEL_VALKEY_PREFIX"), y.Valkey.Prefix, DefaultValkeyPrefix)
	if v := getenv("WWZ_PANEL_VALKEY_CLUSTER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("WWZ_PANEL_VALKEY_CLUSTER", "booleano inválido %q", v)
		}
		c.Valkey.Cluster = b
	} else if y.Valkey.Cluster != nil {
		c.Valkey.Cluster = *y.Valkey.Cluster
	}

	c.WWZ.AdminURL = first(getenv("WWZ_PANEL_WWZ_ADMIN_URL"), y.WWZ.AdminURL, DefaultAdminURL)
	c.WWZ.AdminToken = getenv("WWZ_ADMIN_TOKEN")
	c.Prometheus.URL = first(getenv("WWZ_PANEL_PROMETHEUS_URL"), y.Prometheus.URL)

	c.Session.Key = getenv("WWZ_PANEL_SESSION_KEY")
	c.Session.TTL = DefaultSessionTTL
	if v, field := first(getenv("WWZ_PANEL_SESSION_TTL"), y.Session.TTL), ttlField(getenv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fail(field, "duração inválida %q", v)
		}
		c.Session.TTL = d
	}

	c.Admin.User = first(getenv("WWZ_PANEL_ADMIN_USER"), DefaultAdminUser)
	c.Admin.Password = getenv("WWZ_PANEL_ADMIN_PASSWORD")
	c.APIToken = getenv("WWZ_PANEL_API_TOKEN")

	c.OIDC = OIDC{
		Issuer:         y.OIDC.Issuer,
		ClientID:       y.OIDC.ClientID,
		ClientSecret:   getenv("WWZ_PANEL_OIDC_CLIENT_SECRET"),
		Scopes:         y.OIDC.Scopes,
		UsernameClaim:  first(y.OIDC.UsernameClaim, "email"),
		RoleClaim:      first(y.OIDC.RoleClaim, "groups"),
		OperatorValues: y.OIDC.OperatorValues,
		ViewerValues:   y.OIDC.ViewerValues,
		DefaultRole:    first(y.OIDC.DefaultRole, RoleViewer),
	}
	if len(c.OIDC.Scopes) == 0 {
		c.OIDC.Scopes = []string{"openid", "email", "profile"}
	}
	for _, u := range y.Users {
		c.Users = append(c.Users, User(u))
	}

	errs = append(errs, c.Validate())
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func ttlField(getenv func(string) string) string {
	if getenv("WWZ_PANEL_SESSION_TTL") != "" {
		return "WWZ_PANEL_SESSION_TTL"
	}
	return "session.ttl"
}

// parseFile rejects secrets and unknown fields, then decodes panel.yaml.
func parseFile(data []byte) (file, error) {
	var y file
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return y, fmt.Errorf("panel config: %w", err)
	}
	var errs []error
	findSecrets(raw, "", &errs)
	if err := errors.Join(errs...); err != nil {
		return y, err
	}
	if err := yaml.UnmarshalWithOptions(data, &y, yaml.Strict()); err != nil {
		return y, fmt.Errorf("panel config: %w", err)
	}
	return y, nil
}

func findSecrets(v any, path string, errs *[]error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if env, ok := secretKeys[k]; ok {
				*errs = append(*errs, &FieldError{Field: p, Msg: "segredo não é aceito no panel.yaml; use " + env})
				continue
			}
			findSecrets(x[k], p, errs)
		}
	case []any:
		for i, e := range x {
			findSecrets(e, fmt.Sprintf("%s[%d]", path, i), errs)
		}
	}
}

var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Validate checks every field and returns joined *FieldError values (nil = ok).
func (c *Config) Validate() error {
	var errs []error
	fail := func(field, format string, a ...any) {
		errs = append(errs, &FieldError{Field: field, Msg: fmt.Sprintf(format, a...)})
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		fail("listen", "endereço inválido %q (host:porta)", c.Listen)
	}
	if c.PublicURL != "" {
		if err := checkURL(c.PublicURL); err != nil {
			fail("public_url", "%v", err)
		}
	}
	if strings.TrimSpace(c.DB) == "" {
		fail("db", "obrigatório")
	}
	if !hhmm.MatchString(c.Backup.At) {
		fail("backup.at", "horário inválido %q (HH:MM UTC)", c.Backup.At)
	}
	if c.Backup.Keep < 1 {
		fail("backup.keep", "deve ser ≥ 1 (recebido %d)", c.Backup.Keep)
	}
	if c.RetentionDays < 1 {
		fail("retention_days", "deve ser ≥ 1 (recebido %d)", c.RetentionDays)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("log_level", "valor inválido %q (debug, info, warn, error)", c.LogLevel)
	}
	for i, a := range c.Valkey.Addr {
		if _, _, err := net.SplitHostPort(a); err != nil {
			fail(fmt.Sprintf("valkey.addr[%d]", i), "endereço inválido %q (host:porta)", a)
		}
	}
	if c.Valkey.Prefix == "" {
		fail("valkey.prefix", "obrigatório")
	}
	if err := checkURL(c.WWZ.AdminURL); err != nil {
		fail("wwz.admin_url", "%v", err)
	}
	if c.Prometheus.URL != "" {
		if err := checkURL(c.Prometheus.URL); err != nil {
			fail("prometheus.url", "%v", err)
		}
	}
	if c.Session.Key != "" && len(c.Session.Key) < MinSessionKey {
		fail("WWZ_PANEL_SESSION_KEY", "deve ter ao menos %d bytes", MinSessionKey)
	}
	if c.Session.TTL <= 0 {
		fail("session.ttl", "deve ser positiva")
	}
	if strings.TrimSpace(c.Admin.User) == "" {
		fail("WWZ_PANEL_ADMIN_USER", "obrigatório")
	}
	seen := map[string]bool{}
	for i, u := range c.Users {
		p := fmt.Sprintf("users[%d]", i)
		switch {
		case strings.TrimSpace(u.Name) == "":
			fail(p+".name", "obrigatório")
		case seen[u.Name]:
			fail(p+".name", "duplicado %q", u.Name)
		}
		seen[u.Name] = true
		if u.Role != RoleViewer && u.Role != RoleOperator {
			fail(p+".role", "valor inválido %q (viewer, operator)", u.Role)
		}
		if !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
			fail(p+".password_hash", "esperado hash argon2id em formato PHC (wwz-panel hash-password)")
		}
	}
	if o := c.OIDC; o.Enabled() {
		if err := checkURL(o.Issuer); err != nil {
			fail("oidc.issuer", "%v", err)
		}
		if o.ClientID == "" {
			fail("oidc.client_id", "obrigatório com oidc.issuer")
		}
		if c.PublicURL == "" {
			fail("public_url", "obrigatório com oidc (base do redirect)")
		}
		if o.DefaultRole != RoleViewer && o.DefaultRole != RoleNone {
			fail("oidc.default_role", "valor inválido %q (viewer, none)", o.DefaultRole)
		}
	} else if o.ClientID != "" {
		fail("oidc.issuer", "obrigatório com oidc.client_id")
	}
	return errors.Join(errs...)
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("URL inválida %q (http:// ou https://)", s)
	}
	return nil
}

// first returns the first non-empty value.
func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
