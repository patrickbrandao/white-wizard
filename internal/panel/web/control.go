package web

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/api"
)

// Screens of task 053: Limites, Erros, Cenários and Configuração.

// entityKinds are the kinds offered by the forms.
var entityKinds = []string{"key", "ip", "user", "account", "project"}

// badInput is a 400 of the form (message shown in the page).
func badInput(msg string) error {
	return &api.Error{Status: http.StatusBadRequest, Code: "invalid", Message: msg}
}

// splitList splits a comma-separated field, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// done finishes an action: HTMX gets the live region back (render with the
// flash); a plain form post is redirected (303) to the page with the
// result in the query.
func (w *Web) done(rw http.ResponseWriter, r *http.Request, page string, q url.Values, flash string, render func(view)) {
	if !isHX(r) {
		q.Set("done", flash)
		http.Redirect(rw, r, withQuery(page, q), http.StatusSeeOther) //nolint:gosec // G710: fixed path, user data only in the query
		return
	}
	v := newView(r, "", "")
	v.Flash = flash
	render(v)
}

// flashOf reads the result of a redirected action.
func flashOf(r *http.Request) string { return r.URL.Query().Get("done") }

func auditFlash(what string, id int64) string {
	return what + " (auditoria #" + strconv.FormatInt(id, 10) + ")."
}

// ---- limits ----

// windowOrder sorts the live limits by window (bars per window).
var windowOrder = map[string]int{"second": 0, "minute": 1, "5h": 2, "day": 3, "week": 4, "month": 5, "none": 6}

// windowLabel names a window in the screen.
var windowLabel = map[string]string{
	"second": "segundo", "minute": "minuto", "5h": "5 h", "day": "dia", "week": "semana", "month": "mês", "none": "—",
}

type limitRow struct {
	api.LiveLimit
	Used        float64
	WindowLabel string
}

// entityLink is a known entity linked to its limits.
type entityLink struct{ Label, URL string }

type limitsData struct {
	Filter   url.Values
	ActURL   string
	Kinds    []string
	Entities []entityLink // known entities
	Detail   *api.EntityDetail
	Rows     []limitRow
	// Account is the account that holds the tier and the credits of the
	// entity (the entity itself, or the account of a key; "" = none): the
	// target of the tier and purchase forms (ADR 0051).
	Account string
}

func limitsFilter(q url.Values) url.Values { return pick(q, "kind", "id") }

// limits is GET /limits: one entity with each limit live (limit,
// remaining, reset, bar per window) and its tier; tier and reset actions.
func (w *Web) limits(rw http.ResponseWriter, r *http.Request) {
	v := newView(r, "Limites", "limits")
	v.Flash = flashOf(r)
	w.renderLimits(rw, r, limitsFilter(r.URL.Query()), v, http.StatusOK)
}

func (w *Web) renderLimits(rw http.ResponseWriter, r *http.Request, f url.Values, v view, status int) {
	v.Title, v.Nav, v.Live = "Limites", "limits", withQuery("/limits", f)
	d := limitsData{Filter: f, ActURL: withQuery("/limits/act", f), Kinds: entityKinds}
	if list, err := w.api.Entities(r.Context(), api.EntitiesQuery{Limit: 50}); err == nil {
		for _, s := range list.Entities {
			q := url.Values{"kind": {string(s.Entity.Kind)}, "id": {s.Entity.ID}}
			d.Entities = append(d.Entities, entityLink{Label: string(s.Entity.Kind) + "/" + s.Entity.ID, URL: withQuery("/limits", q)})
		}
	} else {
		status = w.fail(&v, err, "admin")
	}
	if f.Get("kind") != "" || f.Get("id") != "" {
		e, err := api.ParseEntity(f.Get("kind"), f.Get("id"))
		var det api.EntityDetail
		if err == nil {
			det, err = w.api.Entity(r.Context(), e)
		}
		if err != nil {
			status = w.fail(&v, err, "admin")
		} else {
			d.Detail = &det
			switch e.Kind {
			case limits.KindAccount:
				d.Account = e.ID
			case limits.KindKey:
				d.Account = det.AccountID
			}
			for _, l := range det.Limits {
				d.Rows = append(d.Rows, limitRow{LiveLimit: l, Used: l.Limit - l.Remaining, WindowLabel: windowLabel[l.Window]})
			}
			sort.SliceStable(d.Rows, func(i, j int) bool { return windowOrder[d.Rows[i].Window] < windowOrder[d.Rows[j].Window] })
		}
	}
	v.Data = d
	w.render(rw, r, "limits", status, v)
}

