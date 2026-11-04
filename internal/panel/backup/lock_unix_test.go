//go:build unix

package backup

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLockFlockFails(t *testing.T) {
	orig := flock
	t.Cleanup(func() { flock = orig })
	flock = func(int, int) error { return syscall.EBADF }
	if _, err := Lock(filepath.Join(t.TempDir(), "l")); err == nil || errors.Is(err, ErrLocked) || !errors.Is(err, syscall.EBADF) {
		t.Fatalf("flock falhando: %v", err)
	}
}
