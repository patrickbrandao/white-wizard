package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"slices"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Prompt cache as a registry of prefixes (spec 007 "Cache de prompt", spec
// 008, cache/valkey/README.md 2.4, ADR 0046). The engine tokenizes the prompt
// in the provider's order, hashes the candidate prefixes, asks the store
// which ones are alive before Check and writes or renews them when the
// response sends its first byte. Nothing is drawn at random.

// Prompt-cache kinds of models[].prompt_cache.kind.
const (
	pcAutomatic = "automatic"
	pcExplicit  = "explicit"
	pcNone      = "none"
)

// TTLs of the providers (README 2.4).
const (
	pcOpenAITTL     = 10 * time.Minute // in_memory: 5–10 min unused (wwz: 10 min)
	pcOpenAIMaxLife = time.Hour        // in_memory: at most 1 h
	pcAnthropic5m   = 5 * time.Minute
	pcAnthropic1h   = time.Hour
	pcGeminiTTL     = 5 * time.Minute // implicit: undocumented (not checked)
	// pcLookback is how many blocks before a breakpoint (itself included)
	// Anthropic searches for an entry.
	pcLookback = 20
	// pcMaxBreakpoints is Anthropic's limit of cache_control blocks.
	pcMaxBreakpoints = 4
	// pcExactStep is the candidate step with granularity 0 (exact boundary).
	pcExactStep = 128
	// pcImageTokens is what one image costs in the prompt count.
	pcImageTokens = 85
)

// pcRule is the prompt-cache rule of one request.
type pcRule struct {
	kind  string
	min   int
	gran  int
	style driver.CacheStyle // TTLs and fields of the upstream
}

// pcStyleRule is the default rule of each cache style for the current
// generation of real models (spec 007).
func pcStyleRule(style driver.CacheStyle) pcRule {
	switch style {
	case driver.CacheAutomatic:
		return pcRule{kind: pcAutomatic, min: 1024, gran: 128, style: style}
	case driver.CacheBreakpoints:
		return pcRule{kind: pcExplicit, min: 512, style: style}
	case driver.CacheImplicit:
		return pcRule{kind: pcAutomatic, min: 4096, style: style}
	}
	return pcRule{kind: pcNone}
}

// pcRuleFor resolves the rule of the driver that declared decl for model on
// endpoint ep: the style the driver declares (OpenRouter's by the slug's
// author, OpenCode's by the native API of the endpoint), then
// models[].prompt_cache field by field.
func pcRuleFor(decl driver.EngineDecl, model *config.ModelConfig, ep core.Endpoint) pcRule {
	style := driver.CacheNone
	if decl.PromptCache != nil {
		style = decl.PromptCache(model, ep)
	}
	r := pcStyleRule(style)
	if pc := model.PromptCache; pc != nil {
		if pc.Kind != "" {
			r.kind = pc.Kind
			if r.style == driver.CacheNone {
				r.style = driver.CacheAutomatic
				if pc.Kind == pcExplicit {
					r.style = driver.CacheBreakpoints
				}
			}
		}
		if pc.MinTokens != nil {
			r.min = *pc.MinTokens
		}
		if pc.Granularity != nil {
			r.gran = *pc.Granularity
		}
	}
	return r
}

// pcState is the prompt-cache outcome of one request.
type pcState struct {
	cacher  limits.PromptCacher
	account limits.Entity
	read    int // tokens read from the cache
	write   int // tokens written (all TTLs)
	write1h int // part of write with the 1 h TTL
	commit  []limits.PromptCacheEntry
}

// pcPart is a run of prompt tokens: text cut by the tokenizer, raw bytes
// cut every 4 bytes (tool schemas, len/4 tokens), n synthetic tokens of
// the template overhead labelled syn, or (fit) exactly n tokens: those of
// text, cut or completed with syn tokens.
type pcPart struct {
	text string
	raw  bool
	fit  bool
	syn  string
	n    int
}

// pcBlock is a content block (message, part, tool definition); bp is the
// TTL of its cache_control breakpoint (0 = none).
type pcBlock struct {
	parts []pcPart
	bp    time.Duration
}

func (b *pcBlock) text(s string)           { b.parts = append(b.parts, pcPart{text: s}) }
func (b *pcBlock) raw(s string)            { b.parts = append(b.parts, pcPart{text: s, raw: true}) }
func (b *pcBlock) syn(label string, n int) { b.parts = append(b.parts, pcPart{syn: label, n: n}) }

// fit is n tokens carrying the content of s (see pcPart).
func (b *pcBlock) fit(s, label string, n int) {
	b.parts = append(b.parts, pcPart{text: s, fit: true, syn: label, n: n})
}

func partTokens(tk generate.Tokenizer, p pcPart) int {
	switch {
	case p.fit:
		return max(p.n, 0)
	case p.n > 0:
		return p.n
	case p.raw:
		return len(p.text) / 4
	}
	return tk.Count(p.text)
}

// msgOverhead is the chat template cost of a message (generate.CountPrompt).
func msgOverhead(tk generate.Tokenizer, role, name string) int {
	n := generate.MessageOverhead + tk.Count(role)
	if name != "" {
		n += generate.NameOverhead
	}
	return n
}

// pcAutoBlocks lays out the normalized request like generate.CountPrompt,
// token for token (the blocks add up to its total, task 246), in the order
// of the real prompt (ADR 0063, task 303): the tool definitions and the
// json_schema first, as the header of the OpenAI prompt (changing the
// tools invalidates the whole prefix), then system, messages and the reply
// priming. Tool calls and results cost as generate counts them (compact
// arguments, the multi_tool_use rendering of a parallel Chat turn); the
// tool block is generate.ToolsText cut or completed to
// generate.ToolSchemaTokens.
func pcAutoBlocks(tk generate.Tokenizer, req *core.Request) []pcBlock {
	var out []pcBlock
	if n := generate.ToolSchemaTokens(tk, req); n > 0 {
		var b pcBlock
		content := string(req.ResponseSchema)
		if len(req.Tools) > 0 {
			content = generate.ToolsText(req.Tools) + content
		}
		b.fit(content, "tool", n)
		out = append(out, b)
	}
	if sys := req.System; sys != "" {
		var b pcBlock
		b.syn("msg:system", msgOverhead(tk, "system", ""))
		b.text(sys)
		out = append(out, b)
	}
	// lean is generate's Responses template (also GPT-4.1 on Chat): calls
	// one by one, arguments as sent by GPT-4.1, results framed with 3.
	gpt41 := strings.Contains(req.Model, "gpt-4.1")
	lean := req.Endpoint == core.EndpointResponses || gpt41
	type call struct {
		name     string
		parallel bool
	}
	calls := map[string]call{}
	for _, m := range req.Messages {
		var b pcBlock
		switch m.Role {
		case "tool":
			switch c := calls[m.ToolCallID]; {
			case c.parallel:
				b.text(m.Content)
				b.syn("tool_result", pcParallelResult)
			default:
				b.syn("tool_result", pcResultFrame(lean))
				b.text(c.name)
				b.text(m.Content)
			}
		case "assistant":
			if m.Content != "" || len(m.ToolCalls) == 0 { // a turn of calls only has no text message
				b.syn("msg:assistant:"+m.Name, msgOverhead(tk, m.Role, m.Name))
				b.text(m.Content)
			}
			if len(m.ToolCalls) > 1 && !lean {
				b.syn("tool_call", pcParallelFrame)
				b.text(pcParallelCall(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls[tc.ID] = call{name: tc.Name, parallel: true}
				}
				break
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if !gpt41 {
					args = generate.CompactArguments(args)
				}
				b.syn("tool_call", pcCallFrame)
				b.text(tc.Name)
				b.text(args)
				calls[tc.ID] = call{name: tc.Name}
			}
		default:
			b.syn("msg:"+m.Role+":"+m.Name, msgOverhead(tk, m.Role, m.Name))
			b.text(m.Content)
			for _, tr := range m.ToolResults {
				b.text(tr.Content)
				b.syn("tool_result", 6)
			}
		}
		if n := (m.ImageParts + m.AudioParts) * pcImageTokens; n > 0 {
			b.syn("image", n)
		}
		out = append(out, b)
	}
	var p pcBlock
	p.syn("reply", generate.ReplyPriming)
	return append(out, p)
}

// pcTemplateBlocks lays out a request in the order and with the sizes of
// the driver's template (tpl.CountPrompt token for token, Gemini's; tasks
// 235 and 303; the chat template of pcAutoBlocks is not that one): the
// system instruction, each function declaration, then each turn and each
// of its calls, responses and media as a block of its own, so that the
// exact boundaries of granularity 0 fall where the template's parts end.
// The order of the implicit cache's prefix is not documented (não
// conferido): the template's, with the stable parts first.
func pcTemplateBlocks(tk generate.Tokenizer, req *core.Request, tpl driver.PromptTemplate) []pcBlock {
	var out []pcBlock
	add := func(b pcBlock) { out = append(out, b) }
	if req.System != "" {
		var b pcBlock
		b.text(req.System)
		add(b)
	}
	for _, t := range req.Tools {
		var b pcBlock
		b.fit(t.Name+t.Description+string(t.Parameters), "tool", tpl.Declaration(tk, t))
		add(b)
	}
	names := map[string]string{} // call id -> function name
	for _, m := range req.Messages {
		var b pcBlock
		b.syn("content:"+m.Role, tpl.ContentOverhead())
		b.text(m.Content)
		add(b)
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					names[tc.ID] = tc.Name
				}
				var c pcBlock
				c.fit(tc.Name+tc.Arguments, "call", tpl.Call(tk, tc.Name, tc.Arguments))
				add(c)
			}
		} else {
			for _, tr := range m.ToolResults {
				name := tr.ToolCallID
				if fn, ok := names[name]; ok {
					name = fn
				}
				var r pcBlock
				r.fit(name+tr.Content, "response", tpl.Response(tk, name, tr.Content))
				add(r)
			}
		}
		if n := tpl.MediaTokens(m.ImageParts, m.AudioParts); n > 0 {
			var md pcBlock
			md.syn("media", n)
			add(md)
		}
	}
	return out
}

