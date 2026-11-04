package store

import (
	"context"
	"database/sql"
	"errors"
)

// Well-known settings keys.
const (
	SettingSessionKey   = "session_key"    // HMAC key generated on first boot
	SettingLastBackupAt = "last_backup_at" // Unix ms of the last snapshot
)

// Settings is the key/value repository.
type Settings struct{ s *Store }

// Get returns the value of key; ok is false when absent.
func (r *Settings) Get(ctx context.Context, key string) (value string, ok bool, err error) {
	err = r.s.r.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// Set writes key = value (insert or replace).
func (r *Settings) Set(ctx context.Context, key, value string) error {
	_, err := r.s.exec(ctx, false, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, ms(r.s.now()))
	return err
}

// SetIfAbsent writes key = value only when key does not exist and returns
// the stored value (the existing one or value). Used for the generated
// session key, so concurrent first boots agree on one key.
func (r *Settings) SetIfAbsent(ctx context.Context, key, value string) (string, error) {
	var stored string
	err := r.s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES (?, ?, ?)",
			key, value, ms(r.s.now())); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&stored)
	})
	return stored, err
}

// Delete removes key (absent is not an error).
func (r *Settings) Delete(ctx context.Context, key string) error {
	_, err := r.s.exec(ctx, false, "DELETE FROM settings WHERE key = ?", key)
	return err
}
