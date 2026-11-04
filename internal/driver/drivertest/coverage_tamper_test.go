package drivertest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/generate"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
)

// The contract suite is itself code under test: a driver that breaks the
// contract must fail it, in the place that checks the broken rule. The
// helpers here wrap a real driver and break one thing at a time (a JSON
// member, a header, a status, a parsed field, a route, a limit policy field,
// an id); the tests in coverage_mutations_test.go run the suite against each
// broken driver and demand that it fails.

// mutation describes what the wrapped driver breaks. Every hook is optional.
type mutation struct {
	name string
	// json rewrites every JSON document the driver writes (a whole body, or
	// the data of one SSE frame); it returns the new document and whether it
	// changed anything.
	json func(node *jnode) bool
	// header rewrites one response header: the new value, or drop.
	header func(k, v string) (nv string, drop bool)
	// frame rewrites the i-th SSE frame the driver writes into the frames
	// that go out instead (none drops it).
	frame func(i int, p []byte) [][]byte
	// status rewrites the status code.
	status func(code int) int
	// req rewrites the request ParseRequest returns.
	req func(*core.Request)
	// routes rewrites Routes().
	routes func([]driver.Route) []driver.Route
	// auth rewrites Authenticate's credential.
	auth func(driver.Credential) driver.Credential
	// policy rewrites LimitPolicy.
	policy func(driver.LimitPolicy) driver.LimitPolicy
	// id rewrites the ids the IDMinter returns.
	id func(kind string, v string) string
	// hideIDs and hideNamer make the wrapper not implement the optional
	// interfaces.
	hideIDs, hideNamer bool
	// body rewrites every non-SSE write whole (a garbled document).
	body func(p []byte) []byte
	// addHeaders are written before anything else.
	addHeaders [][2]string
	// badLen writes a Content-Length one byte too long.
	badLen bool
	// parse rewrites the outcome of ParseRequest.
	parse func(k driver.Kind, req *core.Request, e *core.Error) (*core.Request, *core.Error)
	// failStream makes that StreamWriter method return an error.
	failStream string
	// failWrite makes WriteResponse return an error.
	failWrite bool
	// sniff rewrites Sniff.
	sniff func(bool) bool
	// modelName and unprefix rewrite the ModelNamer answers; forceNamer
	// gives the ModelNamer to a driver that has none.
	modelName  func(string) string
	unprefix   func(string, bool) (string, bool)
	forceNamer bool
	// afterGetModel is written after GetModel.
	afterGetModel string
	// afterExtra is written after Extra.
	afterExtra string
	// noop is a mutation that must not be able to fail (the control).
	// observe collects what the driver writes.
	observe *observation
}

// observation is what a driver writes during one suite run.
type observation struct {
	mu       sync.Mutex
	keys     map[string]bool
	headers  map[string]bool
	statuses map[int]bool
	values   map[string]bool // scalar values, as written
}

func newObservation() *observation {
	return &observation{keys: map[string]bool{}, headers: map[string]bool{}, statuses: map[int]bool{}, values: map[string]bool{}}
}

// --- ordered JSON ---

// jnode is a JSON value that keeps the order of its members.
type jnode struct {
	kind byte // 'o' object, 'a' array, 's' scalar
	keys []string
	kids []*jnode
	raw  []byte // scalar, as written
}

func parseJSON(b []byte) (*jnode, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := parseNode(dec)
	if err != nil || dec.More() {
		return nil, false
	}
	return n, true
}

func parseNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		if v == '{' {
			n := &jnode{kind: 'o'}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				kid, err := parseNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, kt.(string))
				n.kids = append(n.kids, kid)
			}
			_, err := dec.Token()
			return n, err
		}
		n := &jnode{kind: 'a'}
		for dec.More() {
			kid, err := parseNode(dec)
			if err != nil {
				return nil, err
			}
			n.kids = append(n.kids, kid)
		}
		_, err := dec.Token()
		return n, err
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if num, ok := tok.(json.Number); ok {
			buf.WriteString(num.String())
		} else if err := enc.Encode(tok); err != nil {
			return nil, err
		}
		return &jnode{kind: 's', raw: bytes.TrimSpace(buf.Bytes())}, nil
	}
}