// limitsAct is POST /limits/act: form fields kind, id, action (tier, credits
// or reset), tier ("" removes the override) and add_usd (purchase).
func (w *Web) limitsAct(rw http.ResponseWriter, r *http.Request) {
	f := limitsFilter(r.URL.Query())
	v := newView(r, "Limites", "limits")
	action := r.PostFormValue("action")
	e, err := api.ParseEntity(r.PostFormValue("kind"), strings.TrimSpace(r.PostFormValue("id")))
	req := api.ActionRequest{}
	switch {
	case err != nil:
	case action == api.ActionTier:
		req.Tier, req.HasTier = strings.TrimSpace(r.PostFormValue("tier")), true
	case action == api.ActionCredits:
		usd, perr := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("add_usd")), 64)
		if perr != nil {
			err = badInput("add_usd inválido: informe um valor em dólares")
		}
		req.AddUSD = &usd
	case action == api.ActionReset:
		req.Scope = []string{"counters"}
	default:
		err = badInput("ação desconhecida: " + action)
	}
	var res api.ActionResult
	if err == nil {
		res, err = w.api.Act(r.Context(), w.api.ActorOf(r), action, e, req)
	}
	if err != nil {
		w.renderLimits(rw, r, f, v, w.fail(&v, err, "admin"))
		return
	}
	f.Set("kind", string(e.Kind))
	f.Set("id", e.ID)
	w.done(rw, r, "/limits", f, auditFlash("Ação "+action+" aplicada em "+string(e.Kind)+"/"+e.ID, res.AuditID), func(v view) {
		w.renderLimits(rw, r, f, v, http.StatusOK)
	})
}

// ---- errors ----

type chaosItem struct {
	ID   string
	JSON string
}

type errorsData struct {
	Filter  url.Values
	ActURL  string
	View    api.ErrorsView
	Chaos   []chaosItem
	Pending []string
}

func errorsFilter(q url.Values) url.Values { return pick(q, "window", "driver", "model") }

// errorsPage is GET /errors: injected and simulator errors, active chaos
// events and the last error events; injection and chaos actions.
func (w *Web) errorsPage(rw http.ResponseWriter, r *http.Request) {
	v := newView(r, "Erros", "errors")
	v.Flash = flashOf(r)
	w.renderErrors(rw, r, errorsFilter(r.URL.Query()), v, http.StatusOK)
}

func (w *Web) renderErrors(rw http.ResponseWriter, r *http.Request, f url.Values, v view, status int) {
	v.Title, v.Nav, v.Live = "Erros", "errors", withQuery("/errors", f)
	d := errorsData{Filter: f, ActURL: withQuery("/errors/act", f)}
	win := f.Get("window")
	if win != "" && !slices.Contains([]string{"1m", "5m", "1h"}, win) {
		v.Err, status = "Janela inválida: use 1m, 5m ou 1h.", http.StatusBadRequest
	} else {
		ev, err := w.api.Errors(r.Context(), api.ErrorsQuery{Window: win, Driver: f.Get("driver"), Model: f.Get("model"), Samples: 50})
		if err != nil {
			status = w.fail(&v, err, "admin")
		}
		d.View = ev
		var list struct {
			Events []json.RawMessage `json:"events"`
			Next   []json.RawMessage `json:"pending_next"`
		}
		_ = json.Unmarshal(ev.Chaos, &list)
		for _, raw := range list.Events {
			var id struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &id)
			d.Chaos = append(d.Chaos, chaosItem{ID: id.ID, JSON: string(raw)})
		}
		for _, raw := range list.Next {
			d.Pending = append(d.Pending, string(raw))
		}
	}
	v.Data = d
	w.render(rw, r, "errors", status, v)
}

// Actions of the errors screen.
const (
	actInjectionOn  = "injection_on"
	actInjectionOff = "injection_off"
	actChaosStart   = "chaos_start"
	actChaosStop    = "chaos_stop"
)

