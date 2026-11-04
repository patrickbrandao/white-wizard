package chaos

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestDuplicateFramesInTypes: I19 — duplicate_frames named in errors.types
// becomes a mid-stream fault, never a generic pre-response 500.
func TestDuplicateFramesInTypes(t *testing.T) {
	in := New()
	p := &config.ErrorProfile{Rate: 100, Types: []string{"duplicate_frames"}, AfterChunks: 3}
	d := in.Decide(p, "m", "", rng(), time.Now())
	if d.Pre != nil {
		t.Fatalf("pre error %+v", d.Pre)
	}
	if d.Mid == nil || d.Mid.Kind != core.StreamDupFrames || d.Mid.AfterChunks != 3 || d.Source != "profile" {
		t.Fatalf("mid %+v source %q", d.Mid, d.Source)
	}
}

// TestPreTypesStillBuildErrors: other names in errors.types keep producing
// pre-response errors.
func TestPreTypesStillBuildErrors(t *testing.T) {
	in := New()
	p := &config.ErrorProfile{Rate: 100, Types: []string{"corrupt_body"}}
	d := in.Decide(p, "m", "", rng(), time.Now())
	if d.Pre == nil || d.Pre.Transport != core.TransportCorruptBody || d.Mid != nil {
		t.Fatalf("decision %+v", d)
	}
}
