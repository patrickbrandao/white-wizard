package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/valkey-io/valkey-go"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// Policies are the cluster policies of cfg:policies (ADR 0025,
// cache/valkey/README.md 2.3): tiers that replace (or add to) the tiers of
// the YAML and the tier assigned to single entities.
type Policies struct {
	// Version is the version field; 0 = no policies.
	Version int64
	// Tiers are the tier:<name> fields.
	Tiers map[string]config.Limits
	// Entities are the ent:<kind>:<id> fields (tier name per entity).
	Entities map[Entity]string
}

// PolicySource is implemented by stores that keep cluster policies.
type PolicySource interface {
	// Policies reads the current policies. A transport error comes with a
	// zero Policies; an invalid document comes with its Version and the
	// parse error.
	Policies(ctx context.Context) (Policies, error)
	// WatchPolicies registers fn to receive the policies read after every
	// change notification and every (re)subscription. fn may be called
	// concurrently and more than once for the same version.
	WatchPolicies(fn func(Policies, error))
}

// TierSetter is implemented by stores that can assign a tier to an entity
// (POST /admin/entities/{tipo}/{id}/tier). An empty tier removes it.
type TierSetter interface {
	SetTier(ctx context.Context, e Entity, tier string) error
}

// EntityResetter is implemented by stores that can zero the counters and/or
// the strikes of one entity (POST /admin/entities/{tipo}/{id}/reset),
// keeping block, ban, credits and tier. It never creates the entity.
type EntityResetter interface {
	ResetEntity(ctx context.Context, e Entity, counters, strikes bool) error
}

// Both production stores implement the optional operations.
var (
	_ PolicySource   = (*Memory)(nil)
	_ PolicySource   = (*Fallback)(nil)
	_ PolicySource   = (*Valkey)(nil)
	_ TierSetter     = (*Fallback)(nil)
	_ EntityResetter = (*Fallback)(nil)
	_ TierSetter     = (*Memory)(nil)
	_ EntityResetter = (*Memory)(nil)
	_ TierSwapper    = (*Memory)(nil)
	_ TierSwapper    = (*Valkey)(nil)
	_ TierSwapper    = (*Fallback)(nil)
	_ CreditAdder    = (*Memory)(nil)
	_ CreditAdder    = (*Valkey)(nil)
	_ CreditAdder    = (*Fallback)(nil)
	_ Escalator      = (*Memory)(nil)
	_ Escalator      = (*Valkey)(nil)
	_ Escalator      = (*Fallback)(nil)
)

// policyField is the cfg:policies field of an entity override.
func policyField(e Entity) string { return "ent:" + string(e.Kind) + ":" + e.ID }

// ParsePolicies decodes the fields of cfg:policies. Unknown fields are
// ignored. On error the returned Policies still carries the version, so a
// refused version is not tried again.
func ParsePolicies(h map[string]string) (Policies, error) {
	p := Policies{Tiers: map[string]config.Limits{}, Entities: map[Entity]string{}}
	if v, ok := h["version"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Policies{}, fmt.Errorf("policies: version %q is not a non-negative integer", v)
		}
		p.Version = n
	}
	var errs []error
	for _, f := range slices.Sorted(maps.Keys(h)) {
		val := h[f]
		switch {
		case strings.HasPrefix(f, "tier:"):
			name := strings.TrimPrefix(f, "tier:")
			if name == "" {
				errs = append(errs, errors.New("policies: field tier: without a name"))
				continue
			}
			l, err := parseTierLimits(val)
			if err != nil {
				errs = append(errs, fmt.Errorf("policies: %s: %w", f, err))
				continue
			}
			p.Tiers[name] = l
		case strings.HasPrefix(f, "ent:"):
			kind, id, ok := strings.Cut(strings.TrimPrefix(f, "ent:"), ":")
			if !ok || kind == "" || id == "" {
				errs = append(errs, fmt.Errorf("policies: field %q is not ent:<tipo>:<id>", f))
				continue
			}
			if val != "" {
				p.Entities[Entity{Kind: EntityKind(kind), ID: id}] = val
			}
		}
	}
	return p, errors.Join(errs...)
}

