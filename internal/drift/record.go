package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultRecordDir is where record writes the fixtures (ADR 0014): one
// file per probe in <dir>/<provider>/shape/<probe>.json (opencode-google:
// <dir>/opencode/google/<probe>.json), the format of spec 018 "Fixtures
// reais" read by the TestRealFixtures* tests.
const DefaultRecordDir = "testdata/real"

// modelsFullMax is the largest model list kept whole; above it the list is
// reduced to the probe item, as SignModels compares it (the OpenRouter
// listing has hundreds of models and some megabytes).
const modelsFullMax = 64 << 10

// Markers of the normalized fixtures (ADR 0014, spec 018).
const (
	markKey     = "<key>"
	markSession = "<session>"
)

// Fixture is one recorded real answer (spec 018, "Fixtures reais").
type Fixture struct {
	Provider   string          `json:"provider"`
	Name       string          `json:"name"`
	RecordedAt string          `json:"recorded_at"`
	Note       string          `json:"note"`
	Request    FixtureRequest  `json:"request"`
	Response   FixtureResponse `json:"response"`
}

// FixtureRequest is the request sent, credentials as markers.
type FixtureRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	// Body is the exact body sent; null for a GET.
	Body *string `json:"body"`
}

// FixtureResponse is the normalized answer.
type FixtureResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// Record runs the probes of check against the real providers only and
// writes each answer, normalized, as a fixture under dir (default
// DefaultRecordDir). The gates of check apply: the provider must be
// selected, have a key, and the real base must be confirmed (Options.Real)
// unless BaseURLs gives one; the budget is read before each provider and
// each paid probe adds its cost. Options that only concern the comparison
// (WWZURL, WWZKey, Ignores, SaveRaw) are ignored. The result is a Report of
// Kind RecordKind with the file of each probe; ExitCode maps it.
func Record(ctx context.Context, opts Options, dir string) (*Report, error) {
	opts.defaults()
	opts.SaveRaw = ""
	if dir == "" {
		dir = DefaultRecordDir
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	secrets := make([]string, 0, len(opts.Keys))
	for _, k := range opts.Keys {
		secrets = append(secrets, k)
	}
	r := &runner{o: opts, red: NewRedactor(secrets...), session: opts.Session}
	rep := &Report{
		Version: ReportVersion, Kind: RecordKind, ToolVersion: opts.ToolVersion,
		Started: opts.Now().UTC().Truncate(time.Second), red: r.red,
		Providers: []ProviderReport{}, Dir: dir,
	}
	var err error
	for _, name := range ProviderNames {
		var pr ProviderReport
		pr, err = r.record(ctx, name, dir)
		rep.Providers = append(rep.Providers, pr)
		if err != nil {
			break
		}
	}
	rep.Finished = opts.Now().UTC().Truncate(time.Second)
	rep.summarize()
	return rep, err
}

// record runs the selected probes of one provider on the real side.
func (r *runner) record(ctx context.Context, name, dir string) (ProviderReport, error) {
	p, key, gate, err := r.target(name)
	if gate != nil {
		return *gate, err
	}
	pr := ProviderReport{Provider: name, Base: p.BaseURL, Model: probeModel(r.o.Models, name), Probes: []ProbeReport{}}
	if p.Session {
		pr.Session = r.session
	}
	for _, probe := range ProbeNames {
		if !slices.Contains(r.o.Probes, probe) {
			pr.Probes = append(pr.Probes, ProbeReport{Probe: probe, Status: StatusSkipped, Reason: ReasonNotSelected, Divergences: []Divergence{}})
			continue
		}
		if responsesProbe(probe) && p.ResponsesPath == "" {
			pr.Probes = append(pr.Probes, ProbeReport{Probe: probe, Status: StatusSkipped, Reason: ReasonNotSupported, Divergences: []Divergence{}})
			continue
		}
		res, cost := r.recordProbe(ctx, p, probe, key, pr.Model, dir)
		pr.Probes = append(pr.Probes, res)
		if cost > 0 {
			pr.SpendUSD += cost
			if _, err := AddSpend(r.o.BudgetFile, name, cost, r.o.Now()); err != nil {
				pr.Status = rollup(pr.Probes)
				return pr, err
			}
		}
	}
	pr.Status = rollup(pr.Probes)
	return pr, nil
}

// recordProbe sends one probe to the real provider and writes its fixture.
func (r *runner) recordProbe(ctx context.Context, p Provider, probe, key, model, dir string) (res ProbeReport, cost float64) {
	res = ProbeReport{Probe: probe, Divergences: []Divergence{}}
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	sent := key
	if probe == ProbeErrorAuth {
		sent = InvalidKey
	}
	rq := p.probeRequest(probe, model)
	a, err := r.do(ctx, p.BaseURL, p, rq, sent)
	if err != nil {
		res.Status, res.Reason = StatusError, "real: "+err.Error()
		return res, 0
	}
	res.RealStatus = a.status
	if paid(probe) && a.status/100 == 2 {
		in, out, ok := usageOf(a.body, probe == ProbeChatStream || probe == ProbeResponsesStream)
		cost = CostUSD(model, in, out, ok)
	}
	fx := r.fixture(p, probe, model, rq, a)
	path := fixturePath(dir, p, probe)
	if err := writeFixture(path, fx, key, r.session); err != nil {
		res.Status, res.Reason = StatusError, "record: "+r.red.String(err.Error())
		return res, cost
	}
	res.Status, res.File = StatusOK, path
	return res, cost
}

// fixturePath is <dir>/<provider>/shape/<probe>.json; the Gemini route of
// OpenCode lives with the gateway's other Gemini fixtures.
func fixturePath(dir string, p Provider, probe string) string {
	if p.Name == ProviderOpenCodeGoogle {
		return filepath.Join(dir, ProviderOpenCode, "google", probe+".json")
	}
	return filepath.Join(dir, p.Name, "shape", probe+".json")
}

// fixture builds the normalized fixture of one answer.
func (r *runner) fixture(p Provider, probe, model string, rq request, a answer) Fixture {
	note := "sonda " + probe + " do wwz-drift (wwz-drift record)"
	body := a.body
	if probe == ProbeModels && len(body) > modelsFullMax {
		if focused, n, ok := reduceModels(body, model); ok {
			note += fmt.Sprintf("; lista reduzida ao item da sonda (o real lista %d)", n)
			body = focused
		}
	}
	stream := ContentType(a.header) == "text/event-stream"
	fx := Fixture{
		Provider: p.DriverName(), Name: probe, RecordedAt: r.o.Now().UTC().Format(time.DateOnly), Note: note,
		Request: FixtureRequest{Method: rq.method, Path: basePath(p.BaseURL) + rq.path, Headers: requestHeaders(p, probe)},
		Response: FixtureResponse{
			Status:  a.status,
			Headers: responseHeaders(p, a.header, len(a.body) == 0),
			Body:    normalizeBody(p, probe, stream, body),
		},
	}
	if rq.body != nil {
		s := string(rq.body)
		fx.Request.Body = &s
	}
	return fx
}

// basePath is the path of a base URL ("/api" of https://openrouter.ai/api).
func basePath(base string) string {
	u, _ := url.Parse(base) // validated (real bases are constants)
	return strings.TrimRight(u.Path, "/")
}

// requestHeaders are the headers sent, lower case, credentials as
// markers: <key> (the error_auth probe keeps its literal invalid key) and
// <session>. Accept, User-Agent and Content-Type are not recorded.
func requestHeaders(p Provider, probe string) map[string]string {
	key := markKey
	if probe == ProbeErrorAuth {
		key = InvalidKey
	}
	out := map[string]string{}
	for k, v := range p.headers(key, markSession, "") {
		if k = strings.ToLower(k); k != "accept" && k != "user-agent" {
			out[k] = strings.Join(v, ", ")
		}
	}
	return out
}

// geminiHeaders are the Google front-end headers the Gemini fixtures keep.
var geminiHeaders = []string{
	"server", "server-timing", "content-disposition", "www-authenticate",
	"x-content-type-options", "x-frame-options", "x-xss-protection", "x-gemini-service-tier",
}

// keepHeader reports whether a response header (lower case) is recorded:
// the relevant headers of the comparison, content-type, cache-control,
// CORS, the Google front-end ones on the Gemini dialect and content-length
// of an empty body. Infrastructure (cf-*, date, set-cookie, via, alt-svc,
// strict-transport-security, x-openai-proxy-wasm…) never is.
func keepHeader(p Provider, k string, emptyBody bool) bool {
	switch {
	case relevantHeader(k), k == "content-type", k == "cache-control", strings.HasPrefix(k, "access-control-"):
		return true
	case k == "content-length":
		return emptyBody
	}
	return p.Dialect == dialectGemini && slices.Contains(geminiHeaders, k)
}

var (
	reGenID       = regexp.MustCompile(`^gen-[0-9]+-[A-Za-z0-9]+$`)
	reServerDur   = regexp.MustCompile(`dur=[0-9.]+`)
	headerMarkers = map[string]string{
		"request-id":                "<request_id>",
		"x-request-id":              "<request_id>",
		"x-generation-id":           "<generation_id>",
		"x-opencode-log-id":         "<log_id>",
		"anthropic-organization-id": "<organization_id>",
		"openai-organization":       "<organization_id>",
		"anthropic-workspace-id":    "<workspace_id>",
		"openai-project":            "<project_id>",
		"openai-processing-ms":      "<ms>",
	}
)

// headerValue normalizes the value of a recorded header: ids, accounts,
// times and reset dates become markers; the rest is kept.
func headerValue(k, v string) string {
	switch {
	case reGenID.MatchString(v): // OpenRouter: request-id is the generation id
		return "<generation_id>"
	case headerMarkers[k] != "":
		return headerMarkers[k]
	case strings.HasPrefix(k, "anthropic-ratelimit-") && strings.HasSuffix(k, "-reset"):
		return "<rfc3339>"
	case strings.HasPrefix(k, "x-ratelimit-reset-"):
		return "<duration>"
	case k == "server-timing":
		return reServerDur.ReplaceAllString(v, "dur=<ms>")
	}
	return v
}

// responseHeaders are the recorded response headers, lower case, values
// normalized.
func responseHeaders(p Provider, h http.Header, emptyBody bool) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if k = strings.ToLower(k); keepHeader(p, k, emptyBody) {
			out[k] = headerValue(k, strings.Join(v, ", "))
		}
	}
	return out
}

