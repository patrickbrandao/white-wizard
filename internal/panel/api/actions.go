package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Entity actions (tasks 049 and 053); audit action = "entity." + name.
const (
	ActionBlock   = "block"
	ActionUnblock = "unblock"
	ActionBan     = "ban"
	ActionUnban   = "unban"
	ActionForgive = "forgive"
	ActionLeak    = "leak"
	ActionCredits = "credits"
	ActionTier    = "tier"
	ActionReset   = "reset"
)

const maxActionBody = 64 << 10

// ActionRequest is the body of an entity action (fields used depend on the
// action: block reason/duration, ban and leak reason, credits usd, tier
// tier, reset scope).
type ActionRequest struct {
	Reason   string
	Duration time.Duration // block: 0 = until unblock
	USD      *float64      // credits: sets the balance (exactly one of USD and AddUSD)
	AddUSD   *float64      // credits: simulates a purchase, > 0 (ADR 0051)
	// Tier is the tier of the tier action ("" removes the override); HasTier
	// says it was sent (required).
	Tier    string
	HasTier bool
	// Scope of reset (counters, strikes); empty = the default of wwz
	// (counters).
	Scope []string
}

// ActionResult is the answer of an action: EntityAction (audit_id, entity)
// or, for leak, LeakAction (audit_id, key, account).
type ActionResult struct {
	AuditID int64               `json:"audit_id"`
	Entity  *limits.EntityState `json:"entity,omitempty"`
	Key     *limits.EntityState `json:"key,omitempty"`
	Account *limits.EntityState `json:"account,omitempty"`
}

// Actor is who runs an action (audit user, role and IP).
type Actor struct {
	Name string
	Role string
	IP   string
}

// ActorOf builds the actor of an authenticated request.
func (a *API) ActorOf(r *http.Request) Actor {
	act := Actor{IP: a.clientIP(r)}
	if id, ok := auth.FromContext(r.Context()); ok {
		act.Name, act.Role = id.Name, id.Role
	}
	return act
}

// record writes the audit row of an operator action: diff {request, before,
// after} (after dropped when the action failed) and the result. It returns
// the audit id; the error of the action as is; or a 500 when the audit row
// could not be written.
func (a *API) record(ctx context.Context, actor Actor, action, target string, sent, before, after any, err error) (int64, error) {
	entry := store.AuditEntry{
		User: actor.Name, Role: actor.Role, Action: action, Target: target, IP: actor.IP, Result: store.ResultOK,
	}
	if err != nil {
		entry.Result, entry.Error, after = store.ResultError, err.Error(), nil
	}
	entry.Diff, _ = store.NewDiff(sent, before, after)
	rec, rerr := a.st.Audit.Record(ctx, entry)
	if rerr != nil {
		a.log.Error("painel: gravar auditoria", "action", action, "target", target, "err", rerr)
		if err == nil {
			err = dbErr(rerr)
		}
	}
	if err != nil {
		return 0, err
	}
	return rec.ID, nil
}

// Act runs action on e through the wwz admin API and records the audit row
// (user, target kind/id, diff {request, before, after}, result). Failures of
// the admin are recorded with result error and returned (see ProblemOf);
// a failure to record the audit is a 500.
func (a *API) Act(ctx context.Context, actor Actor, action string, e limits.Entity, req ActionRequest) (ActionResult, error) {
	switch {
	case action == ActionLeak && e.Kind != limits.KindKey:
		return ActionResult{}, invalid("leak só vale para kind = key")
	case action == ActionCredits && (req.USD == nil) == (req.AddUSD == nil):
		return ActionResult{}, invalid("informe exatamente um entre usd e add_usd")
	case action == ActionCredits && req.AddUSD != nil && !(*req.AddUSD > 0):
		return ActionResult{}, invalid("add_usd deve ser maior que 0")
	case action == ActionTier && !req.HasTier:
		return ActionResult{}, invalid("tier é obrigatório", "tier: nome do tier, ou null para remover a sobreposição")
	}
	var before any
	if st, err := a.admin.Entity(ctx, e); err == nil {
		before = st
	}
	by := "panel:" + actor.Name
	var sent any
	var res ActionResult
	var err error
	one := func(st limits.EntityState, e error) {
		if err = e; e == nil {
			res.Entity = &st
		}
	}
	switch action {
	case ActionBlock:
		b := map[string]string{"by": by}
		if req.Reason != "" {
			b["reason"] = req.Reason
		}
		if req.Duration > 0 {
			b["duration"] = req.Duration.String()
		}
		sent = b
		one(a.admin.Block(ctx, e, source.BlockRequest{Reason: req.Reason, Duration: req.Duration, By: by}))
	case ActionUnblock:
		one(a.admin.Unblock(ctx, e))
	case ActionBan:
		b := map[string]string{"by": by}
		if req.Reason != "" {
			b["reason"] = req.Reason
		}
		sent = b
		one(a.admin.Ban(ctx, e, req.Reason, by))
	case ActionUnban:
		one(a.admin.Unban(ctx, e))
	case ActionForgive:
		sent = map[string][]string{"scope": {source.ResetStrikes}}
		one(a.admin.Reset(ctx, e, source.ResetStrikes))
	case ActionCredits:
		if req.AddUSD != nil {
			sent = map[string]float64{"add_usd": *req.AddUSD}
			one(a.admin.AddCredits(ctx, e, *req.AddUSD))
			break
		}
		sent = map[string]float64{"usd": *req.USD}
		one(a.admin.SetCredits(ctx, e, *req.USD))
	case ActionTier:
		sent = map[string]string{"tier": req.Tier}
		one(a.admin.SetTier(ctx, e, req.Tier))
	case ActionReset:
		if len(req.Scope) > 0 {
			sent = map[string][]string{"scope": req.Scope}
		}
		one(a.admin.Reset(ctx, e, req.Scope...))
	case ActionLeak:
		lr := source.LeakRequest{Label: e.ID, Reason: req.Reason}
		if lr.Reason == "" {
			lr.Reason = "leaked"
		}
		sent = lr
		var out source.LeakResult
		if out, err = a.admin.Leak(ctx, lr); err == nil {
			res.Key, res.Account = &out.Key, out.Account
		}
	default:
		return ActionResult{}, invalid("ação desconhecida: " + action)
	}
	var after any = map[string]any{"key": res.Key, "account": res.Account}
	if res.Entity != nil {
		after = res.Entity
	}
	id, err := a.record(ctx, actor, "entity."+action, string(e.Kind)+"/"+e.ID, sent, before, after, err)
	if err != nil {
		return ActionResult{}, err
	}
	res.AuditID = id
	return res, nil
}

