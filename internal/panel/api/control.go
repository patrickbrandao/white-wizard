package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/source"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// Screens "Erros", "Cenários" and "Configuração" and their actions (task
// 053): /api/v1/errors, injection, chaos, scenarios, config and policies.

// maxDocBody bounds the YAML of scenarios and the policies of PUT.
const maxDocBody = 1 << 20

// Audit actions and targets of task 053 (x-wwz-audit-action).
const (
	AuditInjection   = "errors.injection"
	AuditChaosStart  = "chaos.start"
	AuditChaosStop   = "chaos.stop"
	AuditScenarioAdd = "scenario.load"
	AuditScenarioDel = "scenario.delete"
	AuditPolicies    = "policies.publish"
)

func conflict(msg string, details ...string) *Error {
	return &Error{Status: http.StatusConflict, Code: "conflict", Message: msg, Details: details}
}

// readBody reads at most limit bytes of the request body.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return nil, invalid("corpo grande demais ou ilegível")
	}
	return body, nil
}

// decodeStrict decodes one JSON value refusing unknown fields.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("corpo JSON inválido", err.Error())
	}
	return nil
}

// isObject reports a JSON object.
func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// ---- errors ----

// ErrorsQuery selects Errors.
type ErrorsQuery struct {
	Window  string // 1m | 5m | 1h ("" = 5m)
	Driver  string
	Model   string
	Samples int // last error events returned (0 = none)
}

// ErrorsView is the body of GET /api/v1/errors. Counts are over the window
// (Prometheus) or since the start of the instance (admin_stats).
type ErrorsView struct {
	Window           string              `json:"window"`
	Source           string              `json:"source"`
	InjectedByType   map[string]float64  `json:"injected_by_type"`
	InjectedByPhase  map[string]float64  `json:"injected_by_phase"`
	InjectedByDriver map[string]float64  `json:"injected_by_driver"`
	Simulator        map[string]*float64 `json:"simulator"`
	Chaos            json.RawMessage     `json:"chaos"`
	Samples          []StoredEvent       `json:"samples"`
}

// Errors builds the errors screen: injected and simulator errors
// (Prometheus or /admin/stats), the active chaos events (an admin failure
// leaves {}) and the last error events of the panel SQLite.
func (a *API) Errors(ctx context.Context, q ErrorsQuery) (ErrorsView, error) {
	q.Window = cmp.Or(q.Window, "5m")
	eb, err := a.metrics.Errors(ctx, source.Filter{Window: q.Window, Driver: q.Driver, Model: q.Model})
	if err != nil {
		return ErrorsView{}, err
	}
	out := ErrorsView{
		Window: q.Window, Source: SourcePrometheus, InjectedByType: eb.ByType, InjectedByPhase: eb.ByPhase,
		InjectedByDriver: eb.ByDriver, Simulator: eb.Simulator, Chaos: json.RawMessage("{}"), Samples: []StoredEvent{},
	}
	if eb.Source != SourcePrometheus {
		out.Source = SourceAdminStats
	}
	if c, err := a.admin.ListChaos(ctx); err == nil {
		out.Chaos = c
	} else {
		a.log.Warn("painel: caos da tela de erros", "err", err)
	}
	if q.Samples > 0 {
		evs, err := a.st.Events.Query(ctx, store.EventFilter{Types: []string{store.EventError}, Driver: q.Driver, Model: q.Model, Limit: q.Samples})
		if err != nil {
			return ErrorsView{}, dbErr(err)
		}
		for _, e := range evs {
			out.Samples = append(out.Samples, ToStoredEvent(e))
		}
	}
	return out, nil
}

func (a *API) getErrors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	win, err := windowParam(q, "5m", "1m", "5m", "1h")
	if err != nil {
		a.fail(w, err, "")
		return
	}
	n, err := intParam(q, "samples", 50, 0, 1000)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	v, err := a.Errors(r.Context(), ErrorsQuery{Window: win, Driver: q.Get("driver"), Model: q.Get("model"), Samples: n})
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- injection ----