// parseTierLimits decodes the JSON of a tier:<name> field (limits with the
// YAML names; JSON is YAML, so durations are Go strings like "30s").
func parseTierLimits(s string) (config.Limits, error) {
	var l config.Limits
	var probe map[string]any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return l, fmt.Errorf("not a JSON object: %w", err)
	}
	if err := yaml.UnmarshalWithOptions([]byte(s), &l, yaml.Strict()); err != nil {
		return l, err
	}
	return l, nil
}

// appliedPolicies records what applying policies changed in a
// configuration, so the base (YAML + admin patches) can be restored.
type appliedPolicies struct {
	version   int64
	tiers     map[string]config.Tier  // value written
	origTiers map[string]*config.Tier // value before (nil = absent)
}

func findKey(c *config.Config, label string) *config.KeyConfig {
	for i := range c.Keys {
		if c.Keys[i].Label == label {
			return &c.Keys[i]
		}
	}
	return nil
}

// apply writes p on c: tier:<name> replaces the tier of the same name (or
// adds it). The ent:* fields are only checked (their tier must exist): the
// tier of an account is read at request time through
// PolicyApplier.AccountTier, never written in the configuration (ADR 0051,
// task 191). It validates the result and, on error, leaves c as it was.
func (p *Policies) apply(c *config.Config) (*appliedPolicies, error) {
	for _, e := range slices.SortedFunc(maps.Keys(p.Entities), func(a, b Entity) int {
		return strings.Compare(policyField(a), policyField(b))
	}) {
		t := p.Entities[e]
		_, inYAML := c.Tiers[t]
		_, inPol := p.Tiers[t]
		if !inYAML && !inPol {
			return nil, fmt.Errorf("policies: %s: unknown tier %q", policyField(e), t)
		}
	}
	a := &appliedPolicies{version: p.Version, tiers: map[string]config.Tier{}, origTiers: map[string]*config.Tier{}}
	if c.Tiers == nil {
		c.Tiers = map[string]config.Tier{}
	}
	for name, l := range p.Tiers {
		t := config.Tier{Limits: l}
		if old, ok := c.Tiers[name]; ok {
			a.origTiers[name] = &old
			// tier:<name> carries only limits: promote_when, next and plan
			// of the YAML tier stay (ADR 0051, 0072).
			t.PromoteWhen, t.Next, t.Plan = old.PromoteWhen, old.Next, old.Plan
		} else {
			a.origTiers[name] = nil
		}
		c.Tiers[name] = t
		a.tiers[name] = t
	}
	if err := c.Validate(); err != nil {
		a.restore(c)
		return nil, fmt.Errorf("policies: version %d: %w", p.Version, err)
	}
	return a, nil
}

// AccountTiers resolves the ent:* fields to the tier of each account (ADR
// 0051): ent:account:<id> wins over a legacy ent:key:<label>, which counts
// for the account of that key in c (unknown keys are ignored; among legacy
// fields of one account the last label in byte order wins). Other kinds
// carry no tier.
func (p *Policies) AccountTiers(c *config.Config) map[string]string {
	out := map[string]string{}
	for _, e := range slices.SortedFunc(maps.Keys(p.Entities), func(a, b Entity) int {
		return strings.Compare(policyField(a), policyField(b))
	}) {
		if e.Kind != KindKey {
			continue
		}
		if k := findKey(c, e.ID); k != nil && k.Account != "" {
			out[k.Account] = p.Entities[e]
		}
	}
	for e, t := range p.Entities {
		if e.Kind == KindAccount {
			out[e.ID] = t
		}
	}
	return out
}

// restore undoes a on c: every value that is still the one a wrote goes
// back to the value before it (a patch that changed it explicitly wins).
func (a *appliedPolicies) restore(c *config.Config) {
	if a == nil {
		return
	}
	for name, v := range a.tiers {
		cur, ok := c.Tiers[name]
		if !ok || !reflect.DeepEqual(cur, v) {
			continue
		}
		if orig := a.origTiers[name]; orig != nil {
			c.Tiers[name] = *orig
		} else {
			delete(c.Tiers, name)
		}
	}
}

