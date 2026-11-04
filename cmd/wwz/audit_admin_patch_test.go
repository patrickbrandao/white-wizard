package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAdminPatchObservedOnDataPlane is the R13 acceptance (task 385): a real
// `wwz serve` is changed at runtime by POST /admin/config (JSON merge patch)
// and every change is observed in the data plane without a restart: latency,
// errors, limits and scenarios. An invalid patch keeps the previous behaviour.
func TestAdminPatchObservedOnDataPlane(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WWZ_ADMIN_TOKEN", "tok")
	path := filepath.Join(t.TempDir(), "wwz.yaml")
	writeFile(t, path, `
server: {seed: 7}
keys:
  - {key: sk-lat, label: lat}
  - {key: sk-lim, label: lim}
  - {key: sk-scn, label: scn}
models:
  - id: m-lat
    protocols: [openai]
    latency: {ttft: 0ms, itl: 0ms}
  - id: m-err
    protocols: [openai]
    latency: {ttft: 0ms, itl: 0ms}
    errors: {rate: 0}
  - id: m-plain
    protocols: [openai]
    latency: {ttft: 0ms, itl: 0ms}
`)
	in := startServe(t, "-config", path)
	admin := map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/json"}
	patch := func(body string) (int, string) {
		code, _, out := do(t, "POST", url(in.a.Admin, "/admin/config"), admin, body)
		return code, out
	}
	chat := func(key, model string, extra map[string]string) (int, time.Duration, string) {
		h := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
		for k, v := range extra {
			h[k] = v
		}
		start := time.Now()
		code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		return code, time.Since(start), body
	}

	t.Run("latency", func(t *testing.T) {
		if code, d, body := chat("sk-lat", "m-lat", nil); code != 200 || d >= 150*time.Millisecond {
			t.Fatalf("before: %d in %v: %s", code, d, body)
		}
		if code, out := patch(`{"models":{"m-lat":{"latency":{"ttft":"200ms"}}}}`); code != 200 {
			t.Fatalf("patch: %d %s", code, out)
		}
		if code, d, body := chat("sk-lat", "m-lat", nil); code != 200 || d < 200*time.Millisecond {
			t.Fatalf("after: %d in %v (want >= 200ms): %s", code, d, body)
		}
		if code, out := patch(`{"models":{"m-lat":{"latency":{"ttft":"0ms"}}}}`); code != 200 {
			t.Fatalf("patch back: %d %s", code, out)
		}
		if code, d, _ := chat("sk-lat", "m-lat", nil); code != 200 || d >= 150*time.Millisecond {
			t.Fatalf("back to 0: %d in %v", code, d)
		}
	})

	t.Run("errors", func(t *testing.T) {
		if code, _, body := chat("sk-lat", "m-err", nil); code != 200 {
			t.Fatalf("before: %d %s", code, body)
		}
		if code, out := patch(`{"models":{"m-err":{"errors":{"rate":100,"types":["server_error"]}}}}`); code != 200 {
			t.Fatalf("patch: %d %s", code, out)
		}
		for i := 0; i < 3; i++ {
			if code, _, body := chat("sk-lat", "m-err", nil); code != 500 {
				t.Fatalf("after (%d): %d %s", i, code, body)
			}
		}
		if code, out := patch(`{"models":{"m-err":{"errors":{"rate":0}}}}`); code != 200 {
			t.Fatalf("patch back: %d %s", code, out)
		}
		if code, _, body := chat("sk-lat", "m-err", nil); code != 200 {
			t.Fatalf("rate back to 0: %d %s", code, body)
		}
	})

	t.Run("limits", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if code, _, body := chat("sk-lim", "m-plain", nil); code != 200 {
				t.Fatalf("before (%d): %d %s", i, code, body)
			}
		}
		if code, out := patch(`{"keys":{"lim":{"limits":{"rpm":1}}}}`); code != 200 {
			t.Fatalf("patch: %d %s", code, out)
		}
		if code, _, body := chat("sk-lim", "m-plain", nil); code != 200 {
			t.Fatalf("1st after: %d %s", code, body)
		}
		if code, _, body := chat("sk-lim", "m-plain", nil); code != 429 {
			t.Fatalf("2nd after: %d %s", code, body)
		}
	})

	t.Run("scenarios", func(t *testing.T) {
		hdr := map[string]string{"x-wwz-scenario": "patched-scn"}
		if code, _, body := chat("sk-scn", "m-plain", hdr); code == 200 || !strings.Contains(body, "patched-scn") {
			t.Fatalf("before: %d %s", code, body)
		}
		y := map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/yaml"}
		if code, _, body := do(t, "POST", url(in.a.Admin, "/admin/scenarios"), y, "name: patched-scn\nsteps:\n  - id: only\n    content: scenario-sentinel-385\n"); code != 200 {
			t.Fatalf("load scenario: %d %s", code, body)
		}
		if code, _, body := chat("sk-scn", "m-plain", hdr); code != 200 || !strings.Contains(body, "scenario-sentinel-385") {
			t.Fatalf("after: %d %s", code, body)
		}
	})

	t.Run("invalid patch keeps behaviour", func(t *testing.T) {
		if code, out := patch(`{"models":{"m-lat":{"latency":{"ttft":"200ms"}}}}`); code != 200 {
			t.Fatalf("patch: %d %s", code, out)
		}
		for _, bad := range []string{
			`{"models":{"m-lat":{"latency":{"bogus":1}}}}`,
			`{"server":{"auth":"maybe"}}`,
			`{"models":{"m-err":{"errors":{"types":["bogus"]}}}}`,
		} {
			if code, out := patch(bad); code < 400 {
				t.Fatalf("%s: status %d %s", bad, code, out)
			}
		}
		if code, d, body := chat("sk-lat", "m-lat", nil); code != 200 || d < 200*time.Millisecond {
			t.Fatalf("latency lost after invalid patches: %d in %v: %s", code, d, body)
		}
		if code, _, body := chat("sk-lat", "m-err", nil); code != 200 {
			t.Fatalf("errors changed by invalid patch: %d %s", code, body)
		}
	})
}
