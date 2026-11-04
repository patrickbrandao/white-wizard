package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

// revokeUserSQL revokes every active session of a user; args: now, now, id.
// expires_at is clamped to now so Purge drops it after the 7-day grace.
const revokeUserSQL = `UPDATE sessions SET revoked = 1, expires_at = min(expires_at, ?), last_seen_at = max(last_seen_at, ?)
	WHERE user_id = ? AND revoked = 0`

// Session is a row of sessions. ID is the sha256 hex of the cookie id
// (HashSessionID); the raw id never reaches the database.
type Session struct {
	ID         string
	UserID     int64
	CSRF       string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Revoked    bool
	IP         string
	UserAgent  string
}

// HashSessionID is the database key of a raw cookie session id.
func HashSessionID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Sessions is the sessions repository.
type Sessions struct{ s *Store }

// Create stores sess. Zero CreatedAt/LastSeenAt become now; ExpiresAt is
// required. A duplicate id returns ErrConflict, an unknown user
// ErrInvalidInput.
func (r *Sessions) Create(ctx context.Context, sess Session) (Session, error) {
	now := r.s.now().UTC().Truncate(time.Millisecond)
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.LastSeenAt.IsZero() {
		sess.LastSeenAt = now
	}
	_, err := r.s.exec(ctx, false, `INSERT INTO sessions (id, user_id, csrf, created_at, last_seen_at, expires_at, revoked, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, sess.CSRF, ms(sess.CreatedAt), ms(sess.LastSeenAt), ms(sess.ExpiresAt),
		sess.Revoked, nullStr(sess.IP), nullStr(sess.UserAgent))
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Get returns the session by hashed id, revoked or expired included
// (ErrNotFound if absent).
func (r *Sessions) Get(ctx context.Context, id string) (Session, error) {
	var sess Session
	var created, seen, expires int64
	var ip, ua sql.NullString
	err := r.s.r.QueryRowContext(ctx, `SELECT id, user_id, csrf, created_at, last_seen_at, expires_at, revoked, ip, user_agent
		FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.UserID, &sess.CSRF, &created, &seen, &expires, &sess.Revoked, &ip, &ua)
	if err != nil {
		return Session{}, notFound(err)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromMS(created), fromMS(seen), fromMS(expires)
	sess.IP, sess.UserAgent = ip.String, ua.String
	return sess, nil
}

// GetActive is Get that also returns ErrNotFound for a revoked or expired
// session (relative to the store clock).
func (r *Sessions) GetActive(ctx context.Context, id string) (Session, error) {
	sess, err := r.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if sess.Revoked || !r.s.now().Before(sess.ExpiresAt) {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// Touch slides an active session: last_seen_at = now, expires_at = now+ttl.
// A revoked or missing session returns ErrNotFound.
func (r *Sessions) Touch(ctx context.Context, id string, ttl time.Duration) error {
	now := r.s.now()
	_, err := r.s.exec(ctx, true, "UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ? AND revoked = 0 AND expires_at > ?",
		ms(now), ms(now.Add(ttl)), id, ms(now))
	return err
}

// Revoke revokes one session (logout). ErrNotFound if absent.
func (r *Sessions) Revoke(ctx context.Context, id string) error {
	now := ms(r.s.now())
	_, err := r.s.exec(ctx, true, "UPDATE sessions SET revoked = 1, expires_at = min(expires_at, ?) WHERE id = ?", now, id)
	return err
}

// RevokeUser revokes every active session of a user and returns how many.
func (r *Sessions) RevokeUser(ctx context.Context, userID int64) (int64, error) {
	now := ms(r.s.now())
	res, err := r.s.exec(ctx, false, revokeUserSQL, now, now, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
