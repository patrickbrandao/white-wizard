// Package drivertest is the contract suite every provider driver passes
// (spec 005, "Criar um driver novo"). A driver package gains the whole suite
// with one line in a _test.go file:
//
//	func TestContract(t *testing.T) { drivertest.Run(t, New()) }
//
// The built-in drivers register themselves in init() and have no exported
// constructor, so their contract tests build them from the registry:
//
//	func TestContract(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "openai")) }
//
// Run checks, without network and without the engine (bodies are fixed files
// under testdata/, responses go to a core.MemWriter):
//
//   - Routes: declared routes, methods and kinds; /models and /models/{id}
//     in the provider's format; auxiliary routes (key info, usage).
//   - Auth: missing and malformed credentials, the provider's credential
//     header(s), required headers (anthropic-version) and Sniff.
//   - Parse: messages, tools, tool_choice, max_tokens, stream, reasoning; bad
//     bodies yield core.ErrInvalidRequest rendered as 400 in the dialect.
//   - Response: non-streaming envelope, usage, finish_reason/stop_reason.
//   - Stream: full SSE sequence for text, tool calls (start, argument deltas,
//     end) and reasoning, terminator ([DONE] or message_stop), usage chunk and
//     mid-stream error event.
//   - Responses: on drivers with a Responses route, the request parsed
//     into messages and tools, the response object, the event stream and
//     errors on the route (responses.go, Profile.Responses).
//   - Errors: every core.ErrorKind through WriteError (status, envelope,
//     Content-Length), provider-specific cases from spec 006, exact
//     retry-after seconds and rate-limit headers (spec 010).
//   - RateLimitHeaders: x-ratelimit-* / anthropic-ratelimit-* (or none) on
//     success, stream start and error.
//   - IDs: driver.IDMinter formats (chatcmpl-, call_, req_, msg_01,
//     toolu_01, gen-), reproducible under a seed, unique per request token.
//   - LimitPolicy: entities and units declared for the provider.
//
// A driver whose routes are all driver.KindGenerate or KindResource speaks
// the Gemini dialect and runs the same groups with Gemini's checks instead
// (gemini.go). A driver that also has Chat, Completions, Messages or
// Responses routes (opencode) runs one suite per dialect, in the subtests
// "dialeto/openai" and "dialeto/gemini"; the Gemini one sees only the
// generate routes and takes Profile.Gemini, whose Vertex flag selects the
// checks of a gateway that answers in Vertex AI's form (vertex.go).
//
// Provider expectations come from a Profile. The five built-in drivers have
// one each, derived from specs 006 and 010 (see ProfileFor); Run picks it by
// Name(). A new driver passes WithProfile (usually a copy of ProfileFor of the
// dialect it imitates, edited); without a profile only dialect-neutral
// invariants run and the provider-specific checks are skipped with a log line.
//
// A known divergence between a driver and the spec is not hidden: the
// subtest is skipped with the reason (task id) through KnownBug or the
// profile's KnownBugs, and Run fails if a KnownBug names no subtest, so stale
// entries are noticed once the driver is fixed.
package drivertest

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

//go:embed testdata/*.json
var embedded embed.FS

// testdata is where the request bodies come from: the embedded files (a
// test swaps it to reach the errors on missing or broken ones).
var testdata fs.ReadFileFS = embedded

// Option tunes Run.
type Option func(*options)

type options struct {
	profile *Profile
	bugs    map[string]string
}

// WithProfile replaces the built-in profile picked by the driver's Name().
func WithProfile(p Profile) Option {
	return func(o *options) { cp := p; o.profile = &cp }
}

// KnownBug skips the subtest whose path relative to Run (segments joined by
// "/", e.g. "Errors/chat/RetryAfter/rate_limit") matches name, or any subtest
// below it, with reason in the skip message. Cite the task that fixes it.
func KnownBug(name, reason string) Option {
	return func(o *options) { o.bugs[name] = reason }
}

