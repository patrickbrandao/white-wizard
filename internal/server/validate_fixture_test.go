package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// realFixture is a request recorded against the real provider and its
// normalized answer (testdata/real/<provider>/validation/*.json): ids that
// change per call are markers (<request_id>, <user_id>), credentials are
// <key>, and the body of an accepted request (200) is not kept.
type realFixture struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Request  struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status      int     `json:"status"`
		ContentType string  `json:"content_type"`
		Body        *string `json:"body"`
	} `json:"response"`
}

// fixtureCfg serves the models the fixtures name, one driver at a time.
const fixtureCfg = fastCfg + `
models:
  - {id: gpt-4o-mini, latency: fast}
  - {id: claude-haiku-4-5, latency: fast}
  - {id: claude-haiku-5-5, latency: fast}
  - {id: claude-sonnet-5-5, latency: fast}
  - {id: claude-opus-5-5, latency: fast}
  - {id: claude-fable-5, latency: fast}
  - {id: claude-fable-5-1, latency: fast}
  - {id: openai/gpt-4o-mini, latency: fast}
  - {id: openai/gpt-4.1-nano, latency: fast}
  - {id: anthropic/claude-haiku-4.5, latency: fast}
  - {id: glm-5.3-flash, latency: fast}
`

var (
	// req_… (Anthropic) or gen-… (OpenRouter's Messages route, task 174).
	reqIDRe  = regexp.MustCompile(`"request_id":"(req_[A-Za-z0-9]+|gen-[0-9]+-[A-Za-z0-9]+)"`)
	userIDRe = regexp.MustCompile(`"user_id":"user_[A-Za-z0-9]+"`)
)

// normalizeBody replaces the per-call ids the way the fixtures were
// normalized.
func normalizeBody(b string) string {
	b = reqIDRe.ReplaceAllString(b, `"request_id":"<request_id>"`)
	return userIDRe.ReplaceAllString(b, `"user_id":"<user_id>"`)
}

func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(mt)
}

// TestRealFixturesValidation sends every recorded request to the
// simulator and compares status, content type and the error envelope
// with the real answer, byte for byte (ADR 0014; Anthropic since task
// 173; the tool_choice refusals of Anthropic, the Claude 5.5 models
// included, since task 301).
func TestRealFixturesValidation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "*", "validation", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	toolChoice, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "anthropic", "tool-choice", "*.json"))
	if err != nil || len(toolChoice) < 40 {
		t.Fatalf("tool-choice fixtures: %d %v", len(toolChoice), err)
	}
	files = append(files, toolChoice...)
	harnesses := map[string]*harness{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx realFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		h := harnesses[fx.Provider]
		if h == nil {
			h = start(t, fixtureCfg+"drivers: [{name: "+fx.Provider+"}]\n", nil)
			harnesses[fx.Provider] = h
		}
		hdr := map[string]string{}
		for k, v := range fx.Request.Headers {
			switch v {
			case "Bearer <key>":
				v = "Bearer wwz-key-01"
			case "<key>":
				v = "wwz-key-01"
			case "<session>":
				v = "wwz-session"
			}
			hdr[k] = v
		}
		resp := h.do(fx.Request.Method, fx.Request.Path, hdr, fx.Request.Body)
		got := normalizeBody(string(resp.Body()))
		where := fx.Provider + "/" + fx.Name
		if resp.StatusCode() != fx.Response.Status {
			t.Errorf("%s: status %d, real %d\n wwz  %s\n real %v", where, resp.StatusCode(), fx.Response.Status, got, deref(fx.Response.Body))
			continue
		}
		if fx.Response.Body == nil {
			continue // accepted: the generated content is not compared
		}
		ct := mediaType(string(resp.Header.Peek("Content-Type")))
		if fx.Response.ContentType == "" {
			ct = rawContentType(t, h, fx.Request.Path, hdr, fx.Request.Body)
		}
		if want := mediaType(fx.Response.ContentType); ct != want {
			t.Errorf("%s: content type %q, real %q", where, ct, want)
		}
		want := *fx.Response.Body
		if fx.Provider == "openai" || fx.Provider == "anthropic" {
			// The whole Content-Type too (OpenAI's edge adds "; charset=utf-8").
			if got := string(resp.Header.Peek("Content-Type")); got != fx.Response.ContentType {
				t.Errorf("%s: content type %q, real %q", where, got, fx.Response.ContentType)
			}
		}
		if got != want {
			t.Errorf("%s: body\n wwz  %s\n real %s", where, got, want)
		}
	}
}

// rawContentType sends the request over a bare connection and returns the
// Content-Type header as sent ("" when absent): fasthttp's client reports
// a default for a response without one.
func rawContentType(t *testing.T, h *harness, path string, hdr map[string]string, body string) string {
	t.Helper()
	c, err := h.ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var b strings.Builder
	b.WriteString("POST " + path + " HTTP/1.1\r\nHost: wwz\r\nConnection: close\r\nContent-Type: application/json\r\n")
	for k, v := range hdr {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	if _, err := c.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return mediaType(r.Header.Get("Content-Type"))
}

func deref(s *string) string {
	if s == nil {
		return "<accepted>"
	}
	return *s
}