// bodyRule replaces the value of a JSON string field by a marker.
type bodyRule struct {
	re     *regexp.Regexp
	marker string
}

// field matches "<one of keys>": "<value>" with the JSON spacing of any
// provider (compact or indented).
func field(keys, value, marker string) bodyRule {
	return bodyRule{regexp.MustCompile(`("(?:` + keys + `)"\s*:\s*")(?:` + value + `)(")`), marker}
}

func (b bodyRule) apply(s string) string {
	return b.re.ReplaceAllString(s, "${1}"+b.marker+"${2}")
}

var (
	// commonRules apply to every answer, the models list included.
	commonRules = []bodyRule{
		field("request_id", `req_[A-Za-z0-9]+`, "<request_id>"),
		field("user_id", `user_[A-Za-z0-9]+`, "<user_id>"),
		field("system_fingerprint", `fp_[0-9a-f]+`, "<fp>"),
		field("signature|thoughtSignature", `[^"]+`, "<signature>"),
		field("responseId", `[^"]+`, "<response_id>"),
		field("createTime|updateTime|expireTime", `[^"]+`, "<time>"),
	}
	// idRules apply outside the models list, whose ids are model ids.
	idRules = map[string][]bodyRule{
		ProviderAnthropic: {field("id", `msg_[A-Za-z0-9_]+`, "<msg_id>")},
		ProviderOpenCode:  {field("id", `gw-[^"]*|chatcmpl-[^"]*|[0-9a-f]{32}`, "<gw_id>")},
		"": {
			field("id|call_id", `call_[A-Za-z0-9_-]+`, "<call_id>"),
			field("id", `toolu_[A-Za-z0-9_]+`, "<tool_id>"),
			field("id|item_id", `(?:msg|rs|fc)_[0-9a-f]{16,}`, "<item_id>"),
			field("id", `gen-[0-9]+-[A-Za-z0-9]+`, "<generation_id>"),
			field("id", `chatcmpl-[A-Za-z0-9-]+|resp_[0-9a-f]+`, "<id>"),
		},
	}
	// reSSEPad is the Anthropic SSE padding (spaces before the closing
	// brace of a data line), removed as the fixtures of task 173 do.
	reSSEPad = regexp.MustCompile(` +\}\n`)
)

