package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/panel/backup"
	"github.com/patrickbrandao/white-wizard/internal/panel/config"
	"github.com/patrickbrandao/white-wizard/internal/panel/store"
)

// backupNow is the clock of the snapshots and of the daily schedule (tests
// move it).
var backupNow = time.Now

// snapshot runs backup.Snapshot on st and, when the file was written (even
// if the retention then failed), records settings.last_backup_at (Unix ms),
// read by /metrics.
func snapshot(ctx context.Context, st *store.Store, dir string, keep int) (string, error) {
	now := backupNow()
	path, err := backup.Snapshot(ctx, st.DB(), dir, now, keep)
	if path != "" {
		err = errors.Join(err, st.Settings.Set(ctx, store.SettingLastBackupAt, strconv.FormatInt(now.UnixMilli(), 10)))
	}
	return path, err
}

// parseCmd parses the flags of a backup/restore subcommand, checks the
// positional arguments with badArgs (non-empty message = usage error) and
// loads the configuration; code >= 0 means "exit with code".
func parseCmd(fs *flag.FlagSet, args []string, stderr io.Writer, cfgPath *string, badArgs func() string) (cfg *config.Config, code int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, exitOK
		}
		return nil, exitUsage
	}
	if msg := badArgs(); msg != "" {
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", fs.Name(), msg)
		return nil, exitUsage
	}
	cfg, err := config.Load(config.Flags{Config: *cfgPath}, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", fs.Name(), err)
		return nil, exitError
	}
	return cfg, -1
}

// runBackup is `wwz-panel backup [--dir D] [-config F]`: one snapshot on
// demand (it may run with serve up; VACUUM INTO reads a consistent view).
func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wwz-panel backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "panel.yaml (sobrepõe WWZ_PANEL_CONFIG)")
	dir := fs.String("dir", "", "diretório dos snapshots (sobrepõe WWZ_PANEL_BACKUP_DIR)")
	cfg, code := parseCmd(fs, args, stderr, cfgPath, func() string {
		if fs.NArg() > 0 {
			return fmt.Sprintf("argumento inesperado %q", fs.Arg(0))
		}
		return ""
	})
	if code >= 0 {
		return code
	}
	d := *dir
	if d == "" {
		d = cfg.Backup.Dir
	}
	if d == "" {
		_, _ = fmt.Fprintln(stderr, "wwz-panel backup: sem diretório (use --dir ou WWZ_PANEL_BACKUP_DIR)")
		return exitUsage
	}
	if _, err := os.Stat(cfg.DB); err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz-panel backup: banco do painel:", err)
		return exitError
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz-panel backup:", err)
		return exitError
	}
	defer func() { _ = st.Close() }()
	path, err := snapshot(ctx, st, d, cfg.Backup.Keep)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz-panel backup:", err)
		return exitError
	}
	_, _ = fmt.Fprintln(stdout, path)
	return exitOK
}

// runRestore is `wwz-panel restore [-config F] <arquivo>`: replaces
// WWZ_PANEL_DB with the snapshot, refusing while a serve holds the lock.
func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wwz-panel restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "panel.yaml (sobrepõe WWZ_PANEL_CONFIG)")
	cfg, code := parseCmd(fs, args, stderr, cfgPath, func() string {
		if fs.NArg() != 1 {
			return "informe exatamente um arquivo de snapshot"
		}
		return ""
	})
	if code >= 0 {
		return code
	}
	src := fs.Arg(0)
	if err := backup.Restore(ctx, src, cfg.DB); err != nil {
		if errors.Is(err, backup.ErrLocked) {
			_, _ = fmt.Fprintln(stderr, "wwz-panel restore: painel em execução; pare o serve antes de restaurar")
		} else {
			_, _ = fmt.Fprintln(stderr, "wwz-panel restore:", err)
		}
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "restaurado %s em %s\n", src, cfg.DB)
	return exitOK
}
