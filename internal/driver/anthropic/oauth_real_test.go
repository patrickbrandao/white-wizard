package anthropic_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOAuthClaudeCodeFixtures: the OAuth requests of Claude Code 2.1.308
// (testdata/real/claude-code/2.1.308/, byte for byte as captured: -p
// turns, the resumed conversation, the quota probe, the interactive turn
// with safeguards and count_tokens) get a 200 when their fake token is
// swapped for one wwz issued through the login, the Messages answers with
// the anthropic-ratelimit-unified-* instead of the anthropic-ratelimit-*.
// The fake token itself, which is no keys[] entry here, is the invalid
// token's 401 (ADR 0074, spec 006 16.2.1).
func TestOAuthClaudeCodeFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "real", "claude-code", "2.1.308", "*.raw"))
	if err != nil {
		t.Fatal(err)
	}
	// A quota the six answers do not exhaust.
	base := startOAuth(t, strings.Replace(oauthConfig, "monthly_usd: 100,", "monthly_usd: 1000000,", 1), newClock(time.Now()))
	addr := strings.TrimPrefix(base, "http://")
	token := login(t, base, claudeScopes).AccessToken
	const auth = "\r\nAuthorization: Bearer "
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		head, body, _ := strings.Cut(string(raw), "\r\n\r\n")
		i := strings.Index(head, auth+"sk-ant-oat01-")
		if i < 0 {
			continue // the API key captures and the /api/hello probe
		}
		seen++
		end := i + len(auth) + strings.Index(head[i+len(auth):]+"\r\n", "\r\n")
		fake := head[i+len(auth) : end]
		swapped := head[:i] + auth + token + head[end:] + "\r\n\r\n" + body
		countTokens := strings.HasPrefix(head, "POST /v1/messages/count_tokens")
		t.Run(filepath.Base(f), func(t *testing.T) {
			r, got := rawDo(t, addr, "POST", []byte(swapped))
			if r.StatusCode != 200 {
				t.Fatalf("status %d: %s", r.StatusCode, got)
			}
			switch {
			case countTokens:
				if !strings.HasPrefix(got, `{"input_tokens":`) {
					t.Errorf("not a count: %s", got)
				}
			case r.Header.Get("anthropic-ratelimit-unified-status") != "allowed" || r.Header.Get("anthropic-ratelimit-unified-5h-reset") == "" ||
				r.Header.Get("anthropic-ratelimit-requests-limit") != "" || r.Header.Get("anthropic-ratelimit-tokens-limit") != "":
				t.Errorf("headers %v", r.Header)
			}
			r, got = rawDo(t, addr, "POST", []byte(head+"\r\n\r\n"+body))
			if r.StatusCode != 401 || !strings.Contains(got, `"message":"OAuth access token is invalid."`) || !strings.HasPrefix(fake, "sk-ant-oat01-WWZFAKE") {
				t.Errorf("fake token: %d %s", r.StatusCode, got)
			}
		})
	}
	if seen < 6 {
		t.Fatalf("%d OAuth fixtures, want the 6 of the MANIFEST", seen)
	}
}
