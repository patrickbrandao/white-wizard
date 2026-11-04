package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event types of the ev:stream (eventbus/valkey/asyncapi.yaml).
const (
	EventRequest = "request"
	EventError   = "error"
	EventBlock   = "block"
	EventUnblock = "unblock"
	EventBan     = "ban"
	EventUnban   = "unban"
	EventStrike  = "strike"
)

// Event is a row of events.
type Event struct {
	StreamID   string // XADD id; "req:<request_id>" without Valkey
	At         time.Time
	Type       string
	Instance   string
	RequestID  string
	TestID     string
	Driver     string
	Model      string
	EntityKind string
	EntityID   string
	Status     int
	Error      string
	Sample     float64 // 0 = 1 (no sampling); weight in aggregations = 1/Sample
	Data       json.RawMessage
}

// wireEvent is the Event schema of the asyncapi (field d of the stream entry).
type wireEvent struct {
	T         time.Time `json:"t"`
	Type      string    `json:"type"`
	Instance  string    `json:"instance"`
	RequestID string    `json:"request_id"`
	TestID    string    `json:"test_id"`
	Driver    string    `json:"driver"`
	Model     string    `json:"model"`
	Entity    *struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"entity"`
	Status int     `json:"status"`
	Error  string  `json:"error"`
	Sample float64 `json:"sample"`
}

// ParseEvent decodes one asyncapi event JSON (the d field of an ev:stream
// entry) into an Event keyed by streamID; Data keeps the original JSON.
func ParseEvent(streamID string, data []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(data, &w); err != nil {
		return Event{}, fmt.Errorf("%w: evento %s: %v", ErrInvalidInput, streamID, err)
	}
	if w.Type == "" || w.T.IsZero() {
		return Event{}, fmt.Errorf("%w: evento %s sem t ou type", ErrInvalidInput, streamID)
	}
	e := Event{
		StreamID: streamID, At: w.T, Type: w.Type, Instance: w.Instance, RequestID: w.RequestID,
		TestID: w.TestID, Driver: w.Driver, Model: w.Model, Status: w.Status, Error: w.Error,
		Sample: w.Sample, Data: append(json.RawMessage(nil), data...),
	}
	if w.Entity != nil {
		e.EntityKind, e.EntityID = w.Entity.Kind, w.Entity.ID
	}
	return e, nil
}

// Events is the events repository.
type Events struct{ s *Store }

// InsertBatch stores evs in one transaction with INSERT OR IGNORE
// (idempotent by StreamID) and returns how many were new. Empty Data is
// stored as {}; Sample outside (0, 1] is stored as 1. An event without
// StreamID or Type, or whose Data is not JSON, aborts the batch with
// ErrInvalidInput.
func (r *Events) InsertBatch(ctx context.Context, evs []Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	var n int
	err := r.s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO events
			(stream_id, at, type, instance, request_id, test_id, driver, model, entity_kind, entity_id, status, error, sample, data)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, e := range evs {
			if e.StreamID == "" || e.Type == "" {
				return fmt.Errorf("%w: evento sem stream_id ou type", ErrInvalidInput)
			}
			sample := e.Sample
			if sample <= 0 || sample > 1 {
				sample = 1
			}
			data := string(e.Data)
			if data == "" {
				data = "{}"
			}
			if !json.Valid([]byte(data)) {
				return fmt.Errorf("%w: evento %s com data que não é JSON", ErrInvalidInput, e.StreamID)
			}
			status := sql.NullInt64{Int64: int64(e.Status), Valid: e.Status != 0}
			res, err := stmt.ExecContext(ctx, e.StreamID, ms(e.At), e.Type, nullStr(e.Instance), nullStr(e.RequestID),
				nullStr(e.TestID), nullStr(e.Driver), nullStr(e.Model), nullStr(e.EntityKind), nullStr(e.EntityID),
				status, nullStr(e.Error), sample, data)
			if err != nil {
				return err
			}
			k, _ := res.RowsAffected()
			n += int(k)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// EventFilter selects events; zero fields do not filter. Rows come newest
// first; Limit <= 0 means no limit.
type EventFilter struct {
	Types      []string
	Since      time.Time // at >= Since
	Until      time.Time // at < Until
	Driver     string
	Model      string
	EntityKind string
	EntityID   string
	TestID     string
	Limit      int
}

func (f EventFilter) conds() ([]string, []any) {
	var conds []string
	var args []any
	add := func(c string, a any) { conds = append(conds, c); args = append(args, a) }
	if len(f.Types) > 0 {
		conds = append(conds, "type IN (?"+strings.Repeat(", ?", len(f.Types)-1)+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if !f.Since.IsZero() {
		add("at >= ?", ms(f.Since))
	}
	if !f.Until.IsZero() {
		add("at < ?", ms(f.Until))
	}
	for _, c := range []struct{ col, v string }{
		{"driver", f.Driver}, {"model", f.Model}, {"entity_kind", f.EntityKind}, {"entity_id", f.EntityID}, {"test_id", f.TestID},
	} {
		if c.v != "" {
			add(c.col+" = ?", c.v)
		}
	}
	return conds, args
}

func whereSQL(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func scanEvent(rows *sql.Rows) (Event, error) {
	var e Event
	var at int64
	var inst, req, test, drv, model, kind, id, errS sql.NullString
	var status sql.NullInt64
	var data string
	err := rows.Scan(&e.StreamID, &at, &e.Type, &inst, &req, &test, &drv, &model, &kind, &id, &status, &errS, &e.Sample, &data)
	e.At = fromMS(at)
	e.Instance, e.RequestID, e.TestID, e.Driver, e.Model = inst.String, req.String, test.String, drv.String, model.String
	e.EntityKind, e.EntityID, e.Status, e.Error = kind.String, id.String, int(status.Int64), errS.String
	e.Data = json.RawMessage(data)
	return e, err
}

// Query returns the events matching f.
func (r *Events) Query(ctx context.Context, f EventFilter) ([]Event, error) {
	conds, args := f.conds()
	q := `SELECT stream_id, at, type, instance, request_id, test_id, driver, model, entity_kind, entity_id, status, error, sample, data
		FROM events` + whereSQL(conds) + " ORDER BY at DESC, stream_id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return queryAll(ctx, r.s.r, scanEvent, q, args...)
}

// Count returns how many events match f (Limit ignored, sampling ignored).
func (r *Events) Count(ctx context.Context, f EventFilter) (int64, error) {
	conds, args := f.conds()
	var n int64
	err := r.s.r.QueryRowContext(ctx, "SELECT count(*) FROM events"+whereSQL(conds), args...).Scan(&n)
	return n, err
}

// Purge removes events older than retentionDays (<= 0 = 7). Store.Purge
// also drops old sessions; this one touches only events.
func (r *Events) Purge(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	res, err := r.s.exec(ctx, false, "DELETE FROM events WHERE at < ?",
		ms(r.s.now().Add(-time.Duration(retentionDays)*24*time.Hour)))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
