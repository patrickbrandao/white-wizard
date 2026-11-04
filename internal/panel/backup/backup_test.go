package backup

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

var day0 = time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)

// seed opens a store at path with one user, one audit line and one event.
func seed(t *testing.T, path string) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Users.Create(ctx, store.User{Name: "ana", Role: "operator", Source: "yaml", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Audit.Record(ctx, store.AuditEntry{User: "ana", Role: "operator", Action: "entity.block", Target: "key/k1", Diff: []byte("{}"), Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Events.InsertBatch(ctx, []store.Event{{StreamID: "1-0", At: day0, Type: "request", Data: []byte("{}")}}); err != nil {
		t.Fatal(err)
	}
	return st
}

// counts reads users, audit and events of the database at path.
func counts(t *testing.T, path string) (users, audit, events int) {
	t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+(&url.URL{Path: path}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow("SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM audit), (SELECT count(*) FROM events)").Scan(&users, &audit, &events); err != nil {
		t.Fatal(err)
	}
	return
}

func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "panel-*.db"))
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	slices.Sort(m)
	return m
}

func TestSnapshotRetention(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st := seed(t, filepath.Join(tmp, "panel.db"))
	defer func() { _ = st.Close() }()
	dir := filepath.Join(tmp, "bk")
	// Files outside the pattern survive the retention.
	_ = os.MkdirAll(filepath.Join(dir, "panel-20000101.db"), 0o750) // directory
	_ = os.WriteFile(filepath.Join(dir, "notas.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".panel-20261003.db.tmp"), []byte("lixo"), 0o600) // stale temporary

	for i := range 9 {
		p, err := Snapshot(ctx, st.DB(), dir, day0.AddDate(0, 0, i), 7)
		if err != nil {
			t.Fatalf("dia %d: %v", i, err)
		}
		if want := filepath.Join(dir, Name(day0.AddDate(0, 0, i))); p != want {
			t.Fatalf("caminho %s, esperado %s", p, want)
		}
	}
	got := snapshots(t, dir)
	want := []string{"panel-20000101.db", "panel-20261003.db", "panel-20261004.db", "panel-20261005.db", "panel-20261006.db", "panel-20261007.db", "panel-20261008.db", "panel-20261009.db"}
	if !slices.Equal(got, want) {
		t.Fatalf("retenção: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "notas.txt")); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(m) != 0 {
		t.Fatalf("temporários: %v", m)
	}

	// Same day again: replaced, with the newer data; keep <= 0 = 7.
	if _, err := st.Users.Create(ctx, store.User{Name: "bia", Role: "viewer", Source: "yaml", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	last := day0.AddDate(0, 0, 8).Add(5 * time.Hour)
	p, err := Snapshot(ctx, st.DB(), dir, last, 0)
	if err != nil {
		t.Fatal(err)
	}
	if u, a, e := counts(t, p); u != 2 || a != 1 || e != 1 {
		t.Fatalf("snapshot: %d %d %d", u, a, e)
	}
	if err := Verify(ctx, p); err != nil {
		t.Fatal(err)
	}
	if n := len(snapshots(t, dir)); n != 8 {
		t.Fatalf("mesmo dia gerou arquivo novo: %d", n)
	}
	if Name(time.Date(2026, 1, 2, 23, 0, 0, 0, time.FixedZone("x", -3*3600))) != "panel-20260103.db" {
		t.Fatal("nome não usa o dia UTC")
	}
}

func TestSnapshotErrors(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st := seed(t, filepath.Join(tmp, "panel.db"))
	defer func() { _ = st.Close() }()

	file := filepath.Join(tmp, "arquivo")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := Snapshot(ctx, st.DB(), filepath.Join(file, "sub"), day0, 7); err == nil {
		t.Fatal("diretório sob arquivo aceito")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	dir := filepath.Join(tmp, "bk")
	if _, err := Snapshot(cctx, st.DB(), dir, day0, 7); err == nil || !strings.Contains(err.Error(), "VACUUM INTO") {
		t.Fatalf("contexto cancelado: %v", err)
	}
	// The final name is a non-empty directory: rename fails, no temporary left.
	_ = os.MkdirAll(filepath.Join(dir, Name(day0), "x"), 0o750)
	if _, err := Snapshot(ctx, st.DB(), dir, day0, 7); err == nil {
		t.Fatal("rename sobre diretório aceito")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(m) != 0 {
		t.Fatalf("temporário ficou: %v", m)
	}

	if err := Prune(filepath.Join(tmp, "nao-existe"), 7); err == nil {
		t.Fatal("prune em diretório inexistente")
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(tmp, "ro")
		_ = os.MkdirAll(ro, 0o750)
		for i := range 3 {
			_ = os.WriteFile(filepath.Join(ro, Name(day0.AddDate(0, 0, i))), nil, 0o600)
		}
		_ = os.Chmod(ro, 0o500)
		t.Cleanup(func() { _ = os.Chmod(ro, 0o750) })
		if err := Prune(ro, 1); err == nil || !strings.Contains(err.Error(), "retenção") {
			t.Fatalf("remoção negada: %v", err)
		}
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		now  time.Time
		at   string
		want time.Time
	}{
		{time.Date(2026, 10, 1, 2, 59, 0, 0, time.UTC), "03:00", time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)},
		{time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC), "03:00", time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC), "00:15", time.Date(2027, 1, 1, 0, 15, 0, 0, time.UTC)},
		{time.Date(2026, 10, 1, 1, 0, 0, 0, time.FixedZone("x", -3*3600)), "03:00", time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := Next(c.now, c.at)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("Next(%v, %s) = %v %v, esperado %v", c.now, c.at, got, err, c.want)
		}
	}
	if _, err := Next(day0, "3h"); err == nil {
		t.Fatal("horário inválido aceito")
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "panel.db.lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("segundo lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock, err = Lock(p)
	if err != nil {
		t.Fatalf("lock depois de liberar: %v", err)
	}
	_ = unlock()
	if _, err := Lock(filepath.Join(t.TempDir(), "nao", "existe")); err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("diretório inexistente: %v", err)
	}
	if LockPath("/d/panel.db") != "/d/panel.db.lock" {
		t.Fatal(LockPath("/d/panel.db"))
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	db := filepath.Join(tmp, "panel.db")
	st := seed(t, db)
	snap, err := Snapshot(ctx, st.DB(), filepath.Join(tmp, "bk"), day0, 7)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	// The current database is lost: garbage plus stale -wal/-shm.
	for _, f := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.WriteFile(f, []byte("corrompido"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Restore(ctx, snap, db); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{db + "-wal", db + "-shm", db + ".restore.tmp", db + ".restore.tmp-wal", db + ".restore.tmp-shm"} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s ficou: %v", f, err)
		}
	}
	if u, a, e := counts(t, db); u != 1 || a != 1 || e != 1 {
		t.Fatalf("dados restaurados: %d %d %d", u, a, e)
	}
	// Deleted database: restore recreates it.
	_ = os.Remove(db)
	if err := Restore(ctx, snap, db); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if n, _ := st.Users.Count(ctx); n != 1 {
		t.Fatalf("usuários: %d", n)
	}
}

// corruptIndex builds a valid SQLite (user_version 1) whose index disagrees
// with its table, so integrity_check returns a report instead of an error.
func corruptIndex(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+(&url.URL{Path: path}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"PRAGMA user_version = 1",
		"CREATE TABLE t(a)",
		"CREATE INDEX i ON t(a)",
		"INSERT INTO t VALUES (3), (1), (2)",
		"PRAGMA writable_schema = ON",
		"UPDATE sqlite_schema SET sql = 'CREATE INDEX i ON t(a DESC)' WHERE name = 'i'",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	db := filepath.Join(tmp, "panel.db")
	st := seed(t, db)
	good, err := Snapshot(ctx, st.DB(), filepath.Join(tmp, "bk"), day0, 7)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	orig, _ := os.ReadFile(db)
	untouched := func(what string) {
		t.Helper()
		if b, _ := os.ReadFile(db); string(b) != string(orig) {
			t.Fatalf("%s: banco atual alterado", what)
		}
		if _, err := os.Stat(db + ".restore.tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: temporário ficou: %v", what, err)
		}
	}

	// A serve holds the lock.
	unlock, err := Lock(LockPath(db))
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, good, db); !errors.Is(err, ErrLocked) {
		t.Fatalf("com lock: %v", err)
	}
	_ = unlock()
	untouched("lock")

	// Corrupted header.
	bad := filepath.Join(tmp, "cabecalho.db")
	b, _ := os.ReadFile(good)
	copy(b, "NAO E SQLITE NAO E SQLITE")
	_ = os.WriteFile(bad, b, 0o600)
	if err := Restore(ctx, bad, db); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cabeçalho corrompido: %v", err)
	}
	untouched("cabeçalho")

	// integrity_check reports a problem.
	idx := filepath.Join(tmp, "indice.db")
	corruptIndex(t, idx)
	if err := Restore(ctx, idx, db); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "integrity_check") {
		t.Fatalf("índice corrompido: %v", err)
	}
	untouched("índice")

	// user_version out of range: empty file (0) and a newer schema (99).
	empty := filepath.Join(tmp, "vazio.db")
	_ = os.WriteFile(empty, nil, 0o600)
	if err := Restore(ctx, empty, db); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "user_version 0") {
		t.Fatalf("vazio: %v", err)
	}
	newer := filepath.Join(tmp, "novo.db")
	ndb, _ := sql.Open(store.DriverName, "file:"+newer)
	if _, err := ndb.Exec("PRAGMA user_version = 99; CREATE TABLE x(a)"); err != nil {
		t.Fatal(err)
	}
	_ = ndb.Close()
	if err := Restore(ctx, newer, db); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "user_version 99") {
		t.Fatalf("schema novo: %v", err)
	}
	untouched("user_version")

	// Source is the database itself, missing, or a directory.
	if err := Restore(ctx, db, db); !errors.Is(err, ErrInvalid) {
		t.Fatalf("origem = destino: %v", err)
	}
	if err := Restore(ctx, filepath.Join(tmp, "x.db"), filepath.Join(tmp, "x.db")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("origem = destino inexistentes: %v", err)
	}
	if err := Restore(ctx, filepath.Join(tmp, "nao-existe.db"), db); err == nil {
		t.Fatal("origem inexistente aceita")
	}
	if err := Restore(ctx, tmp, db); err == nil {
		t.Fatal("origem diretório aceita")
	}
	untouched("origem")

	// The -wal of the database cannot be removed (non-empty directory).
	_ = os.MkdirAll(filepath.Join(db+"-wal", "x"), 0o750)
	if err := Restore(ctx, good, db); err == nil {
		t.Fatal("-wal irremovível aceito")
	}
	untouched("-wal")
	_ = os.RemoveAll(db + "-wal")

	// The destination is a non-empty directory: rename fails.
	dirDst := filepath.Join(tmp, "dst")
	_ = os.MkdirAll(filepath.Join(dirDst, "x"), 0o750)
	if err := Restore(ctx, good, dirDst); err == nil {
		t.Fatal("rename sobre diretório aceito")
	}

	// The temporary cannot be created (read-only directory, lock file
	// already there).
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(tmp, "ro")
		_ = os.MkdirAll(ro, 0o750)
		_ = os.WriteFile(filepath.Join(ro, "panel.db.lock"), nil, 0o600)
		_ = os.Chmod(ro, 0o500)
		t.Cleanup(func() { _ = os.Chmod(ro, 0o750) })
		if err := Restore(ctx, good, filepath.Join(ro, "panel.db")); err == nil || errors.Is(err, ErrInvalid) {
			t.Fatalf("diretório somente leitura: %v", err)
		}
	}
	if err := Restore(ctx, good, filepath.Join(tmp, "nao", "existe", "panel.db")); err == nil {
		t.Fatal("destino em diretório inexistente aceito")
	}
}
