package gemini

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// Task 265: cachedContents in the shared store, candidateCount, unknown
// routes before the credential and the Gemini token count.

// brokenCC is a store whose cachedContents calls all fail.
type brokenCC struct{ *limits.Memory }

var errStore = errors.New("store down")

func (brokenCC) CachedContentPut(context.Context, limits.CachedContent) error { return errStore }
func (brokenCC) CachedContentGet(context.Context, string, string, time.Time) (*limits.CachedContent, error) {
	return nil, errStore
}

func (brokenCC) CachedContentList(context.Context, string, time.Time) ([]limits.CachedContent, error) {
	return nil, errStore
}
func (brokenCC) CachedContentDelete(context.Context, string, string) error { return errStore }

// shareStore makes s the shared store for the test.
func shareStore(t *testing.T, s limits.Store) {
	t.Helper()
	limits.ShareCachedContents(s)
	t.Cleanup(func() { limits.ShareCachedContents(limits.NewMemory()) })
}

// TestCacheStoreErrors: a write the store refuses is a 500, a read it
// fails is the 403 of a missing entry.
func TestCacheStoreErrors(t *testing.T) {
	d := &Driver{}
	ms := unitModels()
	mem := limits.NewMemory()
	shareStore(t, mem)
	w := resourceCall(d, "POST", "/v1beta/cachedContents", "", `{"model": "m-small", "contents": [{"parts": [{"text": "a b c d e f g"}]}]}`, ms)
	name, _ := decode(t, w.Body())["name"].(string)
	if w.StatusCode() != 200 || name == "" {
		t.Fatalf("create: %d %s", w.StatusCode(), w.Body())
	}
	limits.ShareCachedContents(brokenCC{mem})
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1beta/cachedContents", `{"model": "m-small", "contents": [{"parts": [{"text": "a b c d e f g"}]}]}`, 500},
		{"GET", "/v1beta/cachedContents", "", 500},
		{"GET", "/v1beta/" + name, "", 403},
		{"DELETE", "/v1beta/" + name, "", 403},
	} {
		if w := resourceCall(d, c.method, c.path, "", c.body, ms); w.StatusCode() != c.status {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.StatusCode(), w.Body())
		}
	}
	// Reads work, writes fail: patch and delete are 500.
	limits.ShareCachedContents(readOnlyCC{mem})
	for _, method := range []string{"PATCH", "DELETE"} {
		if w := resourceCall(d, method, "/v1beta/"+name, "", `{}`, ms); w.StatusCode() != 500 {
			t.Errorf("%s with a failing write: %d %s", method, w.StatusCode(), w.Body())
		}
	}
	// A body the store mangled is an empty cache.
	limits.ShareCachedContents(mem)
	e, _ := mem.CachedContentGet(context.Background(), "p:unit/cache", name, time.Now())
	e.Body = []byte("{")
	_ = mem.CachedContentPut(context.Background(), *e)
	if c := getCache("p:unit/cache", name, time.Now()); c == nil || c.system != "" || len(c.messages) != 0 {
		t.Errorf("mangled body: %+v", c)
	}
}

// readOnlyCC reads from the memory store and fails every write.
type readOnlyCC struct{ *limits.Memory }

func (readOnlyCC) CachedContentPut(context.Context, limits.CachedContent) error { return errStore }
func (readOnlyCC) CachedContentDelete(context.Context, string, string) error    { return errStore }

// TestCachedContentsAcrossInstances: two wwz behind a balancer on the same
// store see the same entries (the store, not the process, holds them).
func TestCachedContentsAcrossInstances(t *testing.T) {
	store := limits.NewMemory()
	a, b := startWWZStore(t, testConfig, store), startWWZStore(t, testConfig, store)
	create := loadShape(t, "cached_create")
	status, _, body := call(t, a, "POST", create.Request.Path, create.Request.Headers, create.Request.Body)
	name := reCache.FindString(body)
	if status != 200 || name == "" {
		t.Fatalf("create on a: %d %s", status, body)
	}
	if c, _ := store.CachedContentGet(context.Background(), "p:acme/p1", name, time.Now()); c == nil || c.Tokens == 0 {
		t.Fatalf("not in the store: %+v", c)
	}
	h := map[string]string{"x-goog-api-key": "<key>"}
	if status, _, body := call(t, b, "GET", "/v1beta/"+name, h, ""); status != 200 || !strings.Contains(body, name) {
		t.Errorf("get on b: %d %s", status, body)
	}
	if status, _, _ := call(t, b, "DELETE", "/v1beta/"+name, h, ""); status != 200 {
		t.Errorf("delete on b: %d", status)
	}
	if status, _, _ := call(t, a, "GET", "/v1beta/"+name, h, ""); status != 403 {
		t.Errorf("get on a after delete: %d", status)
	}
}

