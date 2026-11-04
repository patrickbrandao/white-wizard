package client

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

const (
	fixtureDir = "../../../testdata/real/claude-code/2.1.308"
	fakeToken  = "sk-ant-oat01-WWZFAKEtokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBBBBAA"
	fakeKey    = "sk-ant-api03-WWZFAKEapikeyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-AAAAAAAA"
	captureURL = "http://127.0.0.1:18556" // ANTHROPIC_BASE_URL of the captures
)

var oauthCred = claudecode.Credential{Kind: claudecode.CredentialOAuth, Secret: fakeToken}

func fixtureSum(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return sha(raw)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustTarget(t *testing.T, raw string) *Target {
	t.Helper()
	tg, err := ParseTarget(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// fixedDay is the date of the captures.
var fixedDay = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// newTestClient returns a client of the capture URL whose connections go
// to fs, with the clock of the captures and no backoff.
func newTestClient(t *testing.T, fs *fakeServer, o Options) *Client {
	t.Helper()
	if o.Target == nil {
		o.Target = mustTarget(t, captureURL)
	}
	if o.Dial == nil && fs != nil {
		o.Dial = fs.dialer()
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return fixedDay }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestNew(t *testing.T) {
	tg := mustTarget(t, "http://127.0.0.1:1")
	bad := []Options{
		{},
		{Target: tg, Version: "2.1.309"},
		{Target: tg, Entrypoint: "sdk-py"},
		{Target: tg, Arch: "ppc"},
		{Target: tg, OS: "Plan9"},
		{Target: tg, Retries: -1},
	}
	for _, o := range bad {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) accepted", o)
		}
	}
	if _, err := New(Options{Target: tg, Version: "9.9.9"}); !errors.Is(err, claudecode.ErrUnknownVersion) {
		t.Errorf("unknown version: %v", err)
	}
	c, err := New(Options{Target: tg})
	if err != nil {
		t.Fatal(err)
	}
	o := c.Options()
	if o.Version != DefaultVersion || o.Entrypoint != claudecode.EntrypointSDKCLI || o.Arch != "arm64" || o.OS != "MacOS" ||
		o.Model != DefaultModelOAuth || o.SystemPrompt != DefaultSystemPrompt || o.Timeout != DefaultTimeout {
		t.Errorf("defaults: %+v", o)
	}
	c, _ = New(Options{Target: tg, Credential: claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: "k"}})
	if c.Options().Model != DefaultModelAPIKey {
		t.Errorf("api-key model %q", c.Options().Model)
	}
	if id := NewSessionID(); !claudecode.IsUUIDv4(id) {
		t.Errorf("session id %q", id)
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Error("sleep ignored the context")
	}
}

// TestAskMatchesFixtures: ask writes on the socket exactly the bytes Claude
// Code 2.1.308 sent (same sha256 as the fixtures), in both entrypoints and
// with both credentials, including --resume and the interactive quota
// probe.
func TestAskMatchesFixtures(t *testing.T) {
	cases := []struct {
		name    string
		o       Options
		session string
		asks    []string
		want    []string
	}{
		{
			name: "sdk-cli oauth", session: "ce90e450-43fc-4765-8110-6aed6d650ca1",
			o:    Options{Credential: oauthCred, DeviceID: "68159782ff84a80342f7e86ed7cddfaaf32e783f7a2a3e29e09814638ebb1f79"},
			asks: []string{"Hello there, reply with ok"}, want: []string{"002-sdk-cli-oauth-turn.raw"},
		},
		{
			name: "sdk-cli api key", session: "e7d64300-c4ce-4ef1-93d9-9167ef198832",
			o: Options{
				Credential: claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: fakeKey},
				DeviceID:   "4115483067c3994ffd6eaa538f7156a1a3f7e7c5bc601ef6a96d8f87c813faba",
			},
			asks: []string{"Hello there, reply with ok"}, want: []string{"004-sdk-cli-apikey-turn.raw"},
		},
		{
			name: "sdk-cli two turns", session: "c153209a-1d46-4590-85b9-a6494e0dd10a",
			o:    Options{Credential: oauthCred, DeviceID: "68159782ff84a80342f7e86ed7cddfaaf32e783f7a2a3e29e09814638ebb1f79"},
			asks: []string{"Second conversation, first turn", "And the second turn"},
			want: []string{"006-sdk-cli-oauth-turn1.raw", "008-sdk-cli-oauth-turn2.raw"},
		},
		{
			name: "cli quota and safeguards", session: "d559555d-99d8-4d64-92ca-d81b6c0f2022",
			o: Options{
				Credential: oauthCred, Entrypoint: claudecode.EntrypointCLI,
				DeviceID: "b5e0d8d89402035d1ac7a4087d321edf0479e8e8c015ea5c7c3ae85f1611fb6c",
			},
			asks: []string{"Hello there, reply with ok"},
			want: []string{"010-cli-oauth-quota.raw", "012-cli-oauth-turn-safeguards.raw"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFake(t, messagesHandler("ok"))
			c := newTestClient(t, fs, tc.o)
			c.newID = func() string { return tc.session }
			s := c.NewSession()
			var wire []byte
			for _, text := range tc.asks {
				x, err := s.Ask(context.Background(), text)
				if err != nil || !x.OK() || x.Text != "ok" {
					t.Fatalf("ask %q: %v %+v", text, err, x.Results)
				}
				wire = append(wire, x.Wire...)
			}
			s.Close()
			reqs := fs.requests()
			if len(reqs) != len(tc.want) {
				t.Fatalf("%d requests, want %d", len(reqs), len(tc.want))
			}
			var all []byte
			for i, r := range reqs {
				if got, want := sha(r.Raw), fixtureSum(t, tc.want[i]); got != want {
					raw, _ := os.ReadFile(filepath.Join(fixtureDir, tc.want[i]))
					t.Fatalf("request %d: sha256 %s, fixture %s has %s\n%s", i, got, tc.want[i], want, diff(raw, r.Raw))
				}
				if reason, detail, ok := claudecode.Verify(r.Head, r.Body); !ok {
					t.Errorf("Verify refused: %s: %s", reason, detail)
				}
				all = append(all, r.Raw...)
			}
			if !bytes.Equal(all, wire) {
				t.Error("Exchange.Wire differs from the bytes received")
			}
			if fs.conns.Load() != 1 {
				t.Errorf("%d connections, want 1 (keep-alive per session)", fs.conns.Load())
			}
			if len(s.Turns) != len(tc.asks) {
				t.Errorf("turns %+v", s.Turns)
			}
		})
	}
}

