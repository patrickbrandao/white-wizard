package gemini

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
)

// shapeFixture is a real answer recorded on 2026-10-31 (task 164) in
// testdata/real/gemini/shape/, normalized as ADR 0014 says: responseId,
// thoughtSignature, call ids, cache names, timestamps and Server-Timing are
// markers.
type shapeFixture struct {
	Name    string `json:"name"`
	Request struct {
		Method  string            `json:"method"`
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

const shapeDir = "../../../testdata/real/gemini/shape"

func loadShape(t *testing.T, name string) shapeFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(shapeDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx shapeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

var (
	reRespID = regexp.MustCompile(`"responseId": "[A-Za-z0-9_-]+"`)
	reSig    = regexp.MustCompile(`"thoughtSignature": "[A-Za-z0-9+/=]+"`)
	reCallID = regexp.MustCompile(`"id": "call_[0-9]+"`)
	reCache  = regexp.MustCompile(`cachedContents/[0-9a-z]{40}`)
	reTime   = regexp.MustCompile(`"(createTime|updateTime|expireTime)": "[^"]+"`)
	reTiming = regexp.MustCompile(`^gfet4t7; dur=[0-9]+$`)
)

// normalize brings a wwz body to the fixture's markers.
func normalize(b string) string {
	b = reRespID.ReplaceAllString(b, `"responseId": "<response_id>"`)
	b = reSig.ReplaceAllString(b, `"thoughtSignature": "<signature>"`)
	b = reCallID.ReplaceAllString(b, `"id": "<call_id>"`)
	b = reCache.ReplaceAllString(b, `cachedContents/<cache_id>`)
	return reTime.ReplaceAllString(b, `"$1": "<time>"`)
}

// relevant are the headers compared with the fixture's (Vary, Date,
// Alt-Svc and the transfer headers are the front end's, not the dialect's).
var relevant = map[string]bool{
	"content-type": true, "content-disposition": true, "x-gemini-service-tier": true, "server": true,
	"x-xss-protection": true, "x-frame-options": true, "x-content-type-options": true,
	"www-authenticate": true, "server-timing": true,
}

// checkHeaders compares the relevant headers with the fixture's.
func checkHeaders(t *testing.T, fx shapeFixture, got map[string]string) {
	t.Helper()
	low := map[string]string{}
	for k, v := range got {
		if k = strings.ToLower(k); relevant[k] {
			low[k] = v
		}
	}
	for k, want := range fx.Response.Headers {
		v, ok := low[k]
		switch {
		case !ok:
			t.Errorf("%s: header %s missing (real %q)", fx.Name, k, want)
		case k == "server-timing":
			if !reTiming.MatchString(v) {
				t.Errorf("%s: server-timing %q", fx.Name, v)
			}
		case v != want:
			t.Errorf("%s: %s = %q, real %q", fx.Name, k, v, want)
		}
	}
	for k := range low {
		if _, ok := fx.Response.Headers[k]; !ok {
			t.Errorf("%s: header %s not in the real answer", fx.Name, k)
		}
	}
}

// --- driver-level replays of the generated answers ---

type realPart struct {
	Text         *string         `json:"text"`
	Thought      bool            `json:"thought"`
	FunctionCall *realCall       `json:"functionCall"`
	Signature    string          `json:"thoughtSignature"`
	Extra        json.RawMessage `json:"-"`
}

type realCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id"`
}

type realUsage struct {
	Prompt     int `json:"promptTokenCount"`
	Candidates int `json:"candidatesTokenCount"`
	Cached     int `json:"cachedContentTokenCount"`
	Thoughts   int `json:"thoughtsTokenCount"`
}

func (u realUsage) core() core.Usage {
	return core.Usage{PromptTokens: u.Prompt, CompletionTokens: u.Candidates + u.Thoughts, ReasoningTokens: u.Thoughts, CachedTokens: u.Cached}
}

type realResp struct {
	Candidates []struct {
		Content struct {
			Parts []realPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Usage        realUsage `json:"usageMetadata"`
	ModelVersion string    `json:"modelVersion"`
}

var finishOf = map[string]core.FinishReason{"STOP": core.FinishStop, "MAX_TOKENS": core.FinishLength}

const (
	respID = "Zv7latSxD4aiuMYP487m6A8"
	callID = "call_441933"
)

// reqFor builds the engine request of a fixture through ParseRequest.
func reqFor(t *testing.T, fx shapeFixture) *core.Request {
	t.Helper()
	path, query, _ := strings.Cut(fx.Request.Path, "?")
	h := driver.MapHeaders{"x-wwz-path": path, driver.QueryHeader: query, "x-goog-api-key": "k"}
	req, e := (&Driver{}).ParseRequest(driver.KindGenerate, []byte(fx.Request.Body), h)
	if e != nil {
		t.Fatalf("%s: parse: %v", fx.Name, e)
	}
	req.Headers = h
	req.Received = time.Now()
	return req
}

// TestRealFixturesGenerate: the generateContent body is the real one byte
// for byte, with the real headers.
func TestRealFixturesGenerate(t *testing.T) {
	for _, name := range []string{"chat", "chat_querykey", "system_instruction", "max_tokens", "max_tokens_empty", "thoughts", "thoughts_hidden", "tool_use", "candidates_one"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			var m realResp
			if err := json.Unmarshal([]byte(fx.Response.Body), &m); err != nil {
				t.Fatal(err)
			}
			c := m.Candidates[0]
			resp := &core.Response{ID: respID, Model: m.ModelVersion, FinishReason: finishOf[c.FinishReason], Usage: m.Usage.core()}
			for _, p := range c.Content.Parts {
				switch {
				case p.FunctionCall != nil:
					resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{ID: callID, Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args)})
					resp.FinishReason = core.FinishToolCalls
				case p.Thought:
					resp.Reasoning = *p.Text
				case p.Text != nil:
					resp.Content = *p.Text
				}
			}
			if resp.Reasoning == "" && resp.Usage.ReasoningTokens > 0 {
				resp.Reasoning = "hidden thoughts"
			}
			w := core.NewMemWriter()
			if err := (&Driver{}).WriteResponse(w, reqFor(t, fx), resp, nil); err != nil {
				t.Fatal(err)
			}
			if got, want := normalize(string(w.Body())), fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %q\n real %q", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// TestRealFixturesStreams replays each recorded stream through the stream
// writer, chunk by chunk, with the real token counts.
func TestRealFixturesStreams(t *testing.T) {
	for _, name := range []string{"chat_stream", "chat_stream_array", "max_tokens_stream", "thoughts_stream", "tool_use_stream"} {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			chunks := splitStream(t, fx.Response.Body)
			last := chunks[len(chunks)-1]
			resp := &core.Response{ID: respID, Model: last.ModelVersion, Usage: last.Usage.core()}
			req := reqFor(t, fx)
			sw := (&Driver{}).StreamWriter(core.NewMemWriter(), req, nil).(*stream)
			w := core.NewMemWriter()
			sw.w = w
			// The real token count of each delta, in order.
			var counts []int
			prevText, prevThought := 0, 0
			for _, c := range chunks {
				for _, p := range c.Candidates[0].Content.Parts {
					switch {
					case p.Thought:
						counts = append(counts, c.Usage.Thoughts-prevThought)
					case p.FunctionCall != nil:
						counts = append(counts, c.Usage.Candidates-prevText, 0)
					case p.Text != nil && *p.Text != "":
						counts = append(counts, c.Usage.Candidates-prevText)
					}
				}
				prevText, prevThought = c.Usage.Candidates, c.Usage.Thoughts
			}
			sw.count = func(string) int {
				n := counts[0]
				counts = counts[1:]
				return n
			}
			if err := sw.Start(resp); err != nil {
				t.Fatal(err)
			}
			for i, c := range chunks {
				cand := c.Candidates[0]
				for _, p := range cand.Content.Parts {
					switch {
					case p.Thought:
						_ = sw.ReasoningDelta(*p.Text)
					case p.FunctionCall != nil:
						_ = sw.ToolCallStart(0, callID, p.FunctionCall.Name)
						_ = sw.ToolArgsDelta(0, string(p.FunctionCall.Args))
						_ = sw.ToolCallEnd(0)
						resp.FinishReason = core.FinishToolCalls
					case p.Text != nil && *p.Text != "":
						_ = sw.TextDelta(*p.Text)
					}
				}
				if i == len(chunks)-1 {
					reason := finishOf[cand.FinishReason]
					if resp.FinishReason == core.FinishToolCalls {
						reason = core.FinishToolCalls
					}
					_ = sw.Finish(reason, resp.Usage, true)
				}
			}
			_ = sw.Close()
			if got, want := normalize(string(w.Body())), fx.Response.Body; got != want {
				t.Errorf("body\n wwz  %q\n real %q", got, want)
			}
			checkHeaders(t, fx, w.Headers())
		})
	}
}

// splitStream decodes the chunks of an SSE or JSON-array stream.
func splitStream(t *testing.T, body string) []realResp {
	t.Helper()
	var out []realResp
	if strings.HasPrefix(body, "[") {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, ev := range strings.Split(strings.TrimSuffix(body, "\r\n\r\n"), "\r\n\r\n") {
		var r realResp
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &r); err != nil {
			t.Fatalf("%v: %q", err, ev)
		}
		out = append(out, r)
	}
	return out
}

// --- full-stack replays: router, engine and driver ---

const testKey = "wwz-gemini-key"

// testConfig mounts the driver at the root with the models of the
// recordings, their /models fields as the real lists them.
const testConfig = `
server: {listen: "127.0.0.1:0", auth: keys, seed: 1}
drivers:
  - {name: gemini}
keys:
  - {key: wwz-gemini-key, label: t, account: acme, project: p1}
models:
  - id: gemini-2.5-flash
    version: "001"
    display_name: Gemini 2.5 Flash
    protocols: [gemini]
    context_window: 1048576
    max_output: 65536
    extra: {description: "Stable version of Gemini 2.5 Flash, our mid-size multimodal model that supports up to 1 million tokens, released in June of 2025."}
    latency: fast
    errors: none
  - id: gemini-2.5-pro
    version: "2.5"
    display_name: Gemini 2.5 Pro
    protocols: [gemini]
    context_window: 1048576
    max_output: 65536
    extra: {description: "Stable release (June 17th, 2025) of Gemini 2.5 Pro"}
    latency: fast
    errors: none
  - id: gemini-3.5-flash-lite
    version: 3.5-flash-lite-07-2026
    display_name: Gemini 3.5 Flash Lite
    protocols: [gemini]
    context_window: 1048576
    max_output: 65536
    latency: fast
    errors: none
    generation:
      output_tokens: {dist: uniform, min: 1, max: 2}
  - id: gemini-2.5-flash-lite
    protocols: [gemini]
    latency: fast
    errors: none
`

// startWWZ serves cfg in this process and returns its base URL.
func startWWZ(t *testing.T, cfg string) string {
	t.Helper()
	return startWWZStore(t, cfg, limits.NewMemory())
}

// startWWZStore serves cfg on store.
func startWWZStore(t *testing.T, cfg string, store limits.Store) string {
	t.Helper()
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(c.Metrics.MaxSeries), c.Metrics.Buckets)
	eng := engine.New(c, store, scenario.NewRegistry(), m, log)
	srv, err := server.New(eng, server.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return "http://" + ln.Addr().String()
}

// call sends a fixture's request to wwz with the test key in place of the
// real one.
func call(t *testing.T, base, method, path string, headers map[string]string, body string) (int, map[string]string, string) {
	t.Helper()
	path = strings.ReplaceAll(path, "<key>", testKey)
	var rd io.Reader
	if body != "" || method == "POST" || method == "PATCH" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, strings.ReplaceAll(v, "<key>", testKey))
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]string{}
	for k, v := range resp.Header {
		h[strings.ToLower(k)] = strings.Join(v, ",")
	}
	return resp.StatusCode, h, string(b)
}

