package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
)

// Entity kinds and statuses of the contract.
var (
	entityKinds = []string{
		string(limits.KindIP), string(limits.KindKey), string(limits.KindUser), string(limits.KindAccount), string(limits.KindProject),
	}
	entityStatuses = []string{
		string(limits.StatusActive), string(limits.StatusLimited), string(limits.StatusSuspended), string(limits.StatusBanned),
	}
)

// maxEntityScan is the most entities read to serve one page.
const maxEntityScan = 10000

// ParseEntity validates kind and id (path parameters).
func ParseEntity(kind, id string) (limits.Entity, error) {
	if !slices.Contains(entityKinds, kind) {
		return limits.Entity{}, invalid("kind inválido", "kind: use "+strings.Join(entityKinds, ", "))
	}
	if id == "" {
		return limits.Entity{}, invalid("id vazio")
	}
	return limits.Entity{Kind: limits.EntityKind(kind), ID: id}, nil
}

// ---- cursors ----

func encodeCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeCursor(c, prefix string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	rest, ok := strings.CutPrefix(string(b), prefix)
	if err != nil || !ok {
		return "", invalid("cursor inválido")
	}
	return rest, nil
}

func offsetCursor(c, prefix string) (int, error) {
	if c == "" {
		return 0, nil
	}
	s, err := decodeCursor(c, prefix)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, invalid("cursor inválido")
	}
	return n, nil
}

// ---- list ----

// EntitiesQuery selects Entities.
type EntitiesQuery struct {
	Status string // "" = all
	Kind   string // "" = all
	Limit  int    // 1..1000 (<= 0 = 100)
	Cursor string
}

// EntityList is the body of GET /api/v1/entities.
type EntityList struct {
	Entities   []limits.EntityState `json:"entities"`
	NextCursor string               `json:"next_cursor,omitempty"`
	Source     string               `json:"source"`
}