func TestAskMeasures(t *testing.T) {
	fs := newFake(t, messagesHandler("hello"))
	c := newTestClient(t, fs, Options{Credential: oauthCred})
	x, err := c.NewSession().Ask(context.Background(), "hi")
	if err != nil || len(x.Results) != 1 {
		t.Fatal(err, x)
	}
	r := x.Results[0]
	if !r.OK() || r.Status != 200 || r.ContentEvents != 5 || len(r.ITL) != 4 || r.TTFT <= 0 || r.E2E < r.TTFT ||
		r.InputTokens != 12 || r.OutputTokens != 5 || r.FinishReason != "end_turn" || r.TokensEstimated || x.Err() != "" {
		t.Errorf("result %+v", r)
	}
	if (&Exchange{}).OK() || (&Exchange{}).Err() != "" {
		t.Error("empty exchange")
	}
}

func TestAskFailures(t *testing.T) {
	// The quota probe refused: no turn is sent.
	fs := newFake(t, func(rawReq) []byte {
		return jsonResp(403, `{"type":"error","error":{"type":"forged_request_error","message":"x"}}`, "x-should-retry: false")
	})
	c := newTestClient(t, fs, Options{Credential: oauthCred, Entrypoint: claudecode.EntrypointCLI})
	s := c.NewSession()
	x, err := s.Ask(context.Background(), "hi")
	if err != nil || x.OK() || x.Err() != "http_403/forged_request_error" || len(fs.requests()) != 1 || len(s.Turns) != 0 ||
		x.Status != 403 || !bytes.Contains(x.ErrBody, []byte("forged")) {
		t.Fatalf("quota refused: %v %+v", err, x)
	}
	// A turn refused is not recorded.
	c = newTestClient(t, fs, Options{Credential: oauthCred})
	s = c.NewSession()
	if x, _ := s.Ask(context.Background(), "hi"); x.OK() || len(s.Turns) != 0 {
		t.Fatal("refused turn recorded")
	}
	// Requests that cannot be built.
	for _, ep := range []claudecode.Entrypoint{claudecode.EntrypointSDKCLI, claudecode.EntrypointCLI} {
		c = newTestClient(t, fs, Options{Entrypoint: ep})
		if _, err := c.NewSession().Ask(context.Background(), "hi"); err == nil {
			t.Errorf("%s: no credential accepted", ep)
		}
	}
}

