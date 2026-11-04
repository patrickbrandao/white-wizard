//go:build !unix

package backup

import (
	"fmt"
	"os"
)

// Lock creates path and holds it open; without flock (non-unix) it does not
// exclude other holders.
func Lock(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: WWZ_PANEL_DB + ".lock"
	if err != nil {
		return nil, fmt.Errorf("backup: lock: %w", err)
	}
	return f.Close, nil
}