// errorsAct is POST /errors/act: injection_on/off (target model|profile,
// name, rate, types), chaos_start (type, duration, models, rate, test_id)
// and chaos_stop (id; empty = every event).
func (w *Web) errorsAct(rw http.ResponseWriter, r *http.Request) {
	f := errorsFilter(r.URL.Query())
	v := newView(r, "Erros", "errors")
	form := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	ctx, actor := r.Context(), w.api.ActorOf(r)
	var flash string
	var err error
	switch action := form("action"); action {
	case actInjectionOn, actInjectionOff:
		req := api.InjectionRequest{Errors: json.RawMessage("null")}
		if form("target") == "profile" {
			req.Profile = form("name")
		} else {
			req.Model = form("name")
		}
		if action == actInjectionOn {
			rate, perr := strconv.ParseFloat(form("rate"), 64)
			if perr != nil {
				err = badInput("taxa inválida (0 a 100)")
				break
			}
			prof := map[string]any{"rate": rate}
			if types := splitList(form("types")); len(types) > 0 {
				prof["types"] = types
			}
			req.Errors, _ = json.Marshal(prof)
		}
		var res api.InjectionResult
		if res, err = w.api.SetInjection(ctx, actor, req); err == nil {
			flash = auditFlash("Injeção atualizada em "+form("target")+" "+form("name"), res.AuditID)
		}
	case actChaosStart:
		req := map[string]any{"type": form("type"), "duration": form("duration")}
		if models := splitList(form("models")); len(models) > 0 {
			req["models"] = models
		}
		if s := form("rate"); s != "" {
			rate, perr := strconv.ParseFloat(s, 64)
			if perr != nil {
				err = badInput("taxa inválida (0 a 100)")
				break
			}
			req["rate"] = rate
		}
		if s := form("test_id"); s != "" {
			req["test_id"] = s
		}
		body, _ := json.Marshal(req)
		var res api.ChaosStarted
		if res, err = w.api.StartChaos(ctx, actor, body); err == nil {
			flash = auditFlash("Evento de caos disparado", res.AuditID)
		}
	case actChaosStop:
		var res api.ChaosStopped
		if res, err = w.api.StopChaos(ctx, actor, form("id"), ""); err == nil {
			flash = auditFlash(strconv.Itoa(res.Stopped)+" evento(s) de caos encerrado(s)", res.AuditID)
		}
	default:
		err = badInput("ação desconhecida: " + action)
	}
	if err != nil {
		w.renderErrors(rw, r, f, v, w.fail(&v, err, "admin"))
		return
	}
	w.done(rw, r, "/errors", f, flash, func(v view) { w.renderErrors(rw, r, f, v, http.StatusOK) })
}

// ---- scenarios ----

type scenarioRow struct {
	api.ScenarioView
	StepList string // "a: 3 · b: 1"
}

type scenariosData struct {
	Filter url.Values
	ActURL string
	Rows   []scenarioRow
	YAML   string // the text of a failed load, kept in the form
}

func scenariosFilter(q url.Values) url.Values { return pick(q, "window") }

// scenarios is GET /scenarios: loaded scenarios, steps served and the
// on_no_match rate; load (also "simular": validation only) and delete.
func (w *Web) scenarios(rw http.ResponseWriter, r *http.Request) {
	v := newView(r, "Cenários", "scenarios")
	v.Flash = flashOf(r)
	w.renderScenarios(rw, r, scenariosFilter(r.URL.Query()), v, http.StatusOK, "")
}

func (w *Web) renderScenarios(rw http.ResponseWriter, r *http.Request, f url.Values, v view, status int, doc string) {
	v.Title, v.Nav, v.Live = "Cenários", "scenarios", withQuery("/scenarios", f)
	d := scenariosData{Filter: f, ActURL: withQuery("/scenarios/act", f), YAML: doc}
	win := f.Get("window")
	if win != "" && !slices.Contains([]string{"1m", "5m", "1h"}, win) {
		v.Err, status = "Janela inválida: use 1m, 5m ou 1h.", http.StatusBadRequest
	} else if list, err := w.api.Scenarios(r.Context(), win); err != nil {
		status = w.fail(&v, err, "admin")
	} else {
		for _, s := range list.Scenarios {
			var parts []string
			for _, step := range slices.Sorted(maps.Keys(s.ConversationsByStep)) {
				parts = append(parts, step+": "+strconv.Itoa(s.ConversationsByStep[step]))
			}
			d.Rows = append(d.Rows, scenarioRow{ScenarioView: s, StepList: strings.Join(parts, " · ")})
		}
	}
	v.Data = d
	w.render(rw, r, "scenarios", status, v)
}