func TestRetries(t *testing.T) {
	const (
		claim   = "anthropic-ratelimit-unified-representative-claim: five_hour"
		overage = "anthropic-ratelimit-unified-overage-status: rejected"
		xsrT    = "x-should-retry: true"
		xsrF    = "x-should-retry: false"
	)
	disabled := func(reason string) string { return "anthropic-ratelimit-unified-overage-disabled-reason: " + reason }
	apiKey := Options{Credential: claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: fakeKey}}
	sub := Options{Credential: oauthCred}                                     // explicit token: user:inference, no plan
	login := Options{Credential: oauthCred, Scopes: LoginScopes, Plan: "max"} // a login of a subscriber
	enterprise := Options{Credential: oauthCred, Scopes: LoginScopes, Plan: "enterprise"}
	noInference := Options{Credential: oauthCred, Scopes: []string{ScopeProfile}}
	cases := []struct {
		name    string
		o       Options
		status  int
		headers []string
		want    int // requests sent with Retries: 2
	}{
		{"529", sub, 529, nil, 3},
		{"500 should-retry false", sub, 500, []string{xsrF}, 1},
		{"503 should-retry false api key", apiKey, 503, []string{xsrF}, 1},
		{"408", sub, 408, nil, 3},
		{"409", sub, 409, nil, 3},
		{"429", sub, 429, nil, 3},
		{"404", sub, 404, nil, 1},
		{"401 api key", apiKey, 401, nil, 1},
		// Rule 3: the subscriber never retries a 429 of the subscription.
		{"429 claim subscriber", sub, 429, []string{claim}, 1},
		{"429 claim should-retry true subscriber", sub, 429, []string{claim, xsrT}, 1},
		{"429 claim should-retry true login", login, 429, []string{claim, xsrT}, 1},
		{"429 overage status subscriber", sub, 429, []string{overage}, 1},
		{"429 disabled other subscriber", login, 429, []string{disabled("member_zero_credit_limit"), xsrT}, 1},
		// Rule 4: x-should-retry: true forces nothing for the subscriber.
		{"429 should-retry true subscriber", sub, 429, []string{xsrT}, 3},
		{"400 should-retry true subscriber", sub, 400, []string{xsrT}, 1},
		{"400 should-retry true api key", apiKey, 400, []string{xsrT}, 3},
		{"400 should-retry true enterprise", enterprise, 400, []string{xsrT}, 3},
		// API key, enterprise and a token without user:inference: a common 429.
		{"429 claim should-retry true api key", apiKey, 429, []string{claim, xsrT}, 3},
		{"429 claim enterprise", enterprise, 429, []string{claim}, 3},
		{"429 claim no user:inference", noInference, 429, []string{claim}, 3},
		{"429 claim should-retry false api key", apiKey, 429, []string{claim, xsrF}, 1},
		// Rule 2: overage disabled for the organization, any credential.
		{"429 spend cap should-retry true api key", apiKey, 429, []string{disabled("org_spend_cap_reached"), xsrT}, 1},
		{"429 spend cap should-retry true subscriber", sub, 429, []string{disabled("org_spend_cap_reached"), xsrT}, 1},
		{"429 disabled until enterprise", enterprise, 429, []string{disabled("org_level_disabled_until"), xsrT}, 1},
		{"429 out of credits api key", apiKey, 429, []string{disabled("out_of_credits"), xsrT}, 1},
		{"429 org disabled enterprise", enterprise, 429, []string{disabled("org_level_disabled")}, 1},
		{"429 service disabled api key", apiKey, 429, []string{disabled("org_service_level_disabled")}, 1},
		{"429 out of credits with claim api key", apiKey, 429, []string{disabled("out_of_credits"), claim}, 3},
		{"429 out of credits with overage status enterprise", enterprise, 429, []string{disabled("out_of_credits"), overage}, 3},
		{"429 out of credits with claim subscriber", sub, 429, []string{disabled("out_of_credits"), claim, xsrT}, 1},
		{"429 other reason api key", apiKey, 429, []string{disabled("fetch_error")}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFake(t, func(rawReq) []byte {
				return jsonResp(tc.status, `{"type":"error","error":{"type":"x_error","message":"m"}}`, tc.headers...)
			})
			o := tc.o
			o.Retries = 2
			c := newTestClient(t, fs, o)
			x, _ := c.NewSession().Ask(context.Background(), "hi")
			reqs := fs.requests()
			if len(reqs) != tc.want || len(x.Results) != tc.want {
				t.Fatalf("%d requests (%d results), want %d", len(reqs), len(x.Results), tc.want)
			}
			for i, r := range reqs {
				if got, _ := r.Head.Get("X-Stainless-Retry-Count"); got != string(rune('0'+i)) {
					t.Errorf("attempt %d: retry count %q", i, got)
				}
				if !bytes.Equal(r.Body, reqs[0].Body) {
					t.Errorf("attempt %d: body changed", i)
				}
				// Only X-Stainless-Retry-Count changes between attempts.
				want := bytes.Replace(reqs[0].Raw, []byte("X-Stainless-Retry-Count: 0"), []byte("X-Stainless-Retry-Count: "+string(rune('0'+i))), 1)
				if !bytes.Equal(r.Raw, want) {
					t.Errorf("attempt %d: wire differs beyond the retry count", i)
				}
			}
		})
	}
	// Connect errors are retried; the backoff stops on the context.
	dials := 0
	c := newTestClient(t, nil, Options{Credential: oauthCred, Retries: 2, Dial: func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("refused")
	}})
	x, _ := c.NewSession().Ask(context.Background(), "hi")
	if dials != 3 || x.Err() != loadgen.ErrConnect {
		t.Errorf("connect: %d dials, %q", dials, x.Err())
	}
	c.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	dials = 0
	if _, _ = c.NewSession().Ask(context.Background(), "hi"); dials != 1 {
		t.Errorf("canceled backoff: %d dials", dials)
	}
	// A reset (the server hangs up) is retried on a new connection.
	n := 0
	fs := newFake(t, func(rawReq) []byte {
		n++
		if n == 1 {
			return hangUp
		}
		return sseResp("ok")
	})
	c = newTestClient(t, fs, Options{Credential: oauthCred, Retries: 1})
	x, _ = c.NewSession().Ask(context.Background(), "hi")
	if !x.OK() || x.Results[0].Err != loadgen.ErrReset || fs.conns.Load() != 2 {
		t.Errorf("reset: %+v, %d conns", x.Results, fs.conns.Load())
	}
}

