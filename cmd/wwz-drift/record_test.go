package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordWritesFixtures: record against a fake provider writes the
// fixtures under --out, lists them and never writes the key.
func TestRecordWritesFixtures(t *testing.T) {
	fixedClock(t)
	f, url := fakeProvider(t, startWWZ(t), "anthropic", nil)
	out := filepath.Join(t.TempDir(), "real")
	budget := filepath.Join(t.TempDir(), "spend.json")
	code, stdout, errs := runCmd(t, "record", "--providers", "anthropic", "--probes", "chat,error_model",
		"--keys-file", keysFile(t, "ANTHROPIC_API_KEY"), "--base-url", "anthropic="+url, "--budget-file", budget, "--out", out)
	file := filepath.Join(out, "anthropic", "shape", "chat.json")
	if code != exitOK || errs != "" || !strings.Contains(stdout, "record em "+out) || !strings.Contains(stdout, "→ "+file) {
		t.Fatalf("code = %d\n%s\n%s", code, stdout, errs)
	}
	raw, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(raw), `"recorded_at": "2026-10-31"`) || !strings.Contains(string(raw), `"x-api-key": "<key>"`) {
		t.Fatalf("fixture: %v\n%s", err, raw)
	}
	if _, err := os.Stat(filepath.Join(out, "anthropic", "shape", "error_model.json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(budget); err != nil {
		t.Errorf("spend not recorded: %v", err)
	}
	if !f.sawSentinel() {
		t.Error("the fake provider did not receive the key")
	}
	assertNoSentinel(t, stdout, errs, string(raw))
}

func TestRecordFailures(t *testing.T) {
	if code, _, _ := runCmd(t, "record", "-h"); code != exitOK {
		t.Errorf("-h: %d", code)
	}
	if code, _, errs := runCmd(t, "record", "extra"); code != exitUsage || !strings.Contains(errs, `argumento inesperado "extra"`) {
		t.Errorf("extra: %d %q", code, errs)
	}
	if code, _, errs := runCmd(t, "record", "--out", ""); code != exitUsage || !strings.Contains(errs, "--out") {
		t.Errorf("empty out: %d %q", code, errs)
	}
	if code, _, errs := runCmd(t, "record", "--keys-file", filepath.Join(t.TempDir(), "none.env")); code != exitError || !strings.HasPrefix(errs, "wwz-drift record: ") {
		t.Errorf("missing keys file: %d %q", code, errs)
	}

	// Unreadable budget: execution failure, message and partial report.
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	_, url := fakeProvider(t, startWWZ(t), "openai", nil)
	code, stdout, errs := runCmd(t, "record", "--providers", "openai", "--keys-file", keysFile(t, "OPENAI_API_KEY"),
		"--base-url", "openai="+url, "--budget-file", bad, "--out", t.TempDir())
	if code != exitError || !strings.HasPrefix(errs, "wwz-drift record: ") || !strings.Contains(stdout, "openai: error") {
		t.Errorf("bad budget: %d\n%s\n%s", code, stdout, errs)
	}
	assertNoSentinel(t, stdout, errs)
}
