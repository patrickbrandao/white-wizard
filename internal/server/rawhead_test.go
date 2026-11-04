package server

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// rawSeen is what a raw head test driver got in Authenticate.
type rawSeen struct {
	head driver.RawHead
	ok   bool
}

// rawSeenCh receives one rawSeen per request of the raw head test drivers
// (dropped when nobody reads, as in the benchmarks).
var rawSeenCh = make(chan rawSeen, 16)

// rawCaptureDriver is the anthropic driver that reports the raw head it
// finds in the headers, without asking for it.
type rawCaptureDriver struct{ driver.Driver }

func (d rawCaptureDriver) Authenticate(h driver.Headers) driver.Credential {
	head, ok := driver.RawHeadOf(h)
	select {
	case rawSeenCh <- rawSeen{head, ok}:
	default:
	}
	return d.Driver.Authenticate(h)
}

// rawHeadDriver is rawCaptureDriver asking for the raw head.
type rawHeadDriver struct{ rawCaptureDriver }

func (rawHeadDriver) WantsRawHead() bool { return true }

func init() {
	driver.Register("wwz-test-rawhead", func(opts map[string]string) (driver.Driver, error) {
		d, err := driver.New("anthropic", opts)
		return rawHeadDriver{rawCaptureDriver{d}}, err
	})
	driver.Register("wwz-test-rawhead-off", func(opts map[string]string) (driver.Driver, error) {
		d, err := driver.New("anthropic", opts)
		return rawCaptureDriver{d}, err
	})
}

// rawHeadCfg mounts the anthropic driver at the root and the raw head test
// drivers at /raw (asks) and /off (does not ask).
const rawHeadCfg = fastCfg + `drivers:
  - {name: anthropic}
  - {name: wwz-test-rawhead, prefix: /raw}
  - {name: wwz-test-rawhead-off, prefix: /off}
`

// syntheticHead is the head of capture 002 of Claude Code 2.1.308 (spec
// 020) with a fake token, as the client sent it: Bun's order, mixed
// spelling (X-Stainless-OS, lower-case anthropic-*), Host and
// Content-Length last.
const syntheticHead = "POST /raw/v1/messages?beta=true HTTP/1.1\r\n" +
	"Accept: application/json\r\n" +
	"Authorization: Bearer sk-ant-oat01-wwz-fake-token\r\n" +
	"Content-Type: application/json\r\n" +
	"User-Agent: claude-cli/2.1.308 (external, sdk-cli)\r\n" +
	"X-Claude-Code-Session-Id: ce90e450-43fc-4765-8110-6aed6d650ca1\r\n" +
	"X-Stainless-Arch: arm64\r\n" +
	"X-Stainless-Lang: js\r\n" +
	"X-Stainless-OS: MacOS\r\n" +
	"X-Stainless-Package-Version: 0.128.0\r\n" +
	"X-Stainless-Retry-Count: 0\r\n" +
	"X-Stainless-Runtime: node\r\n" +
	"X-Stainless-Runtime-Version: v26.3.0\r\n" +
	"X-Stainless-Timeout: 600\r\n" +
	"anthropic-beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14\r\n" +
	"anthropic-dangerous-direct-browser-access: true\r\n" +
	"anthropic-version: 2023-06-01\r\n" +
	"x-app: cli\r\n" +
	"Connection: keep-alive\r\n" +
	"Host: 127.0.0.1:18556\r\n" +
	"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
	"Content-Length: 2\r\n" +
	"\r\n"