var reTokenCount = regexp.MustCompile(`("totalTokenCount": |"totalTokens": |"tokenCount": |total_token_count=)[0-9]+`)

// TestRealFixturesWire replays the errors, the check order, the models
// routes and cachedContents against a wwz serving the driver: status,
// headers and body byte for byte. The cache's token count is the
// tokenizer's (spec 007: the Gemini tokenizer is "não conferido"), so it is
// compared by form.
func TestRealFixturesWire(t *testing.T) {
	base := startWWZ(t, testConfig)
	names := []string{
		"model_one", "model_unknown", "model_one_prefixed", "models_page", "models_badkey", "models_nokey", "models_pagetoken_bad",
		"error_auth", "error_auth_stream", "error_nokey", "error_bearer", "error_model", "error_model_stream", "error_model_array",
		"count_tokens_model", "count_tokens_badkey", "count_tokens_empty",
		"error_field", "error_field_two", "error_field_nested", "error_field_html", "error_field_stream",
		"error_maxtok_type", "error_contents_type", "error_json", "error_empty", "error_nobody",
		"error_role", "error_role_bad", "error_part_empty", "error_temp",
		"error_method", "error_route", "error_get_generate",
		"order_key_field", "order_key_model", "order_nokey_field", "order_model_field", "order_key_empty",
		"order_nokey_empty", "order_key_json", "order_key_method", "order_bearer_field", "order_model_empty",
		"cached_list", "cached_unknown", "cached_small", "chat_cached_unknown", "chat_cached_sys",
		// task 265: unknown routes before the key; candidateCount.
		"route_badkey", "route_nokey", "route_bearer", "route_post_badkey", "route_method_badkey",
		"route_cached_deep_badkey", "route_cached_put_badkey", "route_models_delete_badkey", "route_get_generate_badkey",
		"candidates_multi", "candidates_zero", "candidates_nine", "candidates_eight", "candidates_temp",
		"candidates_badkey", "candidates_multi_badkey", "candidates_stream", "candidates_stream_array",
		"candidates_stream_model_turn", "candidates_stream_nine", "candidates_stream_badkey", "candidates_model_turn",
		"candidates_role_bad", "candidates_part_empty", "candidates_count_range", "candidates_count",
		// task 235: the model before the backend's checks.
		"candidates_model_unknown",
		// task 387: countTokens with contents and with generateContentRequest.
		"count_tokens", "count_tokens_gcr",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fx := loadShape(t, name)
			status, h, body := call(t, base, fx.Request.Method, fx.Request.Path, fx.Request.Headers, fx.Request.Body)
			if status != fx.Response.Status {
				t.Errorf("status %d, real %d; body %s", status, fx.Response.Status, body)
			}
			got, want := normalize(body), fx.Response.Body
			if name == "cached_small" || name == "candidates_count" || strings.HasPrefix(name, "count_tokens") {
				got, want = reTokenCount.ReplaceAllString(got, "$1<n>"), reTokenCount.ReplaceAllString(want, "$1<n>")
			}
			if got != want {
				t.Errorf("body\n wwz  %q\n real %q", got, want)
			}
			checkHeaders(t, fx, h)
		})
	}
}

