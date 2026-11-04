package config

import (
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Accounts, projects and the limits of each level (ADR 0051, spec 003).
//
// Every limit of the tier (rate, concurrency, ramp, spend, credits, quota)
// counts on the account: the organization (OpenAI, Anthropic), the account
// (OpenRouter) or the subscribing workspace (OpenCode). A project (OpenAI
// project, Anthropic workspace) and a key only count the limits declared for
// them, as extra ceilings below the account.

// AccountConfig is an entry of accounts[]: the tier of the account and the
// limits laid over that tier.
type AccountConfig struct {
	ID string `yaml:"id"`
	// Tier is the tier of the account; empty = the keys[].tier of its keys,
	// else "default".
	Tier string `yaml:"tier"`
	// Limits are the non-zero fields laid over the limits of the tier.
	Limits *Limits `yaml:"limits"`
	// ExtraUsage turns on charging the balance once the plan's quota runs
	// out (OpenCode Extra Usage, ADR 0072); nil = the driver's default for
	// the plan of the tier.
	ExtraUsage *bool `yaml:"extra_usage,omitempty"`
}

// ProjectConfig is an entry of projects[]: a project only has its own
// ceilings (no tier), like the real OpenAI project / Anthropic workspace.
type ProjectConfig struct {
	ID     string  `yaml:"id"`
	Limits *Limits `yaml:"limits"`
}

// FindAccount returns the accounts[] entry with that id, or nil (implicit
// account).
func (c *Config) FindAccount(id string) *AccountConfig {
	for i := range c.Accounts {
		if c.Accounts[i].ID == id {
			return &c.Accounts[i]
		}
	}
	return nil
}

// FindProject returns the projects[] entry with that id, or nil.
func (c *Config) FindProject(id string) *ProjectConfig {
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			return &c.Projects[i]
		}
	}
	return nil
}

// AccountTier is the configured tier of an account: accounts[].tier, else
// the tier of the first key of the account that declares one, else
// "default". Tiers assigned at run time (cfg:policies, promotion) are not
// here: callers pass them to AccountLimits / LimitsFor.
func (c *Config) AccountTier(id string) string {
	if a := c.FindAccount(id); a != nil && a.Tier != "" {
		return a.Tier
	}
	for i := range c.Keys {
		if c.Keys[i].Account == id && c.Keys[i].Tier != "" {
			return c.Keys[i].Tier
		}
	}
	return "default"
}

// AccountLimits are the limits counted on an account: the limits of tier
// (empty = AccountTier(id)) with accounts[].limits on top. A tier that does
// not exist gives no base limits (unlimited).
func (c *Config) AccountLimits(id, tier string) Limits {
	if tier == "" {
		tier = c.AccountTier(id)
	}
	l := c.Tiers[tier].Limits
	if a := c.FindAccount(id); a != nil && a.Limits != nil {
		l = mergeLimits(l, *a.Limits)
	}
	return l
}

// ProjectLimits are the limits declared in projects[] for a project; zero
// (only block and ban) when there is no entry or no limits.
func (c *Config) ProjectLimits(id string) Limits {
	if p := c.FindProject(id); p != nil && p.Limits != nil {
		return *p.Limits
	}
	return Limits{}
}

// KeyLimits are the key's own ceiling (keys[].limits), counted on the key
// entity; zero when the key declares none. They never raise the account.
func KeyLimits(k *KeyConfig) Limits {
	if k == nil || k.Limits == nil {
		return Limits{}
	}
	return *k.Limits
}

// IsZero reports whether no limit is declared (the entity then only carries
// block and ban, ADR 0051).
func (l Limits) IsZero() bool {
	if len(l.SpendMonthlyModelsUSD) > 0 {
		return false
	}
	l.SpendMonthlyModelsUSD = nil
	return reflect.ValueOf(l).IsZero()
}

// LevelLimits are the limits of one level (account, project or key) of a
// key. ID is the entity id (empty = the key has no such level).
type LevelLimits struct {
	ID string
	// Tier is the tier used for the account level (empty for the others).
	Tier   string
	Limits Limits
}

// EffectiveLimits are the three levels a request of a key is counted on.
type EffectiveLimits struct {
	Account LevelLimits
	Project LevelLimits
	Key     LevelLimits
}

// LimitsFor resolves the limits of every level of a key. accountTier is the
// tier the cluster assigned to the account (ent:account:<id> of
// cfg:policies, promotion); empty = the configured AccountTier. Precedence
// (ADR 0051): account = accounts[].limits > accountTier > configured tier;
// project = projects[].limits; key = keys[].limits. A nil key resolves to no
// level at all.
func (c *Config) LimitsFor(k *KeyConfig, accountTier string) EffectiveLimits {
	var e EffectiveLimits
	if k == nil {
		return e
	}
	if k.Account != "" {
		tier := accountTier
		if tier == "" {
			tier = c.AccountTier(k.Account)
		}
		e.Account = LevelLimits{ID: k.Account, Tier: tier, Limits: c.AccountLimits(k.Account, tier)}
	}
	if k.Project != "" {
		e.Project = LevelLimits{ID: k.Project, Limits: c.ProjectLimits(k.Project)}
	}
	e.Key = LevelLimits{ID: k.Label, Limits: KeyLimits(k)}
	return e
}

