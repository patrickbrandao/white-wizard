package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/panel/api"
	"github.com/patrickbrandao/white-wizard/internal/panel/auth"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// withQuery returns p plus the encoded query (if any).
func withQuery(p string, q url.Values) string {
	if e := q.Encode(); e != "" {
		return p + "?" + e
	}
	return p
}

// pick copies the non-empty values of names from src.
func pick(src url.Values, names ...string) url.Values {
	out := url.Values{}
	for _, n := range names {
		if v := strings.TrimSpace(src.Get(n)); v != "" {
			out.Set(n, v)
		}
	}
	return out
}

// ---- login ----

type loginData struct {
	Next  string
	OIDC  bool
	Error string
}

// login is GET /login: the local form (posted to /api/v1/login) and the
// OIDC button. An authenticated visitor goes straight to next.
func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	next := auth.SafeNext(q.Get("next"))
	if _, ok := auth.FromContext(r.Context()); ok {
		http.Redirect(rw, r, next, http.StatusSeeOther) //nolint:gosec // G710: next passed auth.SafeNext (relative panel path only)
		return
	}
	d := loginData{Next: next, OIDC: w.oidc}
	switch q.Get("error") {
	case "":
	case "rate":
		d.Error = "Muitas tentativas de login; aguarde um minuto."
	default:
		d.Error = "Usuário ou senha inválidos."
	}
	v := view{Title: "Entrar", Data: d}
	w.render(rw, r, "login", http.StatusOK, v)
}

// ---- overview ----

type overviewData struct {
	Filter url.Values
	Ov     api.Overview
}

// overview is GET /: cluster rates, latency, errors and instances.
func (w *Web) overview(rw http.ResponseWriter, r *http.Request) {
	f := pick(r.URL.Query(), "window", "driver", "model")
	v := newView(r, "Visão geral", "overview")
	v.Live = withQuery("/", f)
	d := overviewData{Filter: f}
	status := http.StatusOK
	win := f.Get("window")
	if win != "" && !slices.Contains([]string{"1m", "5m", "1h"}, win) {
		v.Err, status = "Janela inválida: use 1m, 5m ou 1h.", http.StatusBadRequest
	} else {
		ov, err := w.api.Overview(r.Context(), api.OverviewQuery{Window: win, Driver: f.Get("driver"), Model: f.Get("model")})
		if err != nil {
			status = w.fail(&v, err, "admin")
		}
		d.Ov = ov
	}
	v.Data = d
	w.render(rw, r, "overview", status, v)
}

// ---- usage ----

type usageData struct {
	Filter url.Values
	Usage  api.Usage
	CSV    string
}

func usageQuery(r *http.Request) (url.Values, api.UsageQuery) {
	f := pick(r.URL.Query(), "group", "window", "driver", "model")
	if f.Get("group") == "" {
		f.Set("group", store.GroupModel)
	}
	return f, api.UsageQuery{Group: f.Get("group"), Window: f.Get("window"), Driver: f.Get("driver"), Model: f.Get("model"), Limit: 500}
}

// usage is GET /usage: the fake inference usage by driver, model, key,
// account or IP.
func (w *Web) usage(rw http.ResponseWriter, r *http.Request) {
	f, q := usageQuery(r)
	v := newView(r, "Uso da inferência falsa", "usage")
	v.Live = withQuery("/usage", f)
	d := usageData{Filter: f, CSV: withQuery("/usage.csv", f)}
	status := http.StatusOK
	u, err := w.api.Usage(r.Context(), q)
	if err != nil {
		status = w.fail(&v, err, "")
	}
	d.Usage = u
	v.Data = d
	w.render(rw, r, "usage", status, v)
}

// inputOrigins label the keys of api.UsageInputBreakdown, in the order of
// Values(); the first seven are the stacked bar, cached and total are not.
var inputOrigins = []string{
	"Sistema", "Esquemas de tools", "Usuário", "Assistente", "Tool calls", "Resultados de tools", "Imagens", "Cache", "Total",
}