// TestRealFixturesModelsProbe: the models probe of wwz-drift record
// (shape/models.json, task 387). The real lists every model of the
// account; the probe model's item and the frame around the list are
// compared byte for byte.
func TestRealFixturesModelsProbe(t *testing.T) {
	base := startWWZ(t, testConfig)
	fx := loadShape(t, "models")
	status, h, body := call(t, base, fx.Request.Method, fx.Request.Path, fx.Request.Headers, fx.Request.Body)
	if status != fx.Response.Status {
		t.Fatalf("status %d, real %d; body %s", status, fx.Response.Status, body)
	}
	item := func(b string) string {
		start := strings.Index(b, "    {\n      \"name\": \"models/gemini-3.5-flash-lite\",")
		if start < 0 {
			return ""
		}
		return b[start : strings.Index(b[start:], "\n    }")+start+len("\n    }")]
	}
	if got, want := item(body), item(fx.Response.Body); want == "" || got != want {
		t.Errorf("gemini-3.5-flash-lite\n wwz  %q\n real %q", got, want)
	}
	const head = "{\n  \"models\": [\n    {\n"
	for _, b := range []string{body, fx.Response.Body} {
		if !strings.HasPrefix(b, head) || !strings.HasSuffix(b, "\n    }\n  ]\n}\n") || strings.Contains(b, "nextPageToken") {
			t.Errorf("listing frame %.40q … %q", b, b[max(0, len(b)-20):])
		}
	}
	checkHeaders(t, fx, h)
}

