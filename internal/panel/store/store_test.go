package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// clock is a fake, settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }

func open(t *testing.T) (*Store, *clock, string) {
	t.Helper()
	c := newClock()
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(ctx, path, WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c, path
}

// raw runs SQL on the writer, for fault injection (triggers, drops).
func raw(t *testing.T, s *Store, q string) {
	t.Helper()
	if _, err := s.w.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func failOn(t *testing.T, s *Store, table, op string) {
	t.Helper()
	raw(t, s, "CREATE TRIGGER fail_"+table+"_"+op+" BEFORE "+op+" ON "+table+" BEGIN SELECT RAISE(ABORT, 'falha injetada'); END")
}

func closed(t *testing.T) *Store {
	t.Helper()
	s, _, _ := open(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeHasher struct{ err error }

func (h fakeHasher) Hash(p string) (string, error) { return "$fake$" + p, h.err }

func TestOpenSchemaIdempotent(t *testing.T) {
	s, _, path := open(t)
	if v, err := s.UserVersion(ctx); err != nil || v != SchemaVersion {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	if _, err := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Second open: schema reapplied without error, data kept.
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if n, _ := s2.Users.Count(ctx); n != 1 {
		t.Fatalf("users after reopen = %d", n)
	}
	var mode string
	if err := s2.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	var fk, busy int
	_ = s2.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
	_ = s2.DB().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy)
	if fk != 1 || busy != 5000 {
		t.Fatalf("foreign_keys=%d busy_timeout=%d", fk, busy)
	}
	// Reader pool is query_only.
	if _, err := s2.r.ExecContext(ctx, "DELETE FROM users"); err == nil {
		t.Fatal("reader accepted a write")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(ctx, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty path: %v", err)
	}
	if _, err := Open(ctx, filepath.Join(t.TempDir(), "nope", "panel.db")); err == nil {
		t.Fatal("missing directory accepted")
	}

	// Newer schema refused.
	_, _, path := open(t)
	db, err := sql.Open(DriverName, dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer schema: %v", err)
	}

	// A conflicting pre-existing table makes the schema fail.
	bad := filepath.Join(t.TempDir(), "bad.db")
	db, _ = sql.Open(DriverName, dsn(bad, false))
	if _, err := db.ExecContext(ctx, "CREATE TABLE sessions (x)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(ctx, bad); err == nil {
		t.Fatal("conflicting schema accepted")
	}

	// Not a database at all.
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(junk, bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, junk); err == nil {
		t.Fatal("junk file accepted")
	}
}

// TestOpenSpecialPath checks that the URI built by dsn reaches the exact file
// on disk with modernc.org/sqlite (ADR 0044), even with URI metacharacters.
func TestOpenSpecialPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b#c?d%e")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "painel 1.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("banco fora do caminho pedido: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "painel 1.db") {
			t.Fatalf("arquivo inesperado %q", e.Name())
		}
	}
}

func TestCheckVersion(t *testing.T) {
	boom := errors.New("boom")
	v := 0
	if err := checkVersion(&v, boom); !errors.Is(err, boom) {
		t.Fatalf("scan error: %v", err)
	}
	v = SchemaVersion + 1
	if err := checkVersion(&v, nil); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("too new: %v", err)
	}
	v = SchemaVersion
	if err := checkVersion(&v, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPathWithSpecialChars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a b?c#d.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created at the literal path: %v", err)
	}
}

func TestBootstrap(t *testing.T) {
	t.Run("empty base with password", func(t *testing.T) {
		s, _, _ := open(t)
		var buf bytes.Buffer
		res, err := s.Bootstrap(ctx, fakeHasher{}, " root ", "s3cret-pw", slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if err != nil || !res.Created || res.Generated || res.User != "root" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		u, err := s.Users.GetByName(ctx, "root")
		if err != nil || u.Role != RoleOperator || u.Source != SourceBootstrap || u.PasswordHash != "$fake$s3cret-pw" {
			t.Fatalf("user %+v %v", u, err)
		}
		if strings.Contains(buf.String(), "s3cret-pw") || !strings.Contains(buf.String(), "operador inicial criado") {
			t.Fatalf("log: %s", buf.String())
		}
	})
	t.Run("empty base without password", func(t *testing.T) {
		s, _, _ := open(t)
		var buf bytes.Buffer
		lg := slog.New(slog.NewJSONHandler(&buf, nil))
		res, err := s.Bootstrap(ctx, fakeHasher{}, "admin", "", lg)
		if err != nil || !res.Created || !res.Generated {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		u, _ := s.Users.GetByName(ctx, "admin")
		pw := strings.TrimPrefix(u.PasswordHash, "$fake$")
		if len(pw) != GeneratedPasswordLen {
			t.Fatalf("generated password %q", pw)
		}
		for _, r := range pw {
			if !strings.ContainsRune(passwordAlphabet, r) {
				t.Fatalf("char %q outside the alphabet", r)
			}
		}
		if strings.Count(buf.String(), pw) != 1 {
			t.Fatalf("password should be logged exactly once: %s", buf.String())
		}
		// Second boot: users not empty, nothing logged with the password.
		buf.Reset()
		res, err = s.Bootstrap(ctx, fakeHasher{}, "admin", "", lg)
		if err != nil || res.Created {
			t.Fatalf("second boot res=%+v err=%v", res, err)
		}
		if strings.Contains(buf.String(), pw) {
			t.Fatal("password logged twice")
		}
	})
	t.Run("base with users", func(t *testing.T) {
		s, _, _ := open(t)
		if _, err := s.Users.UpsertYAML(ctx, "ana", RoleViewer, "$argon2id$x"); err != nil {
			t.Fatal(err)
		}
		res, err := s.Bootstrap(ctx, fakeHasher{}, "admin", "pw", nil)
		if err != nil || res.Created {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if _, err := s.Users.GetByName(ctx, "admin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("admin created over existing users: %v", err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		s, _, _ := open(t)
		if _, err := s.Bootstrap(ctx, fakeHasher{}, "", "x", nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("empty user: %v", err)
		}
		boom := errors.New("boom")
		if _, err := s.Bootstrap(ctx, fakeHasher{err: boom}, "a", "x", nil); !errors.Is(err, boom) {
			t.Fatalf("hasher error: %v", err)
		}
		old := randRead
		randRead = func([]byte) (int, error) { return 0, boom }
		_, err := s.Bootstrap(ctx, fakeHasher{}, "a", "", nil)
		randRead = old
		if !errors.Is(err, boom) {
			t.Fatalf("rand error: %v", err)
		}
		failOn(t, s, "users", "INSERT")
		if _, err := s.Bootstrap(ctx, fakeHasher{}, "a", "x", nil); err == nil {
			t.Fatal("insert failure ignored")
		}
		raw(t, s, "DROP TABLE sessions")
		raw(t, s, "DROP TABLE users")
		if _, err := s.Bootstrap(ctx, fakeHasher{}, "a", "x", nil); err == nil {
			t.Fatal("count failure ignored")
		}
	})
}

func TestUsers(t *testing.T) {
	s, c, _ := open(t)
	u, err := s.Users.Create(ctx, User{Name: "ana", Role: RoleOperator, Source: SourceYAML, PasswordHash: "h1", LastLoginAt: c.now()})
	if err != nil || u.ID == 0 || !u.CreatedAt.Equal(c.now()) {
		t.Fatalf("create %+v %v", u, err)
	}
	if _, err := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.Users.Create(ctx, User{Name: "x", Role: "admin", Source: SourceYAML}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := s.Users.Create(ctx, User{Role: RoleViewer, Source: SourceYAML}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty name: %v", err)
	}
	got, err := s.Users.Get(ctx, u.ID)
	if err != nil || got != u {
		t.Fatalf("get:\n got %+v\nwant %+v (%v)", got, u, err)
	}
	if _, err := s.Users.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	c.add(time.Minute)
	if err := s.Users.SetPassword(ctx, u.ID, "h2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Users.SetPassword(ctx, 999, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set password missing: %v", err)
	}
	if err := s.Users.TouchLogin(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Users.GetByName(ctx, "ana")
	if got.PasswordHash != "h2" || !got.LastLoginAt.Equal(c.now()) || !got.UpdatedAt.Equal(c.now()) {
		t.Fatalf("after updates %+v", got)
	}

	// Role change revokes sessions; same role is a no-op.
	sess := mkSession(t, s, u.ID, "s1", time.Hour)
	if err := s.Users.SetRole(ctx, u.ID, RoleOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); err != nil {
		t.Fatalf("same role revoked the session: %v", err)
	}
	if err := s.Users.SetRole(ctx, u.ID, RoleViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("role change kept the session: %v", err)
	}
	if err := s.Users.SetRole(ctx, 999, RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set role missing: %v", err)
	}
	if err := s.Users.SetRole(ctx, u.ID, "root"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("set role invalid: %v", err)
	}

	// Disable revokes; enable does not need to.
	sess = mkSession(t, s, u.ID, "s2", time.Hour)
	if err := s.Users.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("disable kept the session")
	}
	if err := s.Users.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Users.Get(ctx, u.ID); got.Disabled {
		t.Fatal("still disabled")
	}
	if err := s.Users.SetDisabled(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable missing: %v", err)
	}

	if _, err := s.Users.Create(ctx, User{Name: "bia", Role: RoleViewer, Source: SourceOIDC, OIDCSubject: "iss|1"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Users.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "ana" || list[1].Name != "bia" {
		t.Fatalf("list %+v %v", list, err)
	}
	if b, err := s.Users.GetByOIDCSubject(ctx, "iss|1"); err != nil || b.Name != "bia" {
		t.Fatalf("by subject %+v %v", b, err)
	}
}

func TestUpsertYAML(t *testing.T) {
	s, _, _ := open(t)
	if r, err := s.Users.UpsertYAML(ctx, "ana", RoleViewer, "h1"); err != nil || r != SyncCreated {
		t.Fatalf("create %v %v", r, err)
	}
	ana, _ := s.Users.GetByName(ctx, "ana")
	sess := mkSession(t, s, ana.ID, "y1", time.Hour)
	// Same role: hash refreshed, session kept.
	if r, err := s.Users.UpsertYAML(ctx, "ana", RoleViewer, "h2"); err != nil || r != SyncUpdated {
		t.Fatalf("update %v %v", r, err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); err != nil {
		t.Fatal("same role revoked")
	}
	if r, err := s.Users.UpsertYAML(ctx, "ana", RoleOperator, "h3"); err != nil || r != SyncUpdated {
		t.Fatalf("role update %v %v", r, err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("role change kept session")
	}
	ana, _ = s.Users.GetByName(ctx, "ana")
	if ana.PasswordHash != "h3" || ana.Role != RoleOperator || ana.Source != SourceYAML {
		t.Fatalf("ana %+v", ana)
	}
	// Bootstrap user is never overwritten.
	if _, err := s.Users.Create(ctx, User{Name: "admin", Role: RoleOperator, Source: SourceBootstrap, PasswordHash: "b"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Users.UpsertYAML(ctx, "admin", RoleViewer, "y"); err != nil || r != SyncConflict {
		t.Fatalf("conflict %v %v", r, err)
	}
	if a, _ := s.Users.GetByName(ctx, "admin"); a.PasswordHash != "b" || a.Role != RoleOperator {
		t.Fatalf("bootstrap user overwritten %+v", a)
	}
	if _, err := s.Users.UpsertYAML(ctx, "ana", "bad", "h"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid role: %v", err)
	}

	mkSession(t, s, ana.ID, "y2", time.Hour)
	failOn(t, s, "sessions", "UPDATE")
	if _, err := s.Users.UpsertYAML(ctx, "ana", RoleViewer, "h"); err == nil {
		t.Fatal("revoke failure ignored")
	}
	raw(t, s, "DROP TABLE sessions")
	raw(t, s, "DROP TABLE users")
	if _, err := s.Users.UpsertYAML(ctx, "ana", RoleViewer, "h"); err == nil {
		t.Fatal("select failure ignored")
	}
}

func TestUpsertOIDC(t *testing.T) {
	s, c, _ := open(t)
	u, err := s.Users.UpsertOIDC(ctx, "iss|42", "ana@x", RoleViewer)
	if err != nil || u.Source != SourceOIDC || u.OIDCSubject != "iss|42" || u.PasswordHash != "" {
		t.Fatalf("create %+v %v", u, err)
	}
	sess := mkSession(t, s, u.ID, "o1", time.Hour)
	c.add(time.Second)
	u2, err := s.Users.UpsertOIDC(ctx, "iss|42", "ana@y", RoleViewer)
	if err != nil || u2.ID != u.ID || u2.Name != "ana@y" || !u2.UpdatedAt.Equal(c.now()) {
		t.Fatalf("update %+v %v", u2, err)
	}
	if got, _ := s.Users.Get(ctx, u.ID); got != u2 {
		t.Fatalf("returned user differs from stored:\n%+v\n%+v", got, u2)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); err != nil {
		t.Fatal("same role revoked")
	}
	if _, err := s.Users.UpsertOIDC(ctx, "iss|42", "ana@y", RoleOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sessions.GetActive(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("role change kept session")
	}
	if _, err := s.Users.Create(ctx, User{Name: "local", Role: RoleViewer, Source: SourceYAML}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Users.UpsertOIDC(ctx, "iss|42", "local", RoleOperator); !errors.Is(err, ErrConflict) {
		t.Fatalf("name conflict on update: %v", err)
	}
	if _, err := s.Users.UpsertOIDC(ctx, "iss|43", "local", RoleOperator); !errors.Is(err, ErrConflict) {
		t.Fatalf("name conflict on create: %v", err)
	}
	mkSession(t, s, u.ID, "o2", time.Hour)
	failOn(t, s, "sessions", "UPDATE")
	if _, err := s.Users.UpsertOIDC(ctx, "iss|42", "ana@y", RoleViewer); err == nil {
		t.Fatal("revoke failure ignored")
	}
	raw(t, s, "DROP TABLE sessions")
	raw(t, s, "DROP TABLE users")
	if _, err := s.Users.UpsertOIDC(ctx, "iss|42", "a", RoleViewer); err == nil {
		t.Fatal("select failure ignored")
	}
}

func TestUserRevokeFailures(t *testing.T) {
	s, _, _ := open(t)
	u, _ := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML})
	mkSession(t, s, u.ID, "r1", time.Hour)
	failOn(t, s, "sessions", "UPDATE")
	if err := s.Users.SetRole(ctx, u.ID, RoleOperator); err == nil {
		t.Fatal("set role: revoke failure ignored")
	}
	if err := s.Users.SetDisabled(ctx, u.ID, true); err == nil {
		t.Fatal("disable: revoke failure ignored")
	}
	failOn(t, s, "users", "UPDATE")
	if err := s.Users.SetDisabled(ctx, u.ID, true); err == nil {
		t.Fatal("disable: update failure ignored")
	}
}

func mkSession(t *testing.T, s *Store, uid int64, raw string, ttl time.Duration) Session {
	t.Helper()
	sess, err := s.Sessions.Create(ctx, Session{
		ID: HashSessionID([]byte(raw)), UserID: uid, CSRF: "csrf-" + raw, ExpiresAt: s.now().Add(ttl),
		IP: "203.0.113.7", UserAgent: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestSessions(t *testing.T) {
	s, c, _ := open(t)
	u, _ := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML})
	sess := mkSession(t, s, u.ID, "abc", time.Hour)
	if len(sess.ID) != 64 || sess.ID == "abc" {
		t.Fatalf("id not hashed: %q", sess.ID)
	}
	got, err := s.Sessions.Get(ctx, sess.ID)
	if err != nil || got != sess {
		t.Fatalf("get:\n got %+v\nwant %+v (%v)", got, sess, err)
	}
	if _, err := s.Sessions.Create(ctx, sess); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.Sessions.Create(ctx, Session{ID: "x", UserID: 999, CSRF: "c", ExpiresAt: c.now()}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, err := s.Sessions.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	// Sliding expiration.
	c.add(50 * time.Minute)
	if err := s.Sessions.Touch(ctx, sess.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	c.add(50 * time.Minute)
	if _, err := s.Sessions.GetActive(ctx, sess.ID); err != nil {
		t.Fatalf("touched session expired: %v", err)
	}
	c.add(11 * time.Minute)
	if _, err := s.Sessions.GetActive(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session active: %v", err)
	}
	if err := s.Sessions.Touch(ctx, sess.ID, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch on expired: %v", err)
	}
	if _, err := s.Sessions.GetActive(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active missing: %v", err)
	}

	// Revoke.
	s2 := mkSession(t, s, u.ID, "def", time.Hour)
	if err := s.Sessions.Revoke(ctx, s2.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Sessions.Get(ctx, s2.ID); !g.Revoked {
		t.Fatal("not revoked")
	}
	if err := s.Sessions.Revoke(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
	mkSession(t, s, u.ID, "g1", time.Hour)
	mkSession(t, s, u.ID, "g2", time.Hour)
	if n, err := s.Sessions.RevokeUser(ctx, u.ID); err != nil || n != 3 { // g1, g2 and the expired abc
		t.Fatalf("revoke user n=%d err=%v", n, err)
	}
	// ON DELETE CASCADE.
	raw(t, s, "DELETE FROM users")
	var cnt int
	_ = s.w.QueryRowContext(ctx, "SELECT count(*) FROM sessions").Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("sessions left after user delete: %d", cnt)
	}
}

func TestPurge(t *testing.T) {
	s, c, _ := open(t)
	u, _ := s.Users.Create(ctx, User{Name: "ana", Role: RoleViewer, Source: SourceYAML})
	base := c.now()
	evs := []Event{
		{StreamID: "1-0", At: base.Add(-8 * 24 * time.Hour), Type: EventRequest},
		{StreamID: "2-0", At: base.Add(-6 * 24 * time.Hour), Type: EventRequest},
		{StreamID: "3-0", At: base, Type: EventBlock},
	}
	if _, err := s.Events.InsertBatch(ctx, evs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Audit.Record(ctx, AuditEntry{At: base.Add(-400 * 24 * time.Hour), User: "ana", Role: RoleOperator, Action: "entity.block", Target: "key/a"}); err != nil {
		t.Fatal(err)
	}
	old := mkSession(t, s, u.ID, "old", time.Hour) // expires base+1h
	revoked := mkSession(t, s, u.ID, "rev", 48*time.Hour)
	keep := mkSession(t, s, u.ID, "keep", 30*24*time.Hour)
	_ = s.Sessions.Revoke(ctx, revoked.ID)

	res, err := s.Purge(ctx, 0) // default 7 days
	if err != nil || res.Events != 1 || res.Sessions != 0 {
		t.Fatalf("purge now: %+v %v", res, err)
	}
	c.add(7*24*time.Hour + 2*time.Hour)
	res, err = s.Purge(ctx, 7)
	if err != nil || res.Events != 2 || res.Sessions != 2 {
		t.Fatalf("purge +7d: %+v %v", res, err)
	}
	for _, id := range []string{old.ID, revoked.ID} {
		if _, err := s.Sessions.Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s not purged", id)
		}
	}
	if _, err := s.Sessions.Get(ctx, keep.ID); err != nil {
		t.Fatal("live session purged")
	}
	if a, _ := s.Audit.Query(ctx, AuditFilter{}); len(a) != 1 {
		t.Fatal("audit purged")
	}
	if n, err := s.Events.Purge(ctx, 1); err != nil || n != 0 {
		t.Fatalf("events purge %d %v", n, err)
	}

	if _, err := s.Sessions.Create(ctx, Session{ID: "gone", UserID: u.ID, CSRF: "c", ExpiresAt: c.now().Add(-30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	failOn(t, s, "sessions", "DELETE")
	if _, err := s.Purge(ctx, 7); err == nil {
		t.Fatal("session purge failure ignored")
	}
}

func TestAudit(t *testing.T) {
	s, c, _ := open(t)
	diff, err := NewDiff(map[string]any{"reason": "teste"}, map[string]any{"status": "active"}, map[string]any{"status": "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Audit.Record(ctx, AuditEntry{User: "ana", Role: RoleOperator, Action: "entity.block", Target: "key/a", Diff: diff, IP: "10.0.0.1"})
	if err != nil || e.ID == 0 || e.Result != ResultOK || !e.At.Equal(c.now()) {
		t.Fatalf("record %+v %v", e, err)
	}
	c.add(time.Minute)
	if _, err := s.Audit.Record(ctx, AuditEntry{User: APITokenUser, Role: RoleOperator, Action: "chaos.start", Target: "chaos", Result: ResultError, Error: "502"}); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if _, err := s.Audit.Record(ctx, AuditEntry{User: "ana", Role: RoleOperator, Action: "entity.unblock", Target: "key/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Audit.Record(ctx, AuditEntry{User: "a", Role: "r", Action: "x", Target: "t", Diff: json.RawMessage("{")}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad diff: %v", err)
	}
	if _, err := s.Audit.Record(ctx, AuditEntry{User: "a", Role: "r", Action: "x", Target: "t", Result: "maybe"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad result: %v", err)
	}

	all, err := s.Audit.Query(ctx, AuditFilter{})
	if err != nil || len(all) != 3 || all[0].Action != "entity.unblock" || all[2].Action != "entity.block" {
		t.Fatalf("query all %+v %v", all, err)
	}
	if string(all[0].Diff) != `{"request":null,"before":null,"after":null}` {
		t.Fatalf("default diff %s", all[0].Diff)
	}
	var d Diff
	if err := json.Unmarshal(all[2].Diff, &d); err != nil || d.After.(map[string]any)["status"] != "blocked" {
		t.Fatalf("diff %s", all[2].Diff)
	}
	t0 := c.now().Add(-2 * time.Minute)
	cases := []struct {
		f    AuditFilter
		want int
	}{
		{AuditFilter{User: "ana"}, 2},
		{AuditFilter{Action: "entity.*"}, 2},
		{AuditFilter{Action: "chaos.start"}, 1},
		{AuditFilter{Target: "key/a", Result: ResultOK}, 2},
		{AuditFilter{Result: ResultError}, 1},
		{AuditFilter{Since: t0.Add(time.Minute)}, 2},
		{AuditFilter{Until: t0.Add(time.Minute)}, 1},
		{AuditFilter{Limit: 1, Offset: 1}, 1},
		{AuditFilter{Limit: 2, Offset: -5}, 2},
	}
	for i, tc := range cases {
		got, err := s.Audit.Query(ctx, tc.f)
		if err != nil || len(got) != tc.want {
			t.Errorf("case %d: %d rows, %v", i, len(got), err)
		}
	}

	var buf bytes.Buffer
	if err := s.Audit.Export(ctx, &buf, FormatCSV, AuditFilter{Result: ResultError}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || lines[0] != strings.Join(AuditCSVHeader, ",") ||
		!strings.Contains(lines[1], "2026-10-31T12:01:00.000Z,api-token,operator,chaos.start,chaos,error,502,,") {
		t.Fatalf("csv:\n%s", buf.String())
	}
	buf.Reset()
	if err := s.Audit.Export(ctx, &buf, FormatJSON, AuditFilter{User: "ana"}); err != nil {
		t.Fatal(err)
	}
	var out []AuditEntry
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("json %s %v", buf.String(), err)
	}
	buf.Reset()
	if err := s.Audit.Export(ctx, &buf, FormatJSON, AuditFilter{User: "ninguém"}); err != nil || strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty json %q %v", buf.String(), err)
	}
	if err := s.Audit.Export(ctx, &buf, "xml", AuditFilter{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad format: %v", err)
	}
	if err := s.Audit.Export(ctx, failWriter{}, FormatCSV, AuditFilter{}); err == nil {
		t.Fatal("write failure ignored")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disco cheio") }

func TestSettings(t *testing.T) {
	s, _, _ := open(t)
	if _, ok, err := s.Settings.Get(ctx, SettingSessionKey); ok || err != nil {
		t.Fatalf("absent: %v %v", ok, err)
	}
	if v, err := s.Settings.SetIfAbsent(ctx, SettingSessionKey, "k1"); err != nil || v != "k1" {
		t.Fatalf("set if absent %q %v", v, err)
	}
	if v, err := s.Settings.SetIfAbsent(ctx, SettingSessionKey, "k2"); err != nil || v != "k1" {
		t.Fatalf("set if absent again %q %v", v, err)
	}
	if err := s.Settings.Set(ctx, SettingSessionKey, "k3"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Settings.Get(ctx, SettingSessionKey); v != "k3" || !ok || err != nil {
		t.Fatalf("get %q %v %v", v, ok, err)
	}
	if err := s.Settings.Delete(ctx, SettingSessionKey); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Settings.Get(ctx, SettingSessionKey); ok {
		t.Fatal("not deleted")
	}
	failOn(t, s, "settings", "INSERT")
	if _, err := s.Settings.SetIfAbsent(ctx, "x", "y"); err == nil {
		t.Fatal("insert failure ignored")
	}
}

// TestClosedStore drives every error path that comes from the database
// itself being unavailable.
func TestClosedStore(t *testing.T) {
	s := closed(t)
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error on closed store", name)
		}
	}
	_, err := s.UserVersion(ctx)
	check("UserVersion", err)
	_, err = s.Bootstrap(ctx, fakeHasher{}, "a", "x", nil)
	check("Bootstrap", err)
	_, err = s.Purge(ctx, 7)
	check("Purge", err)
	_, err = s.Users.Create(ctx, User{Name: "a"})
	check("Users.Create", err)
	_, err = s.Users.Get(ctx, 1)
	check("Users.Get", err)
	_, err = s.Users.List(ctx)
	check("Users.List", err)
	_, err = s.Users.Count(ctx)
	check("Users.Count", err)
	check("Users.SetPassword", s.Users.SetPassword(ctx, 1, "h"))
	check("Users.SetRole", s.Users.SetRole(ctx, 1, RoleViewer))
	check("Users.SetDisabled", s.Users.SetDisabled(ctx, 1, true))
	check("Users.TouchLogin", s.Users.TouchLogin(ctx, 1))
	_, err = s.Users.UpsertYAML(ctx, "a", RoleViewer, "h")
	check("Users.UpsertYAML", err)
	_, err = s.Users.UpsertOIDC(ctx, "s", "a", RoleViewer)
	check("Users.UpsertOIDC", err)
	_, err = s.Sessions.Create(ctx, Session{ID: "x"})
	check("Sessions.Create", err)
	_, err = s.Sessions.GetActive(ctx, "x")
	check("Sessions.GetActive", err)
	check("Sessions.Touch", s.Sessions.Touch(ctx, "x", time.Hour))
	check("Sessions.Revoke", s.Sessions.Revoke(ctx, "x"))
	_, err = s.Sessions.RevokeUser(ctx, 1)
	check("Sessions.RevokeUser", err)
	_, err = s.Audit.Record(ctx, AuditEntry{})
	check("Audit.Record", err)
	check("Audit.Export", s.Audit.Export(ctx, &bytes.Buffer{}, FormatCSV, AuditFilter{}))
	_, err = s.Events.InsertBatch(ctx, []Event{{StreamID: "1", Type: EventRequest}})
	check("Events.InsertBatch", err)
	_, err = s.Events.Query(ctx, EventFilter{})
	check("Events.Query", err)
	_, err = s.Events.Count(ctx, EventFilter{})
	check("Events.Count", err)
	_, err = s.Events.Usage(ctx, UsageQuery{GroupBy: GroupModel})
	check("Events.Usage", err)
	_, err = s.Events.UsageSeries(ctx, UsageQuery{}, time.Minute)
	check("Events.UsageSeries", err)
	_, err = s.Events.ToolCallsByName(ctx, UsageQuery{})
	check("Events.ToolCallsByName", err)
	_, _, err = s.Settings.Get(ctx, "k")
	check("Settings.Get", err)
	check("Settings.Set", s.Settings.Set(ctx, "k", "v"))
	_, err = s.Settings.SetIfAbsent(ctx, "k", "v")
	check("Settings.SetIfAbsent", err)
	check("Settings.Delete", s.Settings.Delete(ctx, "k"))
}
