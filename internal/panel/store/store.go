// Package store is the wwz-panel SQLite database (spec 016, "Schema SQLite"):
// schema created on boot with CREATE ... IF NOT EXISTS and versioned in
// PRAGMA user_version (no versioned migrations, specs/002), plus the
// repositories Users, Sessions, Audit, Events and Settings and the first
// operator bootstrap (ADR 0005). Driver: modernc.org/sqlite (ADR 0044, no
// CGO; registered as "sqlite"). Times are Unix milliseconds in the database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DriverName is the database/sql driver registered by modernc.org/sqlite.
const DriverName = "sqlite"

// SchemaVersion is the PRAGMA user_version written by this build.
const SchemaVersion = 1

// DefaultRetentionDays is the events retention when none is given.
const DefaultRetentionDays = 7

// sessionGrace is how long expired or revoked sessions are kept.
const sessionGrace = 7 * 24 * time.Hour

// Errors returned by the store.
var (
	ErrNotFound      = errors.New("store: não encontrado")
	ErrConflict      = errors.New("store: já existe")
	ErrSchemaTooNew  = errors.New("store: banco com user_version maior que o suportado")
	ErrInvalidInput  = errors.New("store: entrada inválida")
	errNoRowsUpdated = ErrNotFound
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  role          TEXT NOT NULL CHECK (role IN ('viewer','operator')),
  source        TEXT NOT NULL CHECK (source IN ('bootstrap','yaml','oidc')),
  password_hash TEXT,
  oidc_subject  TEXT UNIQUE,
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_login_at INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  revoked      INTEGER NOT NULL DEFAULT 0,
  ip           TEXT,
  user_agent   TEXT
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS audit (
  id     INTEGER PRIMARY KEY,
  at     INTEGER NOT NULL,
  user   TEXT NOT NULL,
  role   TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  diff   TEXT NOT NULL,
  result TEXT NOT NULL CHECK (result IN ('ok','error')),
  error  TEXT,
  ip     TEXT
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
CREATE TABLE IF NOT EXISTS events (
  stream_id   TEXT PRIMARY KEY,
  at          INTEGER NOT NULL,
  type        TEXT NOT NULL,
  instance    TEXT,
  request_id  TEXT,
  test_id     TEXT,
  driver      TEXT,
  model       TEXT,
  entity_kind TEXT,
  entity_id   TEXT,
  status      INTEGER,
  error       TEXT,
  sample      REAL NOT NULL DEFAULT 1,
  data        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_at ON events(at);
CREATE INDEX IF NOT EXISTS events_type_at ON events(type, at);
CREATE INDEX IF NOT EXISTS events_entity ON events(entity_kind, entity_id, at);
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// Store is an open panel database. Writes go through a single connection
// (one writer, BEGIN IMMEDIATE); reads use a small query_only pool (WAL).
type Store struct {
	w   *sql.DB
	r   *sql.DB
	now func() time.Time

	Users    *Users
	Sessions *Sessions
	Audit    *Audit
	Events   *Events
	Settings *Settings
}

// Option configures Open.
type Option func(*Store)

// WithClock replaces time.Now (tests, purge by retention).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// dsn builds the driver URI for path with the pragmas of spec 016.
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(wal)")
		q.Add("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the SQLite file at path, applies the schema
// in a transaction and sets PRAGMA user_version. A database whose
// user_version is greater than SchemaVersion returns ErrSchemaTooNew.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: caminho do banco vazio", ErrInvalidInput)
	}
	s := &Store{now: time.Now}
	for _, o := range opts {
		o(s)
	}
	// sql.Open only fails for an unregistered driver; errors surface on the
	// first use (migrate).
	w, _ := sql.Open(DriverName, dsn(path, false))
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	r, _ := sql.Open(DriverName, dsn(path, true))
	r.SetMaxOpenConns(4)
	s.w, s.r = w, r
	s.Users = &Users{s}
	s.Sessions = &Sessions{s}
	s.Audit = &Audit{s}
	s.Events = &Events{s}
	s.Settings = &Settings{s}
	return s, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var v int
	if err := checkVersion(&v, tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schema+fmt.Sprintf("PRAGMA user_version = %d;", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// checkVersion validates the user_version read into *v (scanErr = read
// failure).
func checkVersion(v *int, scanErr error) error {
	if scanErr != nil {
		return scanErr
	}
	if *v > SchemaVersion {
		return fmt.Errorf("%w (%d > %d)", ErrSchemaTooNew, *v, SchemaVersion)
	}
	return nil
}

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// DB is the writer pool, for maintenance (VACUUM INTO, integrity_check).
func (s *Store) DB() *sql.DB { return s.w }

// UserVersion reads PRAGMA user_version.
func (s *Store) UserVersion(ctx context.Context) (int, error) {
	var v int
	err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// PurgeResult counts the rows removed by Purge.
type PurgeResult struct {
	Events   int64
	Sessions int64
}

// Purge removes events older than retentionDays (<= 0 = DefaultRetentionDays)
// and sessions expired or revoked more than 7 days ago. audit is never purged.
func (s *Store) Purge(ctx context.Context, retentionDays int) (PurgeResult, error) {
	var res PurgeResult
	var err error
	if res.Events, err = s.Events.Purge(ctx, retentionDays); err != nil {
		return res, err
	}
	r, err := s.exec(ctx, false, "DELETE FROM sessions WHERE expires_at < ?", ms(s.now().Add(-sessionGrace)))
	if err != nil {
		return res, err
	}
	res.Sessions, _ = r.RowsAffected()
	return res, nil
}

// tx runs fn in a write transaction (BEGIN IMMEDIATE on the single writer).
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// exec runs one write statement and maps "0 rows" to ErrNotFound when
// mustAffect is set.
func (s *Store) exec(ctx context.Context, mustAffect bool, q string, args ...any) (sql.Result, error) {
	r, err := s.w.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	if mustAffect {
		if n, _ := r.RowsAffected(); n == 0 {
			return nil, errNoRowsUpdated
		}
	}
	return r, nil
}

// queryAll runs a read query and scans every row.
func queryAll[T any](ctx context.Context, db *sql.DB, scan func(*sql.Rows) (T, error), q string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// mapErr turns constraint violations into ErrConflict / ErrInvalidInput.
func mapErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_CHECK, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY, sqlite3.SQLITE_CONSTRAINT_NOTNULL:
			return fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
	}
	return err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ms converts a time to Unix milliseconds (zero time = 0).
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMS converts Unix milliseconds back (0 = zero time).
func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func nullMS(v sql.NullInt64) time.Time { return fromMS(v.Int64) }

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullTime(t time.Time) sql.NullInt64 { return sql.NullInt64{Int64: ms(t), Valid: !t.IsZero()} }