// actionFields are the body fields each action accepts.
var actionFields = map[string][]string{
	ActionBlock: {"reason", "duration"}, ActionBan: {"reason"}, ActionLeak: {"reason"},
	ActionCredits: {"usd", "add_usd"}, ActionTier: {"tier"}, ActionReset: {"scope"},
}

// decodeAction reads the optional JSON body of action (unknown fields are
// refused, as in additionalProperties: false).
func decodeAction(action string, body []byte) (ActionRequest, error) {
	var req ActionRequest
	if len(bytes.TrimSpace(body)) == 0 {
		return req, nil
	}
	var in struct {
		Reason   *string         `json:"reason"`
		Duration *string         `json:"duration"`
		USD      *float64        `json:"usd"`
		AddUSD   *float64        `json:"add_usd"`
		Tier     json.RawMessage `json:"tier"`
		Scope    *[]string       `json:"scope"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return req, invalid("corpo JSON inválido", err.Error())
	}
	allowed := actionFields[action]
	for name, set := range map[string]bool{
		"reason": in.Reason != nil, "duration": in.Duration != nil, "usd": in.USD != nil, "add_usd": in.AddUSD != nil, "tier": in.Tier != nil, "scope": in.Scope != nil,
	} {
		if set && !slices.Contains(allowed, name) {
			return req, invalid("campo não aceito por esta ação", name)
		}
	}
	if in.Reason != nil {
		req.Reason = *in.Reason
	}
	if in.Duration != nil && *in.Duration != "" {
		d, err := time.ParseDuration(*in.Duration)
		if err != nil || d < 0 {
			return req, invalid("duration inválida", "duration: duração Go, ex.: 10m")
		}
		req.Duration = d
	}
	req.USD, req.AddUSD = in.USD, in.AddUSD
	if in.Tier != nil {
		req.HasTier = true
		if string(in.Tier) != "null" && json.Unmarshal(in.Tier, &req.Tier) != nil {
			return req, invalid("tier inválido", "tier: string ou null")
		}
	}
	if in.Scope != nil {
		if err := checkScope(*in.Scope); err != nil {
			return req, err
		}
		req.Scope = *in.Scope
	}
	return req, nil
}

// checkScope validates the scope of reset (EntityResetRequest).
func checkScope(scope []string) error {
	if len(scope) == 0 {
		return invalid("scope vazio", "scope: counters e/ou strikes")
	}
	seen := map[string]bool{}
	for _, s := range scope {
		if (s != source.ResetCounters && s != source.ResetStrikes) || seen[s] {
			return invalid("scope inválido", "scope: counters e/ou strikes, sem repetição")
		}
		seen[s] = true
	}
	return nil
}

// action is the handler of POST /api/v1/entities/{kind}/{id}/<name>.
func (a *API) action(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, err := ParseEntity(r.PathValue("kind"), r.PathValue("id"))
		if err != nil {
			a.fail(w, err, "")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBody))
		if err != nil {
			a.fail(w, invalid("corpo grande demais ou ilegível"), "")
			return
		}
		req, err := decodeAction(name, body)
		if err != nil {
			a.fail(w, err, "")
			return
		}
		res, err := a.Act(r.Context(), a.ActorOf(r), name, e, req)
		if err != nil {
			a.fail(w, err, "admin")
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}