func (n *jnode) encode(buf *bytes.Buffer) {
	switch n.kind {
	case 'o':
		buf.WriteByte('{')
		for i, k := range n.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			n.kids[i].encode(buf)
		}
		buf.WriteByte('}')
	case 'a':
		buf.WriteByte('[')
		for i, k := range n.kids {
			if i > 0 {
				buf.WriteByte(',')
			}
			k.encode(buf)
		}
		buf.WriteByte(']')
	default:
		buf.Write(n.raw)
	}
}

func (n *jnode) walk(f func(parent *jnode, i int, key string, kid *jnode)) {
	for i, k := range n.kids {
		key := ""
		if n.kind == 'o' {
			key = n.keys[i]
		}
		f(n, i, key, k)
		k.walk(f)
	}
}

// deleteKey removes every member called key.
func deleteKey(key string) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		var rec func(n *jnode)
		rec = func(n *jnode) {
			if n.kind == 'o' {
				for i := 0; i < len(n.keys); i++ {
					if n.keys[i] == key {
						n.keys = append(n.keys[:i], n.keys[i+1:]...)
						n.kids = append(n.kids[:i], n.kids[i+1:]...)
						i--
						changed = true
					}
				}
			}
			for _, k := range n.kids {
				rec(k)
			}
		}
		rec(root)
		return changed
	}
}

// replaceValue changes every scalar written as raw to another of its type.
func replaceValue(raw string) func(*jnode) bool {
	alt := `"wwz-other"`
	switch {
	case raw == "true":
		alt = "false"
	case raw == "false":
		alt = "true"
	case raw == "null":
		alt = `"wwz-other"`
	case raw[0] != '"':
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			alt = strconv.FormatFloat(f+1, 'f', -1, 64)
		}
	}
	return func(root *jnode) bool {
		changed := false
		root.walk(func(_ *jnode, _ int, _ string, kid *jnode) {
			if kid.kind == 's' && string(kid.raw) == raw {
				kid.raw = []byte(alt)
				changed = true
			}
		})
		return changed
	}
}

// --- the wrapper ---

type tamper struct {
	driver.Driver
	m *mutation
}

type tamperIDs struct{ *tamper }

type tamperFull struct{ *tamperIDs }

func wrap(d driver.Driver, m *mutation) driver.Driver {
	t := &tamper{Driver: d, m: m}
	_, ids := d.(driver.IDMinter)
	_, namer := d.(driver.ModelNamer)
	switch {
	case m.forceNamer && !namer && ids && !m.hideIDs:
		return tamperForced{&tamperIDs{t}}
	case ids && !m.hideIDs && namer && !m.hideNamer:
		return tamperFull{&tamperIDs{t}}
	case ids && !m.hideIDs:
		return &tamperIDs{t}
	}
	return t
}

// Unwrap exposes the driver's optional interfaces the suite finds through
// driver.As (BillingDecider, CatalogRouter, PublicRouter): what they
// decide reaches the bytes only through WriteError, which the tamper
// still breaks.
func (t *tamper) Unwrap() driver.Driver { return t.Driver }

func (t *tamper) Routes() []driver.Route {
	rs := t.Driver.Routes()
	if t.m.routes != nil {
		return t.m.routes(append([]driver.Route(nil), rs...))
	}
	return rs
}

func (t *tamper) Authenticate(h driver.Headers) driver.Credential {
	c := t.Driver.Authenticate(h)
	if t.m.auth != nil {
		return t.m.auth(c)
	}
	return c
}

func (t *tamper) Sniff(h driver.Headers) bool {
	ok := t.Driver.Sniff(h)
	if t.m.sniff != nil {
		return t.m.sniff(ok)
	}
	return ok
}

