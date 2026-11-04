package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
)

var manifestRow = regexp.MustCompile("^\\| `([^`]+\\.raw)` \\|.*\\| `([0-9a-f]{64})` \\|$")

// manifest returns file → sha256 from MANIFEST.md.
func manifest(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "MANIFEST.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := manifestRow.FindStringSubmatch(line); m != nil {
			rows[m[1]] = m[2]
		}
	}
	if len(rows) == 0 {
		t.Fatal("empty MANIFEST.md")
	}
	return rows
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestReplayFixtures: every fixture is rebuilt identical (the sha256 of
// MANIFEST.md), and --send writes exactly those bytes on the socket.
func TestReplayFixtures(t *testing.T) {
	for name, sum := range manifest(t) {
		t.Run(name, func(t *testing.T) {
			raw := readFixture(t, name)
			rp, err := ReplayCapture(raw)
			if err != nil {
				t.Fatal(err)
			}
			if rp.Diff != nil || rp.SHA256() != sum || sha(rp.Got) != sum {
				t.Fatalf("replay: %v, sha256 %s, want %s", rp.Diff, rp.SHA256(), sum)
			}
			fs := newFake(t, messagesHandler("ok"))
			c := newTestClient(t, fs, Options{Target: mustTarget(t, "http://"+rp.Request.Host)})
			x, err := c.Send(context.Background(), rp.Request)
			if err != nil || len(fs.requests()) != 1 {
				t.Fatalf("send: %v %+v", err, x)
			}
			if got := sha(fs.requests()[0].Raw); got != sum {
				t.Fatalf("sent sha256 %s, want %s", got, sum)
			}
		})
	}
}

func TestReplayDiff(t *testing.T) {
	turn := readFixture(t, "002-sdk-cli-oauth-turn.raw")
	cases := []struct {
		name, old, new, where string
	}{
		{"request line", "?beta=true", "?beta=false", "head line 1"},
		{"header order", "X-Stainless-Arch: arm64\r\nX-Stainless-Lang: js", "X-Stainless-Lang: js\r\nX-Stainless-Arch: arm64", "head line 7 (X-Stainless-Lang)"},
		{"fingerprint", "cc_version=2.1.308.9fe", "cc_version=2.1.308.000", "body $.system[0].text"},
		{"encoding", `{"model":"claude-sonnet-5-5",`, `{"model":"claude-sonnet-5-5" ,`, "body (same JSON values, different encoding)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Replace(turn, []byte(tc.old), []byte(tc.new), 1)
			rp, err := ReplayCapture(raw)
			if err != nil {
				t.Fatal(err)
			}
			if rp.Diff == nil || rp.Diff.Where != tc.where {
				t.Fatalf("diff %v, want %s", rp.Diff, tc.where)
			}
			if s := rp.Diff.String(); !strings.HasPrefix(s, "differs at byte ") || !strings.Contains(s, "want: ") {
				t.Errorf("String %q", s)
			}
		})
	}
	// Inputs that cannot be extracted or rebuilt.
	quota := readFixture(t, "010-cli-oauth-quota.raw")
	for name, raw := range map[string][]byte{
		"no head":    []byte("POST /"),
		"bad path":   bytes.Replace(turn, []byte("/v1/messages?"), []byte("/v2/messages?"), 1),
		"no profile": bytes.Replace(quota, []byte("(external, cli)"), []byte("(external, sdk-cli)"), 1),
	} {
		if _, err := ReplayCapture(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPathDiff(t *testing.T) {
	p := func(s string) claudecode.Value {
		v, err := claudecode.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ a, b, want string }{
		{`{"a":1}`, `{"a":1}`, ""},
		{`[1,2]`, `[1,2]`, ""},
		{`{"a":1}`, `[1]`, "$"},
		{`{"a":1}`, `{"b":1}`, "$ (keys)"},
		{`{"a":[1,2]}`, `{"a":[1]}`, "$.a (length)"},
		{`{"a":[1,{"b":"x"}]}`, `{"a":[1,{"b":"y"}]}`, "$.a[1].b"},
	}
	for _, tc := range cases {
		got, ok := pathDiff(p(tc.a), p(tc.b), "$")
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("pathDiff(%s, %s) = %q, %v; want %q", tc.a, tc.b, got, ok, tc.want)
		}
	}
	// Bodies that are not JSON keep the plain "body".
	d := diff([]byte("POST / HTTP/1.1\r\n\r\n{x"), []byte("POST / HTTP/1.1\r\n\r\n{y"))
	if d.Where != "body" || d.Offset != 20 {
		t.Errorf("%+v", d)
	}
}