// cloneConfig deep-copies a configuration the same way POST /admin/config
// does (YAML round trip).
func cloneConfig(c *config.Config) (*config.Config, error) {
	if c == nil {
		return nil, errors.New("policies: no configuration")
	}
	return config.Parse(mustMarshalYAML(c))
}

// mustMarshalYAML marshals a *config.Config, which always marshals: a failure
// is a bug and panics instead of becoming an unreachable error path.
func mustMarshalYAML(v any) []byte {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic("limits: yaml.Marshal of a configuration failed: " + err.Error())
	}
	return b
}

// PolicyApplier applies cluster policies on top of the configuration of the
// YAML (with admin patches), through the same validation and atomic swap of
// POST /admin/config (ADR 0025). Wire Override into admin.Options.Override
// (after the flags), Committed into admin.Server.OnReload and Offer into
// PolicySource.WatchPolicies.
type PolicyApplier struct {
	// Current returns the effective configuration (admin.Server.Config).
	Current func() *config.Config
	// Apply validates and swaps a configuration (admin.Server.Apply); it
	// runs Override and, on success, Committed.
	Apply func(*config.Config) error
	// OnVersion receives the version of every policy set applied
	// (wwz_policies_version).
	OnVersion func(int64)
	// OnError is called for every refused version (wwz_policies_errors_total).
	OnError func()
	Log     *slog.Logger

	offerMu sync.Mutex // serialises Offer

	mu      sync.Mutex
	seen    int64     // highest version offered (applied or refused)
	want    *Policies // policies Override applies
	cur     *Policies // policies of the effective configuration
	applied *appliedPolicies
	cand    *config.Config // configuration of the last Override
	candA   *appliedPolicies
	candP   *Policies

	// tierSnap is the tier of every account under the policies in effect,
	// read lock-free on every request (AccountTier, promote.go).
	tierSnap atomic.Pointer[map[string]string]
}

func (a *PolicyApplier) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// EntityTier is the raw ent:<tipo>:<id> field of the policies in effect
// ("" = absent): the expected value of TierSwapper.SwapTier when promoting
// (ADR 0051). The effective tier of an account is AccountTier.
func (a *PolicyApplier) EntityTier(e Entity) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return ""
	}
	return a.cur.Entities[e]
}

// Version is the version of the policies in effect (0 = none).
func (a *PolicyApplier) Version() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return 0
	}
	return a.cur.Version
}

// Override turns a new configuration (file reload, patch or policy change)
// into base + policies: it first restores what the previous application
// wrote (a patch starts from the effective configuration), then applies the
// wanted policies, falling back to the ones in effect when they no longer
// validate.
func (a *PolicyApplier) Override(c *config.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applied.restore(c)
	a.cand, a.candA, a.candP = c, nil, nil
	if c.Validate() != nil {
		return // the caller refuses it anyway
	}
	for _, p := range []*Policies{a.want, a.cur} {
		if p == nil {
			continue
		}
		ap, err := p.apply(c)
		if err != nil {
			a.log().Warn("policies not applied to the new configuration", "version", p.Version, "err", err)
			continue
		}
		a.candA, a.candP = ap, p
		return
	}
}

// Committed records that c is now the effective configuration.
func (a *PolicyApplier) Committed(c *config.Config) {
	a.mu.Lock()
	if c != a.cand {
		cur := a.cur
		a.mu.Unlock()
		a.refreshTiers(cur, c) // keys may have moved between accounts
		return
	}
	a.applied, a.cur = a.candA, a.candP
	a.cand, a.candA, a.candP = nil, nil, nil
	v := int64(0)
	if a.cur != nil {
		v = a.cur.Version
	}
	cur := a.cur
	a.mu.Unlock()
	a.refreshTiers(cur, c)
	if a.OnVersion != nil {
		a.OnVersion(v)
	}
}