func (t *tamper) LimitPolicy() driver.LimitPolicy {
	p := t.Driver.LimitPolicy()
	if t.m.policy != nil {
		return t.m.policy(p)
	}
	return p
}

func (t *tamper) ParseRequest(k driver.Kind, body []byte, h driver.Headers) (*core.Request, *core.Error) {
	req, e := t.Driver.ParseRequest(k, body, h)
	if t.m.parse != nil {
		return t.m.parse(k, req, e)
	}
	if req != nil && t.m.req != nil {
		t.m.req(req)
	}
	return req, e
}

func (t *tamper) w(w core.ResponseWriter) core.ResponseWriter {
	return &tamperWriter{ResponseWriter: w, m: t.m}
}

func (t *tamper) WriteResponse(w core.ResponseWriter, req *core.Request, resp *core.Response, rl *core.RateLimitState) error {
	if t.m.failWrite {
		return errors.New("wwz: write failed")
	}
	return t.Driver.WriteResponse(t.w(w), req, resp, rl)
}

func (t *tamper) StreamWriter(w core.ResponseWriter, req *core.Request, rl *core.RateLimitState) core.StreamWriter {
	sw := t.Driver.StreamWriter(t.w(w), req, rl)
	if t.m.failStream != "" {
		return &failingStream{StreamWriter: sw, fail: t.m.failStream}
	}
	return sw
}

// failingStream returns an error from one method.
type failingStream struct {
	core.StreamWriter
	fail string
}

func (f *failingStream) err(m string) error {
	if f.fail == m {
		return errors.New("wwz: " + m + " failed")
	}
	return nil
}

func (f *failingStream) LastFrame() []byte {
	if f.fail == "LastFrame" {
		return nil
	}
	return f.StreamWriter.LastFrame()
}

func (f *failingStream) Start(r *core.Response) error {
	if err := f.err("Start"); err != nil {
		return err
	}
	return f.StreamWriter.Start(r)
}

func (f *failingStream) ReasoningDelta(s string) error {
	if err := f.err("ReasoningDelta"); err != nil {
		return err
	}
	return f.StreamWriter.ReasoningDelta(s)
}

func (f *failingStream) TextDelta(s string) error {
	if err := f.err("TextDelta"); err != nil {
		return err
	}
	return f.StreamWriter.TextDelta(s)
}

func (f *failingStream) ToolCallStart(i int, id, n string) error {
	if err := f.err("ToolCallStart"); err != nil {
		return err
	}
	return f.StreamWriter.ToolCallStart(i, id, n)
}

func (f *failingStream) ToolArgsDelta(i int, s string) error {
	if err := f.err("ToolArgsDelta"); err != nil {
		return err
	}
	return f.StreamWriter.ToolArgsDelta(i, s)
}

func (f *failingStream) ToolCallEnd(i int) error {
	if err := f.err("ToolCallEnd"); err != nil {
		return err
	}
	return f.StreamWriter.ToolCallEnd(i)
}

func (f *failingStream) Ping() error {
	if err := f.err("Ping"); err != nil {
		return err
	}
	return f.StreamWriter.Ping()
}

func (f *failingStream) Finish(r core.FinishReason, u core.Usage, inc bool) error {
	if err := f.err("Finish"); err != nil {
		return err
	}
	return f.StreamWriter.Finish(r, u, inc)
}

func (f *failingStream) Error(e *core.Error) error {
	if err := f.err("Error"); err != nil {
		return err
	}
	return f.StreamWriter.Error(e)
}

func (t *tamper) WriteError(w core.ResponseWriter, req *core.Request, e *core.Error, rl *core.RateLimitState) {
	t.Driver.WriteError(t.w(w), req, e, rl)
}

func (t *tamper) ListModels(w core.ResponseWriter, req *core.Request, models []driver.ModelInfo) {
	t.Driver.ListModels(t.w(w), req, models)
}