// PromotionUsage is the accumulated history of an account the store reports
// after each request (fields of P:ent:E) and after a purchase.
type PromotionUsage struct {
	PurchasedUSD float64
	SpentUSD     float64
	Age          time.Duration // since the account was first seen
	Requests     int64
}

// Satisfied reports whether u meets every non-zero condition of p.
func (p Promote) Satisfied(u PromotionUsage) bool {
	return u.PurchasedUSD >= p.PurchasedUSD &&
		u.SpentUSD >= p.SpendUSD &&
		u.Age >= time.Duration(p.AgeDays)*24*time.Hour &&
		u.Requests >= p.Requests
}

// Promotion returns the tier an account on tier moves to, one step, when
// tier has promote_when and u satisfies it; ok is false otherwise.
func (c *Config) Promotion(tier string, u PromotionUsage) (next string, ok bool) {
	t, found := c.Tiers[tier]
	if !found || t.PromoteWhen == nil || t.Next == "" || !t.PromoteWhen.Satisfied(u) {
		return "", false
	}
	return t.Next, true
}

// Warnings lists the legacy forms Validate translated (ADR 0048) and the
// accounts[] and projects[] entries no key refers to: valid, but probably a
// typo.
func (c *Config) Warnings() []string {
	accts, projs := map[string]bool{}, map[string]bool{}
	for _, k := range c.Keys {
		accts[k.Account] = true
		projs[k.Project] = true
	}
	w := slices.Clone(c.warnings)
	for i, a := range c.Accounts {
		if !accts[a.ID] {
			w = append(w, fmt.Sprintf("config: accounts[%d] %q is not referenced by any key", i, a.ID))
		}
	}
	for i, p := range c.Projects {
		if !projs[p.ID] {
			w = append(w, fmt.Sprintf("config: projects[%d] %q is not referenced by any key", i, p.ID))
		}
	}
	return w
}

// validateTierChain checks next and promote_when of every tier: next names
// an existing tier, promote_when has a condition and a next, and the next
// chain has no cycle.
func (c *Config) validateTierChain() error {
	names := slices.Sorted(maps.Keys(c.Tiers))
	for _, name := range names {
		t := c.Tiers[name]
		if t.Next != "" {
			if _, ok := c.Tiers[t.Next]; !ok {
				return fmt.Errorf("config: tiers.%s.next: unknown tier %q", name, t.Next)
			}
		}
		if p := t.PromoteWhen; p != nil {
			if err := p.validate("tiers." + name + ".promote_when"); err != nil {
				return err
			}
			if t.Next == "" {
				return fmt.Errorf("config: tiers.%s.promote_when requires next", name)
			}
		}
	}
	for _, name := range names {
		path := []string{name}
		for cur := c.Tiers[name].Next; cur != ""; cur = c.Tiers[cur].Next {
			path = append(path, cur)
			if cur == name {
				return fmt.Errorf("config: tiers.%s: cycle in next (%s)", name, strings.Join(path, " -> "))
			}
			if len(path) > len(c.Tiers) {
				break // a cycle that does not pass through name: reported from its own tier
			}
		}
	}
	return nil
}

func (p Promote) validate(path string) error {
	if p.PurchasedUSD < 0 || math.IsNaN(p.PurchasedUSD) {
		return fmt.Errorf("config: %s.purchased_usd must not be negative, got %v", path, p.PurchasedUSD)
	}
	if p.SpendUSD < 0 || math.IsNaN(p.SpendUSD) {
		return fmt.Errorf("config: %s.spend_usd must not be negative, got %v", path, p.SpendUSD)
	}
	if p.AgeDays < 0 {
		return fmt.Errorf("config: %s.age_days must not be negative, got %d", path, p.AgeDays)
	}
	if p.Requests < 0 {
		return fmt.Errorf("config: %s.requests must not be negative, got %d", path, p.Requests)
	}
	if p == (Promote{}) {
		return fmt.Errorf("config: %s needs at least one condition > 0", path)
	}
	return nil
}