// TestRealFixturesCacheLifecycle replays create, get, list, generate,
// count, patch and delete of one cachedContent.
func TestRealFixturesCacheLifecycle(t *testing.T) {
	base := startWWZ(t, testConfig)
	create := loadShape(t, "cached_create")
	status, _, body := call(t, base, "POST", create.Request.Path, create.Request.Headers, create.Request.Body)
	if status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
	name := reCache.FindString(body)
	if name == "" {
		t.Fatalf("create: no name in %s", body)
	}
	for _, step := range []string{"cached_create", "cached_get", "cached_list_one", "count_cached", "chat_cached", "chat_cached_stream", "cached_patch", "cached_delete"} {
		fx := loadShape(t, step)
		path := strings.ReplaceAll(fx.Request.Path, "cachedContents/<cache_id>", name)
		reqBody := strings.ReplaceAll(fx.Request.Body, "cachedContents/<cache_id>", name)
		got := body
		gotStatus := status
		var h map[string]string
		if step != "cached_create" {
			gotStatus, h, got = call(t, base, fx.Request.Method, path, fx.Request.Headers, reqBody)
		}
		if gotStatus != fx.Response.Status {
			t.Errorf("%s: status %d, real %d; %s", step, gotStatus, fx.Response.Status, got)
			continue
		}
		if h != nil {
			checkHeaders(t, fx, h)
		}
		got, want := normalize(got), fx.Response.Body
		if step == "chat_cached_stream" {
			got, want = lastEvent(got), lastEvent(want)
		}
		gotShape, wantShape := shapeOf(t, got), shapeOf(t, want)
		if gotShape != wantShape {
			t.Errorf("%s: shape\n wwz  %s\n real %s", step, gotShape, wantShape)
		}
	}
}

// shapeOf is the body with every number replaced, for answers whose counts
// come from the tokenizer and the generator; each SSE event and each array
// element is kept apart.
func shapeOf(t *testing.T, body string) string {
	t.Helper()
	return regexp.MustCompile(`(": |=)[0-9]+`).ReplaceAllString(stripText(body), "$1<n>")
}

// stripText masks the generated text.
func stripText(b string) string {
	return regexp.MustCompile(`"text": "(\\.|[^"\\])*"`).ReplaceAllString(b, `"text": "<text>"`)
}

// lastEvent is the last SSE event of a stream.
func lastEvent(b string) string {
	evs := strings.Split(strings.TrimSuffix(b, "\r\n\r\n"), "\r\n\r\n")
	return evs[len(evs)-1]
}
