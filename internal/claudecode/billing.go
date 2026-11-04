package claudecode

import (
	"fmt"
	"slices"
	"strings"
)

// billingPrefix starts the text of system[0].
const billingPrefix = "x-anthropic-billing-header: "

// firstPartyFields are billing header fields Claude Code only sends to the
// official first-party API, never to an ANTHROPIC_BASE_URL of its own.
var firstPartyFields = []string{
	"cch", "cc_workload", "cc_is_subagent", "cc_prev_req", "cc_prompt_id",
	"cc_turn_origin", "cc_prompt_index", "cc_turn_index", "cc_gateway", "cc_project",
}

// BillingHeader returns the text of system[0]:
// "x-anthropic-billing-header: cc_version=<version>.<fp>; cc_entrypoint=<ep>;",
// with fp = Fingerprint(firstUserText, version).
func BillingHeader(version string, ep Entrypoint, firstUserText string) string {
	return billingPrefix + "cc_version=" + version + "." + Fingerprint(firstUserText, version) +
		"; cc_entrypoint=" + string(ep) + ";"
}

// Billing is a parsed billing header.
type Billing struct {
	Version     string
	Fingerprint string
	Entrypoint  string
}

// ParseBillingHeader parses the text of system[0]. It accepts only the
// exact form [BillingHeader] produces: the fields cc_version and
// cc_entrypoint, in this order, each followed by ";", separated by one
// space, with a 3 lowercase hex fingerprint; any first-party field (cch=…)
// is refused.
func ParseBillingHeader(text string) (Billing, error) {
	rest, ok := strings.CutPrefix(text, billingPrefix)
	if !ok {
		return Billing{}, fmt.Errorf("text does not start with %q", billingPrefix)
	}
	rest, ok = strings.CutSuffix(rest, ";")
	if !ok {
		return Billing{}, fmt.Errorf("text does not end with \";\"")
	}
	var b Billing
	fields := strings.Split(rest, "; ")
	for _, f := range fields {
		name, _, _ := strings.Cut(f, "=")
		if slices.Contains(firstPartyFields, name) {
			return Billing{}, fmt.Errorf("first-party field %s present", name)
		}
	}
	if len(fields) != 2 {
		return Billing{}, fmt.Errorf("expected the fields cc_version and cc_entrypoint, got %d fields", len(fields))
	}
	ver, ok := strings.CutPrefix(fields[0], "cc_version=")
	if !ok {
		return Billing{}, fmt.Errorf("expected cc_version first, got %q", fields[0])
	}
	ep, ok := strings.CutPrefix(fields[1], "cc_entrypoint=")
	if !ok {
		return Billing{}, fmt.Errorf("expected cc_entrypoint second, got %q", fields[1])
	}
	dot := strings.LastIndexByte(ver, '.')
	if dot < 0 || !isLowerHex(ver[dot+1:], 3) {
		return Billing{}, fmt.Errorf("cc_version %q is not <version>.<3 hex>", ver)
	}
	b.Version, b.Fingerprint, b.Entrypoint = ver[:dot], ver[dot+1:], ep
	return b, nil
}

// isLowerHex reports whether s has n lowercase hex characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
