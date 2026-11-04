package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters of spec 016: m=65536 KiB (64 MiB), t=3, p=2, 16-byte
// salt and 32-byte key.
const (
	DefaultMemory  uint32 = 64 * 1024
	DefaultTime    uint32 = 3
	DefaultThreads uint8  = 2
	DefaultSaltLen        = 16
	DefaultKeyLen         = 32
)

// Sanity bounds for hashes read from panel.yaml or the database, so a
// malformed hash cannot panic argon2 or allocate without limit.
const (
	maxMemory = 4 * 1024 * 1024 // KiB = 4 GiB
	maxTime   = 64
	minKeyLen = 16
	maxKeyLen = 1024
)

// ErrInvalidHash is returned by Verify for a string that is not an argon2id
// PHC hash with acceptable parameters.
var ErrInvalidHash = errors.New("auth: hash argon2id inválido")

// randRead is crypto/rand.Read, replaceable in tests.
var randRead = rand.Read

// Argon2Hasher hashes passwords with argon2id in PHC format
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash, base64 without padding).
// The zero value uses the parameters of spec 016. It implements
// store.Hasher.
type Argon2Hasher struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen int
	KeyLen  int
}

func (h Argon2Hasher) params() Argon2Hasher {
	if h.Memory == 0 {
		h.Memory = DefaultMemory
	}
	if h.Time == 0 {
		h.Time = DefaultTime
	}
	if h.Threads == 0 {
		h.Threads = DefaultThreads
	}
	if h.SaltLen == 0 {
		h.SaltLen = DefaultSaltLen
	}
	if h.KeyLen == 0 {
		h.KeyLen = DefaultKeyLen
	}
	return h
}

// Hash returns the PHC string of password with a fresh random salt.
func (h Argon2Hasher) Hash(password string) (string, error) {
	p := h.params()
	salt := make([]byte, p.SaltLen)
	if _, err := randRead(salt); err != nil {
		return "", fmt.Errorf("auth: sal aleatório: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(p.KeyLen)) //nolint:gosec // G115: KeyLen is small and positive
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC hash, using the
// parameters stored in the hash itself and a constant-time comparison.
// A malformed hash returns ErrInvalidHash.
func Verify(password, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, ErrInvalidHash
	}
	var m, t uint32
	var p uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || n != 3 ||
		fmt.Sprintf("m=%d,t=%d,p=%d", m, t, p) != parts[3] {
		return false, ErrInvalidHash
	}
	if t < 1 || t > maxTime || p < 1 || m < 8*uint32(p) || m > maxMemory {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < minKeyLen || len(want) > maxKeyLen {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want))) //nolint:gosec // G115: bounded by maxKeyLen
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
