package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Roles and user sources of spec 016.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"

	SourceBootstrap = "bootstrap"
	SourceYAML      = "yaml"
	SourceOIDC      = "oidc"
)

// User is a row of users.
type User struct {
	ID           int64
	Name         string
	Role         string
	Source       string
	PasswordHash string // PHC argon2id; "" for oidc
	OIDCSubject  string // iss|sub; "" for local users
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  time.Time // zero = never
}

// Users is the users repository.
type Users struct{ s *Store }

const userCols = `id, name, role, source, password_hash, oidc_subject, disabled, created_at, updated_at, last_login_at`

func scanUser(r interface{ Scan(...any) error }) (User, error) {
	var u User
	var hash, sub sql.NullString
	var created, updated int64
	var last sql.NullInt64
	err := r.Scan(&u.ID, &u.Name, &u.Role, &u.Source, &hash, &sub, &u.Disabled, &created, &updated, &last)
	u.PasswordHash, u.OIDCSubject = hash.String, sub.String
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = fromMS(created), fromMS(updated), nullMS(last)
	return u, err
}

func scanUserRows(r *sql.Rows) (User, error) { return scanUser(r) }

// Create inserts u (ID, CreatedAt and UpdatedAt are filled). A duplicate name
// or OIDC subject returns ErrConflict; an invalid role or source,
// ErrInvalidInput.
func (r *Users) Create(ctx context.Context, u User) (User, error) {
	return r.create(ctx, r.s.w, u)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (r *Users) create(ctx context.Context, db execer, u User) (User, error) {
	if u.Name == "" {
		return User{}, fmt.Errorf("%w: nome vazio", ErrInvalidInput)
	}
	now := r.s.now()
	u.CreatedAt, u.UpdatedAt = now.UTC().Truncate(time.Millisecond), now.UTC().Truncate(time.Millisecond)
	res, err := db.ExecContext(ctx, `INSERT INTO users (name, role, source, password_hash, oidc_subject, disabled, created_at, updated_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.Role, u.Source, nullStr(u.PasswordHash), nullStr(u.OIDCSubject), u.Disabled,
		ms(now), ms(now), nullTime(u.LastLoginAt))
	if err != nil {
		return User{}, mapErr(err)
	}
	u.ID, _ = res.LastInsertId()
	return u, nil
}

// Get returns the user by id (ErrNotFound if absent).
func (r *Users) Get(ctx context.Context, id int64) (User, error) {
	return r.one(ctx, "id = ?", id)
}

// GetByName returns the user by name (ErrNotFound if absent).
func (r *Users) GetByName(ctx context.Context, name string) (User, error) {
	return r.one(ctx, "name = ?", name)
}

// GetByOIDCSubject returns the user by "iss|sub" (ErrNotFound if absent).
func (r *Users) GetByOIDCSubject(ctx context.Context, subject string) (User, error) {
	return r.one(ctx, "oidc_subject = ?", subject)
}

func (r *Users) one(ctx context.Context, where string, arg any) (User, error) {
	u, err := scanUser(r.s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE "+where, arg))
	return u, notFound(err)
}

// List returns every user ordered by name.
func (r *Users) List(ctx context.Context) ([]User, error) {
	return queryAll(ctx, r.s.r, scanUserRows, "SELECT "+userCols+" FROM users ORDER BY name")
}

// Count returns the number of users.
func (r *Users) Count(ctx context.Context) (int, error) {
	var n int
	err := r.s.r.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

// SetPassword replaces the password hash.
func (r *Users) SetPassword(ctx context.Context, id int64, hash string) error {
	_, err := r.s.exec(ctx, true, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?",
		nullStr(hash), ms(r.s.now()), id)
	return err
}

// SetRole changes the role and revokes the user's sessions (spec 016).
func (r *Users) SetRole(ctx context.Context, id int64, role string) error {
	return r.s.tx(ctx, func(tx *sql.Tx) error {
		now := ms(r.s.now())
		res, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, updated_at = ? WHERE id = ? AND role <> ?", role, now, id, role)
		if err != nil {
			return mapErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Same role (no-op) or missing user.
			var one int
			return notFound(tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = ?", id).Scan(&one))
		}
		_, err = tx.ExecContext(ctx, revokeUserSQL, now, now, id)
		return err
	})
}

// SetDisabled enables or disables the login; disabling revokes the sessions.
func (r *Users) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	return r.s.tx(ctx, func(tx *sql.Tx) error {
		now := ms(r.s.now())
		res, err := tx.ExecContext(ctx, "UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?", disabled, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if !disabled {
			return nil
		}
		_, err = tx.ExecContext(ctx, revokeUserSQL, now, now, id)
		return err
	})
}

// TouchLogin records a successful login.
func (r *Users) TouchLogin(ctx context.Context, id int64) error {
	_, err := r.s.exec(ctx, true, "UPDATE users SET last_login_at = ? WHERE id = ?", ms(r.s.now()), id)
	return err
}

// SyncResult tells what UpsertYAML did.
type SyncResult int

// UpsertYAML outcomes.
const (
	SyncCreated  SyncResult = iota + 1 // new user with source yaml
	SyncUpdated                        // existing yaml user: hash and role refreshed
	SyncConflict                       // name taken by a bootstrap/oidc user: untouched
)

// UpsertYAML synchronizes one panel.yaml user (spec 016): inserts it with
// source yaml, updates hash and role of a user that already came from the
// YAML (revoking its sessions when the role changes) and never overwrites a
// user of another source (SyncConflict; the caller logs it).
func (r *Users) UpsertYAML(ctx context.Context, name, role, hash string) (SyncResult, error) {
	var out SyncResult
	err := r.s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		var source, oldRole string
		err := tx.QueryRowContext(ctx, "SELECT id, source, role FROM users WHERE name = ?", name).Scan(&id, &source, &oldRole)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = r.create(ctx, tx, User{Name: name, Role: role, Source: SourceYAML, PasswordHash: hash})
			out = SyncCreated
			return err
		case err != nil:
			return err
		case source != SourceYAML:
			out = SyncConflict
			return nil
		}
		now := ms(r.s.now())
		if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, password_hash = ?, updated_at = ? WHERE id = ?",
			role, nullStr(hash), now, id); err != nil {
			return mapErr(err)
		}
		out = SyncUpdated
		if oldRole == role {
			return nil
		}
		_, err = tx.ExecContext(ctx, revokeUserSQL, now, now, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// UpsertOIDC creates or updates the user identified by subject ("iss|sub")
// with source oidc, recomputing name and role at every login (spec 016).
// A role change revokes the previous sessions. A name already used by a
// non-OIDC user returns ErrConflict.
func (r *Users) UpsertOIDC(ctx context.Context, subject, name, role string) (User, error) {
	var u User
	err := r.s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := scanUser(tx.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE oidc_subject = ?", subject))
		if errors.Is(err, sql.ErrNoRows) {
			u, err = r.create(ctx, tx, User{Name: name, Role: role, Source: SourceOIDC, OIDCSubject: subject})
			return err
		}
		if err != nil {
			return err
		}
		now := r.s.now()
		if _, err := tx.ExecContext(ctx, "UPDATE users SET name = ?, role = ?, updated_at = ? WHERE id = ?",
			name, role, ms(now), cur.ID); err != nil {
			return mapErr(err)
		}
		if cur.Role != role {
			if _, err := tx.ExecContext(ctx, revokeUserSQL, ms(now), ms(now), cur.ID); err != nil {
				return err
			}
		}
		u = cur
		u.Name, u.Role, u.UpdatedAt = name, role, now.UTC().Truncate(time.Millisecond)
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}
