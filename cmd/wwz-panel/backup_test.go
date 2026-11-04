package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/backup"
	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// fillPanel adds a user, an audit line and an event to the database at db
// (serve may be running: WAL).
func fillPanel(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Users.Create(ctx, store.User{Name: "ana", Role: "viewer", Source: "yaml", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Audit.Record(ctx, store.AuditEntry{User: "chefe", Role: "operator", Action: "entity.block", Target: "key/k1", Diff: []byte(`{}`), Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Events.InsertBatch(ctx, []store.Event{{StreamID: "9-0", At: time.Now(), Type: "block", Data: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
}

// panelCounts reads users (names), audit and events of the database.
func panelCounts(t *testing.T, db string) (users []string, audit, events int) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	us, _ := st.Users.List(ctx)
	for _, u := range us {
		users = append(users, u.Name)
	}
	_ = st.DB().QueryRow("SELECT (SELECT count(*) FROM audit), (SELECT count(*) FROM events)").Scan(&audit, &events)
	return users, audit, events
}

// TestBackupRestoreRoundTrip is the acceptance test of spec 016 and phase 3:
// backup with serve up, restore refused while it runs, database lost,
// restore, data back and the initial operator not recreated.
func TestBackupRestoreRoundTrip(t *testing.T) {
	db := panelEnv(t)
	dir := filepath.Join(t.TempDir(), "bk")
	s := startServe(t, nil, nil)
	fillPanel(t, db)

	code, out, errs := runCmd(t, "", "backup", "--dir", dir)
	snap := strings.TrimSpace(out)
	if code != exitOK || filepath.Dir(snap) != dir || !strings.HasPrefix(filepath.Base(snap), "panel-") {
		t.Fatalf("backup: %d %q %q", code, out, errs)
	}
	resp, err := http.Get("http://" + s.addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(m), "wwz_panel_last_backup_timestamp_seconds 0\n") || !strings.Contains(string(m), "wwz_panel_last_backup_timestamp_seconds ") {
		t.Fatalf("métrica do último backup:\n%s", m)
	}

	before, _ := os.ReadFile(db)
	if code, _, errs := runCmd(t, "", "restore", snap); code != exitError || !strings.Contains(errs, "painel em execução") {
		t.Fatalf("restore com serve no ar: %d %q", code, errs)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Fatal("restore recusado mexeu no banco")
	}
	if code := s.stop(); code != exitOK {
		t.Fatalf("serve: %d %s", code, s.logs)
	}

	// The database is lost.
	if err := os.WriteFile(db, []byte("perdido"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupted snapshot is refused without touching the current file.
	bad := filepath.Join(t.TempDir(), "ruim.db")
	b, _ := os.ReadFile(snap)
	copy(b, "lixo lixo lixo lixo lixo")
	_ = os.WriteFile(bad, b, 0o600)
	if code, _, errs := runCmd(t, "", "restore", bad); code != exitError || !strings.Contains(errs, "inválido") {
		t.Fatalf("snapshot corrompido: %d %q", code, errs)
	}
	if cur, _ := os.ReadFile(db); string(cur) != "perdido" {
		t.Fatal("snapshot corrompido tocou o banco atual")
	}

	code, out, errs = runCmd(t, "", "restore", snap)
	if code != exitOK || !strings.Contains(out, "restaurado") {
		t.Fatalf("restore: %d %q %q", code, out, errs)
	}
	users, audit, events := panelCounts(t, db)
	if strings.Join(users, ",") != "ana,chefe" || audit != 1 || events != 1 {
		t.Fatalf("dados restaurados: %v %d %d", users, audit, events)
	}

	// serve after the restore: the base has users, so no new operator.
	t.Setenv("WWZ_PANEL_ADMIN_USER", "outro")
	t.Setenv("WWZ_PANEL_ADMIN_PASSWORD", "")
	s = startServe(t, nil, nil)
	resp, err = http.Post("http://"+s.addr+"/api/v1/login", "application/json", strings.NewReader(`{"username":"chefe","password":"senha-forte"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if code := s.stop(); code != exitOK || resp.StatusCode != 200 || strings.Contains(s.logs.String(), "operador inicial") {
		t.Fatalf("boot depois do restore: %d login %d\n%s", code, resp.StatusCode, s.logs)
	}
	if users, _, _ := panelCounts(t, db); strings.Join(users, ",") != "ana,chefe" {
		t.Fatalf("operador recriado: %v", users)
	}
}

func TestBackupCommandErrors(t *testing.T) {
	db := panelEnv(t)
	if code, _, _ := runCmd(t, "", "backup", "-h"); code != exitOK {
		t.Fatalf("-h: %d", code)
	}
	if code, _, _ := runCmd(t, "", "backup", "-nope"); code != exitUsage {
		t.Fatalf("flag ruim: %d", code)
	}
	if code, _, errs := runCmd(t, "", "backup", "--dir", t.TempDir(), "extra"); code != exitUsage || !strings.Contains(errs, `"extra"`) {
		t.Fatalf("argumento: %d %q", code, errs)
	}
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "x")
	if code, _, _ := runCmd(t, "", "backup", "--dir", t.TempDir()); code != exitError {
		t.Fatalf("config inválida: %d", code)
	}
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "")
	// The database does not exist yet: nothing is created.
	if code, _, errs := runCmd(t, "", "backup", "--dir", t.TempDir()); code != exitError || !strings.Contains(errs, "banco do painel") {
		t.Fatalf("banco inexistente: %d %q", code, errs)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("backup criou o banco: %v", err)
	}
	_ = os.WriteFile(db, []byte("não é sqlite, não é sqlite, não é sqlite"), 0o600)
	if code, _, _ := runCmd(t, "", "backup", "--dir", t.TempDir()); code != exitError {
		t.Fatalf("banco inválido: %d", code)
	}
	_ = os.Remove(db)
	fillPanel(t, db)
	file := filepath.Join(t.TempDir(), "arquivo")
	_ = os.WriteFile(file, nil, 0o600)
	if code, _, _ := runCmd(t, "", "backup", "--dir", filepath.Join(file, "sub")); code != exitError {
		t.Fatalf("diretório inválido: %d", code)
	}
	// Directory from WWZ_PANEL_BACKUP_DIR, retention from WWZ_PANEL_BACKUP_KEEP.
	dir := t.TempDir()
	t.Setenv("WWZ_PANEL_BACKUP_DIR", dir)
	t.Setenv("WWZ_PANEL_BACKUP_KEEP", "2")
	orig := backupNow
	t.Cleanup(func() { backupNow = orig })
	for i := range 4 {
		backupNow = func() time.Time { return time.Date(2026, 10, 1+i, 12, 0, 0, 0, time.UTC) }
		if code, _, errs := runCmd(t, "", "backup"); code != exitOK {
			t.Fatalf("backup do dia %d: %d %q", i, code, errs)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "panel-*.db")); len(m) != 2 || filepath.Base(m[0]) != "panel-20261003.db" {
		t.Fatalf("retenção: %v", m)
	}
}

func TestSnapshotSettingFails(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.DB().Exec("CREATE TRIGGER no_settings BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'sem settings'); END"); err != nil {
		t.Fatal(err)
	}
	path, err := snapshot(ctx, st, t.TempDir(), 7)
	if path == "" || err == nil || !strings.Contains(err.Error(), "sem settings") {
		t.Fatalf("setting falhando: %q %v", path, err)
	}
}

func TestRestoreCommandErrors(t *testing.T) {
	panelEnv(t)
	if code, _, _ := runCmd(t, "", "restore", "-h"); code != exitOK {
		t.Fatalf("-h: %d", code)
	}
	if code, _, _ := runCmd(t, "", "restore", "-nope"); code != exitUsage {
		t.Fatalf("flag ruim: %d", code)
	}
	if code, _, _ := runCmd(t, "", "restore", "a.db", "b.db"); code != exitUsage {
		t.Fatalf("dois arquivos: %d", code)
	}
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "x")
	if code, _, _ := runCmd(t, "", "restore", "a.db"); code != exitError {
		t.Fatalf("config inválida: %d", code)
	}
	t.Setenv("WWZ_PANEL_RETENTION_DAYS", "")
	if code, _, errs := runCmd(t, "", "restore", filepath.Join(t.TempDir(), "nao-existe.db")); code != exitError || !strings.Contains(errs, "restauração") {
		t.Fatalf("snapshot inexistente: %d %q", code, errs)
	}
}

// TestServeScheduledBackup moves the clock to just before backup.at: serve
// takes the daily snapshot and logs it.
func TestServeScheduledBackup(t *testing.T) {
	panelEnv(t)
	dir := filepath.Join(t.TempDir(), "bk")
	t.Setenv("WWZ_PANEL_BACKUP_DIR", dir)
	t.Setenv("WWZ_PANEL_BACKUP_AT", "03:00")
	// The clock does not follow the wall time (a slow start under load would
	// pass backup.at): the first reading, the loop's, is 0.1 s before 03:00
	// and every later one is just after it.
	before := time.Date(2026, 10, 5, 2, 59, 59, 900_000_000, time.UTC)
	var reads atomic.Int32
	orig := backupNow
	t.Cleanup(func() { backupNow = orig })
	backupNow = func() time.Time {
		if reads.Add(1) == 1 {
			return before
		}
		return before.Add(200 * time.Millisecond)
	}
	s := startServe(t, nil, nil)
	want := filepath.Join(dir, "panel-20261005.db")
	for i := 0; i < 1500; i++ {
		if _, err := os.Stat(want); err == nil && strings.Contains(s.logs.String(), `"msg":"painel: backup"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := s.stop(); code != exitOK {
		t.Fatalf("serve: %d %s", code, s.logs)
	}
	if _, err := os.Stat(want); err != nil || !strings.Contains(s.logs.String(), `"msg":"painel: backup"`) {
		t.Fatalf("backup agendado: %v\n%s", err, s.logs)
	}
}

func TestScheduledBackupErrors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	var logs syncBuf
	p := &panel{cfg: &config.Config{Backup: config.Backup{Dir: t.TempDir(), At: "03:00", Keep: 7}}, log: slog.New(slog.NewJSONHandler(&logs, nil)), st: st}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	p.backup(cctx)
	if logs.String() != "" {
		t.Fatalf("backup cancelado logou: %s", logs.String())
	}
	p.backup(ctx)
	if !strings.Contains(logs.String(), `"level":"WARN","msg":"painel: backup"`) {
		t.Fatalf("backup falhando: %s", logs.String())
	}
}

// TestServeLocked: a second serve on the same database is refused.
func TestServeLocked(t *testing.T) {
	db := panelEnv(t)
	unlock, err := backup.Lock(backup.LockPath(db))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	var logs syncBuf
	if code := runServe(context.Background(), nil, io.Discard, &logs, nil); code != exitError || !strings.Contains(logs.String(), "banco em uso") {
		t.Fatalf("serve com lock tomado: %d %s", code, logs.String())
	}
}
