package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

var eventTypes = []string{
	store.EventRequest, store.EventError, store.EventBlock, store.EventUnblock, store.EventBan, store.EventUnban, store.EventStrike,
}

// StoredEvent is one event of GET /api/v1/events.
type StoredEvent struct {
	StreamID  string          `json:"stream_id"`
	T         time.Time       `json:"t"`
	Type      string          `json:"type"`
	Instance  string          `json:"instance,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	TestID    string          `json:"test_id,omitempty"`
	Driver    string          `json:"driver,omitempty"`
	Model     string          `json:"model,omitempty"`
	Entity    *EventEntity    `json:"entity,omitempty"`
	Status    int             `json:"status,omitempty"`
	Error     string          `json:"error,omitempty"`
	Sample    float64         `json:"sample"`
	Data      json.RawMessage `json:"data"`
}

// EventEntity is the entity of a StoredEvent.
type EventEntity struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ToStoredEvent converts a row of events.
func ToStoredEvent(e store.Event) StoredEvent {
	out := StoredEvent{
		StreamID: e.StreamID, T: e.At, Type: e.Type, Instance: e.Instance, RequestID: e.RequestID, TestID: e.TestID,
		Driver: e.Driver, Model: e.Model, Status: e.Status, Error: e.Error, Sample: e.Sample, Data: e.Data,
	}
	if e.EntityKind != "" || e.EntityID != "" {
		out.Entity = &EventEntity{Kind: e.EntityKind, ID: e.EntityID}
	}
	return out
}

// EventsQuery selects Events.
type EventsQuery struct {
	Filter store.EventFilter // Limit ignored
	Limit  int               // 1..1000 (<= 0 = 100)
	Cursor string
}

// EventPage is the body of GET /api/v1/events.
type EventPage struct {
	Events     []StoredEvent `json:"events"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// Events reads the events table newest first, paginated by a keyset cursor
// (at, stream_id) of the last event returned.
func (a *API) Events(ctx context.Context, q EventsQuery) (EventPage, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	f := q.Filter
	var curAt int64
	var curSID string
	if q.Cursor != "" {
		s, err := decodeCursor(q.Cursor, "e:")
		if err != nil {
			return EventPage{}, err
		}
		at, sid, _ := strings.Cut(s, ":")
		if curAt, err = strconv.ParseInt(at, 10, 64); err != nil {
			return EventPage{}, invalid("cursor inválido")
		}
		curSID = sid
		if u := time.UnixMilli(curAt + 1); f.Until.IsZero() || u.Before(f.Until) {
			f.Until = u
		}
	}
	var out []store.Event
	for fetch := q.Limit + 1; ; fetch *= 2 {
		f.Limit = fetch
		evs, err := a.st.Events.Query(ctx, f)
		if err != nil {
			return EventPage{}, dbErr(err)
		}
		skip := 0
		for q.Cursor != "" && skip < len(evs) && evs[skip].At.UnixMilli() == curAt && evs[skip].StreamID >= curSID {
			skip++
		}
		out = evs[skip:]
		if len(out) > q.Limit || len(evs) < fetch {
			break
		}
	}
	page := EventPage{Events: []StoredEvent{}}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		page.NextCursor = encodeCursor("e:" + strconv.FormatInt(last.At.UnixMilli(), 10) + ":" + last.StreamID)
	}
	for _, e := range out {
		page.Events = append(page.Events, ToStoredEvent(e))
	}
	return page, nil
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 100, 1, 1000)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	f := store.EventFilter{
		Driver: q.Get("driver"), Model: q.Get("model"), EntityKind: q.Get("entity_kind"), EntityID: q.Get("entity_id"),
		TestID: q.Get("test_id"),
	}
	if t := q.Get("type"); t != "" {
		for _, x := range strings.Split(t, ",") {
			x = strings.TrimSpace(x)
			if !slices.Contains(eventTypes, x) {
				a.fail(w, invalid("type inválido", "type: use "+strings.Join(eventTypes, ", ")), "")
				return
			}
			f.Types = append(f.Types, x)
		}
	}
	if f.EntityKind != "" && !slices.Contains(entityKinds, f.EntityKind) {
		a.fail(w, invalid("entity_kind inválido", "entity_kind: use "+strings.Join(entityKinds, ", ")), "")
		return
	}
	if f.Since, err = timeParam(q, "since"); err == nil {
		f.Until, err = timeParam(q, "until")
	}
	if err != nil {
		a.fail(w, err, "")
		return
	}
	page, err := a.Events(r.Context(), EventsQuery{Filter: f, Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.fail(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ---- audit ----

// AuditQuery selects Audit.
type AuditQuery struct {
	User   string
	Action string // exact, or prefix ending in "." (entity.)
	Target string
	Result string // ok | error
	Since  time.Time
	Until  time.Time
	Limit  int // 1..1000 (<= 0 = 100)
	Cursor string
}

// AuditPage is the body of GET /api/v1/audit.
type AuditPage struct {
	Entries    []store.AuditEntry `json:"entries"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func (q AuditQuery) filter() (store.AuditFilter, error) {
	if q.Result != "" && q.Result != store.ResultOK && q.Result != store.ResultError {
		return store.AuditFilter{}, invalid("result inválido", "result: use ok ou error")
	}
	off, err := offsetCursor(q.Cursor, "a:")
	if err != nil {
		return store.AuditFilter{}, err
	}
	action := q.Action
	if strings.HasSuffix(action, ".") {
		action += "*"
	}
	return store.AuditFilter{
		Since: q.Since, Until: q.Until, User: q.User, Action: action, Target: q.Target, Result: q.Result,
		Limit: q.Limit, Offset: off,
	}, nil
}

// Audit reads the audit trail newest first, paginated by an offset cursor.
func (a *API) Audit(ctx context.Context, q AuditQuery) (AuditPage, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	f, err := q.filter()
	if err != nil {
		return AuditPage{}, err
	}
	f.Limit++
	rows, err := a.st.Audit.Query(ctx, f)
	if err != nil {
		return AuditPage{}, dbErr(err)
	}
	page := AuditPage{Entries: []store.AuditEntry{}}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		page.NextCursor = encodeCursor("a:" + strconv.Itoa(f.Offset+q.Limit))
	}
	page.Entries = append(page.Entries, rows...)
	return page, nil
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format, err := formatParam(q)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	limit, err := intParam(q, "limit", 100, 1, 1000)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	aq := AuditQuery{
		User: q.Get("user"), Action: q.Get("action"), Target: q.Get("target"), Result: q.Get("result"),
		Limit: limit, Cursor: q.Get("cursor"),
	}
	if aq.Since, err = timeParam(q, "since"); err == nil {
		aq.Until, err = timeParam(q, "until")
	}
	if err != nil {
		a.fail(w, err, "")
		return
	}
	if format == store.FormatCSV {
		f, err := aq.filter()
		if err != nil {
			a.fail(w, err, "")
			return
		}
		if q.Get("limit") == "" {
			f.Limit = 0 // the export takes every matching row
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
		if err := a.st.Audit.Export(r.Context(), w, store.FormatCSV, f); err != nil {
			a.log.Error("painel: exportar auditoria", "err", err)
		}
		return
	}
	page, err := a.Audit(r.Context(), aq)
	if err != nil {
		a.fail(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