// scenariosAct is POST /scenarios/act: load (field yaml) or delete (field
// name).
func (w *Web) scenariosAct(rw http.ResponseWriter, r *http.Request) {
	f := scenariosFilter(r.URL.Query())
	v := newView(r, "Cenários", "scenarios")
	doc := r.PostFormValue("yaml")
	var flash string
	var err error
	switch r.PostFormValue("action") {
	case "load":
		var res api.ScenariosLoaded
		if res, err = w.api.LoadScenarios(r.Context(), w.api.ActorOf(r), []byte(doc)); err == nil {
			what := "Cenários válidos e carregados: " + strings.Join(res.Loaded, ", ")
			if len(res.Replaced) > 0 {
				what += " (substituídos: " + strings.Join(res.Replaced, ", ") + ")"
			}
			flash, doc = auditFlash(what, res.AuditID), ""
		}
	case "delete":
		name := strings.TrimSpace(r.PostFormValue("name"))
		var id int64
		if id, err = w.api.DeleteScenario(r.Context(), w.api.ActorOf(r), name); err == nil {
			flash = auditFlash("Cenário "+name+" removido", id)
		}
	default:
		err = badInput("ação desconhecida: " + r.PostFormValue("action"))
	}
	if err != nil {
		w.renderScenarios(rw, r, f, v, w.fail(&v, err, "admin"), doc)
		return
	}
	w.done(rw, r, "/scenarios", f, flash, func(v view) { w.renderScenarios(rw, r, f, v, http.StatusOK, "") })
}

// ---- configuration ----

type configData struct {
	Config   api.ConfigView
	YAML     string
	Policies api.PoliciesView
	Entities []string // "key:test-01 → tier-4"
	Tiers    string   // tiers as indented JSON (form)
	PolErr   string   // why the policies could not be read
}

// configPage is GET /config: effective YAML of the instance of
// WWZ_PANEL_WWZ_ADMIN_URL, live instances and cfg:policies; operators edit
// and publish the tiers (plain form post, outside the live region).
func (w *Web) configPage(rw http.ResponseWriter, r *http.Request) {
	v := newView(r, "Configuração", "config")
	v.Flash = flashOf(r)
	w.renderConfig(rw, r, v, http.StatusOK, "")
}

func (w *Web) renderConfig(rw http.ResponseWriter, r *http.Request, v view, status int, tiers string) {
	v.Title, v.Nav, v.Live = "Configuração", "config", "/config"
	d := configData{}
	if c, err := w.api.Config(r.Context()); err != nil {
		status = w.fail(&v, err, "admin")
	} else {
		d.Config = c
		y, _ := yaml.JSONToYAML(c.Config)
		d.YAML = string(y)
	}
	if p, err := w.api.Policies(r.Context()); err != nil {
		_, pr := api.ProblemOf(err, "valkey")
		d.PolErr = strings.Join(append([]string{pr.Message}, pr.Details...), ": ")
	} else {
		d.Policies = p
		for _, k := range slices.Sorted(maps.Keys(p.Entities)) {
			d.Entities = append(d.Entities, k+" → "+p.Entities[k])
		}
		b, _ := json.MarshalIndent(p.Tiers, "", "  ")
		d.Tiers = string(b)
	}
	if tiers != "" {
		d.Tiers = tiers
	}
	v.Data = d
	w.render(rw, r, "config", status, v)
}

// configPolicies is POST /config/policies: fields version and tiers (JSON
// object name → limits), published with PUT semantics (version +1, 409
// when stale).
func (w *Web) configPolicies(rw http.ResponseWriter, r *http.Request) {
	v := newView(r, "Configuração", "config")
	text := r.PostFormValue("tiers")
	ver, err := strconv.ParseInt(r.PostFormValue("version"), 10, 64)
	var tiers map[string]json.RawMessage
	switch {
	case err != nil:
		err = badInput("versão inválida")
	case json.Unmarshal([]byte(text), &tiers) != nil || tiers == nil:
		err = badInput("tiers deve ser um objeto JSON (nome → limites)")
	}
	var p api.PoliciesView
	if err == nil {
		p, err = w.api.PublishPolicies(r.Context(), w.api.ActorOf(r), api.PoliciesUpdate{Version: &ver, Tiers: tiers})
	}
	if err != nil {
		w.renderConfig(rw, r, v, w.fail(&v, err, "valkey"), text)
		return
	}
	q := url.Values{}
	q.Set("done", auditFlash("Políticas publicadas na versão "+strconv.FormatInt(p.Version, 10), p.AuditID))
	http.Redirect(rw, r, withQuery("/config", q), http.StatusSeeOther)
}
