package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRealFixturesAuthOrder replays testdata/real/<provider>/auth-order/:
// which check comes first when the credential and the body are both wrong
// (ADR 0053, spec 006 16.7). badkey_* send the recorded (fake) credential
// as is, nokey_* send none and validkey_* swap <key> for a wwz key. Status,
// content type and the normalized body are compared byte for byte, as in
// TestRealFixturesValidation (task 387: field order and indentation too).
func TestRealFixturesAuthOrder(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "*", "auth-order", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
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
		if fx.Name == "" {
			fx.Name = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		where := fx.Provider + "/" + fx.Name
		h := harnesses[fx.Provider]
		if h == nil {
			h = start(t, fixtureCfg+"drivers: [{name: "+fx.Provider+"}]\n", nil)
			harnesses[fx.Provider] = h
		}
		t.Run(where, func(t *testing.T) {
			h.t = t
			defer func(parent *testing.T) { h.t = parent }(h.t)
			hdr := map[string]string{}
			for k, v := range fx.Request.Headers {
				switch v {
				case "Bearer <key>":
					v = "Bearer wwz-key-01"
				case "<key>":
					v = "wwz-key-01"
				}
				hdr[k] = v
			}
			resp := h.do(fx.Request.Method, fx.Request.Path, hdr, fx.Request.Body)
			got := normalizeBody(string(resp.Body()))
			if resp.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d\n wwz  %s\n real %v", resp.StatusCode(), fx.Response.Status, got, deref(fx.Response.Body))
			}
			if fx.Response.Body == nil {
				return
			}
			ct := mediaType(string(resp.Header.Peek("Content-Type")))
			if fx.Response.ContentType == "" {
				ct = rawContentType(t, h, fx.Request.Path, hdr, fx.Request.Body)
			}
			if want := mediaType(fx.Response.ContentType); ct != want {
				t.Errorf("content type %q, real %q", ct, want)
			}
			if fx.Provider == "openai" || fx.Provider == "anthropic" {
				// The whole Content-Type too (OpenAI's edge adds "; charset=utf-8").
				if got := string(resp.Header.Peek("Content-Type")); got != fx.Response.ContentType {
					t.Errorf("content type %q, real %q", got, fx.Response.ContentType)
				}
			}
			if want := *fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %s\n real %s", got, want)
			}
		})
	}
}

// TestAuthOrderHeaders: the headers the fixture comparison above leaves
// out. OpenAI without a key answers from the edge, without x-error-json;
// the OpenRouter gateway's own 400s carry a generation id and the CORS
// list (shape/error_validation.json), the unknown model before a type
// error included.
func TestAuthOrderHeaders(t *testing.T) {
	oa := start(t, fixtureCfg+"drivers: [{name: openai}]\n", nil)
	resp := oa.do("POST", "/v1/chat/completions", nil, `{"model": "gpt-4o-mini"}`)
	if resp.StatusCode() != 401 || len(resp.Header.Peek("x-error-json")) != 0 {
		t.Errorf("openai nokey: %d x-error-json %q", resp.StatusCode(), resp.Header.Peek("x-error-json"))
	}
	or := start(t, fixtureCfg+"drivers: [{name: openrouter}]\n", nil)
	auth := map[string]string{"authorization": "Bearer wwz-key-01"}
	for _, body := range []string{
		`{"model": "openai/gpt-4o-mini"}`,
		`{"model": "wwz-nao-existe", "messages": [{"role": "user", "content": "oi"}], "stream": "x"}`,
	} {
		resp := or.do("POST", "/api/v1/chat/completions", auth, body)
		if resp.StatusCode() != 400 || !strings.HasPrefix(string(resp.Header.Peek("x-generation-id")), "gen-") ||
			len(resp.Header.Peek("Access-Control-Expose-Headers")) == 0 {
			t.Errorf("%s: %d generation %q", body, resp.StatusCode(), resp.Header.Peek("x-generation-id"))
		}
	}

	// shape/error_validation.json byte for byte (task 387): status, body
	// and the gateway's headers.
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "openrouter", "shape", "error_validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Request struct {
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		} `json:"request"`
		Response struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Request.Headers["authorization"] != "Bearer <key>" {
		t.Fatalf("request headers %v", fx.Request.Headers)
	}
	resp = or.do("POST", fx.Request.Path, auth, fx.Request.Body)
	if got := normalizeBody(string(resp.Body())); resp.StatusCode() != fx.Response.Status || got != fx.Response.Body {
		t.Errorf("error_validation: %d %s\n real %d %s", resp.StatusCode(), got, fx.Response.Status, fx.Response.Body)
	}
	for k, want := range fx.Response.Headers {
		got := string(resp.Header.Peek(k))
		if want == "<generation_id>" {
			if !regexp.MustCompile(`^gen-[0-9]{10}-[A-Za-z0-9]{20}$`).MatchString(got) {
				t.Errorf("error_validation: %s = %q, not a generation id", k, got)
			}
		} else if got != want {
			t.Errorf("error_validation: %s = %q, real %q", k, got, want)
		}
	}
}

// TestOpenRouterPublicModelList: OpenRouter lists its models whatever the
// key (200 with an invalid key on the real API); its single-model route and
// OpenAI's list still want a valid key.
func TestOpenRouterPublicModelList(t *testing.T) {
	or := start(t, fixtureCfg+"drivers: [{name: openrouter}]\n", nil)
	bad := map[string]string{"authorization": "Bearer sk-or-v1-" + strings.Repeat("0", 64)}
	for _, hdr := range []map[string]string{bad, nil} {
		if resp := or.do("GET", "/api/v1/models", hdr, ""); resp.StatusCode() != 200 {
			t.Errorf("openrouter list %v: %d %s", hdr, resp.StatusCode(), resp.Body())
		}
	}
	oa := start(t, fixtureCfg+"drivers: [{name: openai}]\n", nil)
	if resp := oa.do("GET", "/v1/models", map[string]string{"authorization": "Bearer sk-bad"}, ""); resp.StatusCode() != 401 {
		t.Errorf("openai list: %d", resp.StatusCode())
	}
}