// validateAccounts checks accounts[] and projects[] (ids, tier names,
// limits) and that the keys of one account agree on its tier.
func (c *Config) validateAccounts() error {
	seen := map[string]bool{}
	for i, a := range c.Accounts {
		if a.ID == "" {
			return fmt.Errorf("config: accounts[%d].id is required", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("config: accounts[%d]: duplicate account id %q", i, a.ID)
		}
		seen[a.ID] = true
		if a.Tier != "" && !ValidTierName(a.Tier) {
			return fmt.Errorf("config: accounts[%d].tier: invalid tier name %q (want [A-Za-z0-9._-]{1,64})", i, a.Tier)
		}
		if a.Limits != nil {
			if err := c.validateLimits(fmt.Sprintf("accounts[%d].limits", i), *a.Limits); err != nil {
				return err
			}
		}
	}
	seen = map[string]bool{}
	for i, p := range c.Projects {
		if p.ID == "" {
			return fmt.Errorf("config: projects[%d].id is required", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("config: projects[%d]: duplicate project id %q", i, p.ID)
		}
		seen[p.ID] = true
		if p.Limits != nil {
			if err := c.validateLimits(fmt.Sprintf("projects[%d].limits", i), *p.Limits); err != nil {
				return err
			}
		}
	}
	// The tier of an account is accounts[].tier, else the first key that
	// declares one; every other key of the account must say the same.
	tiers := map[string]string{}
	for _, a := range c.Accounts {
		if a.Tier != "" {
			tiers[a.ID] = a.Tier
		}
	}
	for i, k := range c.Keys {
		if k.Tier == "" || k.Account == "" {
			continue
		}
		t, ok := tiers[k.Account]
		if !ok {
			tiers[k.Account] = k.Tier
			continue
		}
		if t != k.Tier {
			return fmt.Errorf("config: keys[%d].tier %q conflicts with account %q tier %q", i, k.Tier, k.Account, t)
		}
	}
	return nil
}

// validateLimits checks a limits block: its enums and ranges and the model
// keys of quota_usd.models and spend_monthly_models_usd (ADR 0072).
func (c *Config) validateLimits(path string, l Limits) error {
	if err := l.validate(path); err != nil {
		return err
	}
	if q := l.QuotaUSD; q != nil {
		if err := c.validateModelAmounts(path+".quota_usd.models", q.Models); err != nil {
			return err
		}
	}
	return c.validateModelAmounts(path+".spend_monthly_models_usd", l.SpendMonthlyModelsUSD)
}

// validateModelAmounts checks a model → US$ map: every key names a model
// (ModelKeyKnown) and every amount is >= 0.
func (c *Config) validateModelAmounts(path string, amounts map[string]float64) error {
	for _, k := range slices.Sorted(maps.Keys(amounts)) {
		if !c.ModelKeyKnown(k) {
			return fmt.Errorf("config: %s: unknown model %q", path, k)
		}
		if v := amounts[k]; v < 0 || math.IsNaN(v) {
			return fmt.Errorf("config: %s[%q] must not be negative, got %v", path, k, v)
		}
	}
	return nil
}

// ModelKeyKnown reports whether k is a key of quota_usd.models or
// spend_monthly_models_usd: a models[].id (every catalog), or
// <catalog>/<id> with a catalog some driver declares, the model in that
// catalog and served by a driver that declares it (ADR 0072). An id that
// contains "/" itself is matched whole first.
func (c *Config) ModelKeyKnown(k string) bool {
	if c.modelByID(k) != nil {
		return true
	}
	catalog, id, ok := strings.Cut(k, "/")
	if !ok {
		return false
	}
	m := c.modelByID(id)
	if m == nil || !m.InCatalog(catalog) {
		return false
	}
	for _, d := range declaredDrivers() {
		decl, _ := declOf(d)
		if m.ModelSupports(d) && slices.ContainsFunc(decl.Catalogs, func(cd CatalogDecl) bool { return cd.Name == catalog }) {
			return true
		}
	}
	return false
}

// modelByID is the models[] entry with that id (aliases do not count).
func (c *Config) modelByID(id string) *ModelConfig {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

// declaredPlans are the plans the registered drivers declare, sorted and
// without repeats.
func declaredPlans() []string {
	var out []string
	for _, d := range declaredDrivers() {
		decl, _ := declOf(d)
		out = append(out, decl.Plans...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// validatePlan checks tiers.<name>.plan: empty (the driver's default) or a
// plan some registered driver declares.
func validatePlan(name, plan string) error {
	if plan == "" {
		return nil
	}
	plans := declaredPlans()
	if slices.Contains(plans, plan) {
		return nil
	}
	if len(plans) == 0 {
		return fmt.Errorf("config: tiers.%s.plan: unknown plan %q (no driver declares plans)", name, plan)
	}
	return fmt.Errorf("config: tiers.%s.plan: unknown plan %q (want %s)", name, plan, strings.Join(plans, ", "))
}

// reservedBy is the mounted driver that reserves the key value k (ADR
// 0072: the opencode public key), or "".
func (c *Config) reservedBy(k string) string {
	for _, d := range c.Drivers {
		decl, _ := declOf(d.Name)
		if slices.Contains(decl.ReservedKeys, k) {
			return d.Name
		}
	}
	return ""
}
