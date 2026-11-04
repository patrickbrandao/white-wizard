package gemini

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// Explicit context caching, cachedContents (spec 006 16.5, spec 007),
// conferred on 2026-10-31 (task 164): create, get, list, patch (ttl or
// expireTime, with or without updateMask), delete ("{}"), the 403 "not
// found (or permission denied)" of a missing entry, the 400 below the
// minimum and the generateContent that reads it (cachedContentTokenCount
// and cacheTokensDetails; systemInstruction, tools and toolConfig refused
// next to it).
//
// The entries live in the store the engine shares
// (limits.SharedCachedContents; task 265), by account: every wwz instance
// on the same Valkey sees them, and a wwz reload keeps them. A store error
// on a read is "not found", on a write a 500.

// metaCached is the req.Metadata slot naming the cachedContent a request
// reads, and metaCachedTokens its token count; the NUL prefix keeps them
// out of the client's metadata.
const (
	metaCached       = "\x00gemini-cached-content"
	metaCachedTokens = "\x00gemini-cached-tokens" //nolint:gosec // G101: a metadata slot, not a credential
)

// defaultCacheTTL is the TTL without ttl or expireTime (documented: 1 h).
const defaultCacheTTL = time.Hour

// minCacheTokens is the smallest cache (observed with gemini-3.5-flash-lite:
// min_total_token_count=1024); models[].prompt_cache.min_tokens overrides.
const minCacheTokens = 1024

type cachedContent struct {
	name, model, display string
	system               string
	messages             []core.Message
	tools                []core.ToolDef
	tokens               int
	create, update       time.Time
	expire               time.Time
}

// cacheBody is the store's opaque body of an entry.
type cacheBody struct {
	System   string         `json:"s,omitempty"`
	Messages []core.Message `json:"m,omitempty"`
	Tools    []core.ToolDef `json:"t,omitempty"`
}

func (c *cachedContent) entry(owner string) limits.CachedContent {
	body, _ := json.Marshal(cacheBody{System: c.system, Messages: c.messages, Tools: c.tools}) // plain data: never fails
	return limits.CachedContent{
		Owner: owner, Name: c.name, Model: c.model, Display: c.display, Tokens: c.tokens,
		Create: c.create, Update: c.update, Expire: c.expire, Body: body,
	}
}

func fromEntry(e limits.CachedContent) *cachedContent {
	var b cacheBody
	_ = json.Unmarshal(e.Body, &b) // written by entry; a bad body is an empty cache
	return &cachedContent{
		name: e.Name, model: e.Model, display: e.Display, system: b.System, messages: b.Messages, tools: b.Tools,
		tokens: e.Tokens, create: e.Create, update: e.Update, expire: e.Expire,
	}
}

// getCache returns the live entry name of owner; a store error is nil.
func getCache(owner, name string, now time.Time) *cachedContent {
	e, err := limits.SharedCachedContents().CachedContentGet(context.Background(), owner, name, now)
	if err != nil || e == nil {
		return nil
	}
	return fromEntry(*e)
}

func putCache(owner string, c *cachedContent) error {
	return limits.SharedCachedContents().CachedContentPut(context.Background(), c.entry(owner))
}

// listCaches returns the live entries of owner, oldest first.
func listCaches(owner string, now time.Time) ([]*cachedContent, error) {
	l, err := limits.SharedCachedContents().CachedContentList(context.Background(), owner, now)
	out := make([]*cachedContent, len(l))
	for i, e := range l {
		out[i] = fromEntry(e)
	}
	return out, err
}

// storeError is the 500 of a store that failed a write.
func storeError() *core.Error { return &core.Error{Kind: core.ErrServerError} }

// cacheOwner is who a cache belongs to: the account of the key (the real
// scopes caches by project), else the key itself.
func cacheOwner(req *core.Request) string {
	a := driver.AccountOf(req)
	if a.Project != "" {
		return "p:" + a.Account + "/" + a.Project
	}
	if a.Account != "" {
		return "a:" + a.Account
	}
	return "k:" + a.Key
}

