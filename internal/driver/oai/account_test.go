package oai_test

import (
	"regexp"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"":              "",
		"acct-test":     "acct-test",
		"Acme Corp":     "acme-corp",
		"  Ação__X  ":   "a-o-x",
		"--":            "",
		"Team 42/Batch": "team-42-batch",
	} {
		if got := oai.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// accountReq is a request whose key belongs to acct/proj (driver.Account).
func accountReq(acct, proj, clientProj string) *core.Request {
	h := driver.MapHeaders{"authorization": "Bearer sk-x"}
	if clientProj != "" {
		h["openai-project"] = clientProj
	}
	driver.SetAccount(h, driver.Account{Key: "sk-x", Account: acct, Project: proj})
	return &core.Request{Headers: h}
}

// TestAccountHeadersFromAccount (task 201): the account of the key, not the
// key, names the organization and the project; one derivation for all.
func TestAccountHeadersFromAccount(t *testing.T) {
	org, p1 := oai.AccountHeaders(accountReq("Acme", "web", ""))
	if org != "acme" || !regexp.MustCompile(`^proj_[A-Za-z0-9]{24}$`).MatchString(p1) {
		t.Errorf("got %q %q", org, p1)
	}
	if _, p2 := oai.AccountHeaders(accountReq("Acme", "web", "")); p2 != p1 {
		t.Errorf("not stable: %q %q", p1, p2)
	}
	if _, p3 := oai.AccountHeaders(accountReq("Acme", "batch", "")); p3 == p1 {
		t.Error("projects collide")
	}
	if _, p4 := oai.AccountHeaders(accountReq("Other", "web", "")); p4 == p1 {
		t.Error("same project name in two accounts collides")
	}
	if _, p5 := oai.AccountHeaders(accountReq("Acme", "web", "proj_mine")); p5 != "proj_mine" {
		t.Errorf("client openai-project not kept: %q", p5)
	}
	// No account: derived from the presented key.
	org, p6 := oai.AccountHeaders(accountReq("", "web", ""))
	if !regexp.MustCompile(`^user-[a-z0-9]{24}$`).MatchString(org) || p6 == p1 {
		t.Errorf("no account: %q %q", org, p6)
	}
	if org, p := oai.AccountHeaders(nil); len(org) != 29 || len(p) != 29 {
		t.Errorf("nil request: %q %q", org, p)
	}
	m := oai.OpenAISuccessHeaders(accountReq("Acme", "web", ""), nil, nil)
	if m["openai-organization"] != "acme" || m["openai-project"] != p1 {
		t.Errorf("success headers %v", m)
	}
}

// TestOrganizationID (task 201): the 429 names "org-" + 24 alphanumerics,
// stable per account; without an account, org-wwz.
func TestOrganizationID(t *testing.T) {
	a := oai.OrganizationID(accountReq("Acme", "web", ""))
	if !regexp.MustCompile(`^org-[A-Za-z0-9]{24}$`).MatchString(a) {
		t.Errorf("org id %q", a)
	}
	if b := oai.OrganizationID(accountReq("Acme", "batch", "")); b != a {
		t.Errorf("projects of one account: %q %q", a, b)
	}
	if c := oai.OrganizationID(accountReq("Other", "web", "")); c == a {
		t.Error("two accounts share an org id")
	}
	if got := oai.OrganizationID(accountReq("", "", "")); got != "org-wwz" {
		t.Errorf("no account: %q", got)
	}
	if got := oai.OrganizationID(nil); got != "org-wwz" {
		t.Errorf("nil request: %q", got)
	}
}
