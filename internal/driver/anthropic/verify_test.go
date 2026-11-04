package anthropic

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

// rawFixture splits a capture of testdata/real/claude-code/2.1.308/ into
// the raw head the data plane hands the driver and the body.
func rawFixture(t *testing.T, raw []byte) (driver.RawHead, []byte) {
	t.Helper()
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("no end of head")
	}
	line, block, _ := strings.Cut(string(raw[:end+4]), "\r\n")
	return driver.RawHead{Line: line, Block: block}, raw[end+4:]
}

// TestVerifyClientOption: client_verification turns on the raw head and
// the verification of the two inference routes only; off (the default)
// asks for nothing.
func TestVerifyClientOption(t *testing.T) {
	for opts, on := range map[string]bool{"": false, verifyOff: false, verifyClaudeCode: true} {
		d, err := driver.New("anthropic", map[string]string{"client_verification": opts})
		if err != nil {
			t.Fatal(err)
		}
		if driver.WantsRawHead(d) != on {
			t.Errorf("%q: WantsRawHead %v", opts, !on)
		}
		for _, k := range []driver.Kind{driver.KindMessages, driver.KindCountTokens} {
			if _, ok := driver.ClientVerifierOf(d, k); ok != on {
				t.Errorf("%q: %s verified %v", opts, k, ok)
			}
		}
		for _, k := range []driver.Kind{driver.KindModels, driver.KindModel, driver.KindUsage} {
			if _, ok := driver.ClientVerifierOf(d, k); ok {
				t.Errorf("%q: %s verified", opts, k)
			}
		}
	}
	// The configuration (spec 003): off and claude-code on the anthropic
	// driver; another value, or the option on another driver, is invalid.
	for yaml, want := range map[string]string{
		"drivers: [{name: anthropic, options: {client_verification: off}}]":         "",
		"drivers: [{name: anthropic, options: {client_verification: claude-code}}]": "",
		"drivers: [{name: anthropic, options: {client_verification: strict}}]":      `option client_verification="strict" is invalid`,
		"drivers: [{name: openai, options: {client_verification: claude-code}}]":    `unknown option "client_verification"`,
	} {
		_, err := config.Parse([]byte(yaml + "\n"))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", yaml, err)
		}
	}
}

// TestVerifyClientFixtures: every inference capture of Claude Code
// 2.1.308 passes; a mutation of each is refused with its reason, the
// spec 020 message and the reason and detail for the engine.
func TestVerifyClientFixtures(t *testing.T) {
	d := &Driver{verify: true}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "claude-code", "2.1.308", "*.raw"))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte("POST ")) {
			continue // HEAD /api/hello is no inference route
		}
		seen++
		kind := driver.KindMessages
		if bytes.HasPrefix(raw, []byte("POST /v1/messages/count_tokens")) {
			kind = driver.KindCountTokens
		}
		head, body := rawFixture(t, raw)
		if e := d.VerifyClient(kind, head, body); e != nil {
			t.Errorf("%s: %v", filepath.Base(f), e)
		}
		head.Line = strings.Replace(head.Line, "?beta=true", "", 1)
		e := d.VerifyClient(kind, head, body)
		reason, detail, ok := driver.ForgedReason(e)
		want := "Client verification failed: this request does not match Claude Code 2.1.308; the client is forging Claude Code requests (query: " + detail + ")."
		if !ok || reason != claudecode.ReasonQuery || detail == "" || e.Message != want {
			t.Errorf("%s without ?beta=true: %+v", filepath.Base(f), e)
		}
	}
	if seen != 8 {
		t.Fatalf("%d inference fixtures, want 8", seen)
	}
}

// TestVerifyClientHead: without a raw head, or with a line that is not
// three parts, the refusal is the line's; the headers keep their order,
// spelling and repeats.
func TestVerifyClientHead(t *testing.T) {
	d := &Driver{verify: true}
	for _, head := range []driver.RawHead{{}, {Line: "POST", Block: "\r\n"}, {Line: "POST /v1/messages?beta=true", Block: "\r\n"}} {
		if reason, _, _ := driver.ForgedReason(d.VerifyClient(driver.KindMessages, head, nil)); reason != claudecode.ReasonQuery {
			t.Errorf("%q: %q", head.Line, reason)
		}
	}
	h := claudeHead(driver.RawHead{Line: "POST /v1/messages?beta=true HTTP/1.1", Block: "user-agent: a\r\nX-A:b\r\nX-A: c \r\n\r\n"})
	want := claudecode.Head{Method: "POST", Target: "/v1/messages?beta=true", Proto: "HTTP/1.1", Headers: []claudecode.Header{
		{Name: "user-agent", Value: "a"}, {Name: "X-A", Value: "b"}, {Name: "X-A", Value: "c"},
	}}
	if h.Method != want.Method || h.Target != want.Target || h.Proto != want.Proto || len(h.Headers) != 3 ||
		h.Headers[0] != want.Headers[0] || h.Headers[1] != want.Headers[1] || h.Headers[2] != want.Headers[2] {
		t.Errorf("claudeHead = %+v", h)
	}
	if e := d.VerifyClient(driver.KindMessages, driver.RawHead{}, nil); e.Kind != core.ErrForgedRequest {
		t.Errorf("kind %v", e.Kind)
	}
}
