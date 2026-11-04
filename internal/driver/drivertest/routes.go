package drivertest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// resourceMethods are the methods a driver.KindResource route may take
// (Gemini's cachedContents, Anthropic's /api/hello on every method the
// real answers); every other kind is GET or POST.
var resourceMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, "OPTIONS": true}

var knownKinds = map[driver.Kind]bool{
	driver.KindChat: true, driver.KindCompletions: true, driver.KindResponses: true, driver.KindMessages: true,
	driver.KindModels: true, driver.KindModel: true, driver.KindCountTokens: true, driver.KindKeyInfo: true,
	driver.KindUsage: true, driver.KindEmbeddings: true, driver.KindResource: true,
}

func (s *suite) testRoutes(t *testing.T) {
	rs := s.routes()
	if len(rs) == 0 {
		t.Fatal("Routes() is empty")
	}
	seen := map[string]bool{}
	for _, r := range rs {
		switch {
		case r.Kind == driver.KindResource:
			if !resourceMethods[r.Method] {
				t.Errorf("route %+v: a resource route is GET, HEAD, POST, PUT, DELETE, PATCH or OPTIONS", r)
			}
		case r.Method != "GET" && r.Method != "POST":
			t.Errorf("route %+v: method must be GET or POST", r)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "//") {
			t.Errorf("route %+v: path must be absolute and clean", r)
		}
		if strings.Contains(r.Path, "*") && !strings.HasSuffix(r.Path, "/*") {
			t.Errorf("route %+v: wildcard only as the last segment", r)
		}
		if !knownKinds[r.Kind] {
			t.Errorf("route %+v: unknown kind", r)
		}
		inference := r.Kind == driver.KindChat || r.Kind == driver.KindCompletions || r.Kind == driver.KindMessages || r.Kind == driver.KindCountTokens
		if inference && r.Method != "POST" {
			t.Errorf("route %+v: inference routes are POST", r)
		}
		if (r.Kind == driver.KindModels || r.Kind == driver.KindModel) && r.Method != "GET" {
			t.Errorf("route %+v: model listing is GET", r)
		}
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("route %s declared twice", key)
		}
		seen[key] = true
	}
	if !s.hasKind(driver.KindModels) {
		t.Error("no /models route (spec 005: /v1/models in the provider's format)")
	}
	if len(s.surfaces()) == 0 {
		t.Error("no inference route (chat, completions or messages)")
	}
	for _, want := range s.p.Routes {
		found := false
		for _, r := range rs {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing route %s %s (%s)", want.Method, want.Path, want.Kind)
		}
	}
	if s.p.Surfaces != nil {
		for _, k := range s.surfaces() {
			if _, ok := s.p.Surfaces[k]; !ok {
				t.Errorf("profile has no surface for routed kind %s", k)
			}
		}
	}

	s.run(t, "Extra", func(t *testing.T) {
		for _, r := range rs {
			if r.Kind != driver.KindKeyInfo && r.Kind != driver.KindUsage {
				continue
			}
			w := core.NewMemWriter()
			s.d.Extra(r.Kind, w, driver.MapHeaders(s.p.Credential), nil, driver.ExtraContext{Models: testModels()})
			body := expectJSONResponse(t, w, 200)
			switch r.Kind {
			case driver.KindKeyInfo: // OpenRouter GET /api/v1/key
				for _, p := range []string{"data.label", "data.limit", "data.limit_remaining", "data.usage", "data.is_free_tier", "data.free_model_daily_requests"} {
					expectPresent(t, body, p)
				}
			case driver.KindUsage: // OpenCode GET /zen/go/v1/usage
				for _, p := range []string{"usage.rolling.status", "usage.weekly.percent", "usage.monthly.resetsAt"} {
					expectPresent(t, body, p)
				}
			}
		}
		// A kind the driver has no auxiliary handler for is a 404.
		w := core.NewMemWriter()
		s.d.Extra(driver.KindEmbeddings, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
		if w.StatusCode() != 404 {
			t.Errorf("Extra(embeddings): status %d, want 404", w.StatusCode())
		}
	})
}