// Entities lists entities from Valkey (idx:ent / idx:status:*) or, without
// Valkey, from GET /admin/entities, newest first, paginated by an offset
// cursor.
func (a *API) Entities(ctx context.Context, q EntitiesQuery) (EntityList, error) {
	if q.Status != "" && !slices.Contains(entityStatuses, q.Status) {
		return EntityList{}, invalid("status inválido", "status: use "+strings.Join(entityStatuses, ", "))
	}
	if q.Kind != "" && !slices.Contains(entityKinds, q.Kind) {
		return EntityList{}, invalid("kind inválido", "kind: use "+strings.Join(entityKinds, ", "))
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	off, err := offsetCursor(q.Cursor, "o:")
	if err != nil {
		return EntityList{}, err
	}
	want := min(off+q.Limit+1, maxEntityScan)
	if q.Kind != "" {
		want = maxEntityScan
	}
	var list []limits.EntityState
	out := EntityList{Source: SourceValkey}
	if a.vk != nil {
		list, err = a.vk.Entities(ctx, limits.Status(q.Status), want)
	} else {
		out.Source = SourceAdmin
		list, err = a.admin.Entities(ctx, limits.Status(q.Status), want)
	}
	if err != nil {
		return EntityList{}, err
	}
	if q.Kind != "" {
		list = slices.DeleteFunc(list, func(s limits.EntityState) bool { return string(s.Entity.Kind) != q.Kind })
	}
	page := list[min(off, len(list)):]
	if len(page) > q.Limit {
		page = page[:q.Limit]
		out.NextCursor = encodeCursor("o:" + strconv.Itoa(off+q.Limit))
	}
	out.Entities = append([]limits.EntityState{}, page...)
	return out, nil
}

func (a *API) listEntities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 100, 1, 1000)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	list, err := a.Entities(r.Context(), EntitiesQuery{Status: q.Get("status"), Kind: q.Get("kind"), Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.fail(w, err, a.entitySource())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) entitySource() string {
	if a.vk != nil {
		return "valkey"
	}
	return "admin"
}

// ---- detail ----

// LiveLimit is one limit of EntityDetail.
type LiveLimit struct {
	Name      string     `json:"name"`
	Window    string     `json:"window"`
	Limit     float64    `json:"limit"`
	Remaining float64    `json:"remaining"`
	Reset     *time.Time `json:"reset,omitempty"`
}

// EntityDetail is the body of GET /api/v1/entities/{kind}/{id}.
type EntityDetail struct {
	State         limits.EntityState `json:"state"`
	EffectiveTier string             `json:"effective_tier,omitempty"`
	// AccountID is the account whose tier EffectiveTier is, for a key.
	AccountID string      `json:"account_id,omitempty"`
	Limits    []LiveLimit `json:"limits"`
	Source    string      `json:"source"`
}

// Entity returns the state of e and, with Valkey, its live limits computed
// with the effective limits of its tier (/admin/config + cfg:policies). A
// failure to read the tier limits leaves Limits empty instead of failing.
func (a *API) Entity(ctx context.Context, e limits.Entity) (EntityDetail, error) {
	if a.vk == nil {
		st, err := a.admin.Entity(ctx, e)
		if err != nil {
			return EntityDetail{}, err
		}
		d := EntityDetail{State: st, Limits: []LiveLimit{}, Source: SourceAdmin}
		if e.Kind == limits.KindKey {
			d.AccountID = a.keyAccount(ctx, e.ID)
		}
		return d, nil
	}
	st, err := a.vk.Entity(ctx, e)
	if err != nil {
		return EntityDetail{}, err
	}
	out := EntityDetail{State: st, Limits: []LiveLimit{}, Source: SourceValkey}
	tier, acct, lim, err := a.effectiveLimits(ctx, e, st)
	if err == nil {
		var live []source.LimitState
		live, err = a.vk.Limits(ctx, e, lim)
		out.EffectiveTier, out.AccountID = tier, acct
		for _, l := range live {
			ll := LiveLimit{Name: l.Name, Window: l.Window, Limit: l.Limit, Remaining: l.Remaining}
			if ll.Window == "" {
				ll.Window = "none"
			}
			if !l.Reset.IsZero() {
				t := l.Reset.UTC()
				ll.Reset = &t
			}
			out.Limits = append(out.Limits, ll)
		}
	}
	if err != nil {
		a.log.Warn("painel: limites ao vivo", "entity", e, "err", err)
	}
	return out, nil
}

// adminConfig is the YAML of /admin/config decoded (no defaults applied).
func (a *API) adminConfig(ctx context.Context) (config.Config, error) {
	var c config.Config
	raw, err := a.admin.Config(ctx)
	if err != nil {
		return c, err
	}
	err = yaml.Unmarshal(raw, &c)
	return c, err
}

// keyAccount is the account of the key labeled label in /admin/config ("" if
// the configuration is unavailable or has no such key).
func (a *API) keyAccount(ctx context.Context, label string) string {
	c, err := a.adminConfig(ctx)
	if err != nil {
		return ""
	}
	for _, k := range c.Keys {
		if k.Label == label {
			return k.Account
		}
	}
	return ""
}

// effectiveLimits resolves what e is counted against (ADR 0051): an
// account has the tier (override on the entity or in cfg:policies, then
// accounts[]/keys[].tier, then default) with accounts[].limits on top; a
// key shows the tier of its account but only counts its own keys[].limits;
// a project counts projects[].limits; ip and user have no tier. The tiers of
// /admin/config are replaced by the tier:<name> of cfg:policies. The second
// result is the account of a key ("" for the other kinds).
func (a *API) effectiveLimits(ctx context.Context, e limits.Entity, st limits.EntityState) (string, string, config.Limits, error) {
	c, err := a.adminConfig(ctx)
	if err != nil {
		return "", "", config.Limits{}, err
	}
	pol, err := a.vk.Policies(ctx)
	if err != nil {
		return "", "", config.Limits{}, err
	}
	if c.Tiers == nil {
		c.Tiers = map[string]config.Tier{}
	}
	for name, tr := range pol.Tiers {
		var l config.Limits
		if yaml.Unmarshal(tr, &l) == nil {
			c.Tiers[name] = config.Tier{Limits: l}
		}
	}
	switch e.Kind {
	case limits.KindAccount:
		tier := cmp.Or(st.Tier, pol.Entities[e])
		tier = cmp.Or(tier, c.AccountTier(e.ID))
		return tier, "", c.AccountLimits(e.ID, tier), nil
	case limits.KindProject:
		return "", "", c.ProjectLimits(e.ID), nil
	case limits.KindKey:
		var k *config.KeyConfig
		for i := range c.Keys {
			if c.Keys[i].Label == e.ID {
				k = &c.Keys[i]
			}
		}
		if k == nil {
			return "", "", config.Limits{}, nil
		}
		acct := limits.Entity{Kind: limits.KindAccount, ID: k.Account}
		var as limits.EntityState
		if k.Account != "" {
			as, _ = a.vk.Entity(ctx, acct)
		}
		// ent:account wins over the legacy ent:key (ADR 0051 §5).
		tier := cmp.Or(as.Tier, pol.Entities[acct], pol.Entities[e])
		return cmp.Or(tier, c.AccountTier(k.Account)), k.Account, config.KeyLimits(k), nil
	}
	return "", "", config.Limits{}, nil
}

func (a *API) getEntity(w http.ResponseWriter, r *http.Request) {
	e, err := ParseEntity(r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		a.fail(w, err, "")
		return
	}
	d, err := a.Entity(r.Context(), e)
	if err != nil {
		a.fail(w, err, a.entitySource())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// timeParam parses an RFC 3339 query parameter ("" = zero).
func timeParam(q url.Values, name string) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, invalid(name+" inválido", name+": use RFC 3339")
	}
	return t, nil
}