// protoTime formats a timestamp like proto3 JSON: 0, 3, 6 or 9 fractional
// digits, whichever is exact (observed: createTime with 6, expireTime
// with 9).
func protoTime(t time.Time) string {
	t = t.UTC()
	ns := t.Nanosecond()
	switch {
	case ns == 0:
		return t.Format("2006-01-02T15:04:05Z")
	case ns%1e6 == 0:
		return t.Format("2006-01-02T15:04:05.000Z")
	case ns%1e3 == 0:
		return t.Format("2006-01-02T15:04:05.000000Z")
	}
	return t.Format("2006-01-02T15:04:05.000000000Z")
}

func (c *cachedContent) obj() oai.Obj {
	o := oai.O("name", c.name, "model", c.model, "createTime", protoTime(c.create),
		"updateTime", protoTime(c.update), "expireTime", protoTime(c.expire))
	if c.display != "" {
		o = append(o, oai.Field{Key: "displayName", Val: c.display})
	}
	return append(o, oai.Field{Key: "usageMetadata", Val: oai.O("totalTokenCount", c.tokens)})
}

const nameAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// newCacheName is cachedContents/<40 [0-9a-z]> (observed).
func newCacheName() string {
	b := make([]byte, 40)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(nameAlphabet))))
		b[i] = nameAlphabet[n.Int64()]
	}
	return "cachedContents/" + string(b)
}

// parseTTL reads a proto3 Duration ("300s", "1.5s").
func parseTTL(s string) (time.Duration, bool) {
	n, ok := strings.CutSuffix(s, "s")
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// expiry applies ttl or expireTime of body to from; ok is false when
// neither is present.
func expiry(body map[string]any, from time.Time) (time.Time, bool, *core.Error) {
	if s, ok := body["ttl"].(string); ok {
		d, ok := parseTTL(s)
		if !ok {
			return time.Time{}, false, invalid("Invalid value at 'cached_content.ttl' (type.googleapis.com/google.protobuf.Duration), " + quote(s))
		}
		return from.Add(d), true, nil
	}
	if s, ok := body["expireTime"].(string); ok {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, false, invalid("Invalid value at 'cached_content.expire_time' (type.googleapis.com/google.protobuf.Timestamp), " + quote(s))
		}
		return t, true, nil
	}
	return time.Time{}, false, nil
}

// findModel resolves a model name ("models/<id>" or "<id>") among the
// listed models, by id or alias.
func findModel(models []driver.ModelInfo, name string) *driver.ModelInfo {
	name = strings.TrimPrefix(name, "models/")
	for i := range models {
		c := models[i].Cfg
		if c.ID == name {
			return &models[i]
		}
		for _, a := range c.Aliases {
			if a == name {
				return &models[i]
			}
		}
	}
	return nil
}

// cacheRequest serves the cachedContents routes.
func (d *Driver) cacheRequest(w core.ResponseWriter, req *core.Request, r route, method string, body []byte, ctx driver.ExtraContext) {
	now := time.Now()
	owner := cacheOwner(req)
	name := r.resource
	switch {
	case name == "cachedContents" && method == "POST":
		d.createCache(w, req, owner, body, ctx, now)
	case name == "cachedContents" && method == "GET":
		list, err := listCaches(owner, now)
		if err != nil {
			d.WriteError(w, req, storeError(), nil)
			return
		}
		if len(list) == 0 {
			writeJSON(w, req, 200, oai.Obj{})
			return
		}
		items := make([]any, len(list))
		for i, c := range list {
			items[i] = c.obj()
		}
		writeJSON(w, req, 200, oai.O("cachedContents", items))
	default: // cachedContents/{id} under GET, PATCH or DELETE (served)
		c := getCache(owner, name, now)
		if c == nil {
			d.WriteError(w, req, &core.Error{Kind: core.ErrPermission, Message: msgCacheMissing}, nil)
			return
		}
		switch method {
		case "GET":
			writeJSON(w, req, 200, c.obj())
		case "DELETE":
			if limits.SharedCachedContents().CachedContentDelete(context.Background(), owner, name) != nil {
				d.WriteError(w, req, storeError(), nil)
				return
			}
			writeJSON(w, req, 200, oai.Obj{})
		default:
			obj, serr, vs := transcode(body, msgCached)
			if e := transcodeError(serr, vs); e != nil {
				d.WriteError(w, req, e, nil)
				return
			}
			exp, ok, e := expiry(obj, now)
			if e != nil {
				d.WriteError(w, req, e, nil)
				return
			}
			upd := *c
			if ok {
				upd.expire = exp
			}
			upd.update = now.Truncate(time.Microsecond)
			if putCache(owner, &upd) != nil {
				d.WriteError(w, req, storeError(), nil)
				return
			}
			writeJSON(w, req, 200, upd.obj())
		}
	}
}