// rawRequests are the requests the raw head test sends: the synthetic
// head, the Claude Code fixtures in testdata/real/claude-code/ (task 442)
// and the captures 002, 004, 006 and 008 in tmp/claude-code/captures/
// when this checkout has them (outside git, ADR 0074), each rewritten to
// the /raw mount.
func rawRequests(t *testing.T) map[string][]byte {
	t.Helper()
	reqs := map[string][]byte{"synthetic": []byte(syntheticHead + "{}")}
	var files []string
	for _, pat := range []string{
		"../../testdata/real/claude-code/*/*.raw",
		"../../tmp/claude-code/captures/00[2468].raw",
	} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		method, rest, ok := bytes.Cut(b, []byte(" "))
		if !ok || !bytes.HasPrefix(rest, []byte("/")) {
			continue // not a request with a path target
		}
		if string(method) != "POST" {
			// HEAD /api/hello (fixture 001) is no route of the test
			// drivers: a route miss, answered without Authenticate.
			continue
		}
		reqs[f] = append(append(append([]byte{}, method...), " /raw"...), rest...)
	}
	return reqs
}

// splitHead splits a raw request at the end of its head: the request line
// and the header section with the empty line that ends it.
func splitHead(t *testing.T, raw []byte) (line, block string) {
	t.Helper()
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("no end of head")
	}
	l, b, _ := strings.Cut(string(raw[:end+4]), "\r\n")
	return l, b
}

