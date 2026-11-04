package main

import (
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// After a streaming request /metrics carries the ITL and E2E histograms
// with driver and model labels, next to TTFT (R14, spec 013).
func TestServeMetricsStreamHistograms(t *testing.T) {
	cleanEnv(t)
	in := startServe(t)
	body := `{"model":"wwz-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	if code, _, out := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	res := map[string]*regexp.Regexp{}
	for _, n := range []string{"wwz_ttft_seconds", "wwz_itl_seconds", "wwz_e2e_seconds"} {
		res[n] = regexp.MustCompile(`(?m)^` + n + `_count\{(?:[^}]*,)?driver="openai"(?:,[^}]*)?\} (\d+)$`)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, m := do(t, "GET", url(in.a.Admin, "/metrics"), nil, "")
		missing := ""
		for n, re := range res {
			g := re.FindStringSubmatch(m)
			if g == nil || g[1] == "0" || !strings.Contains(m, n+`_count{driver="openai",model="wwz-fast"}`) {
				missing = n
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s_count with driver and model not > 0:\n%s", missing, m)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// /metrics publishes wwz_queue_depth, wwz_connections_open and, only with
// metrics.vllm_mirror, the vllm:* mirror (spec 013).
func TestServeMetricsQueueConnectionsAndVLLMMirror(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "wwz.yaml")
	writeFile(t, path, "metrics: {vllm_mirror: true}\n")
	in := startServe(t, "-config", path)

	// One idle connection held open on the data plane.
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(url(in.a.Data, ""), "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, _, body := do(t, "POST", url(in.a.Data, "/v1/chat/completions"), oaiAuth, `{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`); code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	re := regexp.MustCompile(`(?m)^wwz_connections_open (\d+)$`)
	var m string
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, m = do(t, "GET", url(in.a.Admin, "/metrics"), nil, "")
		if g := re.FindStringSubmatch(m); g != nil {
			if n, _ := strconv.Atoi(g[1]); n >= 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("wwz_connections_open not >= 1:\n%s", m)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{
		"# TYPE wwz_queue_depth gauge",
		"# TYPE wwz_connections_open gauge",
		`vllm:num_requests_running{model_name="wwz-fast"} 0`,
		"# TYPE vllm:num_requests_waiting gauge",
		`vllm:time_to_first_token_seconds_count{model_name="wwz-fast"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("missing %q in:\n%s", want, m)
		}
	}

	off := startServe(t)
	_, _, m = do(t, "GET", url(off.a.Admin, "/metrics"), nil, "")
	if !strings.Contains(m, "# TYPE wwz_connections_open gauge") || !strings.Contains(m, "# TYPE wwz_queue_depth gauge") || strings.Contains(m, "vllm:") {
		t.Fatalf("without vllm_mirror:\n%s", m)
	}
}
