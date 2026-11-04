package config

import (
	"strings"
	"testing"
	"time"
)

// TestModelLifecycleValidation: deprecated_at and retires_at are YYYY-MM-DD
// and retires_at is never before deprecated_at; the error cites models[i]
// (spec 003, ADR 0049).
func TestModelLifecycleValidation(t *testing.T) {
	for _, c := range []struct {
		model, want string
	}{
		{`{id: m, deprecated_at: "2026/01/01"}`, "models[1].deprecated_at"},
		{`{id: m, retires_at: "tomorrow"}`, "models[1].retires_at"},
		{`{id: m, deprecated_at: "2026-07-14", retires_at: "2026-07-13"}`, "is before deprecated_at"},
	} {
		_, err := Parse([]byte("models:\n  - {id: ok}\n  - " + c.model + "\n"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.model, err, c.want)
		}
	}
	cfg, err := Parse([]byte(`models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, deprecated_at: "2026-07-14", retires_at: "2026-07-14"}
  - {id: plain}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.FindModel("gpt-4o-mini")
	if m.Version != "gpt-4o-mini-2024-07-18" || m.DeprecatedAt != "2026-07-14" || m.RetiresAt != "2026-07-14" {
		t.Fatalf("fields not parsed: %+v", m)
	}
	if got := m.ResponseModel(); got != "gpt-4o-mini-2024-07-18" {
		t.Errorf("ResponseModel = %q", got)
	}
	if got := cfg.FindModel("plain").ResponseModel(); got != "plain" {
		t.Errorf("ResponseModel without version = %q", got)
	}
}

// TestModelRetired: from 00:00 UTC of retires_at on, the model is retired;
// without the date it never is. A model built without Validate parses the
// date on demand.
func TestModelRetired(t *testing.T) {
	cfg, err := Parse([]byte("models:\n  - {id: m, retires_at: \"2026-10-23\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.FindModel("m")
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	if m.Retired(day.Add(-time.Nanosecond)) || !m.Retired(day) || !m.Retired(day.AddDate(1, 0, 0)) {
		t.Errorf("Retired around %s wrong", day)
	}
	raw := &ModelConfig{ID: "raw", RetiresAt: "2026-10-23"}
	if !raw.Retired(day) || raw.Retired(day.Add(-time.Hour)) {
		t.Error("Retired without Validate wrong")
	}
	if (&ModelConfig{ID: "bad", RetiresAt: "x"}).Retired(day) {
		t.Error("an unparsable date retired the model")
	}
	if (&ModelConfig{ID: "none"}).Retired(day) {
		t.Error("a model without retires_at is retired")
	}
}