// MustNew builds a registered driver with no mount options.
func MustNew(t testing.TB, name string) driver.Driver {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type suite struct {
	d      driver.Driver
	p      Profile
	strict bool // a profile was found or given: provider checks run
	root   string
	bugs   map[string]string

	// dialect, when set, limits the routes the checks see to one dialect of
	// a driver that speaks several (see dialects).
	dialect func(driver.Kind) bool

	mu   sync.Mutex
	used map[string]bool
}

// routes returns the driver's routes of the dialect under test.
func (s *suite) routes() []driver.Route {
	rs := s.d.Routes()
	if s.dialect == nil {
		return rs
	}
	var out []driver.Route
	for _, r := range rs {
		if s.dialect(r.Kind) {
			out = append(out, r)
		}
	}
	return out
}

// sub returns a copy of s that sees only the routes accepted by dialect and
// the profile of the dialect (WithDialectProfile) in place of the driver's.
func (s *suite) sub(dialect func(driver.Kind) bool, p *Profile) *suite {
	c := &suite{d: s.d, p: s.p, strict: s.strict, root: s.root, bugs: s.bugs, dialect: dialect, used: s.used}
	if p != nil {
		c.p, c.strict = *p, true
		for k, v := range p.KnownBugs {
			if _, ok := c.bugs[k]; !ok {
				c.bugs[k] = v
			}
		}
	}
	return c
}

// isGenerate selects the Gemini routes; notGenerate everything else.
func isGenerate(k driver.Kind) bool { return k == driver.KindGenerate || k == driver.KindResource }

func notGenerate(k driver.Kind) bool { return !isGenerate(k) }

// Run executes the contract suite against d.
func Run(t *testing.T, d driver.Driver, opts ...Option) {
	t.Helper()
	o := options{bugs: map[string]string{}}
	for _, opt := range opts {
		opt(&o)
	}
	s := &suite{d: d, root: t.Name(), bugs: o.bugs, used: map[string]bool{}}
	switch {
	case o.profile != nil:
		s.p, s.strict = *o.profile, true
	default:
		if p, ok := ProfileFor(d.Name()); ok {
			s.p, s.strict = p, true
		}
	}
	for k, v := range s.p.KnownBugs {
		if _, ok := s.bugs[k]; !ok {
			s.bugs[k] = v
		}
	}
	if !s.strict {
		t.Logf("drivertest: no profile for driver %q; provider-specific checks are skipped (pass drivertest.WithProfile)", d.Name())
	}

	switch {
	case s.gemini() && len(s.sub(notGenerate, nil).surfaces()) > 0:
		// A driver with several dialects runs one suite per dialect
		// (task 283): the Chat/Messages/Responses checks on the routes of
		// those kinds, the Gemini checks on the generate routes.
		s.run(t, "dialeto", func(t *testing.T) {
			s.sub(notGenerate, nil).runDialect(t, "openai", s.sub(notGenerate, nil).runCommon)
			g := s.sub(isGenerate, s.p.Gemini)
			if g.p.Vertex {
				g.runDialect(t, "gemini", g.runVertex)
			} else {
				g.runDialect(t, "gemini", g.runGemini)
			}
		})
	case s.gemini():
		s.runGemini(t)
	default:
		s.runCommon(t)
	}

	var stale []string
	for name := range s.bugs {
		if !s.used[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("drivertest.KnownBug(%q): no such subtest (fixed or renamed? remove the entry)", name)
	}
}

// runDialect runs one dialect suite as the subtest name.
func (s *suite) runDialect(t *testing.T, name string, fn func(*testing.T)) {
	s.run(t, name, fn)
}

// runCommon is the Chat/Completions/Messages/Responses suite.
func (s *suite) runCommon(t *testing.T) {
	s.run(t, "Routes", s.testRoutes)
	s.run(t, "Models", s.testModels)
	s.run(t, "Auth", s.testAuth)
	s.run(t, "Parse", s.testParse)
	s.run(t, "Response", s.testResponse)
	s.run(t, "Stream", s.testStream)
	if s.hasKind(driver.KindResponses) {
		s.run(t, "Responses", s.testResponses)
	}
	s.run(t, "Errors", s.testErrors)
	s.run(t, "RateLimitHeaders", s.testRateLimitHeaders)
	s.run(t, "IDs", s.testIDs)
	s.run(t, "LimitPolicy", s.testLimitPolicy)
}

// run is t.Run plus the KnownBug lookup.
func (s *suite) run(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		rel := strings.TrimPrefix(t.Name(), s.root+"/")
		for bug, reason := range s.bugs {
			if rel == bug || strings.HasPrefix(rel, bug+"/") {
				s.mu.Lock()
				s.used[bug] = true
				s.mu.Unlock()
				t.Skipf("KnownBug: %s", reason)
			}
		}
		fn(t)
	})
}

// surfaces returns the inference kinds the driver routes, in a stable order.
func (s *suite) surfaces() []driver.Kind {
	seen := map[driver.Kind]bool{}
	for _, r := range s.routes() {
		switch r.Kind {
		case driver.KindChat, driver.KindCompletions, driver.KindMessages:
			seen[r.Kind] = true
		}
	}
	var out []driver.Kind
	for _, k := range []driver.Kind{driver.KindChat, driver.KindCompletions, driver.KindMessages} {
		if seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func (s *suite) hasKind(k driver.Kind) bool {
	for _, r := range s.routes() {
		if r.Kind == k {
			return true
		}
	}
	return false
}

// surface returns the profile surface for kind, or a neutral one.
func (s *suite) surface(k driver.Kind) Surface {
	if sf, ok := s.p.Surfaces[k]; ok {
		return sf
	}
	return Surface{}
}

func endpointOf(k driver.Kind) core.Endpoint {
	switch k {
	case driver.KindCompletions:
		return core.EndpointCompletions
	case driver.KindMessages:
		return core.EndpointMessages
	case driver.KindCountTokens:
		return core.EndpointCountTokens
	}
	return core.EndpointChat
}

// anthropicWire reports whether kind speaks the Messages wire format.
func anthropicWire(k driver.Kind) bool { return k == driver.KindMessages }

// --- testdata ---

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := testdata.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("testdata %s: %v", name, err)
	}
	return b
}

