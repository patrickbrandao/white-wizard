package server

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// responsesCfg serves the model of the Responses fixtures (snapshot as the
// model field, ADR 0049) and a retired one (deprecated_model).
const responsesCfg = fastCfg + `
models:
  - {id: gpt-4o-mini, version: gpt-4o-mini-2024-07-18, latency: fast, prompt_cache: {kind: none}}
  - {id: gpt-4.1-nano, latency: fast, deprecated_at: "2025-01-01", retires_at: "2025-06-01"}
drivers: [{name: openai}]
`

var (
	respIDRe   = regexp.MustCompile(`"(resp|msg|fc|rs)_[0-9a-f]{50}"`)
	callIDRe   = regexp.MustCompile(`"call_[0-9A-Za-z]{24}"`)
	timeRe     = regexp.MustCompile(`"(created_at|completed_at)": ?[0-9]+`)
	obfRe      = regexp.MustCompile(`"obfuscation":"[0-9A-Za-z]+"`)
	respIDOnly = regexp.MustCompile(`resp_[0-9a-f]{50}`)
)

// normalizeResponses replaces what changes per call: ids, times and the
// obfuscation padding.
func normalizeResponses(b string) string {
	b = respIDRe.ReplaceAllString(b, `"${1}_<id>"`)
	b = callIDRe.ReplaceAllString(b, `"call_<id>"`)
	b = timeRe.ReplaceAllString(b, `"$1":<t>`)
	return obfRe.ReplaceAllString(b, `"obfuscation":"<o>"`)
}

func loadResponsesFixture(t *testing.T, name string) realFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "real", "openai", "responses", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx realFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

// responsesReply is what the generated content of each accepted fixture
// was: the simulator is told to say the same (x-wwz-response,
// x-wwz-tool-calls), so everything else must match byte for byte.
var responsesReply = map[string]map[string]string{
	"default_store":                   {"x-wwz-response-base64": b64("Hello there!")},
	"previous_response_id":            {"x-wwz-response-base64": b64("Hello there!")},
	"text_stream":                     {"x-wwz-response-base64": b64("Hello there!")},
	"strict_default_no_additional":    {"x-wwz-response-base64": b64(`It looks like your message was just "x." How can I assist you`)},
	"incomplete_stream":               {"x-wwz-response-base64": b64("Sure! Here you go:\n\n1 2 3 4 5 ")},
	"function_call_stream":            {"x-wwz-tool-calls": `[{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}]`},
	"no_obfuscation_stream":           {"x-wwz-response-base64": b64("Hello there!")},
	"strict_default_open_schema":      {"x-wwz-response": "Ok.", "x-wwz-tool-prob": "0"},
	"strict_default_partial_required": {"x-wwz-response": "Ok.", "x-wwz-tool-prob": "0"},
	"echo_fields":                     {"x-wwz-tool-calls": `[{"name":"get_weather","arguments":"{}"}]`},
	"input_items":                     {"x-wwz-response": "The weather in Paris is sunny!", "x-wwz-tool-prob": "0"},
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestRealFixturesResponses replays every recorded Responses request
// (testdata/real/openai/responses/, task 231) through the server and
// compares status, Content-Type and the body byte for byte, ids and times
// normalized (ADR 0014, ADR 0055). Chained fixtures first record the
// response they point to, as the real calls did.
func TestRealFixturesResponses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "openai", "responses", "*.json"))
	if err != nil || len(files) < 16 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		fx := loadResponsesFixture(t, name)
		if fx.Request.Method == "" {
			continue // steps, not one call: TestResponsesPromptCacheFixture, TestResponsesChainFixtures
		}
		t.Run(name, func(t *testing.T) {
			h := start(t, responsesCfg, nil)
			hdr := map[string]string{"authorization": "Bearer wwz-key-01"}
			for k, v := range responsesReply[name] {
				hdr[k] = v
			}
			body, want := fx.Request.Body, deref(fx.Response.Body)
			// The id the fixture chains to, recorded here first.
			if id := respIDOnly.FindString(body); id != "" && name != "previous_response_not_found" {
				first := loadResponsesFixture(t, "default_store").Request.Body
				if name == "previous_response_unstored" {
					first = strings.Replace(first, `"max_output_tokens": 16`, `"max_output_tokens": 16, "store": false`, 1)
				}
				r := h.do("POST", "/v1/responses", map[string]string{"authorization": "Bearer wwz-key-01", "x-wwz-response": "Hello there!"}, first)
				var got struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(r.Body(), &got); err != nil || got.ID == "" {
					t.Fatalf("first call: %d %s", r.StatusCode(), r.Body())
				}
				body = strings.ReplaceAll(body, id, got.ID)
				want = strings.ReplaceAll(want, id, got.ID)
			}
			resp := h.do(fx.Request.Method, fx.Request.Path, hdr, body)
			got := normalizeResponses(string(resp.Body()))
			if resp.StatusCode() != fx.Response.Status {
				t.Fatalf("status %d, real %d\n wwz  %s\n real %s", resp.StatusCode(), fx.Response.Status, got, want)
			}
			if ct := string(resp.Header.Peek("Content-Type")); ct != fx.Response.ContentType {
				t.Errorf("content type %q, real %q", ct, fx.Response.ContentType)
			}
			want = normalizeResponses(want)
			if got != want {
				t.Errorf("body differs at %s", firstDiff(got, want))
			}
		})
	}
}

// firstDiff shows where two bodies part: the first differing line (or
// event) of each.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var a, b string
		if i < len(g) {
			a = g[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if a != b {
			j := 0
			for j < len(a) && j < len(b) && a[j] == b[j] {
				j++
			}
			from := max(0, j-60)
			return "line " + strconv.Itoa(i+1) + " col " + strconv.Itoa(j+1) + "\n wwz  …" + a[from:min(len(a), j+60)] + "\n real …" + b[from:min(len(b), j+60)]
		}
	}
	return "(equal lines)"
}
