// Package backup is the wwz-panel SQLite snapshot and restore (spec 016,
// "Backup e restauração"; ADR 0009): VACUUM INTO a temporary file renamed to
// <dir>/panel-AAAAMMDD.db, retention by name, the exclusive flock that serve
// keeps on WWZ_PANEL_DB+".lock", and the restore that checks
// integrity_check and user_version before the atomic rename.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// DefaultKeep is the number of snapshots retained (WWZ_PANEL_BACKUP_KEEP).
const DefaultKeep = 7

// Errors returned by the package.
var (
	// ErrLocked: a serve holds the lock of the database.
	ErrLocked = errors.New("backup: painel em execução")
	// ErrInvalid: the snapshot fails integrity_check or has an unsupported
	// user_version.
	ErrInvalid = errors.New("backup: snapshot inválido")
)

var nameRE = regexp.MustCompile(`^panel-\d{8}\.db$`)

// Name is the snapshot file name of the UTC day of t.
func Name(t time.Time) string { return "panel-" + t.UTC().Format("20060102") + ".db" }

// LockPath is the lock file of the database at db.
func LockPath(db string) string { return db + ".lock" }

// Snapshot writes a consistent copy of db to <dir>/panel-AAAAMMDD.db (UTC
// day of now; the one of the same day is replaced) through VACUUM INTO a
// temporary file plus rename, then keeps only the newest keep snapshots
// (<= 0 = DefaultKeep). It returns the snapshot path.
func Snapshot(ctx context.Context, db *sql.DB, dir string, now time.Time, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	name := Name(now)
	final := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".tmp")
	_ = os.Remove(tmp) // VACUUM INTO refuses a non-empty existing file
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backup: VACUUM INTO: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backup: %w", err)
	}
	return final, Prune(dir, keep)
}

// Prune removes the oldest panel-AAAAMMDD.db of dir beyond keep (<= 0 =
// DefaultKeep); other files are left alone.
func Prune(dir string, keep int) error {
	if keep <= 0 {
		keep = DefaultKeep
	}
	entries, err := os.ReadDir(dir) // sorted by name = by day
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && nameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	var errs []error
	for _, n := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			errs = append(errs, fmt.Errorf("backup: retenção: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Next is the first instant after now at the UTC time at ("HH:MM").
func Next(now time.Time, at string) (time.Time, error) {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup: horário %q: %w", at, err)
	}
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

// Restore replaces the database dst with the snapshot src, with the panel
// stopped: it takes the lock of dst without blocking (ErrLocked when a serve
// holds it), copies src to <dst>.restore.tmp (fsync), checks the copy with
// Verify, removes the -wal/-shm of dst and renames the copy over dst. An
// invalid snapshot leaves dst untouched.
func Restore(ctx context.Context, src, dst string) error {
	if same(src, dst) {
		return fmt.Errorf("%w: a origem é o próprio banco", ErrInvalid)
	}
	unlock, err := Lock(LockPath(dst))
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	tmp := dst + ".restore.tmp"
	err = copyFile(src, tmp)
	if err == nil {
		err = Verify(ctx, tmp)
	}
	if err == nil {
		err = removeSide(dst)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		_ = removeSide(tmp)
		return fmt.Errorf("backup: restauração: %w", err)
	}
	_ = removeSide(tmp)
	syncDir(filepath.Dir(dst))
	return nil
}

// Verify checks that the SQLite file at path passes PRAGMA integrity_check
// and has 1 <= user_version <= store.SchemaVersion.
func Verify(ctx context.Context, path string) error {
	db, _ := sql.Open(store.DriverName, "file:"+(&url.URL{Path: path}).EscapedPath()) // driver registered by store
	defer func() { _ = db.Close() }()
	var res string
	var v int
	err := db.QueryRowContext(ctx, `SELECT (SELECT integrity_check FROM pragma_integrity_check LIMIT 1),
		(SELECT user_version FROM pragma_user_version)`).Scan(&res, &v)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case res != "ok":
		return fmt.Errorf("%w: integrity_check: %s", ErrInvalid, res)
	case v < 1 || v > store.SchemaVersion:
		return fmt.Errorf("%w: user_version %d fora de 1..%d", ErrInvalid, v, store.SchemaVersion)
	}
	return nil
}

// copyFile copies src to dst (created or truncated) and fsyncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the operator names the snapshot
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: next to WWZ_PANEL_DB
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

// removeSide removes the -wal and -shm of the database at db (absent is
// fine).
func removeSide(db string) error {
	var errs []error
	for _, s := range []string{"-wal", "-shm"} {
		if err := os.Remove(db + s); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncDir fsyncs a directory so the rename survives a crash (best effort).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: directory of WWZ_PANEL_DB
		_ = d.Sync()
		_ = d.Close()
	}
}

// same reports whether a and b name the same file.
func same(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(sa, sb)
}
