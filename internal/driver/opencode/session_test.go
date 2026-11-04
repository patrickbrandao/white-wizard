package opencode_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestRequireSessionAlwaysOn: x-opencode-session is required by default and
// require_session accepts only true (ADR 0059).
func TestRequireSessionAlwaysOn(t *testing.T) {
	d, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := d.Authenticate(driver.MapHeaders{"authorization": "Bearer k"})
	if len(c.Missing) != 1 || c.Missing[0] != "x-opencode-session" {
		t.Fatalf("Missing = %v, want [x-opencode-session]", c.Missing)
	}
	if _, err := driver.New("opencode", map[string]string{"require_session": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.New("opencode", map[string]string{"require_session": "false"}); err == nil || !strings.Contains(err.Error(), "require_session") {
		t.Fatalf("require_session=false: err = %v", err)
	}
}