// ibCell is one origin of the "Composição da entrada" block.
type ibCell struct {
	Label string
	Value *float64
	Share *float64 // Value / total (nil without data or total <= 0)
	Class string   // in-<n>, the colour of the bar segment
	X, W  float64  // bar segment, in % of total (W = 0: no segment)
}

// breakdown renders an input composition as cells (tokens and % of total)
// and the segments of the stacked bar.
func breakdown(b api.UsageInputBreakdown) []ibCell {
	vals := b.Values()
	total := b.Total
	out := make([]ibCell, len(vals))
	x := 0.0
	for i, v := range vals {
		c := ibCell{Label: inputOrigins[i], Value: v, Class: "in-" + strconv.Itoa(i)}
		if v != nil && total != nil && *total > 0 {
			s := *v / *total
			c.Share = &s
			if i < 7 {
				c.X, c.W = x, s*100
				x += c.W
			}
		}
		out[i] = c
	}
	return out
}

// usageCSV is GET /usage.csv: the same table as CSV (api.WriteUsageCSV).
func (w *Web) usageCSV(rw http.ResponseWriter, r *http.Request) {
	_, q := usageQuery(r)
	q.Limit = 10000
	u, err := w.api.Usage(r.Context(), q)
	if err != nil {
		st, p := api.ProblemOf(err, "")
		auth.WriteProblem(rw, st, p.Error, p.Message, p.Details...)
		return
	}
	rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="usage-`+u.Group+`.csv"`)
	_ = api.WriteUsageCSV(rw, u)
}

// ---- blocks and bans ----

// blockActions are the actions of the blocks screen.
var blockActions = []string{
	api.ActionBlock, api.ActionUnblock, api.ActionBan, api.ActionUnban, api.ActionForgive, api.ActionLeak,
}

// Default status filter of the blocks screen: suspended and banned.
const statusBlocked = ""

type blocksData struct {
	Filter   url.Values
	ActURL   string // POST target keeping the filter
	Entities []limits.EntityState
	Source   string
	Kinds    []string
	Actions  []string
}

func blocksFilter(q url.Values) url.Values { return pick(q, "status", "kind") }

// blocks is GET /blocks: the live list of blocked and banned entities with
// the operator actions.
func (w *Web) blocks(rw http.ResponseWriter, r *http.Request) {
	f := blocksFilter(r.URL.Query())
	v := newView(r, "Bloqueios e banimentos", "blocks")
	if q := r.URL.Query(); q.Get("done") != "" {
		v.Flash = doneMessage(q.Get("done"), q.Get("target"), q.Get("audit"))
	}
	w.renderBlocks(rw, r, f, v, http.StatusOK)
}

func (w *Web) renderBlocks(rw http.ResponseWriter, r *http.Request, f url.Values, v view, status int) {
	v.Live = withQuery("/blocks", f)
	d := blocksData{
		Filter: f, ActURL: withQuery("/blocks/act", f), Kinds: []string{"key", "ip", "user", "account", "project"}, Actions: blockActions,
	}
	statuses := []string{f.Get("status")}
	switch statuses[0] {
	case statusBlocked:
		statuses = []string{string(limits.StatusSuspended), string(limits.StatusBanned)}
	case "all":
		statuses = []string{""}
	}
	for _, st := range statuses {
		list, err := w.api.Entities(r.Context(), api.EntitiesQuery{Status: st, Kind: f.Get("kind"), Limit: 1000})
		if err != nil {
			status = w.fail(&v, err, "admin")
			break
		}
		d.Entities, d.Source = append(d.Entities, list.Entities...), list.Source
	}
	v.Data = d
	w.render(rw, r, "blocks", status, v)
}

// doneMessage is the confirmation of an action (fixed text plus the
// escaped target).
func doneMessage(action, target, auditID string) string {
	return "Ação " + action + " aplicada em " + target + " (auditoria #" + auditID + ")."
}

// blocksAct is POST /blocks/act (operator, CSRF checked by the
// middleware): form fields kind, id, action, reason and duration; the
// filter of the list comes in the query. HTMX gets the live list back with
// the result; a plain form post is redirected (303) to the list.
func (w *Web) blocksAct(rw http.ResponseWriter, r *http.Request) {
	f := blocksFilter(r.URL.Query())
	v := newView(r, "Bloqueios e banimentos", "blocks")
	action := r.PostFormValue("action")
	e, err := api.ParseEntity(r.PostFormValue("kind"), strings.TrimSpace(r.PostFormValue("id")))
	req := api.ActionRequest{Reason: strings.TrimSpace(r.PostFormValue("reason"))}
	switch {
	case err != nil:
	case !slices.Contains(blockActions, action):
		err = &api.Error{Status: http.StatusBadRequest, Code: "invalid", Message: "ação desconhecida: " + action}
	default:
		if ds := strings.TrimSpace(r.PostFormValue("duration")); ds != "" && action == api.ActionBlock {
			d, perr := time.ParseDuration(ds)
			if perr != nil || d < 0 {
				err = &api.Error{Status: http.StatusBadRequest, Code: "invalid", Message: "duração inválida (ex.: 10m, 2h)"}
			}
			req.Duration = d
		}
	}
	var res api.ActionResult
	if err == nil {
		res, err = w.api.Act(r.Context(), w.api.ActorOf(r), action, e, req)
	}
	if err != nil {
		w.renderBlocks(rw, r, f, v, w.fail(&v, err, "admin"))
		return
	}
	target, audit := string(e.Kind)+"/"+e.ID, strconv.FormatInt(res.AuditID, 10)
	if !isHX(r) {
		q := f
		q.Set("done", action)
		q.Set("target", target)
		q.Set("audit", audit)
		http.Redirect(rw, r, withQuery("/blocks", q), http.StatusSeeOther) //nolint:gosec // G710: fixed path, user data only in the query
		return
	}
	v.Flash = doneMessage(action, target, audit)
	w.renderBlocks(rw, r, f, v, http.StatusOK)
}

// ---- audit ----

type auditData struct {
	Filter url.Values
	Page   api.AuditPage
	Next   string
	CSV    string
}

// parseTime accepts RFC 3339, the datetime-local format and a date (UTC).
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// audit is GET /audit: the audit trail with filters, pagination and the
// CSV export (GET /api/v1/audit?format=csv with the same filter).
func (w *Web) audit(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := pick(q, "user", "action", "target", "result", "since", "until")
	v := newView(r, "Auditoria", "audit")
	v.Live = withQuery("/audit", pick(q, "user", "action", "target", "result", "since", "until", "cursor"))
	d := auditData{Filter: f}
	status := http.StatusOK
	since, ok1 := parseTime(f.Get("since"))
	until, ok2 := parseTime(f.Get("until"))
	if !ok1 || !ok2 {
		v.Err, status = "Data inválida: use AAAA-MM-DD, AAAA-MM-DDTHH:MM ou RFC 3339.", http.StatusBadRequest
	} else {
		page, err := w.api.Audit(r.Context(), api.AuditQuery{
			User: f.Get("user"), Action: f.Get("action"), Target: f.Get("target"), Result: f.Get("result"),
			Since: since, Until: until, Limit: 100, Cursor: q.Get("cursor"),
		})
		if err != nil {
			status = w.fail(&v, err, "")
		}
		d.Page = page
		exp := pick(f, "user", "action", "target", "result")
		exp.Set("format", store.FormatCSV)
		for name, t := range map[string]time.Time{"since": since, "until": until} {
			if !t.IsZero() {
				exp.Set(name, t.UTC().Format(time.RFC3339))
			}
		}
		d.CSV = withQuery("/api/v1/audit", exp)
		if page.NextCursor != "" {
			nq := pick(f, "user", "action", "target", "result", "since", "until")
			nq.Set("cursor", page.NextCursor)
			d.Next = withQuery("/audit", nq)
		}
	}
	v.Data = d
	w.render(rw, r, "audit", status, v)
}