// variant returns one named body from a *_variants.json file.
func variant(t *testing.T, file, name string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(readTestdata(t, file), &m); err != nil {
		t.Fatalf("testdata %s: %v", file, err)
	}
	b, ok := m[name]
	if !ok {
		t.Fatalf("testdata %s: no variant %q", file, name)
	}
	return b
}

// invalidBodies returns the raw invalid bodies for a dialect key.
func invalidBodies(t *testing.T, key string) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(readTestdata(t, "invalid.json"), &m); err != nil {
		t.Fatalf("testdata invalid.json: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(m[key], &out); err != nil {
		t.Fatalf("testdata invalid.json[%s]: %v", key, err)
	}
	return out
}

// --- JSON and header helpers ---

func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, b)
	}
	return m
}

// path walks a decoded JSON value by dotted path; numeric segments index
// arrays. ok is false when a segment is missing.
func path(v any, p string) (any, bool) {
	if p == "" {
		return v, true
	}
	for _, seg := range strings.Split(p, ".") {
		switch cur := v.(type) {
		case map[string]any:
			nv, ok := cur[seg]
			if !ok {
				return nil, false
			}
			v = nv
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(cur) {
				return nil, false
			}
			v = cur[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func str(v any, p string) string {
	x, _ := path(v, p)
	s, _ := x.(string)
	return s
}

func num(v any, p string) (float64, bool) {
	x, ok := path(v, p)
	if !ok {
		return 0, false
	}
	if n, ok := x.(json.Number); ok {
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func arr(v any, p string) []any {
	x, _ := path(v, p)
	a, _ := x.([]any)
	return a
}

// equalJSON compares a decoded value with an expected Go value.
func equalJSON(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case int:
		f, ok := num(got, "")
		return ok && f == float64(w)
	case float64:
		f, ok := num(got, "")
		return ok && f == w
	case bool, string:
		return got == want
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	return bytes.Equal(gb, wb)
}

func expectPath(t *testing.T, body any, p string, want any) {
	t.Helper()
	got, ok := path(body, p)
	if !ok {
		t.Errorf("%s: missing, want %v", p, want)
		return
	}
	if !equalJSON(got, want) {
		t.Errorf("%s = %v (%T), want %v", p, got, got, want)
	}
}

func expectPresent(t *testing.T, body any, p string) {
	t.Helper()
	if _, ok := path(body, p); !ok {
		t.Errorf("%s: missing", p)
	}
}

func expectAbsent(t *testing.T, body any, p string) {
	t.Helper()
	if v, ok := path(body, p); ok {
		t.Errorf("%s = %v, want absent", p, v)
	}
}

// header looks a response header up case-insensitively, like HTTP does.
func header(h map[string]string, name string) (string, bool) {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func expectHeader(t *testing.T, h map[string]string, name, want string) {
	t.Helper()
	got, ok := header(h, name)
	switch {
	case !ok:
		t.Errorf("header %s missing (have %v)", name, sortedKeys(h))
	case want != "*" && got != want:
		t.Errorf("header %s = %q, want %q", name, got, want)
	}
}

func sortedKeys(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expectJSONResponse checks status, Content-Type and Content-Length and
// returns the decoded object.
func expectJSONResponse(t *testing.T, w *core.MemWriter, status int) map[string]any {
	t.Helper()
	if w.StatusCode() != status {
		t.Fatalf("status %d, want %d; body %s", w.StatusCode(), status, w.Body())
	}
	h := w.Headers()
	if ct, _ := header(h, "Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	checkContentLength(t, w)
	return decodeJSON(t, w.Body())
}

func checkContentLength(t *testing.T, w *core.MemWriter) {
	t.Helper()
	if cl, ok := header(w.Headers(), "Content-Length"); ok {
		if cl != strconv.Itoa(len(w.Body())) {
			t.Errorf("Content-Length %s, body has %d bytes", cl, len(w.Body()))
		}
	} else {
		t.Errorf("Content-Length missing")
	}
}

func jsonEq(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// noErr fails the test when a driver call that must work returns an error.
func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