// Offer applies p when its version is newer than every version seen;
// invalid policies are logged, counted and skipped (the previous ones stay).
func (a *PolicyApplier) Offer(p Policies, err error) {
	a.offerMu.Lock()
	defer a.offerMu.Unlock()
	a.mu.Lock()
	if p.Version <= a.seen {
		a.mu.Unlock()
		if err != nil && p.Version == 0 {
			a.log().Debug("policies: read failed", "err", err)
		}
		return
	}
	a.seen = p.Version
	applied := a.applied
	a.mu.Unlock()
	if err != nil {
		a.reject(p.Version, err)
		return
	}
	// Dry run on a copy, so an invalid version never reaches Apply.
	x, err := cloneConfig(a.Current())
	if err == nil {
		applied.restore(x)
		_, err = p.apply(x)
	}
	if err != nil {
		a.reject(p.Version, err)
		return
	}
	a.mu.Lock()
	prev := a.want
	a.want = &p
	a.mu.Unlock()
	c, err := cloneConfig(a.Current())
	if err == nil {
		err = a.Apply(c)
	}
	a.mu.Lock()
	ok := err == nil && a.cur != nil && a.cur.Version == p.Version
	if !ok {
		a.want = prev
	}
	a.mu.Unlock()
	if !ok {
		if err == nil {
			err = errors.New("not applied")
		}
		a.reject(p.Version, err)
		return
	}
	a.log().Info("policies applied", "version", p.Version, "tiers", len(p.Tiers), "entities", len(p.Entities))
}

func (a *PolicyApplier) reject(version int64, err error) {
	a.log().Error("policies refused; keeping the previous ones", "version", version, "err", err)
	if a.OnError != nil {
		a.OnError()
	}
}

// ---- Memory ----

// memPolicies is the local policy state of the memory store.
type memPolicies struct {
	mu  sync.Mutex
	p   Policies
	fns []func(Policies, error)
}

func (mp *memPolicies) snapshot() Policies {
	p := Policies{Version: mp.p.Version, Tiers: maps.Clone(mp.p.Tiers), Entities: maps.Clone(mp.p.Entities)}
	if p.Tiers == nil {
		p.Tiers = map[string]config.Limits{}
	}
	if p.Entities == nil {
		p.Entities = map[Entity]string{}
	}
	return p
}

func (mp *memPolicies) notify() {
	mp.mu.Lock()
	p := mp.snapshot()
	fns := slices.Clone(mp.fns)
	mp.mu.Unlock()
	for _, fn := range fns {
		fn(p, nil)
	}
}

// Policies implements PolicySource: the policies of this instance.
func (m *Memory) Policies(context.Context) (Policies, error) {
	m.pol.mu.Lock()
	defer m.pol.mu.Unlock()
	return m.pol.snapshot(), nil
}

// WatchPolicies implements PolicySource.
func (m *Memory) WatchPolicies(fn func(Policies, error)) {
	m.pol.mu.Lock()
	m.pol.fns = append(m.pol.fns, fn)
	m.pol.mu.Unlock()
}

// SetPolicies replaces the local policies and notifies the watchers (the
// memory equivalent of the panel writing cfg:policies; tests).
func (m *Memory) SetPolicies(p Policies) {
	m.pol.mu.Lock()
	m.pol.p = Policies{Version: p.Version, Tiers: maps.Clone(p.Tiers), Entities: maps.Clone(p.Entities)}
	m.pol.mu.Unlock()
	m.pol.notify()
}

// SetTier implements TierSetter: the override is local to this instance.
func (m *Memory) SetTier(ctx context.Context, e Entity, tier string) error {
	_, err := m.setTier(ctx, e, tier, false, "")
	return err
}

// SwapTier implements TierSwapper (local to this instance).
func (m *Memory) SwapTier(ctx context.Context, e Entity, expected, tier string) (bool, error) {
	return m.setTier(ctx, e, tier, true, expected)
}

func (m *Memory) setTier(_ context.Context, e Entity, tier string, cas bool, expected string) (bool, error) {
	// The policy first (compare and write under one lock, like set_policy),
	// then the display field.
	m.pol.mu.Lock()
	if cas && m.pol.p.Entities[e] != expected {
		m.pol.mu.Unlock()
		return false, nil
	}
	if m.pol.p.Entities == nil {
		m.pol.p.Entities = map[Entity]string{}
	}
	if tier == "" {
		delete(m.pol.p.Entities, e)
	} else {
		m.pol.p.Entities[e] = tier
	}
	m.pol.p.Version++
	m.pol.mu.Unlock()
	m.mu.Lock()
	m.rec(e).tier = tier
	m.mu.Unlock()
	m.pol.notify()
	return true, nil
}