// normalizeBody turns the volatile values of a body into markers (ADR
// 0014). Generated content and token counts are kept: the driver tests
// compare them against the same configuration.
func normalizeBody(p Provider, probe string, stream bool, body []byte) string {
	s := string(body)
	rules := commonRules
	if probe != ProbeModels {
		rules = append(append(slices.Clone(idRules[p.Name]), idRules[""]...), rules...)
	}
	for _, rule := range rules {
		s = rule.apply(s)
	}
	if stream && p.Dialect == dialectAnthropic {
		s = reSSEPad.ReplaceAllString(s, "}\n")
	}
	return s
}

var reTotalCount = regexp.MustCompile(`"total_count":\s*[0-9]+`)

// reduceModels keeps a model list with only the probe item (focusModel)
// and total_count set to the items kept; n is the size of the real list.
func reduceModels(body []byte, model string) (out []byte, n int, ok bool) {
	focused, found, ok := focusModel(body, model)
	if !ok {
		return nil, 0, false
	}
	var top struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"` // Gemini
	}
	_ = json.Unmarshal(body, &top) // valid JSON with a list (focusModel)
	n = max(len(top.Data), len(top.Models))
	kept := 0
	if found {
		kept = 1
	}
	return reTotalCount.ReplaceAll(focused, []byte(`"total_count":`+strconv.Itoa(kept))), n, true
}

// writeFixture writes the fixture as indented JSON (two spaces, markers
// unescaped, final newline), with any leftover key or session replaced by
// its marker: no secret reaches a fixture.
func writeFixture(path string, fx Fixture, key, session string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(fx) // plain structs: cannot fail
	data := b.Bytes()
	if key != "" {
		data = bytes.ReplaceAll(data, []byte(key), []byte(markKey))
	}
	if session != "" {
		data = bytes.ReplaceAll(data, []byte(session), []byte(markSession))
	}
	return writeAtomic(path, data)
}