func testModels() []driver.ModelInfo {
	yes := true
	mk := func(id, name string) driver.ModelInfo {
		return driver.ModelInfo{Cfg: &config.ModelConfig{
			ID: id, DisplayName: name, OwnedBy: "wwz", Created: 1700000000,
			ContextWindow: 128000, MaxOutput: 8192,
			Capabilities: config.Capabilities{Tools: &yes, Reasoning: &yes},
			Pricing:      config.Pricing{InputPerM: 1, OutputPerM: 2, CacheReadPerM: 0.1, CacheWritePerM: 1.25},
		}}
	}
	return []driver.ModelInfo{mk("model-a", "Model A"), mk("model-b", "Model B"), mk("author/model-c", "Model C")}
}

// wantModelName is the name the profile expects for models[].id.
func (s *suite) wantModelName(id string) string { return s.p.ModelName(id) }

func (s *suite) testModels(t *testing.T) {
	models := testModels()
	s.run(t, "List", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.ListModels(w, nil, models)
		body := expectJSONResponse(t, w, 200)
		data := arr(body, "data")
		if len(data) != len(models) {
			t.Fatalf("data has %d models, want %d: %v", len(data), len(models), body)
		}
		for i, m := range data {
			id := str(m, "id")
			if !strings.HasSuffix(id, models[i].Cfg.ID) {
				t.Errorf("data.%d.id = %q, want it to name %q", i, id, models[i].Cfg.ID)
			}
			if s.p.ModelName != nil {
				expectPath(t, m, "id", s.wantModelName(models[i].Cfg.ID))
			}
			if s.p.Models == "openrouter" {
				expectPath(t, m, "canonical_slug", s.wantModelName(models[i].Cfg.ID))
			}
			s.checkModel(t, m, models[i].Cfg)
		}
		switch s.p.Models {
		case "openai", "opencode", "azure":
			expectPath(t, body, "object", "list")
		case "anthropic":
			expectPath(t, body, "has_more", false)
			expectPath(t, body, "first_id", "model-a")
			expectPath(t, body, "last_id", "author/model-c")
		}
	})
	s.run(t, "Get", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.GetModel(w, nil, &models[0], models[0].Cfg.ID)
		if s.p.ModelGetEmpty {
			if w.StatusCode() != 404 || len(w.Body()) != 0 {
				t.Fatalf("status %d body %q, want 404 with an empty body", w.StatusCode(), w.Body())
			}
			return
		}
		body := expectJSONResponse(t, w, 200)
		m := any(body)
		if s.p.Models == "openrouter" {
			m, _ = path(body, "data")
		}
		if id := str(m, "id"); !strings.HasSuffix(id, "model-a") {
			t.Errorf("id = %q, want model-a", id)
		}
		if s.p.ModelName != nil {
			expectPath(t, m, "id", s.wantModelName("model-a"))
		}
		s.checkModel(t, m, models[0].Cfg)
	})
	s.run(t, "Name", func(t *testing.T) { s.testModelName(t, models) })
	s.run(t, "GetUnknown", func(t *testing.T) {
		w := core.NewMemWriter()
		s.d.GetModel(w, nil, nil, "nope")
		st := w.StatusCode()
		if st < 400 || st > 499 {
			t.Fatalf("status %d, want 4xx", st)
		}
		if s.p.ModelNotFoundStatus != 0 && st != s.p.ModelNotFoundStatus {
			t.Errorf("status %d, want %d", st, s.p.ModelNotFoundStatus)
		}
		if s.p.ModelGetEmpty {
			if len(w.Body()) != 0 {
				t.Errorf("body %q, want empty", w.Body())
			}
			return
		}
		body := decodeJSON(t, w.Body())
		for p, want := range s.p.ModelNotFoundFields {
			expectPath(t, body, p, want)
		}
	})
}

