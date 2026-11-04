package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

func errMessage(t *testing.T, w *core.MemWriter) string {
	t.Helper()
	e, _ := decode(t, w.Body())["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func TestTranscode(t *testing.T) {
	cases := []struct {
		name, body string
		syntax     string // prefix of the syntax message
		viol       []string
	}{
		{"empty", "  ", "", nil},
		{"root_array", `[]`, `Invalid JSON payload received. Unknown name "": Root element must be a message.`, nil},
		{"trailing", `{} x`, "Invalid JSON payload received. Unexpected token.", nil},
		{"bad_token", `{"contents" 1}`, "Invalid JSON payload received. Unexpected token.\n{\"contents\" 1}\n", nil},
		{"eof_object", `{"model": "x"`, "Invalid JSON payload received. Unexpected end of string. Expected , or } after key:value pair.", nil},
		{"eof_root", `{`, "Invalid JSON payload received. Unexpected end of string.", nil},
		{"nulls", `{"model": null, "contents": null, "labels": null}`, "", nil},
		{"scalar_for_list", `{"generationConfig": {"stopSequences": "x", "responseModalities": [1]}}`, "", []string{
			"Invalid value at 'generation_config.response_modalities[0]' (TYPE_STRING), 1",
		}},
		{"container_for_scalar", `{"generationConfig": {"stopSequences": [{"a": [1]}]}}`, "", []string{
			"Invalid value at 'generation_config.stop_sequences[0]' (TYPE_STRING), {}",
		}},
		{"array_for_message", `{"generationConfig": [1]}`, "", []string{
			"Invalid value at 'generation_config' (type.googleapis.com/google.ai.generativelanguage.v1beta.GenerationConfig), []",
		}},
		{"scalar_for_message", `{"generationConfig": true}`, "", []string{
			"Invalid value at 'generation_config' (type.googleapis.com/google.ai.generativelanguage.v1beta.GenerationConfig), true",
		}},
		{"numbers", `{"generationConfig": {"temperature": "0.5", "topK": "x", "topP": false, "candidateCount": 1.5, "responseLogprobs": "no"}}`, "", []string{
			"Invalid value at 'generation_config.top_k' (TYPE_INT32), \"x\"",
			"Invalid value at 'generation_config.top_p' (TYPE_FLOAT), false",
			"Invalid value at 'generation_config.candidate_count' (TYPE_INT32), 1.5",
			"Invalid value at 'generation_config.response_logprobs' (TYPE_BOOL), \"no\"",
		}},
		{"nested_unknown", `{"contents": [{"parts": [{"txt": 1}]}], "tools": [{"mcpServers": [{"a": 1}]}]}`, "", []string{
			"Invalid JSON payload received. Unknown name \"txt\" at 'contents[0].parts[0]': Cannot find field.",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, serr, vs := transcode([]byte(c.body), msgGenerate)
			switch {
			case c.syntax != "" && (serr == nil || !strings.HasPrefix(serr.msg, c.syntax)):
				t.Errorf("syntax = %+v, want prefix %q", serr, c.syntax)
			case c.syntax == "" && serr != nil:
				t.Errorf("unexpected syntax error %q", serr.msg)
			}
			var got []string
			for _, v := range vs {
				got = append(got, v.desc)
			}
			if strings.Join(got, "|") != strings.Join(c.viol, "|") {
				t.Errorf("violations\n got  %q\n want %q", got, c.viol)
			}
		})
	}
	// Cut a body with every kind of value at each byte: the transcoder
	// answers a syntax error, never panics nor accepts a prefix.
	full := `{"contents": [{"parts": [{"text": "a", "foo": [1, {"b": 2}]}]}], "labels": {"k": "v"}, "mcp": 1,
		"tools": [{"mcpServers": [{"a": 1}]}], "generationConfig": {"stopSequences": ["x", {"y": [1]}], "topK": 3, "thinkingConfig": {}}}`
	for i := 1; i < len(full); i++ {
		if _, serr, _ := transcode([]byte(full[:i]), msgGenerate); serr == nil && strings.TrimSpace(full[:i]) != "" {
			t.Errorf("prefix %q accepted", full[:i])
		}
	}
	for _, bad := range []string{`}`, `{"model": "x"]`, `{"contents": [{"role": "user"}}`} {
		if _, serr, _ := transcode([]byte(bad), msgGenerate); serr == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if raw(nil) != "null" || raw(true) != "true" {
		t.Error("raw")
	}
}

func TestProtoTime(t *testing.T) {
	base := time.Date(2026, 10, 31, 14, 46, 8, 0, time.UTC)
	for ns, want := range map[int]string{
		0: "2026-10-31T14:46:08Z", 23e6: "2026-10-31T14:46:08.023Z",
		23182e3: "2026-10-31T14:46:08.023182Z", 249554743: "2026-10-31T14:46:08.249554743Z",
	} {
		if got := protoTime(base.Add(time.Duration(ns))); got != want {
			t.Errorf("%d: %s, want %s", ns, got, want)
		}
	}
}

func TestTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, s := range []string{"300", "xs", "-1s"} {
		if _, ok := parseTTL(s); ok {
			t.Errorf("parseTTL(%q) accepted", s)
		}
	}
	if d, ok := parseTTL("1.5s"); !ok || d != 1500*time.Millisecond {
		t.Errorf("1.5s = %v", d)
	}
	if _, ok, e := expiry(map[string]any{"ttl": "bad"}, now); ok || e == nil {
		t.Error("bad ttl accepted")
	}
	if exp, ok, e := expiry(map[string]any{"expireTime": "2026-10-31T15:00:00Z"}, now); !ok || e != nil || exp.Year() != 2026 {
		t.Errorf("expireTime: %v %v %v", exp, ok, e)
	}
	if _, ok, e := expiry(map[string]any{"expireTime": "x"}, now); ok || e == nil {
		t.Error("bad expireTime accepted")
	}
	if _, ok, e := expiry(map[string]any{}, now); ok || e != nil {
		t.Error("neither must be ok=false")
	}
}

func TestCacheOwnerAndExpiry(t *testing.T) {
	h := driver.MapHeaders{}
	driver.SetAccount(h, driver.Account{Key: "k", Account: "a", Project: "p"})
	if cacheOwner(&core.Request{Headers: h}) != "p:a/p" {
		t.Error("project owner")
	}
	driver.SetAccount(h, driver.Account{Key: "k", Account: "a"})
	if cacheOwner(&core.Request{Headers: h}) != "a:a" {
		t.Error("account owner")
	}
	if cacheOwner(nil) != "k:" {
		t.Error("key owner")
	}
	now := time.Now()
	_ = putCache("o-expiry", &cachedContent{name: "cachedContents/a", expire: now.Add(-time.Second)})
	_ = putCache("o-expiry", &cachedContent{name: "cachedContents/b", expire: now.Add(-time.Second)})
	if getCache("o-expiry", "cachedContents/a", now) != nil || getCache("o-expiry", "nope", now) != nil {
		t.Error("expired entry returned")
	}
	if l, err := listCaches("o-expiry", now); len(l) != 0 || err != nil {
		t.Error("expired entry listed")
	}
}

// resourceCall runs one KindResource request through Extra.
// unitAccount scopes the cachedContents in the shared store; a test that
// lists them takes a fresh one (isolateAccount) so repeated runs
// (-count>1) do not see each other's entries.
var unitAccount = "unit"

var accountSeq atomic.Int64

func isolateAccount(t *testing.T) {
	t.Helper()
	prev := unitAccount
	unitAccount = fmt.Sprintf("unit-%s-%d", t.Name(), accountSeq.Add(1))
	t.Cleanup(func() { unitAccount = prev })
}

func resourceCall(d *Driver, method, path, query, body string, models []driver.ModelInfo) *core.MemWriter {
	h := driver.MapHeaders{"x-goog-api-key": "k", "x-wwz-path": path, driver.QueryHeader: query, driver.MethodHeader: method}
	driver.SetAccount(h, driver.Account{Key: "k", Account: unitAccount, Project: "cache"})
	w := core.NewMemWriter()
	d.Extra(driver.KindResource, w, h, []byte(body), driver.ExtraContext{Models: models})
	return w
}

func unitModels() []driver.ModelInfo {
	minTok := 5
	return []driver.ModelInfo{
		{Cfg: &config.ModelConfig{ID: "m-1", Aliases: []string{"alias-1"}}},
		{Cfg: &config.ModelConfig{ID: "m-small", PromptCache: &config.PromptCacheRule{MinTokens: &minTok}}},
	}
}

func TestCacheRoutes(t *testing.T) {
	isolateAccount(t)
	d := &Driver{}
	ms := unitModels()
	long := strings.Repeat("word ", 1200)
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		msg                      string
	}{
		{"bad_body", "POST", "/v1beta/cachedContents", `{"foo": 1}`, 400, `Invalid JSON payload received. Unknown name "foo": Cannot find field.`},
		{"syntax", "POST", "/v1beta/cachedContents", `{`, 400, ""},
		{"no_model", "POST", "/v1beta/cachedContents", `{}`, 400, "* CreateCachedContentRequest.cached_content.model: model is not specified\n"},
		{"unknown_model", "POST", "/v1/cachedContents", `{"model": "models/x"}`, 404, "models/x is not found for API version v1, or is not supported for createCachedContent. Call ModelService.ListModels to see the list of available models and their supported methods."},
		{"bad_ttl", "POST", "/v1beta/cachedContents", `{"model": "m-small", "contents": [{"parts": [{"text": "a b c d e f g"}]}], "ttl": "x"}`, 400, ""},
		{"too_small", "POST", "/v1beta/cachedContents", `{"model": "alias-1", "contents": [{"parts": [{"text": "hi"}]}]}`, 400, ""},
		{"put", "PUT", "/v1beta/cachedContents/x", ``, 404, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := resourceCall(d, c.method, c.path, "", c.body, ms)
			if w.StatusCode() != c.status {
				t.Fatalf("status %d, want %d: %s", w.StatusCode(), c.status, w.Body())
			}
			if c.msg != "" && errMessage(t, w) != c.msg {
				t.Errorf("message %q", errMessage(t, w))
			}
		})
	}
	// A small model with tools and a system instruction, the default TTL.
	w := resourceCall(d, "POST", "/v1beta/cachedContents", "", `{"model": "m-small", "systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [{"parts": [{"text": "a b c d e f g"}]}], "tools": [{"functionDeclarations": [{"name": "f"}]}]}`, ms)
	created := decode(t, w.Body())
	name, _ := created["name"].(string)
	if w.StatusCode() != 200 || name == "" || created["displayName"] != nil {
		t.Fatalf("create: %d %s", w.StatusCode(), w.Body())
	}
	w = resourceCall(d, "POST", "/v1beta/cachedContents", "", `{"model": "models/m-1", "contents": [{"parts": [{"text": "`+long+`"}]}], "expireTime": "2099-01-01T00:00:00Z"}`, ms)
	if w.StatusCode() != 200 {
		t.Fatalf("create long: %d %s", w.StatusCode(), w.Body())
	}
	list := decode(t, resourceCall(d, "GET", "/v1beta/cachedContents", "", "", ms).Body())
	if items, _ := list["cachedContents"].([]any); len(items) != 2 {
		t.Errorf("list: %v", list)
	}
	for _, c := range []struct{ body string }{{`{"foo": 1}`}, {`{"ttl": "x"}`}} {
		if w := resourceCall(d, "PATCH", "/v1beta/"+name, "", c.body, ms); w.StatusCode() != 400 {
			t.Errorf("patch %s: %d", c.body, w.StatusCode())
		}
	}
	if w := resourceCall(d, "PATCH", "/v1beta/"+name, "updateMask=expireTime", `{"expireTime": "2099-01-01T00:00:00Z"}`, ms); !strings.Contains(string(w.Body()), `"expireTime": "2099-01-01T00:00:00Z"`) {
		t.Errorf("patch expireTime: %s", w.Body())
	}
	if w := resourceCall(d, "PATCH", "/v1beta/"+name, "", `{}`, ms); w.StatusCode() != 200 {
		t.Errorf("patch without change: %d", w.StatusCode())
	}
	// A generateContent that reads it: the cache's contents come first.
	h := driver.MapHeaders{"x-goog-api-key": "k", "x-wwz-path": "/v1beta/models/m-small:generateContent"}
	driver.SetAccount(h, driver.Account{Key: "k", Account: unitAccount, Project: "cache"})
	req, e := d.ParseRequest(driver.KindGenerate, []byte(`{"contents": [{"parts": [{"text": "q"}]}], "cachedContent": "`+name+`"}`), h)
	if e != nil || req.System != "be brief" || len(req.Messages) != 2 || len(req.Tools) != 1 {
		t.Fatalf("cached generate: %+v %v", req, e)
	}
	if cachedTokens(req, core.Usage{CachedTokens: 99999}) == 99999 {
		t.Error("the explicit cache's count wins over the implicit one")
	}
	if w := resourceCall(d, "DELETE", "/v1beta/"+name, "", "", ms); string(w.Body()) != "{}\n" {
		t.Errorf("delete: %q", w.Body())
	}
}

