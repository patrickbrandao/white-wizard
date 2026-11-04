package opencode_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Task 425 (ADR 0072, item 17; spec 004 "OpenCode: modo public e
// Console"): the contract of each Console route, as spec 004 writes it:
// status, content type and the exact members of every answer, with their
// types. The values the Console tests already pin (times, the device flow
// steps, budgets) are not repeated; verification_uri_complete is only a
// string here, its origin is the subject of ADR 0073 (task 436).

// shape is the members of a JSON object and the type of each: string,
// number, bool, object or array.
type shape map[string]string

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}

// wantShape checks that obj has exactly the members of s, typed as s says.
func wantShape(t *testing.T, name string, obj map[string]any, s shape) {
	t.Helper()
	if g, w := slices.Sorted(maps.Keys(obj)), slices.Sorted(maps.Keys(s)); !slices.Equal(g, w) {
		t.Errorf("%s: members %v, spec 004 %v", name, g, w)
	}
	for k, typ := range s {
		if v, ok := obj[k]; ok && jsonType(v) != typ {
			t.Errorf("%s: %s is %s (%v), want %s", name, k, jsonType(v), v, typ)
		}
	}
}

// consoleList decodes a Console answer that is a JSON array.
func consoleList(t *testing.T, name string, w *core.MemWriter) []map[string]any {
	t.Helper()
	if w.StatusCode() != 200 || w.Headers()["Content-Type"] != "application/json" {
		t.Fatalf("%s: %d %q %s", name, w.StatusCode(), w.Headers()["Content-Type"], w.Body())
	}
	var out []map[string]any
	if err := json.Unmarshal(w.Body(), &out); err != nil {
		t.Fatalf("%s: %s: %v", name, w.Body(), err)
	}
	return out
}