// testModelName checks driver.ModelNamer against the profile (ADR 0038): the
// response name of each model and the prefix the request may carry.
func (s *suite) testModelName(t *testing.T, models []driver.ModelInfo) {
	n, ok := s.d.(driver.ModelNamer)
	if s.p.ModelName == nil {
		if ok {
			t.Fatal("driver implements driver.ModelNamer but the profile names models by id")
		}
		return
	}
	if !ok {
		t.Fatal("profile names models by slug but the driver does not implement driver.ModelNamer")
	}
	for _, m := range models {
		want := s.wantModelName(m.Cfg.ID)
		if got := n.ModelName(m.Cfg); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", m.Cfg.ID, got, want)
		}
		if want == m.Cfg.ID {
			continue // already author/model: nothing to strip
		}
		if got, ok := n.UnprefixModel(want); !ok || got != m.Cfg.ID {
			t.Errorf("UnprefixModel(%q) = %q, %v; want %q, true", want, got, ok, m.Cfg.ID)
		}
	}
	if got, ok := n.UnprefixModel("model-a"); ok {
		t.Errorf("UnprefixModel(model-a) = %q, true; want no prefix", got)
	}
}

// checkModel checks one model object against the provider's /models shape.
func (s *suite) checkModel(t *testing.T, m any, cfg *config.ModelConfig) {
	t.Helper()
	switch s.p.Models {
	case "openai":
		expectPath(t, m, "object", "model")
		expectPath(t, m, "created", 1700000000)
		expectPath(t, m, "owned_by", "wwz")
	case "azure":
		expectPath(t, m, "object", "model")
		expectPath(t, m, "created_at", 1700000000)
		expectPath(t, m, "status", "succeeded")
		expectPath(t, m, "capabilities.chat_completion", true)
		expectPresent(t, m, "lifecycle_status")
	case "anthropic":
		expectPath(t, m, "type", "model")
		expectPath(t, m, "display_name", cfg.DisplayName)
		if _, err := time.Parse(time.RFC3339, str(m, "created_at")); err != nil {
			t.Errorf("created_at %q is not RFC 3339", str(m, "created_at"))
		}
	case "opencode":
		expectPath(t, m, "object", "model")
		expectPath(t, m, "owned_by", "opencode")
		expectPath(t, m, "capabilities.tools.supported", true)
		expectPath(t, m, "capabilities.reasoning.supported", true)
		expectPath(t, m, "limit.context", cfg.ContextWindow)
		expectPath(t, m, "limit.output", cfg.MaxOutput)
		expectPath(t, m, "cost.text.input.price", cfg.Pricing.InputPerM)
		expectPath(t, m, "cost.text.output.price", cfg.Pricing.OutputPerM)
		expectPresent(t, m, "api.chat-completions.base_url")
	case "openrouter":
		expectPath(t, m, "context_length", cfg.ContextWindow)
		for _, p := range []string{"pricing.prompt", "pricing.completion"} {
			if _, ok := mustPath(m, p).(string); !ok {
				t.Errorf("%s must be a string (USD per token)", p)
			}
		}
		expectPath(t, m, "top_provider.max_completion_tokens", cfg.MaxOutput)
		expectPresent(t, m, "top_provider.is_moderated")
		expectPresent(t, m, "per_request_limits")
		if len(arr(m, "supported_parameters")) == 0 {
			t.Error("supported_parameters is empty")
		}
	}
}

func mustPath(v any, p string) any { x, _ := path(v, p); return x }

