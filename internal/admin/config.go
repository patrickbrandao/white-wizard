package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/goccy/go-yaml"
	"github.com/patrickbrandao/white-wizard/internal/config"
)

// masked replaces secrets in GET /admin/config. A patch that sends it back
// keeps the current secret.
const masked = "***"

// namedLists are the configuration lists addressed by name in a patch.
var namedLists = map[string]string{"models": "id", "keys": "label", "drivers": "mount"}

// OnReload registers fn to receive every configuration that becomes
// effective (patch, file reload, SIGHUP), after the engine already uses it.
// fn runs synchronously on the goroutine that applied the change.
func (s *Server) OnReload(fn func(*config.Config)) {
	s.obsMu.Lock()
	s.observers = append(s.observers, fn)
	s.obsMu.Unlock()
}

// Config returns the effective configuration.
func (s *Server) Config() *config.Config { return s.eng.Config() }

// Apply validates c and swaps it in atomically: requests in progress finish
// with the previous configuration. On error nothing changes (I23).
func (s *Server) Apply(c *config.Config) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.applyLocked(c)
}

func (s *Server) applyLocked(c *config.Config) error {
	if c == nil {
		return errors.New("config: nil configuration")
	}
	if s.opts.Override != nil {
		s.opts.Override(c)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if s.opts.Check != nil {
		if err := s.opts.Check(c); err != nil {
			return err
		}
	}
	s.eng.SetConfig(c)
	s.obsMu.RLock()
	obs := slices.Clone(s.observers)
	s.obsMu.RUnlock()
	for _, fn := range obs {
		fn(c)
	}
	return nil
}

// Reload re-reads the configuration file and applies it (SIGHUP and empty
// POST /admin/config). On error the current configuration stays.
func (s *Server) Reload() (*config.Config, error) {
	load := s.opts.Load
	if load == nil {
		if s.opts.ConfigPath == "" {
			return nil, errors.New("config: no configuration file to reload")
		}
		path := s.opts.ConfigPath
		load = func() (*config.Config, error) { return config.Load(path) }
	}
	c, err := load()
	if err != nil {
		return nil, err
	}
	if err := s.Apply(c); err != nil {
		return nil, err
	}
	s.log.Info("admin: configuration reloaded from file", "path", s.opts.ConfigPath)
	return c, nil
}

// Patch applies a JSON merge patch (RFC 7396) on the effective
// configuration; models, keys and drivers are addressed by id, label and
// mount (prefix, or name when there is no prefix). On error nothing changes.
func (s *Server) Patch(patch []byte) (*config.Config, error) {
	var p any
	dec := json.NewDecoder(strings.NewReader(string(patch)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	if dec.More() {
		return nil, errors.New("patch: trailing data after JSON body")
	}
	pm, ok := p.(map[string]any)
	if !ok {
		return nil, errors.New("patch: body must be a JSON object")
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	doc := Document(s.eng.Config())
	keepSecrets(pm)
	if err := applyPatch(doc, pm); err != nil {
		return nil, err
	}
	c, err := config.Parse(mustMarshalYAML(normalize(doc)))
	if err != nil {
		return nil, err
	}
	if err := s.applyLocked(c); err != nil {
		return nil, err
	}
	s.log.Info("admin: configuration patched")
	return c, nil
}

// WatchSIGHUP reloads the configuration file on every SIGHUP until ctx ends.
// Errors are logged and the previous configuration stays.
func (s *Server) WatchSIGHUP(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				if _, err := s.Reload(); err != nil {
					s.log.Error("admin: SIGHUP reload failed; keeping current configuration", "err", err)
				}
			}
		}
	}()
}

// Document renders a configuration as the generic document of
// GET /admin/config (same keys as the YAML), secrets included.
func Document(c *config.Config) map[string]any {
	doc := map[string]any{}
	if c != nil {
		mustUnmarshalYAML(mustMarshalYAML(c), &doc)
	}
	return doc
}

// mustMarshalYAML marshals a configuration value. Those values (the config
// structs and documents decoded from JSON or YAML) always marshal, so a
// failure is a bug and panics instead of becoming an unreachable error path.
func mustMarshalYAML(v any) []byte {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic("admin: yaml.Marshal of a configuration value failed: " + err.Error())
	}
	return b
}

// mustUnmarshalYAML is the counterpart of mustMarshalYAML for its output.
func mustUnmarshalYAML(b []byte, out any) {
	if err := yaml.Unmarshal(b, out); err != nil {
		panic("admin: yaml.Unmarshal of marshaled YAML failed: " + err.Error())
	}
}

// maskSecrets hides admin.token, store.password and keys[].key.
func maskSecrets(doc map[string]any) {
	maskField(doc, "admin", "token")
	maskField(doc, "store", "password")
	if keys, ok := doc["keys"].([]any); ok {
		for _, k := range keys {
			if km, ok := k.(map[string]any); ok {
				if v, ok := km["key"].(string); ok && v != "" {
					km["key"] = masked
				}
			}
		}
	}
}

