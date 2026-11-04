package main

import (
	"bytes"
	"context"
	"runtime/debug"
	"strings"
	"testing"
)

func TestRunCommands(t *testing.T) {
	cleanEnv(t)
	cases := []struct {
		args       []string
		want       int
		out, error string
	}{
		{[]string{"version"}, exitOK, "wwz dev", ""},
		{[]string{"serve", "-version"}, exitOK, "wwz dev", ""},
		{[]string{"--version"}, exitOK, "wwz dev", ""},
		{[]string{"help"}, exitOK, "Usage: wwz", ""},
		{[]string{"serve", "-h"}, exitOK, "", "-admin-port"},
		{[]string{"validate"}, exitOK, "valid: config ok; 6 scenarios", ""},
		{[]string{"validate", "-h"}, exitOK, "", "Usage: wwz validate"},
		{[]string{"simulate"}, exitUsage, "", "-scenario is required"},
		{[]string{"simulate", "-h"}, exitOK, "", "Usage: wwz simulate"},
		{[]string{"bogus"}, exitUsage, "", `unknown command "bogus"`},
		{[]string{"-nope"}, exitUsage, "", "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(context.Background(), tc.args, &out, &errb)
			if code != tc.want || !strings.Contains(out.String(), tc.out) || !strings.Contains(errb.String(), tc.error) {
				t.Fatalf("code %d (want %d)\nstdout: %s\nstderr: %s", code, tc.want, out.String(), errb.String())
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v1.2.3"
	if got := versionString(); got != "wwz v1.2.3" {
		t.Fatalf("got %q", got)
	}
}

// TestVersionStringModule covers the fallback to the module version recorded
// by the toolchain (go install ...@v1.2.3) when no -ldflags version is set.
func TestVersionStringModule(t *testing.T) {
	oldV, oldC, oldRead := version, commit, readBuildInfo
	t.Cleanup(func() { version, commit, readBuildInfo = oldV, oldC, oldRead })
	version, commit = "dev", ""
	for _, tc := range []struct {
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{&debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}}, true, "wwz v1.4.0"},
		{&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "wwz dev"},
		{&debug.BuildInfo{}, true, "wwz dev"},
		{nil, false, "wwz dev"},
	} {
		readBuildInfo = func() (*debug.BuildInfo, bool) { return tc.bi, tc.ok }
		if got := versionString(); got != tc.want {
			t.Errorf("build info %+v: got %q, want %q", tc.bi, got, tc.want)
		}
	}
	// An -ldflags version wins over the module version.
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}}, true
	}
	version = "v2.0.0"
	if got := versionString(); got != "wwz v2.0.0" {
		t.Errorf("got %q", got)
	}
}

// TestVersionStringCommit covers -ldflags "-X main.commit=<rev>".
func TestVersionStringCommit(t *testing.T) {
	oldV, oldC := version, commit
	t.Cleanup(func() { version, commit = oldV, oldC })
	version, commit = "v1.2.3", "abc123"
	if got := versionString(); got != "wwz v1.2.3 (commit abc123)" {
		t.Fatalf("got %q", got)
	}
}