func (s *suite) testAuth(t *testing.T) {
	s.run(t, "Missing", func(t *testing.T) {
		c := s.d.Authenticate(driver.MapHeaders{})
		if c.Key != "" {
			t.Errorf("Key = %q without credentials", c.Key)
		}
		if c.Scheme != "none" {
			t.Errorf("Scheme = %q without credentials, want none", c.Scheme)
		}
	})
	s.run(t, "Malformed", func(t *testing.T) {
		for _, h := range []driver.MapHeaders{
			{"authorization": "Basic Zm9vOmJhcg=="},
			{"authorization": "sk-no-scheme"},
		} {
			if c := s.d.Authenticate(h); c.Key != "" {
				t.Errorf("Authenticate(%v).Key = %q, want empty", h, c.Key)
			}
		}
	})
	if s.p.Credential == nil {
		return
	}
	s.run(t, "Valid", func(t *testing.T) {
		c := s.d.Authenticate(driver.MapHeaders(s.p.Credential))
		if c.Key != s.p.Key {
			t.Errorf("Key = %q, want %q", c.Key, s.p.Key)
		}
		if len(c.Missing) != 0 {
			t.Errorf("Missing = %v with every required header", c.Missing)
		}
		if c.Scheme == "none" || c.Scheme == "" {
			t.Errorf("Scheme = %q with a credential", c.Scheme)
		}
	})
	if s.p.AltCredential != nil {
		s.run(t, "Alternative", func(t *testing.T) {
			if c := s.d.Authenticate(driver.MapHeaders(s.p.AltCredential)); c.Key != s.p.Key {
				t.Errorf("Key = %q, want %q", c.Key, s.p.Key)
			}
		})
	}
	s.run(t, "RequiredHeaders", func(t *testing.T) {
		for _, req := range s.p.RequiredHeaders {
			h := driver.MapHeaders{}
			for k, v := range s.p.Credential {
				if k != req {
					h[k] = v
				}
			}
			c := s.d.Authenticate(h)
			found := false
			for _, m := range c.Missing {
				if m == req {
					found = true
				}
			}
			if !found {
				t.Errorf("without %s: Missing = %v, want it listed", req, c.Missing)
			}
		}
		if len(s.p.RequiredHeaders) == 0 {
			if c := s.d.Authenticate(driver.MapHeaders(s.p.Credential)); len(c.Missing) != 0 {
				t.Errorf("Missing = %v, profile requires no header", c.Missing)
			}
		}
	})
	s.run(t, "Sniff", func(t *testing.T) {
		if !s.d.Sniff(driver.MapHeaders(s.p.Credential)) {
			t.Errorf("Sniff rejects its own canonical credential %v", s.p.Credential)
		}
		for _, f := range s.p.Foreign {
			if s.d.Sniff(driver.MapHeaders(f)) {
				t.Errorf("Sniff accepts another provider's credential %v", f)
			}
		}
	})
}

func (s *suite) testLimitPolicy(t *testing.T) {
	pol := s.d.LimitPolicy()
	valid := map[string]bool{"ip": true, "key": true, "user": true, "project": true, "account": true, "deployment": true}
	seen := map[string]bool{}
	if len(pol.Entities) == 0 {
		t.Error("Entities is empty")
	}
	for _, e := range pol.Entities {
		if !valid[e] {
			t.Errorf("entity %q is not one of ip, key, user, project, account, deployment", e)
		}
		if seen[e] {
			t.Errorf("entity %q repeated", e)
		}
		seen[e] = true
	}
	// Spec 006: every provider limits by organization/account.
	if !seen["account"] {
		t.Errorf("Entities %v: providers limit by account (spec 006)", pol.Entities)
	}
	if pol.TPMReserve != "max_tokens" && pol.TPMReserve != "none" {
		t.Errorf("TPMReserve %q, want max_tokens or none", pol.TPMReserve)
	}
	switch pol.ResetFormat {
	case "go_duration", "rfc3339", "seconds":
	default:
		t.Errorf("ResetFormat %q, want go_duration, rfc3339 or seconds", pol.ResetFormat)
	}
	if s.p.Policy.Entities != nil && !reflect.DeepEqual(pol, s.p.Policy) {
		t.Errorf("LimitPolicy = %+v, want %+v", pol, s.p.Policy)
	}
}