// ResetEntity implements EntityResetter.
func (m *Memory) ResetEntity(_ context.Context, e Entity, counters, strikes bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.ents[e]
	if !ok {
		return nil
	}
	if counters {
		r = &entityRec{entity: r.entity, since: r.since, block: r.block, ban: r.ban, credits: r.credits, purchased: r.purchased, strikes: r.strikes, esc: r.esc, tier: r.tier}
		m.ents[e] = r
	}
	if strikes {
		r.strikes, r.esc = nil, escRec{}
	}
	return nil
}

// ---- Valkey ----

// polWatch is the cfg:changed subscription of a Valkey store.
type polWatch struct {
	mu   sync.Mutex
	fns  []func(Policies, error)
	once sync.Once
	kick chan struct{}
}

func (v *Valkey) policiesKey() string { return v.p + ":cfg:policies" }
func (v *Valkey) changedChan() string { return v.p + ":cfg:changed" }

// Policies implements PolicySource (HGETALL cfg:policies).
func (v *Valkey) Policies(ctx context.Context) (Policies, error) {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	h, err := v.c.Do(cctx, v.c.B().Hgetall().Key(v.policiesKey()).Build()).AsStrMap()
	if err != nil {
		return Policies{}, err
	}
	return ParsePolicies(h)
}

// WatchPolicies implements PolicySource: the first call subscribes to
// cfg:changed on a dedicated connection, re-subscribing after every
// disconnection; every message and every subscription re-reads the hash.
func (v *Valkey) WatchPolicies(fn func(Policies, error)) {
	select {
	case <-v.stop:
		return
	default:
	}
	v.pw.mu.Lock()
	v.pw.fns = append(v.pw.fns, fn)
	v.pw.mu.Unlock()
	v.pw.once.Do(func() {
		v.pw.kick = make(chan struct{}, 1)
		v.wg.Go(v.subscribeLoop)
		v.wg.Go(v.reloadLoop)
	})
}

func (v *Valkey) kickPolicies() {
	select {
	case v.pw.kick <- struct{}{}:
	default:
	}
}

func (v *Valkey) subscribeLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-v.stop
		cancel()
	}()
	sctx := valkey.WithOnSubscriptionHook(ctx, func(s valkey.PubSubSubscription) {
		if s.Kind == "subscribe" {
			v.kickPolicies()
		}
	})
	for {
		err := v.c.Receive(sctx, v.c.B().Subscribe().Channel(v.changedChan()).Build(), func(valkey.PubSubMessage) { v.kickPolicies() })
		if ctx.Err() != nil {
			return
		}
		v.log.Debug("limits: cfg:changed subscription lost; re-subscribing", "err", err)
		select {
		case <-v.stop:
			return
		case <-time.After(time.Second):
		}
	}
}

func (v *Valkey) reloadLoop() {
	for {
		select {
		case <-v.stop:
			return
		case <-v.pw.kick:
		}
		v.deliverPolicies(context.Background())
	}
}

// deliverPolicies reads the policies and hands them to every watcher.
func (v *Valkey) deliverPolicies(ctx context.Context) {
	p, err := v.Policies(ctx)
	v.pw.mu.Lock()
	fns := slices.Clone(v.pw.fns)
	v.pw.mu.Unlock()
	for _, fn := range fns {
		fn(p, err)
	}
}

// SetTier implements TierSetter (README 2.3): the ent:<tipo>:<id> field of
// cfg:policies with version + 1, the display field tier of ent, then
// PUBLISH cfg:changed. The local watchers get the new version before it
// returns.
func (v *Valkey) SetTier(ctx context.Context, e Entity, tier string) error {
	_, err := v.setTier(ctx, e, tier, false, "")
	return err
}

// SwapTier implements TierSwapper: set_policy with cas = 1 (README 3.12);
// when it swaps, the rest of SetTier follows.
func (v *Valkey) SwapTier(ctx context.Context, e Entity, expected, tier string) (bool, error) {
	return v.setTier(ctx, e, tier, true, expected)
}

