package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// GeneratedPasswordLen is the length of the bootstrap password generated when
// WWZ_PANEL_ADMIN_PASSWORD is empty (ADR 0005).
const GeneratedPasswordLen = 24

// passwordAlphabet has 64 symbols, so byte%64 is uniform.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// randRead is crypto/rand.Read, replaceable in tests.
var randRead = rand.Read

// Hasher turns a password into the PHC string stored in users.password_hash.
// Task 047 implements it with argon2id.
type Hasher interface {
	Hash(password string) (string, error)
}

// BootstrapResult tells what Bootstrap did.
type BootstrapResult struct {
	Created   bool   // false: users was not empty, nothing done
	User      string // name of the created operator
	Generated bool   // the password was generated (and logged once)
}

// GeneratePassword returns GeneratedPasswordLen random characters from
// crypto/rand.
func GeneratePassword() (string, error) {
	b := make([]byte, GeneratedPasswordLen)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("store: senha aleatória: %w", err)
	}
	for i := range b {
		b[i] = passwordAlphabet[b[i]%byte(len(passwordAlphabet))]
	}
	return string(b), nil
}

// Bootstrap creates the first operator (ADR 0005, spec 016): only when users
// is empty, with source bootstrap. An empty password is replaced by a
// generated one, logged a single time at warn level after the commit; a given
// password is never logged. With users already present it does nothing.
// Count and insert run in one write transaction, so two panels booting on the
// same file create one operator. It must run before the panel.yaml users are
// synced (UpsertYAML). A nil logger uses slog.Default().
func (s *Store) Bootstrap(ctx context.Context, h Hasher, user, password string, logger *slog.Logger) (BootstrapResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return BootstrapResult{}, fmt.Errorf("%w: usuário do bootstrap vazio", ErrInvalidInput)
	}
	var res BootstrapResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		generated := password == ""
		if generated {
			var err error
			if password, err = GeneratePassword(); err != nil {
				return err
			}
		}
		hash, err := h.Hash(password)
		if err != nil {
			return fmt.Errorf("store: hash da senha do bootstrap: %w", err)
		}
		if _, err := s.Users.create(ctx, tx, User{Name: user, Role: RoleOperator, Source: SourceBootstrap, PasswordHash: hash}); err != nil {
			return err
		}
		res = BootstrapResult{Created: true, User: user, Generated: generated}
		return nil
	})
	if err != nil {
		return BootstrapResult{}, err
	}
	switch {
	case !res.Created:
		logger.Debug("painel: bootstrap ignorado, já há usuários")
	case res.Generated:
		// Shown once: the next boot finds users and never reaches this branch.
		logger.Warn("painel: operador inicial criado com senha gerada; anote-a, ela não será exibida de novo",
			"user", user, "password", password)
	default:
		logger.Info("painel: operador inicial criado", "user", user)
	}
	return res, nil
}