// transcodeError is the 400 of a body the transcoder refused.
func transcodeError(serr *errSyntax, vs []violation) *core.Error {
	if serr != nil {
		return invalid(serr.msg)
	}
	if len(vs) > 0 {
		return badRequest(vs)
	}
	return nil
}

func (d *Driver) createCache(w core.ResponseWriter, req *core.Request, owner string, body []byte, ctx driver.ExtraContext, now time.Time) {
	obj, serr, vs := transcode(body, msgCached)
	if e := transcodeError(serr, vs); e != nil {
		d.WriteError(w, req, e, nil)
		return
	}
	modelName := str(obj, "model")
	if modelName == "" {
		// Não conferido: the text of a create without model.
		d.WriteError(w, req, invalid("* CreateCachedContentRequest.cached_content.model: model is not specified\n"), nil)
		return
	}
	m := findModel(ctx.Models, modelName)
	if m == nil {
		req.Model = strings.TrimPrefix(modelName, "models/")
		msg := fmt.Sprintf(msgModelMethod, req.Model, routeOfReq(req).version, "createCachedContent") // não conferido
		d.WriteError(w, req, &core.Error{Kind: core.ErrModelNotFound, Extra: map[string]any{extraMessage: msg}}, nil)
		return
	}
	c := &cachedContent{name: newCacheName(), model: "models/" + m.Cfg.ID, display: str(obj, "displayName")}
	if si, ok := obj["systemInstruction"].(map[string]any); ok {
		c.system = contentText(si)
	}
	for _, ct := range asList(obj["contents"]) {
		c.messages = append(c.messages, toMessage(ct))
	}
	tmp := &core.Request{System: c.system, Messages: c.messages}
	_ = fill(tmp, map[string]any{"tools": obj["tools"]}) // only tools: no cache lookup, no error
	c.tools = tmp.Tools
	c.tokens = gtok.CountPrompt(tokenizerOf(m.Cfg), tmp).Total
	minTok := minCacheTokens
	if pc := m.Cfg.PromptCache; pc != nil && pc.MinTokens != nil {
		minTok = *pc.MinTokens
	}
	if c.tokens < minTok {
		d.WriteError(w, req, invalid(fmt.Sprintf("Cached content is too small. total_token_count=%d, min_total_token_count=%d", c.tokens, minTok)), nil)
		return
	}
	c.create = now.Truncate(time.Microsecond)
	c.update = c.create
	exp, ok, e := expiry(obj, now)
	if e != nil {
		d.WriteError(w, req, e, nil)
		return
	}
	if !ok {
		exp = now.Add(defaultCacheTTL)
	}
	c.expire = exp
	if putCache(owner, c) != nil {
		d.WriteError(w, req, storeError(), nil)
		return
	}
	writeJSON(w, req, 200, c.obj())
}

// writeJSON writes a prettyPrint JSON answer.
func writeJSON(w core.ResponseWriter, req *core.Request, status int, v any) {
	body := pretty(v)
	w.Status(status)
	w.Header("Content-Type", contentJSON)
	w.Header("Content-Length", strconv.Itoa(len(body)))
	commonHeaders(w, req)
	_, _ = w.Write(body)
	_ = w.Flush()
}

// tokenizerOf is the tokenizer a model counts with on this dialect, as the
// engine picks it: its BPE or an explicit simulated, else the Gemini
// calibration (gtok).
func tokenizerOf(m *config.ModelConfig) generate.Tokenizer {
	switch m.Encoding() {
	case "o200k_base":
		return tokenize.O200k
	case "cl100k_base":
		return tokenize.Cl100k
	}
	if m.Tokenizer == config.TokenizerSimulated {
		return generate.Simulated{}
	}
	return gtok.Tokenizer{}
}
