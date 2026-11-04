//go:build unix

package backup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// flock is syscall.Flock (tests replace it).
var flock = syscall.Flock

// Lock takes the exclusive flock of path (created if needed) without
// blocking: ErrLocked when another holder has it. unlock releases it.
func Lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: WWZ_PANEL_DB + ".lock"
	if err != nil {
		return nil, fmt.Errorf("backup: lock: %w", err)
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: a descriptor fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("backup: lock: %w", err)
	}
	return f.Close, nil // closing the descriptor releases the flock
}