// InjectionRequest is the body of POST /api/v1/injection: exactly one of
// Model and Profile; Errors is an ErrorProfile of wwz, or null to turn the
// injection off.
type InjectionRequest struct {
	Model   string          `json:"model,omitempty"`
	Profile string          `json:"profile,omitempty"`
	Errors  json.RawMessage `json:"errors"`
}

// InjectionResult is the answer of POST /api/v1/injection.
type InjectionResult struct {
	AuditID int64 `json:"audit_id"`
	Applied bool  `json:"applied"`
}

// injectionTarget returns the errors of the model (profile name or inline
// ErrorProfile) or the error profile in the effective configuration doc
// (nil when absent).
func injectionTarget(doc json.RawMessage, req InjectionRequest) any {
	var c struct {
		Models []struct {
			ID     string          `json:"id"`
			Errors json.RawMessage `json:"errors"`
		} `json:"models"`
		Profiles struct {
			Errors map[string]json.RawMessage `json:"errors"`
		} `json:"profiles"`
	}
	_ = json.Unmarshal(doc, &c)
	out := c.Profiles.Errors[req.Profile]
	for _, m := range c.Models {
		if req.Model != "" && m.ID == req.Model {
			out = m.Errors
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// injectionPatch is the merge patch of req on the configuration document:
// models.<id>.errors (inline ErrorProfile) or profiles.errors.<name> (the
// error profile itself, the way to reach every model that uses it). Null
// removes the block.
func injectionPatch(req InjectionRequest) (patch map[string]any, target string) {
	if req.Profile != "" {
		return map[string]any{"profiles": map[string]any{"errors": map[string]json.RawMessage{req.Profile: req.Errors}}}, "profile/" + req.Profile
	}
	return map[string]any{"models": map[string]any{req.Model: map[string]json.RawMessage{"errors": req.Errors}}}, "model/" + req.Model
}

// SetInjection turns the error injection of one model or profile on (with
// req.Errors) or off (null), as a merge patch on POST /admin/config of the
// instance of WWZ_PANEL_WWZ_ADMIN_URL, audited as errors.injection.
func (a *API) SetInjection(ctx context.Context, actor Actor, req InjectionRequest) (InjectionResult, error) {
	if (req.Model == "") == (req.Profile == "") {
		return InjectionResult{}, invalid("informe model ou profile (um só)")
	}
	if len(req.Errors) == 0 || (string(req.Errors) != "null" && !isObject(req.Errors)) {
		return InjectionResult{}, invalid("errors é obrigatório", "errors: ErrorProfile do wwz, ou null para desligar")
	}
	patch, target := injectionPatch(req)
	var before any
	if doc, err := a.admin.Config(ctx); err == nil {
		before = injectionTarget(doc, req)
	}
	doc, err := a.admin.PatchConfig(ctx, patch)
	id, err := a.record(ctx, actor, AuditInjection, target, patch, before, injectionTarget(doc, req), err)
	if err != nil {
		return InjectionResult{}, err
	}
	return InjectionResult{AuditID: id, Applied: true}, nil
}

func (a *API) postInjection(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r, maxActionBody)
	var req InjectionRequest
	if err == nil {
		err = decodeStrict(body, &req)
	}
	if err != nil {
		a.fail(w, err, "")
		return
	}
	res, err := a.SetInjection(r.Context(), a.ActorOf(r), req)
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- chaos ----

// ChaosStarted is the answer of POST /api/v1/chaos.
type ChaosStarted struct {
	AuditID int64           `json:"audit_id"`
	Event   json.RawMessage `json:"event"`
}

// ChaosStopped is the answer of DELETE /api/v1/chaos.
type ChaosStopped struct {
	AuditID int64 `json:"audit_id"`
	Stopped int   `json:"stopped"`
}

// Chaos is GET /admin/chaos (ChaosList of wwz).
func (a *API) Chaos(ctx context.Context) (json.RawMessage, error) { return a.admin.ListChaos(ctx) }

// beforeChaos is the chaos list for the audit diff (nil when unavailable).
func (a *API) beforeChaos(ctx context.Context) any {
	if c, err := a.admin.ListChaos(ctx); err == nil {
		return c
	}
	return nil
}

// StartChaos passes req (a ChaosRequest of wwz, JSON object) to POST
// /admin/chaos, audited as chaos.start.
func (a *API) StartChaos(ctx context.Context, actor Actor, req json.RawMessage) (ChaosStarted, error) {
	if !isObject(req) {
		return ChaosStarted{}, invalid("corpo deve ser um objeto JSON (ChaosRequest do wwz)")
	}
	before := a.beforeChaos(ctx)
	ev, err := a.admin.StartChaos(ctx, req)
	id, err := a.record(ctx, actor, AuditChaosStart, "chaos", req, before, ev, err)
	if err != nil {
		return ChaosStarted{}, err
	}
	return ChaosStarted{AuditID: id, Event: ev}, nil
}

// StopChaos is DELETE /admin/chaos (id and testID "" = every event),
// audited as chaos.stop.
func (a *API) StopChaos(ctx context.Context, actor Actor, id, testID string) (ChaosStopped, error) {
	sent := map[string]string{}
	if id != "" {
		sent["id"] = id
	}
	if testID != "" {
		sent["test_id"] = testID
	}
	before := a.beforeChaos(ctx)
	n, err := a.admin.StopChaos(ctx, id, testID)
	aid, err := a.record(ctx, actor, AuditChaosStop, "chaos", sent, before, map[string]int{"stopped": n}, err)
	if err != nil {
		return ChaosStopped{}, err
	}
	return ChaosStopped{AuditID: aid, Stopped: n}, nil
}

func (a *API) getChaos(w http.ResponseWriter, r *http.Request) {
	c, err := a.Chaos(r.Context())
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *API) postChaos(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r, maxActionBody)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	res, err := a.StartChaos(r.Context(), a.ActorOf(r), body)
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (a *API) deleteChaos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := a.StopChaos(r.Context(), a.ActorOf(r), q.Get("id"), q.Get("test_id"))
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- scenarios ----

// ScenarioView is one scenario of GET /api/v1/scenarios. ConversationsByStep
// counts the requests served by each step in the window and OnNoMatchRate
// the fraction that matched no step; both nil without data.
type ScenarioView struct {
	Name                string         `json:"name"`
	Description         string         `json:"description,omitempty"`
	Steps               int            `json:"steps"`
	OnNoMatch           string         `json:"on_no_match,omitempty"`
	OnExhausted         string         `json:"on_exhausted,omitempty"`
	ConversationsByStep map[string]int `json:"conversations_by_step"`
	OnNoMatchRate       *float64       `json:"on_no_match_rate"`
}

// ScenarioList is the body of GET /api/v1/scenarios.
type ScenarioList struct {
	Scenarios []ScenarioView `json:"scenarios"`
}

// ScenariosLoaded is the answer of POST /api/v1/scenarios.
type ScenariosLoaded struct {
	AuditID  int64    `json:"audit_id"`
	Loaded   []string `json:"loaded"`
	Replaced []string `json:"replaced,omitempty"`
}

// stepCounts is scenario → step → requests in the window: Prometheus
// (wwz_scenario_steps_total) or, without it, the request/error events with
// scenario/step (weighted by 1/sample). Step "" = no step matched.
func (a *API) stepCounts(ctx context.Context, window string) map[string]map[string]float64 {
	counts, err := a.metrics.ScenarioSteps(ctx, window)
	if err == nil {
		return counts
	}
	if !errors.Is(err, source.ErrPromOff) {
		a.log.Warn("painel: passos de cenário no prometheus", "err", err)
	}
	counts = map[string]map[string]float64{}
	d := source.Windows[window]
	evs, err := a.st.Events.Query(ctx, store.EventFilter{
		Types: []string{store.EventRequest, store.EventError}, Since: a.now().Add(-d), Limit: maxScan,
	})
	if err != nil {
		a.log.Warn("painel: passos de cenário nos eventos", "err", err)
		return counts
	}
	for _, e := range evs {
		var s struct {
			Scenario string `json:"scenario"`
			Step     string `json:"step"`
		}
		if json.Unmarshal(e.Data, &s) != nil || s.Scenario == "" {
			continue
		}
		if counts[s.Scenario] == nil {
			counts[s.Scenario] = map[string]float64{}
		}
		counts[s.Scenario][s.Step] += 1 / cmp.Or(e.Sample, 1)
	}
	return counts
}

// Scenarios lists the scenarios of GET /admin/scenarios with the steps
// served and the on_no_match rate over window ("" = 5m).
func (a *API) Scenarios(ctx context.Context, window string) (ScenarioList, error) {
	window = cmp.Or(window, "5m")
	list, err := a.admin.Scenarios(ctx)
	if err != nil {
		return ScenarioList{}, err
	}
	counts := a.stepCounts(ctx, window)
	out := ScenarioList{Scenarios: make([]ScenarioView, 0, len(list))}
	for _, s := range list {
		v := ScenarioView{Name: s.Name, Description: s.Description, Steps: s.Steps, OnNoMatch: s.OnNoMatch, OnExhausted: s.OnExhausted}
		var total float64
		for step, n := range counts[s.Name] {
			total += n
			if step == "" {
				continue
			}
			if v.ConversationsByStep == nil {
				v.ConversationsByStep = map[string]int{}
			}
			v.ConversationsByStep[step] = int(n + 0.5)
		}
		if total > 0 {
			r := counts[s.Name][""] / total
			v.OnNoMatchRate = &r
		}
		out.Scenarios = append(out.Scenarios, v)
	}
	return out, nil
}

// LoadScenarios passes the YAML to POST /admin/scenarios (also the
// "simular" of the screen: a validation error is a 400 with the messages of
// wwz in details and nothing is applied), audited as scenario.load.
func (a *API) LoadScenarios(ctx context.Context, actor Actor, doc []byte) (ScenariosLoaded, error) {
	if len(bytes.TrimSpace(doc)) == 0 {
		return ScenariosLoaded{}, invalid("YAML vazio")
	}
	res, err := a.admin.LoadScenarios(ctx, doc)
	target := "scenarios"
	if err == nil && len(res.Loaded) > 0 {
		target = "scenario/" + strings.Join(res.Loaded, ",")
	}
	id, err := a.record(ctx, actor, AuditScenarioAdd, target, string(doc), nil, res, err)
	if err != nil {
		return ScenariosLoaded{}, err
	}
	out := ScenariosLoaded{AuditID: id, Loaded: res.Loaded, Replaced: res.Replaced}
	if out.Loaded == nil {
		out.Loaded = []string{}
	}
	return out, nil
}

// DeleteScenario removes the scenario name (DELETE /admin/scenarios?name=),
// audited as scenario.delete; returns the audit id.
func (a *API) DeleteScenario(ctx context.Context, actor Actor, name string) (int64, error) {
	var before any
	if list, err := a.admin.Scenarios(ctx); err == nil {
		for _, s := range list {
			if s.Name == name {
				before = s
			}
		}
	}
	err := a.admin.DeleteScenario(ctx, name)
	return a.record(ctx, actor, AuditScenarioDel, "scenario/"+name, map[string]string{"name": name}, before, nil, err)
}

func (a *API) getScenarios(w http.ResponseWriter, r *http.Request) {
	win, err := windowParam(r.URL.Query(), "5m", "1m", "5m", "1h")
	if err != nil {
		a.fail(w, err, "")
		return
	}
	list, err := a.Scenarios(r.Context(), win)
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) postScenarios(w http.ResponseWriter, r *http.Request) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/yaml" && mt != "application/x-yaml" {
		auth.WriteProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "envie o YAML com Content-Type application/yaml")
		return
	}
	body, err := readBody(w, r, maxDocBody)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	res, err := a.LoadScenarios(r.Context(), a.ActorOf(r), body)
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) deleteScenario(w http.ResponseWriter, r *http.Request) {
	if _, err := a.DeleteScenario(r.Context(), a.ActorOf(r), r.PathValue("name")); err != nil {
		a.fail(w, err, "admin")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- configuration ----

// ConfigView is the body of GET /api/v1/config.
type ConfigView struct {
	AdminURL  string          `json:"admin_url"`
	Config    json.RawMessage `json:"config"`
	Instances []string        `json:"instances"`
}

// Config returns the effective configuration of the instance of
// WWZ_PANEL_WWZ_ADMIN_URL and the live instances (heartbeat; a failing
// instance source leaves the list empty).
func (a *API) Config(ctx context.Context) (ConfigView, error) {
	doc, err := a.admin.Config(ctx)
	if err != nil {
		return ConfigView{}, err
	}
	out := ConfigView{AdminURL: a.admin.BaseURL(), Config: doc, Instances: []string{}}
	insts, _, err := a.Instances(ctx)
	if err != nil {
		a.log.Warn("painel: instâncias da configuração", "err", err)
	}
	for _, in := range insts {
		out.Instances = append(out.Instances, in.ID)
	}
	return out, nil
}

func (a *API) getConfig(w http.ResponseWriter, r *http.Request) {
	c, err := a.Config(r.Context())
	if err != nil {
		a.fail(w, err, "admin")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---- policies (ADR 0025) ----

// Policies sources.
const SourceConfig = "config"

// PoliciesView is the body of GET /api/v1/policies (schema Policies); the
// answer of PUT adds AuditID.
type PoliciesView struct {
	Version  int64                      `json:"version"`
	Tiers    map[string]json.RawMessage `json:"tiers"`
	Entities map[string]string          `json:"entities"`
	Source   string                     `json:"source"`
	Writable bool                       `json:"writable"`
	AuditID  int64                      `json:"audit_id,omitempty"`
}

// PoliciesUpdate is the body of PUT /api/v1/policies.
type PoliciesUpdate struct {
	Version *int64                     `json:"version"`
	Tiers   map[string]json.RawMessage `json:"tiers"`
}

func policiesView(p source.Policies) PoliciesView {
	v := PoliciesView{Version: p.Version, Tiers: p.Tiers, Entities: map[string]string{}, Source: SourceValkey, Writable: true}
	for e, tier := range p.Entities {
		v.Entities[string(e.Kind)+":"+e.ID] = tier
	}
	return v
}

// Policies reads cfg:policies; without Valkey, the tiers of /admin/config
// read-only (source config, version 0).
func (a *API) Policies(ctx context.Context) (PoliciesView, error) {
	if a.vk != nil {
		p, err := a.vk.Policies(ctx)
		if err != nil {
			return PoliciesView{}, err
		}
		return policiesView(p), nil
	}
	doc, err := a.admin.Config(ctx)
	if err != nil {
		return PoliciesView{}, err
	}
	var c struct {
		Tiers map[string]struct {
			Limits json.RawMessage `json:"limits"`
		} `json:"tiers"`
	}
	_ = json.Unmarshal(doc, &c)
	v := PoliciesView{Tiers: map[string]json.RawMessage{}, Entities: map[string]string{}, Source: SourceConfig}
	for name, t := range c.Tiers {
		v.Tiers[name] = t.Limits
		if len(t.Limits) == 0 || string(t.Limits) == "null" {
			v.Tiers[name] = json.RawMessage("{}")
		}
	}
	return v, nil
}

// checkTiers validates names and limits (fields and types of the YAML,
// spec 003); the wwz does the final validation.
func checkTiers(tiers map[string]json.RawMessage) error {
	var details []string
	for name, raw := range tiers {
		var l config.Limits
		switch {
		case !config.ValidTierName(name):
			details = append(details, name+": nome de tier inválido")
		case !isObject(raw):
			details = append(details, name+": limits deve ser um objeto")
		default:
			if err := yaml.UnmarshalWithOptions(raw, &l, yaml.Strict()); err != nil {
				details = append(details, name+": "+strings.ReplaceAll(err.Error(), "\n", " "))
			}
		}
	}
	if len(details) > 0 {
		sort.Strings(details)
		return invalid("políticas inválidas", details...)
	}
	return nil
}

// PublishPolicies replaces the tier:<name> fields of cfg:policies by
// u.Tiers with the optimistic version (README §2.3: WATCH, version check,
// MULTI/HINCRBY/EXEC, PUBLISH cfg:changed), audited as policies.publish.
// A tier still assigned to an entity (ent:* fields) cannot be removed: the
// panel cannot tell a tier of the YAML from one of the policies in the
// effective document, so it refuses instead of letting the wwz drop the
// whole version. Without Valkey → 409.
func (a *API) PublishPolicies(ctx context.Context, actor Actor, u PoliciesUpdate) (PoliciesView, error) {
	if a.vk == nil {
		return PoliciesView{}, conflict("políticas indisponíveis sem Valkey", "o painel roda sem Valkey (store: memory); edite os tiers no YAML do wwz")
	}
	if u.Version == nil || *u.Version < 0 || u.Tiers == nil {
		return PoliciesView{}, invalid("version e tiers são obrigatórios")
	}
	if err := checkTiers(u.Tiers); err != nil {
		return PoliciesView{}, err
	}
	var before *source.Policies
	check := func(cur source.Policies) error {
		before = &cur
		var stuck []string
		for e, tier := range cur.Entities {
			if _, kept := u.Tiers[tier]; !kept {
				if _, had := cur.Tiers[tier]; had {
					stuck = append(stuck, tier+": atribuído a "+string(e.Kind)+":"+e.ID)
				}
			}
		}
		if len(stuck) > 0 {
			sort.Strings(stuck)
			return invalid("tier removido ainda atribuído a entidade", stuck...)
		}
		return nil
	}
	n, err := a.vk.ReplaceTiers(ctx, *u.Version, u.Tiers, check)
	if errors.Is(err, source.ErrPolicyConflict) {
		err = conflict("cfg:policies mudou desde a versão lida", err.Error())
	}
	var bv, av any
	out := PoliciesView{}
	if before != nil {
		bv = policiesView(*before)
		out = policiesView(source.Policies{Version: n, Tiers: u.Tiers, Entities: before.Entities})
		av = out
	}
	id, err := a.record(ctx, actor, AuditPolicies, "policies", u, bv, av, err)
	if err != nil {
		return PoliciesView{}, err
	}
	out.AuditID = id
	return out, nil
}

func (a *API) getPolicies(w http.ResponseWriter, r *http.Request) {
	p, err := a.Policies(r.Context())
	if err != nil {
		a.fail(w, err, a.entitySource())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) putPolicies(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r, maxDocBody)
	var u PoliciesUpdate
	if err == nil {
		err = decodeStrict(body, &u)
	}
	if err != nil {
		a.fail(w, err, "")
		return
	}
	p, err := a.PublishPolicies(r.Context(), a.ActorOf(r), u)
	if err != nil {
		a.fail(w, err, "valkey")
		return
	}
	writeJSON(w, http.StatusOK, p)
}