func TestModelsRoutes(t *testing.T) {
	d := &Driver{}
	no := false
	ms := []driver.ModelInfo{
		{Cfg: &config.ModelConfig{ID: "a", Capabilities: config.Capabilities{Reasoning: &no}, Extra: map[string]any{"description": "D", "baseModelId": "a"}}},
		{Cfg: &config.ModelConfig{ID: "b"}},
	}
	body := decode(t, resourceCall(d, "GET", "/v1beta/models", "pageSize=x", "", ms).Body())
	items, _ := body["models"].([]any)
	first, _ := items[0].(map[string]any)
	if len(items) != 2 || first["thinking"] != nil || first["description"] != "D" || first["baseModelId"] != "a" || first["inputTokenLimit"] != 1048576.0 {
		t.Errorf("list: %v", body)
	}
	if w := resourceCall(d, "GET", "/v1beta/models", "pageSize=5000", "", ms); !strings.Contains(string(w.Body()), `"models/b"`) {
		t.Errorf("pageSize above the maximum: %s", w.Body())
	}
	tok := pageToken("models/b")
	if w := resourceCall(d, "GET", "/v1beta/models", "pageToken="+tok, "", ms); string(w.Body()) != "{}\n" {
		t.Errorf("past the last page: %q", w.Body())
	}
	for _, bad := range []string{pageToken("models/zzz"), "@@"} {
		if w := resourceCall(d, "GET", "/v1beta/models", "pageToken="+bad, "", ms); w.StatusCode() != 400 {
			t.Errorf("token %q: %d", bad, w.StatusCode())
		}
	}
	if w := resourceCall(d, "GET", "/v1beta/models/x/y", "", "", ms); w.StatusCode() != 404 || len(w.Body()) != 0 {
		t.Errorf("nested model path: %d %q", w.StatusCode(), w.Body())
	}
	if w := resourceCall(d, "GET", "/v1beta/tunedModels", "", "", ms); w.StatusCode() != 404 {
		t.Errorf("unknown resource: %d", w.StatusCode())
	}
	w := core.NewMemWriter()
	d.Extra(driver.KindUsage, w, driver.MapHeaders{}, nil, driver.ExtraContext{})
	if w.StatusCode() != 404 {
		t.Errorf("other kind: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.ListModels(w, nil, ms)
	if !strings.Contains(string(w.Body()), `"models/a"`) {
		t.Errorf("ListModels: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.GetModel(w, nil, &ms[1], "b")
	if !strings.Contains(string(w.Body()), `"models/b"`) {
		t.Errorf("GetModel: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.GetModel(w, nil, nil, "z")
	if errMessage(t, w) != "Model is not found: models/z for api version v1beta" {
		t.Errorf("GetModel miss: %s", w.Body())
	}
}

func TestParseCorners(t *testing.T) {
	d := &Driver{}
	h := func(method string) driver.MapHeaders {
		return driver.MapHeaders{"x-goog-api-key": "k", "x-wwz-path": "/v1beta/models/m:" + method}
	}
	if _, e := d.ParseRequest(driver.KindChat, nil, h("generateContent")); e == nil || e.Extra[extraNoRoute] != true {
		t.Error("another kind is no route")
	}
	if _, e := d.ParseRequest(driver.KindGenerate, nil, driver.MapHeaders{"x-wwz-path": "/v1beta/models/m"}); e == nil {
		t.Error("no method is no route")
	}
	bearer := driver.MapHeaders{"authorization": "Bearer t", "x-wwz-path": "/v1beta/models/m:generateContent"}
	for _, body := range []string{`{"foo": 1}`, `{`} {
		if _, e := d.ParseRequest(driver.KindGenerate, []byte(body), bearer); e == nil || driver.BeforeAuth(e) {
			t.Errorf("%s with a bearer: the 401 comes first", body)
		}
	}
	req, e := d.ParseRequest(driver.KindGenerate, []byte(`{"generateContentRequest": {"contents": [{"parts": [{"text": "x"}]}]}}`), h("countTokens"))
	if e != nil || req.Endpoint != core.EndpointCountTokens || len(req.Messages) != 1 {
		t.Errorf("countTokens with generateContentRequest: %+v %v", req, e)
	}
	body := `{"contents": [
		{"role": "user", "parts": [{"text": "a"}, {"text": "hidden", "thought": true}, {"fileData": {"mimeType": "audio/mp3", "fileUri": "u"}}]},
		{"role": "assistant", "parts": [{"functionCall": {"name": "f"}}]},
		{"parts": [{"functionResponse": {"name": "f"}}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "NONE", "allowedFunctionNames": ["f", "g"]}},
		"tools": [{"functionDeclarations": [{"name": "f"}, {"name": "g"}, {"name": "h"}]}],
		"generationConfig": {"responseMimeType": "application/json", "responseJsonSchema": {"type": "object"}, "thinkingConfig": {"thinkingBudget": -1, "thinkingLevel": "medium"}}}`
	req, e = d.ParseRequest(driver.KindGenerate, []byte(body), h("streamGenerateContent"))
	if e != nil {
		t.Fatal(e)
	}
	m := req.Messages
	if m[0].Content != "ahidden" || m[0].AudioParts != 1 || m[1].Role != "assistant" || m[1].ToolCalls[0].Arguments != "{}" ||
		m[2].ToolResults[0].ToolCallID != "f" || m[2].ToolResults[0].Content != "{}" {
		t.Errorf("messages: %+v", m)
	}
	if req.ToolChoice.Mode != core.ToolChoiceNone || len(req.Tools) != 2 || req.ResponseFormat != "json_schema" ||
		!req.Reasoning.Requested || req.Reasoning.Effort != "medium" || !req.Reasoning.Exclude {
		t.Errorf("request: %+v", req)
	}
	for _, c := range []struct{ cfg, effort string }{{`{"thinkingLevel": "LOW"}`, "low"}, {`{"thinkingBudget": 0}`, ""}} {
		req, _ = d.ParseRequest(driver.KindGenerate, []byte(`{"contents": [{"parts": [{"text": "x"}]}], "generationConfig": {"responseMimeType": "application/json", "thinkingConfig": `+c.cfg+`}}`), h("generateContent"))
		if req.Reasoning.Effort != c.effort || req.ResponseFormat != "json_object" {
			t.Errorf("%s: %+v %q", c.cfg, req.Reasoning, req.ResponseFormat)
		}
	}
	if _, e := d.ParseRequest(driver.KindGenerate, []byte(`{"contents": [{"parts": [{"text": "x"}]}], "cachedContent": "cachedContents/none"}`), h("generateContent")); e == nil || e.Kind != core.ErrPermission {
		t.Errorf("unknown cache: %v", e)
	}
}

func TestErrorCorners(t *testing.T) {
	d := &Driver{}
	gen := func(query string, hs ...string) *core.Request {
		h := driver.MapHeaders{"x-wwz-path": "/v1beta/models/m:streamGenerateContent", driver.QueryHeader: query}
		for i := 0; i+1 < len(hs); i += 2 {
			h[hs[i]] = hs[i+1]
		}
		return &core.Request{Headers: h}
	}
	w := core.NewMemWriter()
	d.WriteError(w, gen("", "x-goog-api-key", "k"), &core.Error{Kind: core.ErrInvalidAPIKey, Message: "This API key has expired."}, nil)
	if !strings.HasPrefix(string(w.Body()), "[{") || !strings.Contains(string(w.Body()), "API_KEY_EXPIRED") {
		t.Errorf("expired key in an array stream: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.WriteError(w, gen("key=k"), &core.Error{Kind: core.ErrInvalidAPIKey}, nil)
	if w.StatusCode() != 400 {
		t.Errorf("query key: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrQuota, Extra: map[string]any{"limit": "rpd"}}, nil)
	if !strings.Contains(string(w.Body()), "GenerateRequestsPerDayPerProjectPerModel") || strings.Contains(string(w.Body()), "RetryInfo") {
		t.Errorf("rpd without retry: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, nil, nil)
	if w.StatusCode() != 500 {
		t.Errorf("nil error: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrServerError, Status: 502, Extra: map[string]any{core.ExtraRawBody: "upstream"}}, nil)
	if w.StatusCode() != 502 || string(w.Body()) != "upstream" {
		t.Errorf("raw body: %d %q", w.StatusCode(), w.Body())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrServerError, Extra: map[string]any{core.ExtraRawBody: "x"}}, nil)
	if w.StatusCode() != 500 {
		t.Errorf("raw body default status: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrBadGateway}, nil)
	if w.StatusCode() != 502 || !strings.Contains(string(w.Body()), "502") {
		t.Errorf("bad gateway: %d", w.StatusCode())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrContextLength}, nil)
	if w.StatusCode() != 400 || errMessage(t, w) != msgContext {
		t.Errorf("context: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrRequestTooLarge}, nil)
	if errMessage(t, w) != msgTooLarge {
		t.Errorf("too large: %s", w.Body())
	}
	w = core.NewMemWriter()
	d.WriteError(w, &core.Request{Model: "m"}, &core.Error{Kind: core.ErrModelNotFound}, nil)
	if !strings.Contains(errMessage(t, w), "models/m is not found for API version v1beta, or is not supported for generateContent") {
		t.Errorf("model without a route: %s", w.Body())
	}
}

func TestRPCMethod(t *testing.T) {
	for path, want := range map[string]string{
		"/v1beta/models/m:streamGenerateContent": "GenerativeService.StreamGenerateContent",
		"/v1beta/models/m:countTokens":           "GenerativeService.CountTokens",
		"/v1beta/models":                         "ModelService.ListModels",
		"/v1/models/m":                           "ModelService.GetModel",
		"/v1beta/cachedContents|POST":            "CacheService.CreateCachedContent",
		"/v1beta/cachedContents|GET":             "CacheService.ListCachedContents",
		"/v1beta/cachedContents/x|DELETE":        "CacheService.DeleteCachedContent",
		"/v1beta/cachedContents/x|PATCH":         "CacheService.UpdateCachedContent",
		"/v1beta/cachedContents/x|GET":           "CacheService.GetCachedContent",
		"|POST":                                  "GenerativeService.GenerateContent",
	} {
		p, m, _ := strings.Cut(path, "|")
		if got := rpcMethod(routeOf(p), m); !strings.HasSuffix(got, want) {
			t.Errorf("%s: %s", path, got)
		}
	}
	if routeOfHeaders(nil).version != "v1beta" || routeOfReq(nil).version != "v1beta" || reqHeaders(nil).Get("x") != "" {
		t.Error("nil routes")
	}
}

// failWriter fails every write.
type failWriter struct{ *core.MemWriter }

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("gone") }

func TestWritersAndIDs(t *testing.T) {
	d := &Driver{}
	req := &core.Request{Headers: driver.MapHeaders{"x-wwz-path": "/v1beta/models/m:generateContent"}}
	resp := &core.Response{
		ID: "x", Model: "m", FinishReason: core.FinishContentFilter, Content: "c",
		ToolCalls: []core.ToolCall{{ID: "call_1", Name: "f", Arguments: "not json"}}, Headers: map[string]string{"x-wwz-seed": "1"},
	}
	if err := d.WriteResponse(failWriter{core.NewMemWriter()}, req, resp, nil); err == nil {
		t.Error("write error not returned")
	}
	w := core.NewMemWriter()
	_ = d.WriteResponse(w, req, resp, nil)
	if !strings.Contains(string(w.Body()), `"args": {}`) || !strings.Contains(string(w.Body()), `"finishReason": "SAFETY"`) || w.Headers()["x-wwz-seed"] != "1" {
		t.Errorf("body: %s", w.Body())
	}
	if finishReason(core.FinishError) != "OTHER" {
		t.Error("error finish")
	}
	// Stream corners: unknown tool index, write errors, array without frames.
	sreq := &core.Request{Headers: driver.MapHeaders{"x-wwz-path": "/v1beta/models/m:streamGenerateContent"}}
	sw := d.StreamWriter(failWriter{core.NewMemWriter()}, sreq, nil)
	_ = sw.Start(resp)
	if sw.TextDelta("x") == nil || sw.Raw([]byte("x")) == nil || sw.Close() == nil {
		t.Error("stream write errors not returned")
	}
	if sw.ToolArgsDelta(9, "x") != nil || sw.ToolCallEnd(9) != nil {
		t.Error("unknown tool index")
	}
	w = core.NewMemWriter()
	sw = d.StreamWriter(w, sreq, nil)
	_ = sw.Start(resp)
	_ = sw.Close()
	_ = sw.Close()
	if string(w.Body()) != "[]" {
		t.Errorf("empty array stream: %q", w.Body())
	}
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		id := d.ToolCallID(driver.KindGenerate, generate.NewRNG(1, "c", string(rune(i))))
		if len(id) != 11 {
			t.Fatalf("call id %q", id)
		}
		seen[int(id[5]-'0')] = true
	}
	if !seen[0] {
		t.Error("no zero-padded call id in 200 draws")
	}
}

func TestWriteRouteMiss(t *testing.T) {
	d := &Driver{}
	w := core.NewMemWriter()
	d.WriteRouteMiss(w, &core.Request{Headers: map[string]string{"x-wwz-path": "/nope"}}, driver.RouteMiss{Method: "GET", Path: "/nope"}, nil)
	if w.StatusCode() != 404 || len(w.Body()) != 0 {
		t.Fatalf("status %d body %q", w.StatusCode(), w.Body())
	}
	if w.Headers()["Server"] != "scaffolding on HTTPServer2" || w.Headers()["Content-Type"] != "text/html" {
		t.Errorf("headers: %v", w.Headers())
	}
}
