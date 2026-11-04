package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Audit results.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// APITokenUser is the audit user of requests authenticated by
// WWZ_PANEL_API_TOKEN.
const APITokenUser = "api-token"

// AuditEntry is a row of audit.
type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	User   string          `json:"user"`
	Role   string          `json:"role"`
	Action string          `json:"action"` // entity.block, entity.tier, chaos.start, policies.publish, ...
	Target string          `json:"target"` // key/test-01, policies, chaos, scenario/nome
	Diff   json.RawMessage `json:"diff"`   // {"request": ..., "before": ..., "after": ...}
	Result string          `json:"result"` // ok | error
	Error  string          `json:"error,omitempty"`
	IP     string          `json:"ip,omitempty"`
}

// Diff is the audit diff document of spec 016.
type Diff struct {
	Request any `json:"request"`
	Before  any `json:"before"`
	After   any `json:"after"`
}

// NewDiff marshals {"request", "before", "after"} for AuditEntry.Diff.
func NewDiff(request, before, after any) (json.RawMessage, error) {
	return json.Marshal(Diff{Request: request, Before: before, After: after})
}

// Audit is the audit repository (never purged).
type Audit struct{ s *Store }

// Record appends e. Zero At = now; empty Diff = {"request":null,...};
// empty Result = ok. Diff must be valid JSON (ErrInvalidInput).
func (r *Audit) Record(ctx context.Context, e AuditEntry) (AuditEntry, error) {
	if e.At.IsZero() {
		e.At = r.s.now()
	}
	e.At = e.At.UTC().Truncate(time.Millisecond)
	if len(e.Diff) == 0 {
		e.Diff, _ = NewDiff(nil, nil, nil)
	}
	if !json.Valid(e.Diff) {
		return AuditEntry{}, fmt.Errorf("%w: diff não é JSON", ErrInvalidInput)
	}
	if e.Result == "" {
		e.Result = ResultOK
	}
	res, err := r.s.exec(ctx, false, `INSERT INTO audit (at, user, role, action, target, diff, result, error, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(e.At), e.User, e.Role, e.Action, e.Target, string(e.Diff), e.Result, nullStr(e.Error), nullStr(e.IP))
	if err != nil {
		return AuditEntry{}, err
	}
	e.ID, _ = res.LastInsertId()
	return e, nil
}

// AuditFilter selects audit rows; zero fields do not filter. Rows come newest
// first; Limit <= 0 means no limit.
type AuditFilter struct {
	Since  time.Time // at >= Since
	Until  time.Time // at < Until
	User   string
	Action string // exact, or prefix when it ends with "*" (entity.*)
	Target string
	Result string
	Limit  int
	Offset int
}

func (f AuditFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(c string, a any) { conds = append(conds, c); args = append(args, a) }
	if !f.Since.IsZero() {
		add("at >= ?", ms(f.Since))
	}
	if !f.Until.IsZero() {
		add("at < ?", ms(f.Until))
	}
	if f.User != "" {
		add("user = ?", f.User)
	}
	if p, ok := strings.CutSuffix(f.Action, "*"); ok {
		add("substr(action, 1, ?) = ?", len(p))
		args = append(args, p)
	} else if f.Action != "" {
		add("action = ?", f.Action)
	}
	if f.Target != "" {
		add("target = ?", f.Target)
	}
	if f.Result != "" {
		add("result = ?", f.Result)
	}
	q := ""
	if len(conds) > 0 {
		q = " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY at DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, max(f.Offset, 0))
	}
	return q, args
}

func scanAudit(rows *sql.Rows) (AuditEntry, error) {
	var e AuditEntry
	var at int64
	var diff string
	var errS, ip sql.NullString
	err := rows.Scan(&e.ID, &at, &e.User, &e.Role, &e.Action, &e.Target, &diff, &e.Result, &errS, &ip)
	e.At, e.Diff, e.Error, e.IP = fromMS(at), json.RawMessage(diff), errS.String, ip.String
	return e, err
}

// Query returns the rows matching f.
func (r *Audit) Query(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	where, args := f.where()
	return queryAll(ctx, r.s.r, scanAudit,
		"SELECT id, at, user, role, action, target, diff, result, error, ip FROM audit"+where, args...)
}

// Export formats.
const (
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// AuditCSVHeader is the first line of the CSV export.
var AuditCSVHeader = []string{"id", "at", "user", "role", "action", "target", "result", "error", "ip", "diff"}

// Export writes the rows matching f to w as CSV (header AuditCSVHeader, at in
// RFC 3339 UTC with milliseconds) or as a JSON array.
func (r *Audit) Export(ctx context.Context, w io.Writer, format string, f AuditFilter) error {
	if format != FormatCSV && format != FormatJSON {
		return fmt.Errorf("%w: formato %q (csv, json)", ErrInvalidInput, format)
	}
	rows, err := r.Query(ctx, f)
	if err != nil {
		return err
	}
	if format == FormatJSON {
		if rows == nil {
			rows = []AuditEntry{}
		}
		return json.NewEncoder(w).Encode(rows)
	}
	cw := csv.NewWriter(w)
	_ = cw.Write(AuditCSVHeader)
	for _, e := range rows {
		_ = cw.Write([]string{
			strconv.FormatInt(e.ID, 10), e.At.Format("2006-01-02T15:04:05.000Z07:00"), e.User, e.Role,
			e.Action, e.Target, e.Result, e.Error, e.IP, string(e.Diff),
		})
	}
	cw.Flush()
	return cw.Error()
}