func TestBackoff(t *testing.T) {
	c := newTestClient(t, nil, Options{})
	c.rand = func() float64 { return 0 }
	hdr := func(h ...string) *Response {
		r := &Response{}
		for _, x := range h {
			k, v, _ := strings.Cut(x, ": ")
			r.Headers = append(r.Headers, claudecode.Header{Name: k, Value: v})
		}
		return r
	}
	cases := []struct {
		attempt int
		resp    *Response
		want    time.Duration
	}{
		{0, nil, 500 * time.Millisecond},
		{1, hdr(), time.Second},
		{6, nil, 8 * time.Second},
		{0, hdr("retry-after-ms: 1500"), 1500 * time.Millisecond},
		{0, hdr("retry-after: 2"), 2 * time.Second},
		{0, hdr("retry-after: 120"), 500 * time.Millisecond},
		{0, hdr("retry-after-ms: 0", "retry-after: x"), 500 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := c.backoff(tc.attempt, tc.resp); got != tc.want {
			t.Errorf("backoff(%d, %v) = %s, want %s", tc.attempt, tc.resp, got, tc.want)
		}
	}
	c.rand = func() float64 { return 1 }
	if got := c.backoff(0, nil); got != 375*time.Millisecond {
		t.Errorf("jitter: %s", got)
	}
}

// doRaw sends one turn whose response is resp and returns the result.
func doRaw(t *testing.T, resp []byte, o Options) (loadgen.Result, *fakeServer, *Session) {
	t.Helper()
	fs := newFake(t, func(rawReq) []byte { return resp })
	if o.Credential.Kind == 0 {
		o.Credential = oauthCred
	}
	s := newTestClient(t, fs, o).NewSession()
	x, err := s.Ask(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	return x.Results[len(x.Results)-1], fs, s
}

func compress(t *testing.T, enc, body string) string {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	switch enc {
	case "gzip":
		w = gzip.NewWriter(&b)
	case "zlib":
		w = zlib.NewWriter(&b)
	default:
		w, _ = flate.NewWriter(&b, flate.DefaultCompression)
	}
	_, _ = io.WriteString(w, body)
	_ = w.Close()
	return b.String()
}

const okMessage = `{"type":"message","content":[{"type":"text","text":"hi there"},{"type":"tool_use"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":1}}`

func TestResponses(t *testing.T) {
	sse := []string{"Content-Type: text/event-stream"}
	cases := []struct {
		name string
		resp []byte
		o    Options
		err  string
		text string
	}{
		{name: "non-stream", resp: jsonResp(200, okMessage), o: Options{NonStream: true}, text: "hi there"},
		{name: "gzip", resp: response(200, []string{"Content-Encoding: gzip"}, compress(t, "gzip", okMessage)), text: "hi there"},
		{name: "deflate zlib", resp: response(200, []string{"Content-Encoding: deflate"}, compress(t, "zlib", okMessage)), text: "hi there"},
		{name: "deflate raw", resp: response(200, []string{"Content-Encoding: Deflate"}, compress(t, "flate", okMessage)), text: "hi there"},
		{name: "deflate zlib with dictionary", resp: response(200, []string{"Content-Encoding: deflate"}, "\x78\xbb\x00\x00\x00\x01xx"), err: loadgen.ErrReset},
		{name: "bad gzip", resp: response(200, []string{"Content-Encoding: gzip"}, "nope"), err: loadgen.ErrParse},
		{name: "br", resp: response(200, []string{"Content-Encoding: br"}, "x"), err: loadgen.ErrParse},
		{name: "not a message", resp: jsonResp(200, `{"type":"other"}`), err: loadgen.ErrParse},
		{name: "empty 2xx", resp: response(204, nil, ""), text: ""},
		{name: "unframed", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + okMessage), text: "hi there"},
		{name: "http/1.0", resp: []byte("HTTP/1.0 200 OK\r\nContent-Length: " + itoa(len(okMessage)) + "\r\n\r\n" + okMessage), text: "hi there"},
		{name: "connection close", resp: jsonResp(200, okMessage, "Connection: close"), text: "hi there"},
		{name: "truncated length", resp: []byte("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 999\r\n\r\n{\"type\""), err: loadgen.ErrReset},
		{name: "bad length", resp: []byte("HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n"), err: loadgen.ErrParse},
		{name: "bad status line", resp: []byte("HTTP/2 200\r\n\r\n"), err: loadgen.ErrParse},
		{name: "bad status code", resp: []byte("HTTP/1.1 2x0 OK\r\n\r\n"), err: loadgen.ErrParse},
		{name: "bad header", resp: []byte("HTTP/1.1 200 OK\r\nnocolon\r\n\r\n"), err: loadgen.ErrParse},
		{name: "space before colon", resp: []byte("HTTP/1.1 200 OK\r\nX-A : b\r\n\r\n"), err: loadgen.ErrParse},
		{name: "long header", resp: []byte("HTTP/1.1 200 OK\r\nX-A: " + strings.Repeat("a", 40<<10) + "\r\n\r\n"), err: loadgen.ErrParse},
		{name: "head cut", resp: []byte("HTTP/1.1 200 OK\r\nX-A: b"), err: loadgen.ErrReset},
		{name: "no response", resp: hangUp, err: loadgen.ErrReset},
		{name: "redirect", resp: response(302, []string{"Location: https://api.anthropic.com/v1/messages"}, ""), err: "http_302"},
		{
			name: "error envelope", resp: jsonResp(403, `{"type":"error","error":{"type":"forged_request_error","message":"m"}}`, "x-should-retry: false"),
			err: "http_403/forged_request_error",
		},
		{name: "error body cut", resp: []byte("HTTP/1.1 500 X\r\nConnection: close\r\nContent-Length: 50\r\nx-should-retry: false\r\n\r\n{"), err: "http_500"},
		{name: "sse chunked extensions and trailers", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n" +
			itox(len(sseEvents("ok"))) + ";ext=1\r\n" + sseEvents("ok") + "\r\n0\r\nX-Trailer: 1\r\n\r\n"), text: "ok"},
		{name: "sse truncated", resp: chunked(200, sse, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", 8), err: loadgen.ErrStreamTruncated},
		{
			name: "sse error event", resp: chunked(200, sse, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", 1000),
			err: "stream_error/overloaded_error",
		},
		{name: "sse error without type", resp: chunked(200, sse, "data: {\"type\":\"error\"}\n\n", 1000), err: "stream_error/unknown"},
		{name: "sse bad json", resp: chunked(200, sse, "data: nope\n\n", 1000), err: loadgen.ErrParse},
		{
			name: "sse bad chunk size", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n"),
			err: loadgen.ErrParse,
		},
		{
			name: "sse chunk longer than size", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nab\r\n0\r\n\r\n"),
			err: loadgen.ErrParse,
		},
		{
			name: "sse cut in chunk", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n10\r\nab"),
			err: loadgen.ErrReset,
		},
		{
			name: "sse cut in chunk end", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n1\r\na"),
			err: loadgen.ErrReset,
		},
		{name: "sse cut in size", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"), err: loadgen.ErrReset},
		{name: "sse cut in trailer", resp: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\nX"), err: loadgen.ErrReset},
		{name: "sse no content, no usage", resp: chunked(200, sse,
			": comment\r\nevent: message_start\r\ndata: {}\r\n\r\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{}}\n\ndata: {\"type\":\"message_stop\"}\n\n", 7), text: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _, s := doRaw(t, tc.resp, tc.o)
			if res.Err != tc.err {
				t.Fatalf("err %q (%v), want %q", res.Err, res.Cause, tc.err)
			}
			if tc.err == "" && len(s.Turns) == 1 && s.Turns[0].Assistant != tc.text {
				t.Errorf("text %q, want %q", s.Turns[0].Assistant, tc.text)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func itox(n int) string { return strconv.FormatInt(int64(n), 16) }

func TestStreamEvents(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hm\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"a\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"signature_delta\"}}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\n" + "data: \"text\":\"b\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7,\"input_tokens\":9}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\nignored: x\ndata: cut"
	res, _, s := doRaw(t, chunked(200, []string{"Content-Type: text/event-stream"}, body, 5), Options{})
	if !res.OK() || res.ContentEvents != 5 || res.InputTokens != 9 || res.OutputTokens != 7 || res.FinishReason != "tool_use" ||
		s.Turns[0].Assistant != "ab" {
		t.Errorf("%+v %+v", res, s.Turns)
	}
	// No usage: tokens estimated, TTFT is the end.
	res, _, _ = doRaw(t, jsonResp(200, `{"type":"message","content":[]}`), Options{})
	if !res.OK() || !res.TokensEstimated || res.TTFT == 0 {
		t.Errorf("no usage: %+v", res)
	}
}

func TestKeepAlive(t *testing.T) {
	// Unframed and HTTP/1.0 bodies end the connection; a framed one keeps it.
	for _, tc := range []struct {
		resp  []byte
		conns int32
	}{
		{jsonResp(200, okMessage), 1},
		{[]byte("HTTP/1.1 200 OK\r\n\r\n" + okMessage), 2},
		{jsonResp(200, okMessage, "Connection: close"), 2},
	} {
		fs := newFake(t, func(rawReq) []byte { return tc.resp })
		s := newTestClient(t, fs, Options{Credential: oauthCred}).NewSession()
		for range 2 {
			if x, _ := s.Ask(context.Background(), "hi"); !x.OK() {
				t.Fatalf("%+v", x.Results)
			}
		}
		s.Close()
		if got := fs.conns.Load(); got != tc.conns {
			t.Errorf("%q: %d connections, want %d", tc.resp[:30], got, tc.conns)
		}
	}
	// A body longer than the cap cannot be drained.
	old := maxBody
	maxBody = 4
	defer func() { maxBody = old }()
	fs := newFake(t, func(rawReq) []byte { return jsonResp(500, strings.Repeat("x", 20), "x-should-retry: false") })
	s := newTestClient(t, fs, Options{Credential: oauthCred}).NewSession()
	_, _ = s.Ask(context.Background(), "a")
	_, _ = s.Ask(context.Background(), "b")
	if fs.conns.Load() != 2 {
		t.Errorf("undrained body kept the connection (%d)", fs.conns.Load())
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	fs := newFake(t, func(rawReq) []byte { return hangOpen })
	c := newTestClient(t, fs, Options{Credential: oauthCred, Timeout: 50 * time.Millisecond})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); x.Err() != loadgen.ErrTimeout {
		t.Errorf("timeout: %q", x.Err())
	}
	c = newTestClient(t, fs, Options{Credential: oauthCred})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if x, _ := c.NewSession().Ask(ctx, "hi"); x.Err() != loadgen.ErrCanceled {
		t.Errorf("cancel: %q", x.Err())
	}
	// Dialing with an ended context.
	c = newTestClient(t, nil, Options{Credential: oauthCred, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, Timeout: 20 * time.Millisecond})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); x.Err() != loadgen.ErrTimeout {
		t.Errorf("dial timeout: %q", x.Err())
	}
	if got := transportClass(context.Background(), os.ErrDeadlineExceeded); got != loadgen.ErrTimeout {
		t.Errorf("deadline: %q", got)
	}
}

func TestDefaultDialAndTLS(t *testing.T) {
	// The default dialer, without the seam.
	fs := newFake(t, messagesHandler("ok"))
	c := newTestClient(t, nil, Options{Credential: oauthCred, Target: mustTarget(t, "http://"+fs.addr())})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); !x.OK() {
		t.Fatalf("plain: %+v", x.Results)
	}
	// TLS.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseEvents("tls"))
	}))
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tg := mustTarget(t, srv.URL)
	c = newTestClient(t, nil, Options{Credential: oauthCred, Target: tg, TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); !x.OK() || x.Text != "tls" {
		t.Fatalf("tls: %+v", x.Results)
	}
	// Unknown CA (default TLS config) and a plain server: connect errors.
	c = newTestClient(t, nil, Options{Credential: oauthCred, Target: tg})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); x.Err() != loadgen.ErrConnect {
		t.Errorf("unknown CA: %q", x.Err())
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
			_ = c.Close()
		}
	}()
	c = newTestClient(t, nil, Options{Credential: oauthCred, Target: mustTarget(t, "https://"+ln.Addr().String())})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); x.Err() != loadgen.ErrConnect {
		t.Errorf("plain server: %q", x.Err())
	}
}

func TestSendHello(t *testing.T) {
	fs := newFake(t, func(rawReq) []byte {
		return response(200, []string{"content-type: application/json"}, `{"message": "hello"}`)
	})
	c := newTestClient(t, fs, Options{})
	raw, _ := os.ReadFile(filepath.Join(fixtureDir, "001-api-hello-head.raw"))
	rp, err := ReplayCapture(raw)
	if err != nil {
		t.Fatal(err)
	}
	x, err := c.Send(context.Background(), rp.Request)
	if err != nil || !x.OK() || x.Status != 200 || sha(fs.requests()[0].Raw) != rp.SHA256() {
		t.Fatalf("hello: %v %+v", err, x.Results)
	}
	if _, err := c.Send(context.Background(), claudecode.Request{Version: "0.0.0"}); err == nil {
		t.Error("bad request sent")
	}
}

func TestEnvironment(t *testing.T) {
	for _, tc := range []struct{ os, want string }{
		{"Linux", " - Platform: linux\n - Shell: bash\n - OS Version: Linux 6.8.0\n"},
		{"Windows", " - Platform: win32\n - Shell: bash\n - OS Version: Windows 11\n"},
	} {
		c := newTestClient(t, nil, Options{OS: tc.os, Model: "wwz-fast"})
		env := c.environment("s")
		if !strings.Contains(env, tc.want) || !strings.Contains(env, "model named wwz-fast.") {
			t.Errorf("%s: %s", tc.os, env)
		}
	}
	for in, want := range map[string]string{
		"claude-sonnet-5-5": "Sonnet 5.5", "claude-opus-4-1-x": "Opus 4.1.x", "claude-": "claude-", "claude-x": "claude-x",
		"wwz-fast": "wwz-fast",
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
	// API key, three turns: no short system block; the last user message
	// carries cache_control.
	c := newTestClient(t, nil, Options{Credential: claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: "k"}})
	msgs := c.messages("s", []Turn{{"a", "b"}, {"c", "d"}}, "e")
	if len(msgs) != 6 {
		t.Fatalf("%d messages", len(msgs))
	}
	last := string(claudecode.Marshal(msgs[5]))
	if last != `{"role":"user","content":[{"type":"text","text":"e","cache_control":{"type":"ephemeral"}}]}` {
		t.Errorf("last %s", last)
	}
	if safeguards.Kind() != claudecode.KindArray {
		t.Error("safeguards did not parse")
	}
}

func TestState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	p, err := DefaultStatePath()
	if err != nil || p != filepath.Join(dir, "wwz-load", "claude-code.json") {
		t.Fatal(p, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", dir)
	if p, _ := DefaultStatePath(); p != filepath.Join(dir, ".local", "state", "wwz-load", "claude-code.json") {
		t.Errorf("home path %s", p)
	}
	t.Setenv("HOME", "")
	if _, err := DefaultStatePath(); err == nil {
		t.Error("no HOME accepted")
	}

	path := filepath.Join(dir, "s", "claude-code.json")
	st, err := LoadState(path)
	if err != nil || len(st.DeviceID) != 64 {
		t.Fatal(st, err)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("modes %v %v", fi.Mode(), di.Mode())
	}
	st.Conversation = &Conversation{SessionID: "x", Turns: []Turn{{"a", "b"}}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := LoadState(path)
	if err != nil || again.DeviceID != st.DeviceID || again.Conversation.Turns[0].Assistant != "b" {
		t.Fatalf("reload %+v %v", again, err)
	}
	for name, body := range map[string]string{"corrupt": "{", "device": `{"device_id":"ABC"}`} {
		p := filepath.Join(dir, name+".json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadState(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadState(dir); err == nil {
		t.Error("directory read as state")
	}
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if err := st.Save(filepath.Join(file, "x.json")); err == nil {
		t.Error("save under a file")
	}
	_ = os.MkdirAll(filepath.Join(dir, "tmpdir.json.tmp", "x"), 0o700)
	if err := st.Save(filepath.Join(dir, "tmpdir.json")); err == nil {
		t.Error("save over a .tmp directory")
	}
	_ = os.MkdirAll(filepath.Join(dir, "full", "x"), 0o700)
	if err := st.Save(filepath.Join(dir, "full")); err == nil {
		t.Error("rename over a directory")
	}
}

// writeFailConn is a connection whose writes fail.
type writeFailConn struct{ net.Conn }

func (writeFailConn) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestWriteError(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newTestClient(t, nil, Options{Credential: oauthCred, Dial: func(context.Context, string, string) (net.Conn, error) {
		return writeFailConn{a}, nil
	}})
	if x, _ := c.NewSession().Ask(context.Background(), "hi"); x.Err() != loadgen.ErrReset {
		t.Errorf("write error: %q", x.Err())
	}
}

func TestRun(t *testing.T) {
	fs := newFake(t, messagesHandler("ok"))
	c := newTestClient(t, fs, Options{Credential: oauthCred, Entrypoint: claudecode.EntrypointCLI})
	cfg := loadgen.RunConfig{Mode: loadgen.ModeConcurrency, Concurrency: 1, Requests: 3}
	res, err := c.Run(context.Background(), cfg, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	// 3 sessions × (quota + 2 turns).
	if res.Total.Stats.Requests != 9 || res.Total.Stats.OK != 9 || len(fs.requests()) != 9 {
		t.Errorf("requests %d ok %d sent %d", res.Total.Stats.Requests, res.Total.Stats.OK, len(fs.requests()))
	}
	if _, err := c.Run(context.Background(), cfg, 0, "x"); !errors.Is(err, ErrNoTurns) {
		t.Errorf("turns 0: %v", err)
	}
	// A failed request ends the session.
	bad := newFake(t, func(rawReq) []byte { return jsonResp(404, `{}`) })
	c = newTestClient(t, bad, Options{Credential: oauthCred})
	res, _ = c.Run(context.Background(), loadgen.RunConfig{Mode: loadgen.ModeConcurrency, Concurrency: 1, Requests: 2}, 3, "x")
	if res.Total.Stats.Requests != 2 || res.Total.Stats.ErrorsByType["http_404"] != 2 {
		t.Errorf("failures %+v", res.Total.Stats)
	}
	// The run stops starting turns once it is done.
	slow := newFake(t, func(r rawReq) []byte {
		time.Sleep(30 * time.Millisecond)
		return sseResp("ok")
	})
	c = newTestClient(t, slow, Options{Credential: oauthCred})
	res, _ = c.Run(context.Background(), loadgen.RunConfig{Mode: loadgen.ModeRPS, RPS: 100, Requests: 1}, 5, "x")
	if res.Total.Stats.Requests != 1 {
		t.Errorf("turns after done: %d", res.Total.Stats.Requests)
	}

	rc := c.ReportConfig(loadgen.RunConfig{Mode: loadgen.ModeRPS, RPS: 5, Duration: time.Second, Requests: 7})
	if rc.Protocol != "claude-code" || rc.Path != "/v1/messages" || rc.MaxTokens != 128000 || !rc.Stream || rc.RPS != 5 ||
		rc.DurationMs != 1000 || rc.Requests != 7 || rc.Target != captureURL || rc.Model != DefaultModelOAuth || rc.Instances != 1 {
		t.Errorf("report config %+v", rc)
	}
	c = newTestClient(t, nil, Options{NonStream: true})
	if rc := c.ReportConfig(loadgen.RunConfig{}); rc.MaxTokens != 64000 || rc.Stream {
		t.Errorf("non-stream %+v", rc)
	}
}