// The frames of tool calls and results in generate (tools.go): a call
// costs pcCallFrame + name + arguments; a parallel Chat turn
// pcParallelFrame + its multi_tool_use arguments, each result content +
// pcParallelResult; other results the frame + the call's name + content.
const (
	pcCallFrame      = 8
	pcParallelFrame  = 20
	pcParallelResult = 2
)

func pcResultFrame(lean bool) int {
	if lean {
		return 3
	}
	return 5
}

// pcParallelCall is the multi_tool_use arguments of a parallel Chat turn.
func pcParallelCall(calls []core.ToolCall) string {
	var b strings.Builder
	b.WriteString(`{"tool_uses":[`)
	for i, tc := range calls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"recipient_name":"functions.`)
		b.WriteString(tc.Name)
		b.WriteString(`","parameters":`)
		b.WriteString(generate.CompactArguments(tc.Arguments))
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.String()
}

// pcRawCC is a cache_control object.
type pcRawCC struct {
	Type string `json:"type"`
	TTL  string `json:"ttl"`
}

func (c *pcRawCC) ttl() time.Duration {
	switch {
	case c == nil:
		return 0
	case c.TTL == "1h":
		return pcAnthropic1h
	}
	return pcAnthropic5m
}

// pcRawBody is the part of a request body the prompt cache reads: the
// OpenAI retention fields and the Anthropic (or OpenRouter) breakpoints.
type pcRawBody struct {
	pcRawOpts
	CacheControl *pcRawCC        `json:"cache_control"`
	System       json.RawMessage `json:"system"`
	Messages     []struct {
		Role         string          `json:"role"`
		Name         string          `json:"name"`
		Content      json.RawMessage `json:"content"`
		CacheControl *pcRawCC        `json:"cache_control"`
	} `json:"messages"`
	Tools []struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl *pcRawCC        `json:"cache_control"`
		Function     *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

// pcRawOpts are the OpenAI fields that shape the automatic cache.
type pcRawOpts struct {
	PromptCacheKey string `json:"prompt_cache_key"`
	Retention      string `json:"prompt_cache_retention"`
	Options        *struct {
		TTL string `json:"ttl"`
	} `json:"prompt_cache_options"`
}

// pcRawBlock is one content block of system or of a message.
type pcRawBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Thinking     string          `json:"thinking"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	Content      json.RawMessage `json:"content"`
	CacheControl *pcRawCC        `json:"cache_control"`
}

// pcContent decodes a content field: a string or an array of blocks.
func pcContent(raw json.RawMessage) (text string, blocks []pcRawBlock, isText bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", nil, false
	}
	if raw[0] == '"' {
		_ = json.Unmarshal(raw, &text)
		return text, nil, true
	}
	if raw[0] == '[' {
		_ = json.Unmarshal(raw, &blocks)
	}
	return "", blocks, false
}

// pcBlockOf lays out one content block.
func pcBlockOf(b *pcBlock, rb pcRawBlock) {
	switch rb.Type {
	case "text", "":
		b.text(rb.Text)
	case "thinking":
		b.text(rb.Thinking)
	case "tool_use":
		b.text(rb.Name)
		b.text(string(rb.Input))
		b.syn("tool_call", 8)
	case "tool_result":
		text, blocks, isText := pcContent(rb.Content)
		if isText {
			b.text(text)
		}
		for _, x := range blocks {
			b.text(x.Text)
		}
		b.syn("tool_result", 6)
	default: // image, document, image_url, input_audio...
		b.syn(rb.Type, pcImageTokens)
	}
	b.bp = rb.CacheControl.ttl()
}

// pcExplicitBlocks lays out the raw body in Anthropic's order (tools →
// system → messages), each content block its own block with its
// breakpoint; the top-level cache_control marks the last block.
func pcExplicitBlocks(tk generate.Tokenizer, body *pcRawBody) []pcBlock {
	var out []pcBlock
	for _, t := range body.Tools {
		var b pcBlock
		name, desc, schema := t.Name, t.Description, t.InputSchema
		if t.Function != nil {
			name, desc, schema = t.Function.Name, t.Function.Description, t.Function.Parameters
		}
		b.text(name)
		b.text(desc)
		b.raw(string(schema))
		b.syn("tool", 6)
		b.bp = t.CacheControl.ttl()
		out = append(out, b)
	}
	addMessage := func(role, name string, content json.RawMessage, cc *pcRawCC) {
		text, blocks, isText := pcContent(content)
		head := pcPart{syn: "msg:" + role + ":" + name, n: msgOverhead(tk, role, name)}
		if isText || len(blocks) == 0 {
			b := pcBlock{parts: []pcPart{head, {text: text}}, bp: cc.ttl()}
			out = append(out, b)
			return
		}
		for i, rb := range blocks {
			var b pcBlock
			if i == 0 {
				b.parts = append(b.parts, head)
			}
			pcBlockOf(&b, rb)
			out = append(out, b)
		}
		if cc != nil {
			out[len(out)-1].bp = cc.ttl()
		}
	}
	if len(bytes.TrimSpace(body.System)) > 0 && !bytes.Equal(bytes.TrimSpace(body.System), []byte("null")) {
		addMessage("system", "", body.System, nil)
	}
	for _, m := range body.Messages {
		addMessage(m.Role, m.Name, m.Content, m.CacheControl)
	}
	if body.CacheControl != nil && len(out) > 0 && out[len(out)-1].bp == 0 {
		out[len(out)-1].bp = body.CacheControl.ttl()
	}
	var p pcBlock
	p.syn("reply", generate.ReplyPriming)
	return append(out, p)
}

// pcHashSeed starts h0 (README 2.4). Version 2 (ADR 0063) put the tools
// first: entries hashed in the old order never hit by mistake.
const pcHashSeed = "wwz-pc/2"

// pcHasher hashes the chained prefixes (README 2.4): h0 from pcHashSeed,
// driver and model, then h_i = SHA-256(h_{i-1} ‖ segment_i) at every cut.
// The chain runs through every cut point of the rule (so a prefix hashes the same
// whatever else the prompt holds) and emits the hashes of the candidates.
type pcHasher struct {
	h    hash.Hash
	buf  []byte
	prev [32]byte
	pos  int
	cuts []int // every cut point, ascending
	ci   int
	emit []int // the candidates, an ascending subset of cuts
	ei   int
	out  []string
}

func newPCHasher(seed []string, cuts, emit []int) *pcHasher {
	p := &pcHasher{h: sha256.New(), cuts: cuts, emit: emit, out: make([]string, 0, len(emit))}
	p.h.Write([]byte(pcHashSeed))
	for _, s := range seed {
		p.h.Write([]byte{0})
		p.h.Write([]byte(s))
	}
	p.h.Sum(p.prev[:0])
	p.start()
	return p
}

func (p *pcHasher) start() {
	p.h.Reset()
	p.h.Write(p.prev[:])
}

func (p *pcHasher) done() bool { return p.ei >= len(p.emit) }

func (p *pcHasher) tok(t string) {
	if p.done() {
		return
	}
	p.buf = binary.AppendUvarint(p.buf, uint64(len(t)))
	p.buf = append(p.buf, t...)
	if len(p.buf) >= 4096 {
		p.h.Write(p.buf)
		p.buf = p.buf[:0]
	}
	p.pos++
	for p.ci < len(p.cuts) && p.pos == p.cuts[p.ci] {
		p.h.Write(p.buf)
		p.buf = p.buf[:0]
		p.h.Sum(p.prev[:0])
		if !p.done() && p.pos == p.emit[p.ei] {
			p.out = append(p.out, hex.EncodeToString(p.prev[:16]))
			p.ei++
		}
		p.ci++
		p.start()
	}
}

// hashes runs blocks through the hasher and returns one hash per cut.
func (p *pcHasher) hashes(tk generate.Tokenizer, blocks []pcBlock) []string {
	for _, b := range blocks {
		for _, part := range b.parts {
			if p.done() {
				return p.out
			}
			switch {
			case part.fit:
				n := 0
				generate.EachToken(tk, part.text, func(t string) {
					if n < part.n {
						p.tok(t)
						n++
					}
				})
				for ; n < part.n; n++ {
					p.tok(part.syn)
				}
			case part.n > 0:
				for range part.n {
					p.tok(part.syn)
				}
			case part.raw:
				n := len(part.text) / 4
				for i := range n {
					end := 4 * (i + 1)
					if i == n-1 {
						end = len(part.text)
					}
					p.tok(part.text[4*i : end])
				}
			default:
				generate.EachToken(tk, part.text, p.tok)
			}
		}
	}
	return p.out
}

// pcAccount is the entity that owns the cache: the key's account, else the
// key, else the client ip (auth: none).
func pcAccount(r *request) limits.Entity {
	switch {
	case r.key != nil && r.key.Account != "":
		return limits.Entity{Kind: limits.KindAccount, ID: r.key.Account}
	case r.key != nil:
		return limits.Entity{Kind: limits.KindKey, ID: r.key.Label}
	}
	return limits.Entity{Kind: limits.KindIP, ID: r.req.ClientIP}
}

// lastN keeps the n largest of the ascending lens.
func lastN(lens []int, n int) []int {
	if len(lens) > n {
		return lens[len(lens)-n:]
	}
	return lens
}

// promptCacheLookup decides the cache outcome of r before Check: which
// prefix is read and which entries the first byte will write.
func (e *Engine) promptCacheLookup(ctx context.Context, r *request) {
	if r.chained() { // previous_response_id: the recorded conversation is the prefix
		e.chainedCacheLookup(ctx, r)
		return
	}
	cacher, ok := e.Store.(limits.PromptCacher)
	if !ok || r.model == nil {
		return
	}
	rule := pcRuleFor(r.decl, r.model, r.req.Endpoint)
	if rule.kind != pcAutomatic && rule.kind != pcExplicit {
		return
	}
	if r.prompt.Total < max(rule.min, 1) {
		return
	}
	var body pcRawBody
	var blocks []pcBlock
	if rule.kind == pcExplicit {
		if json.Unmarshal(r.req.Raw, &body) != nil {
			return
		}
		blocks = pcExplicitBlocks(r.tk, &body)
	} else {
		if len(r.req.Raw) > 0 {
			_ = json.Unmarshal(r.req.Raw, &body.pcRawOpts) // only the cache fields matter
		}
		if tpl := r.decl.Template; tpl != nil {
			blocks = pcTemplateBlocks(r.tk, r.req, tpl) // as countPrompt counts it
		} else {
			blocks = pcAutoBlocks(r.tk, r.req)
		}
	}
	ends := make([]int, len(blocks))
	total := 0
	for i, b := range blocks {
		for _, p := range b.parts {
			total += partTokens(r.tk, p)
		}
		ends[i] = total
	}
	st := &pcState{cacher: cacher, account: pcAccount(r)}
	var lens, chain []int
	type write struct {
		n   int
		ttl time.Duration
	}
	var writes []write
	if rule.kind == pcAutomatic {
		if rule.gran > 0 {
			for l := rule.min; l <= total-respCacheTail(r, rule); l += rule.gran {
				if l > 0 {
					lens = append(lens, l)
				}
			}
		} else {
			for l := pcExactStep; l <= total; l += pcExactStep {
				if l >= rule.min {
					lens = append(lens, l)
				}
			}
			for _, end := range ends {
				if end >= rule.min && end > 0 {
					lens = append(lens, end)
				}
			}
		}
	} else {
		for _, end := range ends {
			if end >= rule.min && end > 0 {
				chain = append(chain, end)
			}
		}
		nbp := 0
		for i, b := range blocks {
			if b.bp == 0 || nbp == pcMaxBreakpoints {
				continue
			}
			nbp++
			for j := max(0, i-pcLookback+1); j <= i; j++ {
				if ends[j] >= rule.min && ends[j] > 0 {
					lens = append(lens, ends[j])
				}
			}
			if ends[i] >= rule.min && ends[i] > 0 {
				writes = append(writes, write{ends[i], b.bp})
			}
		}
	}
	slices.Sort(lens)
	lens = slices.Compact(lens)
	if rule.kind == pcAutomatic {
		chain = lens // every candidate is a cut point
	} else {
		slices.Sort(chain)
		chain = slices.Compact(chain)
	}
	lens = lastN(lens, limits.PromptCacheMaxCandidates)
	if len(lens) == 0 {
		return
	}
	seed := []string{r.in.Driver.Name(), r.model.ID}
	if body.PromptCacheKey != "" {
		seed = append(seed, body.PromptCacheKey) // routing key: other keys miss, as observed
	}
	hashes := newPCHasher(seed, chain, lens).hashes(r.tk, blocks)
	hit, err := cacher.PromptCacheLookup(ctx, st.account, hashes, time.Now())
	if err != nil {
		e.debug("prompt cache lookup failed", err)
		hit = -1
	}
	clamp := func(n int) int { return min(n, r.prompt.Total) }
	if rule.kind == pcExplicit {
		// input_tokens is what comes after the last breakpoint (spec 007),
		// never 0 while the layout has tokens after the cut: when the
		// layout costs more than the prompt count (the chat frame against
		// the Claude template of generate), its cuts scale down onto the
		// count, so the closing of the last turn stays out of the cache as
		// in the real (task 394).
		clamp = func(n int) int { return min(n, n*r.prompt.Total/total) }
	}
	if hit >= 0 {
		st.read = clamp(lens[hit])
	}
	hashOf := func(n int) string { return hashes[slices.Index(lens, n)] }
	if rule.kind == pcAutomatic {
		ttl, life := pcAutoTTL(rule.style, &body.pcRawOpts)
		for _, h := range hashes {
			st.commit = append(st.commit, limits.PromptCacheEntry{Hash: h, TTL: ttl, MaxLife: life})
		}
	} else {
		last, last1h := 0, 0
		for _, w := range writes {
			last = max(last, clamp(w.n))
			if w.ttl == pcAnthropic1h {
				last1h = max(last1h, clamp(w.n))
			}
			st.commit = append(st.commit, limits.PromptCacheEntry{Hash: hashOf(w.n), TTL: w.ttl})
		}
		if hit >= 0 {
			st.commit = append(st.commit, limits.PromptCacheEntry{Hash: hashes[hit], TTL: pcAnthropic5m})
		}
		st.write = max(0, last-st.read)
		st.write1h = min(st.write, max(0, last1h-st.read))
	}
	r.pc = st
}

// pcAutoTTL is the TTL and maximum life of an automatic entry.
func pcAutoTTL(style driver.CacheStyle, body *pcRawOpts) (ttl, life time.Duration) {
	if style == driver.CacheImplicit {
		return pcGeminiTTL, 0
	}
	if body.Options != nil {
		switch body.Options.TTL {
		case "30m":
			return 30 * time.Minute, 30 * time.Minute
		case "12h":
			return 12 * time.Hour, 12 * time.Hour
		}
	}
	if body.Retention == "24h" {
		return 24 * time.Hour, 24 * time.Hour
	}
	return pcOpenAITTL, pcOpenAIMaxLife
}

// applyPromptCache puts the cache outcome in the planned usage and cost.
func (r *request) applyPromptCache() {
	if r.pc == nil || r.resp == nil {
		return
	}
	u := &r.resp.Usage
	u.CachedTokens, u.CacheWriteTokens, u.CacheWrite1hTokens = r.pc.read, r.pc.write, r.pc.write1h
	setCost(r.model.Pricing, u)
}

// promptCacheCommit writes and renews the entries of r; the engine calls
// it when the response sends its first byte, once. A failure only logs.
func (e *Engine) promptCacheCommit(ctx context.Context, r *request) {
	st := r.pc
	if st == nil || len(st.commit) == 0 {
		return
	}
	entries := st.commit
	st.commit = nil
	if _, err := st.cacher.PromptCacheCommit(ctx, st.account, entries, time.Now()); err != nil {
		e.debug("prompt cache commit failed", err)
	}
}

// debug logs a store failure of the prompt cache, when there is a logger.
func (e *Engine) debug(msg string, err error) {
	if e.Log != nil {
		e.Log.Debug(msg, "err", err)
	}
}