func (t *tamper) GetModel(w core.ResponseWriter, req *core.Request, m *driver.ModelInfo, id string) {
	t.Driver.GetModel(t.w(w), req, m, id)
	if t.m.afterGetModel != "" {
		_, _ = w.Write([]byte(t.m.afterGetModel))
	}
}

func (t *tamper) Extra(kind driver.Kind, w core.ResponseWriter, h driver.Headers, body []byte, ctx driver.ExtraContext) {
	t.Driver.Extra(kind, t.w(w), h, body, ctx)
	if t.m.afterExtra != "" {
		_, _ = w.Write([]byte(t.m.afterExtra))
	}
}

func (t *tamperIDs) minter() driver.IDMinter { return t.Driver.(driver.IDMinter) }

func (t *tamperIDs) RequestID(kind driver.Kind, unique string) string {
	return t.fix("request", t.minter().RequestID(kind, unique))
}

func (t *tamperIDs) ResponseID(kind driver.Kind, rng *generate.RNG, now time.Time) string {
	return t.fix("response", t.minter().ResponseID(kind, rng, now))
}

func (t *tamperIDs) ToolCallID(kind driver.Kind, rng *generate.RNG) string {
	return t.fix("tool", t.minter().ToolCallID(kind, rng))
}

func (t *tamperIDs) fix(kind, v string) string {
	if t.m.id != nil {
		return t.m.id(kind, v)
	}
	return v
}

func (t tamperFull) ModelName(c *config.ModelConfig) string {
	n := t.Driver.(driver.ModelNamer).ModelName(c)
	if t.m.modelName != nil {
		return t.m.modelName(n)
	}
	return n
}

func (t tamperFull) UnprefixModel(name string) (string, bool) {
	got, ok := t.Driver.(driver.ModelNamer).UnprefixModel(name)
	if t.m.unprefix != nil {
		return t.m.unprefix(got, ok)
	}
	return got, ok
}

// tamperForced gives a ModelNamer to a driver that has none.
type tamperForced struct{ *tamperIDs }

func (t tamperForced) ModelName(c *config.ModelConfig) string { return "x/" + c.ID }

func (t tamperForced) UnprefixModel(name string) (string, bool) { return name, false }

// tamperWriter applies the mutation to what the driver writes.
type tamperWriter struct {
	core.ResponseWriter
	m          *mutation
	contentLen bool
	frames     int
	injected   bool
}

func (w *tamperWriter) inject() {
	if w.injected {
		return
	}
	w.injected = true
	for _, h := range w.m.addHeaders {
		w.ResponseWriter.Header(h[0], h[1])
	}
}

func (w *tamperWriter) Status(code int) {
	w.inject()
	if w.m.observe != nil {
		w.m.observe.mu.Lock()
		w.m.observe.statuses[code] = true
		w.m.observe.mu.Unlock()
	}
	if w.m.status != nil {
		code = w.m.status(code)
	}
	w.ResponseWriter.Status(code)
}

func (w *tamperWriter) Header(k, v string) {
	w.inject()
	if w.m.badLen && strings.EqualFold(k, "Content-Length") {
		n, _ := strconv.Atoi(v)
		v = strconv.Itoa(n + 1)
	}
	if w.m.observe != nil {
		w.m.observe.mu.Lock()
		w.m.observe.headers[strings.ToLower(k)] = true
		w.m.observe.mu.Unlock()
	}
	if strings.EqualFold(k, "Content-Length") {
		w.contentLen = true
	}
	if w.m.header != nil {
		nv, drop := w.m.header(k, v)
		if drop {
			return
		}
		v = nv
	}
	w.ResponseWriter.Header(k, v)
}