func maskField(doc map[string]any, section, field string) {
	if m, ok := doc[section].(map[string]any); ok {
		if v, ok := m[field].(string); ok && v != "" {
			m[field] = masked
		}
	}
}

// keepSecrets drops masked secrets from a patch so the current ones stay.
func keepSecrets(p map[string]any) {
	for _, sf := range [][2]string{{"admin", "token"}, {"store", "password"}} {
		if m, ok := p[sf[0]].(map[string]any); ok && m[sf[1]] == masked {
			delete(m, sf[1])
		}
	}
	switch keys := p["keys"].(type) {
	case map[string]any:
		for _, k := range keys {
			if km, ok := k.(map[string]any); ok && km["key"] == masked {
				delete(km, "key")
			}
		}
	}
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	doc := Document(s.eng.Config())
	maskSecrets(doc)
	writeJSON(w, http.StatusOK, doc)
}

// ConfigApplied is the body of a successful POST /admin/config.
type ConfigApplied struct {
	Applied bool           `json:"applied"`
	Source  string         `json:"source"`
	Config  map[string]any `json:"config"`
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		invalid(w, "cannot read body", err.Error())
		return
	}
	if len(body) > maxBody {
		invalid(w, "body too large")
		return
	}
	var c *config.Config
	source := "patch"
	if strings.TrimSpace(string(body)) == "" {
		source = "file"
		c, err = s.Reload()
	} else {
		switch mediaType(r) {
		case "", "application/json", "application/merge-patch+json":
		default:
			problem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "use application/json or application/merge-patch+json")
			return
		}
		c, err = s.Patch(body)
	}
	if err != nil {
		invalid(w, "configuration not applied; the previous one is still in effect", err.Error())
		return
	}
	doc := Document(c)
	maskSecrets(doc)
	writeJSON(w, http.StatusOK, ConfigApplied{Applied: true, Source: source, Config: doc})
}

// applyPatch merges p into doc (RFC 7396), addressing named lists by name.
func applyPatch(doc, p map[string]any) error {
	for k, pv := range p {
		idField, named := namedLists[k]
		pm, isMap := pv.(map[string]any)
		if !named || !isMap {
			if pv == nil {
				delete(doc, k)
				continue
			}
			doc[k] = mergePatch(doc[k], pv)
			continue
		}
		list, err := patchNamedList(k, idField, doc[k], pm)
		if err != nil {
			return err
		}
		doc[k] = list
	}
	return nil
}

// itemName returns the name of a list item for addressing.
func itemName(idField string, item map[string]any) string {
	if idField == "mount" {
		if p, _ := item["prefix"].(string); p != "" {
			return p
		}
		n, _ := item["name"].(string)
		return n
	}
	n, _ := item[idField].(string)
	return n
}

func patchNamedList(section, idField string, cur any, p map[string]any) ([]any, error) {
	items := map[string]any{}
	var order []string
	if list, ok := cur.([]any); ok {
		for i, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("patch: %s[%d] is not an object", section, i)
			}
			name := itemName(idField, m)
			if name == "" {
				return nil, fmt.Errorf("patch: %s[%d] has no %s to be addressed by", section, i, idField)
			}
			if _, dup := items[name]; dup {
				return nil, fmt.Errorf("patch: %s has duplicate %s %q; cannot address it by name", section, idField, name)
			}
			items[name] = m
			order = append(order, name)
		}
	}
	var added []string
	for name, pv := range p {
		old, exists := items[name]
		if pv == nil {
			delete(items, name)
			continue
		}
		if _, ok := pv.(map[string]any); !ok {
			return nil, fmt.Errorf("patch: %s.%s must be an object or null", section, name)
		}
		merged := mergePatch(old, pv).(map[string]any)
		switch {
		case idField != "mount":
			if itemName(idField, merged) == "" {
				merged[idField] = name
			}
		case strings.HasPrefix(name, "/"):
			if p, _ := merged["prefix"].(string); p == "" {
				merged["prefix"] = name
			}
		default:
			if n, _ := merged["name"].(string); n == "" {
				merged["name"] = name
			}
		}
		items[name] = merged
		if !exists {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	out := make([]any, 0, len(items))
	for _, name := range append(order, added...) {
		if it, ok := items[name]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// mergePatch implements RFC 7396 for one value.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	} else {
		cp := make(map[string]any, len(tm))
		for k, v := range tm {
			cp[k] = v
		}
		tm = cp
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

// normalize drops nulls and turns JSON numbers into int64 or float64 so the
// YAML decoder of config accepts them in integer fields.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if val == nil {
				delete(x, k)
				continue
			}
			x[k] = normalize(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return normalize(f)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	}
	return v
}