var (
	deviceShape = shape{"device_code": "string", "user_code": "string", "verification_uri_complete": "string", "expires_in": "number", "interval": "number"}
	tokenShape  = shape{"access_token": "string", "refresh_token": "string", "token_type": "string", "expires_in": "number", "org_id": "string"}
	userShape   = shape{"id": "string", "email": "string"}
	orgShape    = shape{"id": "string", "name": "string"}
	budgetShape = shape{
		"ruleID": "string", "limitDollars": "number", "spentDollars": "number", "scope": "string", "per": "string",
		"coverage": "string", "models": "array", "vendors": "array", "picked": "bool", "shared": "bool", "resetsAt": "string",
	}
	reWrk = regexp.MustCompile(`^wrk_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// TestConsoleContract walks the device flow, the refresh and every api/*
// route, checking each answer against spec 004.
func TestConsoleContract(t *testing.T) {
	b, c := newConsole(t, consoleCfg, consoleOpts)

	// The device flow answers without a credential and without the
	// session header, and a bad credential does not change that (a public
	// route, driver.PublicRouter).
	for _, hdr := range []map[string]string{nil, {"authorization": "Bearer oc_sk_nope"}, {"authorization": "Bearer public"}} {
		w := b.do(t, "POST", "/console/auth/device/code", hdr, `{"client_id":"opencode-cli"}`)
		wantShape(t, fmt.Sprintf("device/code %v", hdr), consoleJSON(t, w, 200), deviceShape)
	}
	c.at(0)
	dc := b.deviceCode(t)
	wantShape(t, "device/code", dc, deviceShape)
	if !regexp.MustCompile(`^[A-Z]{4}-[A-Z]{4}$`).MatchString(dc["user_code"].(string)) || dc["expires_in"] != 900.0 || dc["interval"] != 5.0 {
		t.Errorf("device/code: %v", dc)
	}
	code := dc["device_code"].(string)
	for _, step := range []struct {
		at   time.Duration
		want string
	}{{time.Second, "slow_down"}, {6 * time.Second, "authorization_pending"}} {
		c.at(step.at)
		for _, form := range []bool{false, true} {
			w := b.poll(t, code, form)
			if m := consoleJSON(t, w, 400); m["error"] != step.want {
				t.Errorf("poll at %s (form %v): %s, want %s", step.at, form, w.Body(), step.want)
			} else {
				wantShape(t, "oauth error", m, shape{"error": "string"})
			}
		}
	}
	c.at(11 * time.Second)
	tok := consoleJSON(t, b.poll(t, code, true), 200)
	wantShape(t, "device/token", tok, tokenShape)
	if tok["token_type"] != "Bearer" || tok["expires_in"] != 3600.0 || !reWrk.MatchString(tok["org_id"].(string)) {
		t.Errorf("device/token: %v", tok)
	}
	c.at(time.Minute)
	fresh := consoleJSON(t, b.refresh(t, tok["refresh_token"].(string)), 200)
	wantShape(t, "refresh", fresh, tokenShape)
	for name, body := range map[string]string{
		"refresh_token as JSON": `{"grant_type":"refresh_token","refresh_token":"x.y"}`,
		"refresh_token as form": `grant_type=refresh_token&refresh_token=x.y`,
	} {
		ct := "application/json"
		if !strings.HasPrefix(body, "{") {
			ct = "application/x-www-form-urlencoded"
		}
		w := b.do(t, "POST", "/console/auth/device/token", map[string]string{"content-type": ct}, body)
		if m := consoleJSON(t, w, 400); m["error"] != "invalid_grant" || len(m) != 1 {
			t.Errorf("%s: %s", name, w.Body())
		}
	}

	// api/*: the access token and a key of keys[] are both accepted;
	// x-org-id is ignored (b.api sends one that names no workspace).
	for _, bearer := range []string{fresh["access_token"].(string), "oc_sk_go"} {
		user := consoleJSON(t, b.api(t, "user", bearer), 200)
		wantShape(t, "api/user", user, userShape)
		if user["email"] != "go-key@wwz.invalid" || !strings.HasPrefix(user["id"].(string), "usr_") {
			t.Errorf("api/user: %v", user)
		}
		orgs := consoleList(t, "api/orgs", b.api(t, "orgs", bearer))
		if len(orgs) != 1 {
			t.Fatalf("api/orgs: %v", orgs)
		}
		wantShape(t, "api/orgs", orgs[0], orgShape)
		if orgs[0]["id"] != tok["org_id"] || orgs[0]["name"] != "acme" {
			t.Errorf("api/orgs %v, org_id %v", orgs, tok["org_id"])
		}
		cfg := consoleJSON(t, b.api(t, "v2/config", bearer), 200)
		wantShape(t, "api/v2/config", cfg, shape{"providers": "object"})
		consoleList(t, "api/policies/current-budgets", b.api(t, "policies/current-budgets", bearer))
	}

	// The service account: 403 Forbidden on the budgets only.
	if w := b.api(t, "policies/current-budgets", "oc_sk_svc"); w.StatusCode() != 403 || string(w.Body()) != `{"_tag":"Forbidden"}` || w.Headers()["Content-Type"] != "application/json" {
		t.Errorf("service account budgets: %d %q %s", w.StatusCode(), w.Headers()["Content-Type"], w.Body())
	}
	for _, path := range []string{"user", "orgs", "v2/config"} {
		if w := b.api(t, path, "oc_sk_svc"); w.StatusCode() != 200 {
			t.Errorf("service account %s: %d %s", path, w.StatusCode(), w.Body())
		}
	}

	// A token past its hour is a missing credential: 401 Unauthorized.
	c.at(2 * time.Hour)
	for _, path := range []string{"user", "orgs", "v2/config", "policies/current-budgets"} {
		w := b.api(t, path, fresh["access_token"].(string))
		if m := consoleJSON(t, w, 401); m["_tag"] != "Unauthorized" || len(m) != 1 {
			t.Errorf("expired token on %s: %s", path, w.Body())
		}
	}
}

// TestConsoleBudgetsContract: each rule of current-budgets has exactly the
// members of spec 004, with scope, per and coverage in their sets.
func TestConsoleBudgetsContract(t *testing.T) {
	b, _ := newConsole(t, budgetCfg, nil)
	rules := consoleList(t, "current-budgets", b.api(t, "policies/current-budgets", "oc_sk_b"))
	if len(rules) == 0 {
		t.Fatal("no budget rule")
	}
	for i, r := range rules {
		wantShape(t, fmt.Sprintf("rule %d", i), r, budgetShape)
		if !slices.Contains([]string{"workspace", "member", "key"}, r["scope"].(string)) || r["per"] != "month" ||
			!slices.Contains([]string{"all", "models"}, r["coverage"].(string)) {
			t.Errorf("rule %d: %v", i, r)
		}
		if _, err := time.Parse(time.RFC3339, r["resetsAt"].(string)); err != nil {
			t.Errorf("rule %d: resetsAt %v", i, r["resetsAt"])
		}
	}
}

// TestConsoleOrgIsTheWorkspace: the id of api/orgs is metadata.workspace
// of the account's GoUsageLimitError (spec 004).
func TestConsoleOrgIsTheWorkspace(t *testing.T) {
	b, _ := newConsole(t, consoleCfg, consoleOpts)
	_ = b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true)
	w := b.chat(t, "/inference/go", "oc_sk_tiny", "deepseek-v4.1-flash", true)
	var body struct {
		Metadata struct {
			Workspace string `json:"workspace"`
		} `json:"metadata"`
	}
	if w.StatusCode() != 429 || json.Unmarshal(w.Body(), &body) != nil || !reWrk.MatchString(body.Metadata.Workspace) {
		t.Fatalf("quota: %d %s", w.StatusCode(), w.Body())
	}
	orgs := consoleList(t, "api/orgs", b.api(t, "orgs", "oc_sk_tiny"))
	if len(orgs) != 1 || orgs[0]["id"] != body.Metadata.Workspace || orgs[0]["name"] != "tiny-strict" {
		t.Errorf("api/orgs %v, workspace %s", orgs, body.Metadata.Workspace)
	}
}