// serveTCP serves yaml on a loopback TCP listener and returns its address.
func serveTCP(t *testing.T, yaml string) string {
	t.Helper()
	s, err := New(newEngine(t, yaml), Options{Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return ln.Addr().String()
}

// sendRaw writes raw on a new TCP connection to addr and reads one
// response; it returns what the test driver saw.
func sendRaw(t *testing.T, addr string, raw []byte) rawSeen {
	t.Helper()
	for len(rawSeenCh) > 0 {
		<-rawSeenCh
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(raw); err != nil {
		t.Fatal(err)
	}
	var resp fasthttp.Response
	if err := resp.Read(bufio.NewReader(c)); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-rawSeenCh:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("the driver was not called (status %d: %s)", resp.StatusCode(), resp.Body())
		return rawSeen{}
	}
}

// TestRawHeadOverTCP: the head of each Claude Code request sent raw over
// TCP reaches a driver that asks for it byte for byte: the request line
// with the query, every header in order, spelling (X-Stainless-OS,
// lower-case anthropic-*) and value, Host, User-Agent, Content-Type and
// Content-Length where they were sent.
func TestRawHeadOverTCP(t *testing.T) {
	addr := serveTCP(t, rawHeadCfg)
	for name, raw := range rawRequests(t) {
		t.Run(filepath.Base(name), func(t *testing.T) {
			line, block := splitHead(t, raw)
			got := sendRaw(t, addr, raw)
			if !got.ok {
				t.Fatal("no raw head")
			}
			if got.head.Line != line {
				t.Errorf("line\n got %q\nwant %q", got.head.Line, line)
			}
			if got.head.Block != block {
				t.Errorf("block\n got %q\nwant %q", got.head.Block, block)
			}
			if want := rawHeaderLines(block); !reflect.DeepEqual(got.head.Headers(), want) {
				t.Errorf("headers\n got %q\nwant %q", got.head.Headers(), want)
			}
		})
	}
}

// rawHeaderLines is the list of headers of a block, split by hand.
func rawHeaderLines(block string) []driver.RawHeader {
	var out []driver.RawHeader
	for _, l := range strings.Split(strings.TrimSuffix(block, "\r\n\r\n"), "\r\n") {
		n, v, _ := strings.Cut(l, ": ")
		out = append(out, driver.RawHeader{Name: n, Value: v})
	}
	return out
}

// TestRawHeadRepeatsAndChunked: a repeated header arrives as two lines
// (the lower-cased Headers join them), a lower-case user-agent keeps its
// spelling, and a chunked body leaves Transfer-Encoding in the head.
func TestRawHeadRepeatsAndChunked(t *testing.T) {
	addr := serveTCP(t, rawHeadCfg)
	head := "POST /raw/v1/messages?beta=true&x=%2F HTTP/1.1\r\n" +
		"Host: wwz\r\n" +
		"x-api-key: wwz-key-01\r\n" +
		"anthropic-version: 2023-06-01\r\n" +
		"anthropic-beta: a-2025-01-01\r\n" +
		"user-agent: lower/1.0\r\n" +
		"X-Stainless-OS: Linux\r\n" +
		"anthropic-beta: b-2025-02-02\r\n" +
		"Content-Type: application/json\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"
	body := `{"model":"wwz-fast","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	raw := head + strconv.FormatInt(int64(len(body)), 16) + "\r\n" + body + "\r\n0\r\n\r\n"
	got := sendRaw(t, addr, []byte(raw))
	line, block := splitHead(t, []byte(raw))
	if !got.ok || got.head.Line != line || got.head.Block != block {
		t.Fatalf("raw head %v\n got %q %q\nwant %q %q", got.ok, got.head.Line, got.head.Block, line, block)
	}
	want := []driver.RawHeader{
		{Name: "Host", Value: "wwz"},
		{Name: "x-api-key", Value: "wwz-key-01"},
		{Name: "anthropic-version", Value: "2023-06-01"},
		{Name: "anthropic-beta", Value: "a-2025-01-01"},
		{Name: "user-agent", Value: "lower/1.0"},
		{Name: "X-Stainless-OS", Value: "Linux"},
		{Name: "anthropic-beta", Value: "b-2025-02-02"},
		{Name: "Content-Type", Value: "application/json"},
		{Name: "Transfer-Encoding", Value: "chunked"},
	}
	if hs := got.head.Headers(); !reflect.DeepEqual(hs, want) {
		t.Errorf("headers\n got %q\nwant %q", hs, want)
	}
}

// TestRawHeadNotAsked: a driver that does not ask finds no raw head, and
// the anthropic driver without the option answers the same request as
// before.
func TestRawHeadNotAsked(t *testing.T) {
	addr := serveTCP(t, rawHeadCfg)
	raw := strings.Replace(syntheticHead, "/raw/", "/off/", 1) + "{}"
	if got := sendRaw(t, addr, []byte(raw)); got.ok || got.head != (driver.RawHead{}) {
		t.Errorf("a driver that did not ask got %#v", got)
	}
}

// benchHandle measures Handle for one parsed request on path: the whole
// data plane path of a non-streaming Messages request.
func benchHandle(b *testing.B, path string) {
	s, err := New(newEngine(b, rawHeadCfg), Options{Logger: quietLog})
	if err != nil {
		b.Fatal(err)
	}
	body := `{"model":"wwz-fast","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`
	raw := strings.Replace(syntheticHead, "/raw/v1/messages", path, 1)
	raw = strings.Replace(raw, "Content-Length: 2", "Content-Length: "+strconv.Itoa(len(body)), 1)
	raw = strings.Replace(raw, "Authorization: Bearer sk-ant-oat01-wwz-fake-token", "x-api-key: wwz-key-01", 1)
	var req fasthttp.Request
	if err := req.Read(bufio.NewReader(strings.NewReader(raw + body))); err != nil {
		b.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}
	var ctx fasthttp.RequestCtx
	ctx.Init(&req, addr, nil)
	s.Handle(&ctx)
	if ctx.Response.StatusCode() != 200 {
		b.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctx.Init(&req, addr, nil)
		s.Handle(&ctx)
	}
}

// BenchmarkHandleRawHeadOff is the hot path: the anthropic driver without
// the raw head. Its allocs/op must not change with the raw head support
// (spec 014, spec 020 RNF-01).
func BenchmarkHandleRawHeadOff(b *testing.B) { benchHandle(b, "/v1/messages") }

// BenchmarkHandleRawHeadOn is the same request on a test driver that asks
// for the raw head, and BenchmarkHandleRawHeadOnBaseline on the same
// wrapper without asking (the wrapper hides the anthropic driver's
// optional interfaces, so only these two compare): the copy costs one
// allocation.
func BenchmarkHandleRawHeadOn(b *testing.B) { benchHandle(b, "/raw/v1/messages") }

func BenchmarkHandleRawHeadOnBaseline(b *testing.B) { benchHandle(b, "/off/v1/messages") }
