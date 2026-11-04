package opencode_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Task 425 (ADR 0072, items 4–6): every recording of
// testdata/real/opencode/free/ (task 416, curl/8.7.1, no User-Agent
// opencode/...) replayed through the engine gives the real status, body
// byte for byte and headers (TestRealFreeFixtures, in errors_billing_test.go,
// replays the six by name); here the directory is read, so a new
// recording without an expectation fails, and the same requests from the
// OpenCode client (sent to the wwz only, never to the real) show that the
// client gate is the only thing between the generic client and the free
// model.

// freeWithClient is what each recording becomes with User-Agent
// opencode/...: the free model of the pay-as-you-go base is served as
// free; every other answer stays the real one.
var freeWithClient = map[string]int{
	"public_generic_client": 200,
	"public_messages":       200,
	"public_paid_model":     0,
	"public_go_base":        0,
	"messages_x_api_key":    0,
	"public_usage":          0,
}

func freeFixtureNames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "opencode", "free", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no free/ recordings (%v)", err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	slices.Sort(names)
	return names
}

func replayFree(t *testing.T, b *billEngine, fx freeFixture, ua string) (status int, body string, hdr map[string]string) {
	t.Helper()
	h := map[string]string{}
	for k, v := range fx.Request.Headers {
		h[k] = v
	}
	if ua != "" {
		h["user-agent"] = ua
	}
	body = ""
	if fx.Request.Body != nil {
		body = *fx.Request.Body
	}
	w := b.do(t, fx.Request.Method, fx.Request.Path, h, body)
	return w.StatusCode(), string(w.Body()), w.Headers()
}

func TestRealFreeFixturesAll(t *testing.T) {
	names := freeFixtureNames(t)
	for _, name := range names {
		if _, ok := freeWithClient[name]; !ok {
			t.Errorf("free/%s.json has no expectation in freeWithClient", name)
		}
	}
	for name := range freeWithClient {
		if !slices.Contains(names, name) {
			t.Errorf("free/%s.json is gone", name)
		}
	}
	b := newBillEngine(t, billCfg, nil)
	for _, name := range names {
		fx := loadFree(t, name)
		if ua := fx.Request.Headers["user-agent"]; strings.HasPrefix(ua, "opencode/") {
			t.Fatalf("free/%s.json was recorded with User-Agent %q: the real is never sent opencode/ (ADR 0072, item 5)", name, ua)
		}
		status, body, hdr := replayFree(t, b, fx, "")
		if status != fx.Response.Status || body != fx.Response.Body {
			t.Errorf("%s: %d %s\n real %d %s", name, status, body, fx.Response.Status, fx.Response.Body)
		}
		checkHeaders(t, shapeFixture{Name: name, Response: struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}{Headers: withoutRetryAfter(fx.Response.Headers)}}, hdr)
		if _, ok := fx.Response.Headers["retry-after"]; ok {
			checkRetryAfter(t, name, hdr)
		} else if v, has := hdr["retry-after"]; has {
			t.Errorf("%s: retry-after %q, the real sends none", name, v)
		}
	}
}

func TestRealFreeFixturesOpenCodeClient(t *testing.T) {
	b := newBillEngine(t, billCfg, nil)
	for _, name := range freeFixtureNames(t) {
		fx := loadFree(t, name)
		status, body, _ := replayFree(t, b, fx, ocClient)
		want := freeWithClient[name]
		switch {
		case want == 0:
			if status != fx.Response.Status || body != fx.Response.Body {
				t.Errorf("%s with the OpenCode client: %d %s, want the real %d %s", name, status, body, fx.Response.Status, fx.Response.Body)
			}
		case status != want:
			t.Errorf("%s with the OpenCode client: %d %s, want %d", name, status, body, want)
		case strings.Contains(fx.Request.Path, "/chat/completions") && !strings.Contains(body, `"billing_source":"free"`):
			t.Errorf("%s with the OpenCode client: %s, want usage.opencode.billing_source free", name, body)
		}
	}
}