func (w *tamperWriter) Write(p []byte) (int, error) {
	w.inject()
	if w.m.body != nil && !bytes.HasPrefix(p, []byte("data: ")) && !bytes.HasPrefix(p, []byte("event: ")) && !bytes.HasPrefix(p, []byte(":")) {
		p = w.m.body(p)
	}
	if w.m.frame != nil && (bytes.HasPrefix(p, []byte("data: ")) || bytes.HasPrefix(p, []byte("event: "))) {
		i := w.frames
		w.frames++
		for _, f := range w.m.frame(i, p) {
			if _, err := w.ResponseWriter.Write(f); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}
	out := w.rewrite(p)
	if w.contentLen && len(out) != len(p) && !w.m.badLen {
		w.ResponseWriter.Header("Content-Length", strconv.Itoa(len(out)))
	}
	if _, err := w.ResponseWriter.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// rewrite applies the JSON mutation to a whole JSON body or to the data
// lines of SSE frames.
func (w *tamperWriter) rewrite(p []byte) []byte {
	if w.m.json == nil && w.m.observe == nil {
		return p
	}
	if n, ok := parseJSON(p); ok && (p[0] == '{' || p[0] == '[') {
		return w.rewriteDoc(p, n)
	}
	lines := bytes.Split(p, []byte("\n"))
	changed := false
	for i, l := range lines {
		if !bytes.HasPrefix(l, []byte("data: ")) {
			continue
		}
		doc := l[len("data: "):]
		n, ok := parseJSON(doc)
		if !ok {
			continue
		}
		if out := w.rewriteDoc(doc, n); !bytes.Equal(out, doc) {
			lines[i] = append([]byte("data: "), out...)
			changed = true
		}
	}
	if !changed {
		return p
	}
	return bytes.Join(lines, []byte("\n"))
}

func (w *tamperWriter) rewriteDoc(doc []byte, n *jnode) []byte {
	if w.m.observe != nil {
		w.m.observe.mu.Lock()
		n.walk(func(_ *jnode, _ int, key string, kid *jnode) {
			if key != "" {
				w.m.observe.keys[key] = true
			}
			// Generated ids (id, responseId, tool_call_id, ...) vary per run and
			// may repeat when two runs share a second: never a mutation value.
			if kid.kind == 's' && len(kid.raw) <= 48 && (!strings.HasSuffix(strings.ToLower(key), "id") || strings.Contains(string(kid.raw), "/")) {
				w.m.observe.values[string(kid.raw)] = true
			}
		})
		w.m.observe.mu.Unlock()
	}
	if w.m.json == nil || !w.m.json(n) {
		return doc
	}
	var buf bytes.Buffer
	n.encode(&buf)
	return buf.Bytes()
}

// --- running the suite on a broken driver ---

// runSuite runs drivertest.Run on d in a test of its own and reports whether
// it passed. The failures are not the caller's: the nested run's output is
// discarded.
func runSuite(d driver.Driver, opts ...drivertest.Option) bool {
	return runNested(func(t *testing.T) { drivertest.Run(t, d, opts...) })
}

// runNested runs fn as a test of its own and reports whether it passed.
func runNested(fn func(t *testing.T)) bool {
	suiteMu.Lock()
	defer suiteMu.Unlock()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r); close(done) }()
	ok := testing.RunTests(func(string, string) (bool, error) { return true, nil }, []testing.InternalTest{{
		Name: "Broken", F: fn,
	}})
	_ = w.Close()
	<-done
	os.Stdout = old
	return ok
}

var suiteMu sync.Mutex

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// zeroField returns a req mutation that zeroes one field of core.Request.
func zeroField(name string) func(*core.Request) {
	return func(r *core.Request) {
		f := reflect.ValueOf(r).Elem().FieldByName(name)
		f.Set(reflect.Zero(f.Type()))
	}
}

func requestFields() []string {
	var out []string
	t := reflect.TypeOf(core.Request{})
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			out = append(out, t.Field(i).Name)
		}
	}
	return out
}

func policyFields() []string {
	var out []string
	t := reflect.TypeOf(driver.LimitPolicy{})
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	return out
}

func zeroPolicyField(name string) func(driver.LimitPolicy) driver.LimitPolicy {
	return func(p driver.LimitPolicy) driver.LimitPolicy {
		f := reflect.ValueOf(&p).Elem().FieldByName(name)
		f.Set(reflect.Zero(f.Type()))
		return p
	}
}
