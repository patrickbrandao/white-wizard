package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// State is what the client keeps between runs (spec 015, --state): the
// tokens of the last login, the account, the device id (generated once) and
// the last conversation. It lives outside the repository, in
// $XDG_STATE_HOME/wwz-load/claude-code.json by default, with mode 0600.
type State struct {
	DeviceID         string        `json:"device_id"`
	Target           string        `json:"target,omitempty"`
	AccessToken      string        `json:"access_token,omitempty"`
	RefreshToken     string        `json:"refresh_token,omitempty"`
	ExpiresAt        int64         `json:"expires_at,omitempty"`         // Unix seconds
	RefreshExpiresAt int64         `json:"refresh_expires_at,omitempty"` // Unix seconds
	Scopes           []string      `json:"scopes,omitempty"`
	Account          *Account      `json:"account,omitempty"`
	Organization     *Organization `json:"organization,omitempty"`
	Plan             string        `json:"plan,omitempty"`
	Conversation     *Conversation `json:"conversation,omitempty"`
}

// Conversation is the last conversation of ask, for --resume.
type Conversation struct {
	SessionID  string `json:"session_id"`
	Entrypoint string `json:"entrypoint"`
	Auth       string `json:"auth"`
	Model      string `json:"model"`
	Turns      []Turn `json:"turns"`
}

// stateFile is the name of the state under the state directory.
const stateFile = "wwz-load/claude-code.json"

// DefaultStatePath returns $XDG_STATE_HOME/wwz-load/claude-code.json or,
// without the variable, ~/.local/state/wwz-load/claude-code.json.
func DefaultStatePath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, stateFile), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state: %w (pass --state)", err)
	}
	return filepath.Join(home, ".local", "state", stateFile), nil
}

// NewDeviceID returns 64 random lowercase hex characters, like the
// device id of Claude Code.
func NewDeviceID() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LoadState reads the state at path. A missing file gives a new state with
// a new device id, saved at once so the id is generated only once.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path
	if errors.Is(err, fs.ErrNotExist) {
		st := &State{DeviceID: NewDeviceID()}
		return st, st.Save(path)
	}
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if b, err := hex.DecodeString(st.DeviceID); err != nil || len(b) != 32 || hex.EncodeToString(b) != st.DeviceID {
		return nil, fmt.Errorf("state %s: device_id %q is not 64 lowercase hex characters", path, st.DeviceID)
	}
	return st, nil
}

// Save writes the state at path with mode 0600 (directories 0700), through
// <path>.tmp renamed over the old one.
func (s *State) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	data, _ := json.MarshalIndent(s, "", "  ") //nolint:gosec // G117: the state holds the tokens on purpose (mode 0600, outside the repository)
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: %w", err)
	}
	return nil
}
