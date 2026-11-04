package drivertest_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
)

// TestKnownBugSkipsTheNamedSubtest: a known bug skips its subtest (the run
// passes even when the driver is broken there) and a name that matches no
// subtest is reported as stale.
func TestKnownBugSkipsTheNamedSubtest(t *testing.T) {
	broken := wrap(mustNew(t, "openai"), &mutation{name: "no credential", auth: nil, policy: zeroPolicyField("Entities")})
	if runSuite(broken) {
		t.Fatal("the broken driver passes the suite; the test cannot show the skip")
	}
	if !runSuite(broken, drivertest.KnownBug("LimitPolicy", "task 000")) {
		t.Error("a KnownBug did not skip its subtest")
	}
	if runSuite(mustNew(t, "openai"), drivertest.KnownBug("NoSuchGroup", "task 000")) {
		t.Error("a stale KnownBug did not fail the run")
	}
}

// TestProfileKnownBugs: the bugs a profile lists work like the option, on
// the profile of the driver and on the profile of its Gemini dialect; an
// option wins over the profile's entry of the same name.
func TestProfileKnownBugs(t *testing.T) {
	p, _ := drivertest.ProfileFor("openai")
	p.KnownBugs = map[string]string{"IDs": "task 000", "NoSuchGroup": "task 000"}
	if runSuite(mustNew(t, "openai"), drivertest.WithProfile(p)) {
		t.Error("the stale entry of the profile did not fail the run")
	}
	p.KnownBugs = map[string]string{"IDs": "task 000"}
	if !runSuite(mustNew(t, "openai"), drivertest.WithProfile(p)) {
		t.Error("the profile's KnownBug broke the run")
	}
	if !runSuite(mustNew(t, "openai"), drivertest.WithProfile(p), drivertest.KnownBug("IDs", "option wins")) {
		t.Error("the option and the profile entry together broke the run")
	}

	oc, _ := drivertest.ProfileFor("opencode")
	if oc.Gemini == nil {
		t.Fatal("opencode profile without a Gemini dialect")
	}
	g := *oc.Gemini
	g.KnownBugs = map[string]string{"dialeto/gemini/IDs": "task 000"}
	oc.Gemini = &g
	if !runSuite(mustNew(t, "opencode"), drivertest.WithProfile(oc)) {
		t.Error("a Gemini dialect KnownBug broke the run")
	}
	if !runSuite(mustNew(t, "opencode"), drivertest.WithProfile(oc), drivertest.KnownBug("dialeto/gemini/IDs", "option wins")) {
		t.Error("the option and the Gemini dialect entry together broke the run")
	}
}

func TestMustNewFailsOnAnUnknownDriver(t *testing.T) {
	if runNested(func(t *testing.T) { drivertest.MustNew(t, "no-such-driver") }) {
		t.Error("MustNew accepted an unregistered driver")
	}
}