func (v *Valkey) setTier(ctx context.Context, e Entity, tier string, cas bool, expected string) (bool, error) {
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	flag := "0"
	if cas {
		flag = "1"
	}
	res := v.pipe(cctx, scriptCall(luaSetPolicy, []string{v.policiesKey()}, []string{policyField(e), tier, flag, expected}))
	version, err := res[0].AsInt64()
	if err != nil {
		return false, err
	}
	if version == 0 {
		return false, nil // cas: the field no longer holds expected
	}
	now := ms(time.Now())
	ent := v.key("ent", e)
	msg, _ := json.Marshal(map[string]int64{"version": version})
	// The policy is already written: publish even if the display write fails.
	// PUBLISH has no slot, so it goes alone: valkey-go refuses (panics) to mix
	// a no-slot command with keyed ones in one DoMulti under store.cluster.
	err = firstErr(v.pipe(cctx,
		scriptCall(luaSetTier, []string{ent}, []string{tier, now, string(e.Kind), e.ID}),
		cmdCall(v.idxEnt(), "ZADD", v.idxEnt(), "NX", now, member(e)),
	))
	pubErr := v.c.Do(cctx, v.c.B().Publish().Channel(v.changedChan()).Message(string(msg)).Build()).Error()
	v.deliverPolicies(cctx)
	if err != nil {
		return true, err
	}
	return true, pubErr
}

// ResetEntity implements EntityResetter (script reset_entity, README 3.9).
func (v *Valkey) ResetEntity(ctx context.Context, e Entity, counters, strikes bool) error {
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	keys := append(v.ekeys(e), v.key("strikes", e), v.key("esc", e))
	cctx, cancel := v.adminCtx(ctx)
	defer cancel()
	if err := firstErr(v.pipe(cctx, scriptCall(luaResetEntity, keys, []string{flag(counters), flag(strikes)}))); err != nil {
		return err
	}
	if counters && v.lf != nil {
		v.lf.forget(e)
	}
	return nil
}

// forget drops the local buckets of one entity (local_first).
func (lf *localFirst) forget(e Entity) {
	lf.mu.Lock()
	delete(lf.ents, e)
	lf.mu.Unlock()
}

// ---- Fallback ----

// Policies implements PolicySource; ErrStoreDown while the circuit is open.
func (f *Fallback) Policies(ctx context.Context) (Policies, error) {
	v, err := f.admin()
	if err != nil {
		return Policies{}, err
	}
	return v.Policies(ctx)
}

// WatchPolicies implements PolicySource. With Valkey unreachable at boot,
// the subscription starts as soon as the first connection succeeds.
func (f *Fallback) WatchPolicies(fn func(Policies, error)) {
	if v := f.Valkey(); v != nil {
		v.WatchPolicies(fn)
		return
	}
	f.wg.Go(func() {
		t := time.NewTicker(f.probe)
		defer t.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-t.C:
			}
			if v := f.Valkey(); v != nil {
				v.WatchPolicies(fn)
				return
			}
		}
	})
}

// SetTier implements TierSetter.
func (f *Fallback) SetTier(ctx context.Context, e Entity, tier string) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.SetTier(ctx, e, tier)
}

// SwapTier implements TierSwapper; ErrStoreDown while the circuit is open.
func (f *Fallback) SwapTier(ctx context.Context, e Entity, expected, tier string) (bool, error) {
	v, err := f.admin()
	if err != nil {
		return false, err
	}
	return v.SwapTier(ctx, e, expected, tier)
}

// AddCredits implements CreditAdder; ErrStoreDown while the circuit is open.
func (f *Fallback) AddCredits(ctx context.Context, e Entity, usd float64) (EntityState, error) {
	v, err := f.admin()
	if err != nil {
		return EntityState{}, err
	}
	return v.AddCredits(ctx, e, usd)
}

// ResetEntity implements EntityResetter.
func (f *Fallback) ResetEntity(ctx context.Context, e Entity, counters, strikes bool) error {
	v, err := f.admin()
	if err != nil {
		return err
	}
	return v.ResetEntity(ctx, e, counters, strikes)
}
