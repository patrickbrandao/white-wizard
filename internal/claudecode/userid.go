package claudecode

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

var userIDKeys = []string{"device_id", "account_uuid", "session_id"}

// UserID returns metadata.user_id: the compact JSON string
// {"device_id":…,"account_uuid":…,"session_id":…}, in this order.
// account is "" with CLAUDE_CODE_OAUTH_TOKEN or an API key.
func UserID(device, account, session string) string {
	return string(Marshal(Object(
		M("device_id", String(device)),
		M("account_uuid", String(account)),
		M("session_id", String(session)),
	)))
}

// ParseUserID parses metadata.user_id and checks its form: exactly the
// [UserID] encoding, device_id with 64 lowercase hex characters,
// account_uuid empty or a lowercase UUID, session_id a lowercase UUID v4.
func ParseUserID(s string) (device, account, session string, err error) {
	v, err := Parse([]byte(s))
	if err != nil {
		return "", "", "", fmt.Errorf("user_id is not JSON: %w", err)
	}
	if v.Kind() != KindObject || !slices.Equal(v.Keys(), userIDKeys) {
		return "", "", "", errors.New("user_id is not {device_id, account_uuid, session_id} in this order")
	}
	for _, m := range v.Members() {
		if m.Value.Kind() != KindString {
			return "", "", "", fmt.Errorf("user_id.%s is not a string", m.Key)
		}
	}
	if !bytes.Equal(Marshal(v), []byte(s)) {
		return "", "", "", errors.New("user_id is not compact JSON.stringify")
	}
	device, account, session = v.Members()[0].Value.Str(), v.Members()[1].Value.Str(), v.Members()[2].Value.Str()
	switch {
	case !isLowerHex(device, 64):
		return "", "", "", fmt.Errorf("device_id %q is not 64 lowercase hex characters", device)
	case account != "" && !IsUUID(account):
		return "", "", "", fmt.Errorf("account_uuid %q is not a lowercase UUID", account)
	case !IsUUIDv4(session):
		return "", "", "", fmt.Errorf("session_id %q is not a lowercase UUID v4", session)
	}
	return device, account, session, nil
}

// IsUUID reports whether s is a UUID in lowercase hex (8-4-4-4-12).
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// IsUUIDv4 reports whether s is a lowercase UUID of version 4 and the
// RFC 4122 variant, as crypto.randomUUID makes.
func IsUUIDv4(s string) bool {
	return IsUUID(s) && s[14] == '4' && (s[19] == '8' || s[19] == '9' || s[19] == 'a' || s[19] == 'b')
}