func TestServed(t *testing.T) {
	for _, c := range []struct {
		path, method string
		want         bool
	}{
		{"/v2/models", "GET", false},
		{"/v1beta/models", "GET", true},
		{"/v1/models", "POST", false},
		{"/v1beta/models/m", "GET", true},
		{"/v1beta/models/m:generateContent", "GET", false},
		{"/v1beta/models/a/b", "GET", false},
		{"/v1beta/models/m:countTokens", "POST", true},
		{"/v1beta/models/m:foo", "POST", false},
		{"/v1beta/models/:generateContent", "POST", false},
		{"/v1beta/models/m", "DELETE", false},
		{"/v1beta/cachedContents", "POST", true},
		{"/v1beta/cachedContents", "PUT", false},
		{"/v1beta/cachedContents/x", "PATCH", true},
		{"/v1beta/cachedContents/x", "POST", false},
		{"/v1beta/cachedContents/x/y", "GET", false},
		{"/v1beta/foo", "GET", false},
	} {
		if got := served(routeOf(c.path), c.method); got != c.want {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	// Without the method header (a direct call) nothing is overridden.
	if unservedCredential(&core.Request{Headers: driver.MapHeaders{"x-wwz-path": "/v1beta/foo"}}, &core.Error{Kind: core.ErrInvalidAPIKey}) {
		t.Error("no method: overridden")
	}
	h := driver.MapHeaders{"x-wwz-path": "/v1beta/foo", driver.MethodHeader: "GET"}
	if unservedCredential(&core.Request{Headers: h}, &core.Error{Kind: core.ErrRateLimit}) {
		t.Error("a rate limit is not a credential error")
	}
	if !unservedCredential(&core.Request{Headers: h}, &core.Error{Kind: core.ErrAccountSuspended}) {
		t.Error("a suspended key on an unknown route")
	}
}

func TestTokenizerOf(t *testing.T) {
	for _, c := range []struct {
		m    config.ModelConfig
		want generate.Tokenizer
	}{
		{config.ModelConfig{ID: "gemini-x"}, gtok.Tokenizer{}},
		{config.ModelConfig{ID: "gemini-x", Tokenizer: config.TokenizerSimulated}, generate.Simulated{}},
		{config.ModelConfig{ID: "gemini-x", Tokenizer: config.TokenizerO200k}, tokenize.O200k},
		{config.ModelConfig{ID: "gemini-x", Tokenizer: config.TokenizerCl100k}, tokenize.Cl100k},
	} {
		if got := tokenizerOf(&c.m); got != c.want {
			t.Errorf("%q: %T", c.m.Tokenizer, got)
		}
	}
}

// TestCandidateChecks: the order of the candidateCount checks (fixtures
// candidates_*): the range with the front end's lines, the stream's own
// refusal, then the backend's after roles and parts.
func TestCandidateChecks(t *testing.T) {
	d := &Driver{}
	parse := func(method, body string) *core.Error {
		h := driver.MapHeaders{"x-goog-api-key": "k", "x-wwz-path": "/v1beta/models/m:" + method}
		_, e := d.ParseRequest(driver.KindGenerate, []byte(body), h)
		return e
	}
	for _, c := range []struct {
		method, body, msg string
		backend           bool
	}{
		{"generateContent", `{"contents": [{"parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 2}}`, "Multiple candidates is not enabled for this model", true},
		{"generateContent", `{"contents": [{"parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": "8"}}`, "Multiple candidates is not enabled for this model", true},
		{"streamGenerateContent", `{"contents": [{"role": "model", "parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 2}}`, "Only one candidate can be specified in the current model", false},
		{"generateContent", `{"contents": [{"role": "bot", "parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 2}}`, "Role 'bot' is not supported. Please use a valid role: MODEL, USER.", true},
		{
			"countTokens", `{"generateContentRequest": {"contents": [{"parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 0, "temperature": 3}}}`,
			"* CountTokensRequest.generate_content_request.generation_config.candidate_count: candidate_count must be in the range [1, 8].\n* CountTokensRequest.generate_content_request.generation_config.temperature: temperature must be in the range [0.0, 2.0].\n", false,
		},
	} {
		e := parse(c.method, c.body)
		if e == nil || e.Message != c.msg || (e.Extra[extraBackend] == true) != c.backend {
			t.Errorf("%s %s: %+v", c.method, c.body, e)
		}
	}
	if e := parse("countTokens", `{"generateContentRequest": {"contents": [{"parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 2}}}`); e != nil {
		t.Errorf("countTokens ignores candidateCount: %v", e)
	}
	if e := parse("generateContent", `{"contents": [{"parts": [{"text": "a"}]}], "generationConfig": {"candidateCount": 1}}`); e != nil {
		t.Errorf("one candidate: %v", e)
	}
}

// TestRealCountWire: countTokens through the engine counts with the Gemini
// template and tokenizer: the probes whose texts are one-token words come
// out exact (function declarations within 2).
func TestRealCountWire(t *testing.T) {
	base := startWWZ(t, testConfig+`
  - id: gemini-3.1-flash-lite
    protocols: [gemini]
  - id: gemini-3.8-flash
    protocols: [gemini]
`)
	exact := map[string]bool{
		"u:a": true, "u:ok": true, "u:hello": true, "uu:a,a": true, "um:a,a": true, "umu:a,a,a": true, "sys:a+u:a": true, "gcr:u:a": true,
		"base": true, "x": true, "xx": true, "x_x": true, "x_desc": true, "x_desc2": true, "x_params_empty": true, "x_params_p": true,
		"x_params_p_req": true, "x_params_pq": true, "x_params_p_desc": true, "x_params_p_enum": true, "x_jsonschema_p": true,
		"call_noargs": true, "call2": true, "call_text": true, "resp_empty": true, "resp_str": true, "mod": true,
		"img": true, "img2": true, "wav1s": true, "json_mode": true, "json_schema": true, "emptytext": true,
		"img_gemini-3.8-flash": true, "img_gemini-3.1-flash-lite": true,
	}
	h := map[string]string{"x-goog-api-key": "<key>"}
	n := 0
	for _, p := range loadCount(t, "template").Probes {
		if !exact[p.ID] {
			continue
		}
		n++
		status, _, body := call(t, base, "POST", "/v1beta/models/"+p.Model+":countTokens", h, p.Body)
		want := int(p.TotalTokens.(float64))
		got := countOf(t, body)
		tol := 0
		if strings.HasPrefix(p.ID, "x") {
			tol = 2
		}
		if status != 200 || got < want-tol || got > want+tol {
			t.Errorf("%s: %d %d tokens, real %d", p.ID, status, got, want)
		}
	}
	if n < 30 {
		t.Fatalf("only %d probes", n)
	}
	// The drift probe through generateContent: promptTokenCount is the
	// countTokens total (no reply priming).
	fx := loadShape(t, "chat")
	_, _, gen := call(t, base, "POST", fx.Request.Path, fx.Request.Headers, fx.Request.Body)
	_, _, cnt := call(t, base, "POST", strings.Replace(fx.Request.Path, "generateContent", "countTokens", 1), h, `{"contents":[{"role":"user","parts":[{"text":"Responda só: ok"}]}]}`)
	if !strings.Contains(gen, `"promptTokenCount": `+strconv.Itoa(countOf(t, cnt))+",") {
		t.Errorf("generate %s vs count %s", gen, cnt)
	}
}

func countOf(t *testing.T, body string) int {
	t.Helper()
	m := decode(t, []byte(body))
	f, ok := m["totalTokens"].(float64)
	if !ok {
		t.Fatalf("no totalTokens in %s", body)
	}
	return int(f)
}
