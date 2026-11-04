package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
	"github.com/patrickbrandao/white-wizard/internal/generate"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// blocksTokens is what the blocks of pcAutoBlocks add up to.
func blocksTokens(tk generate.Tokenizer, blocks []pcBlock) int {
	n := 0
	for _, b := range blocks {
		for _, p := range b.parts {
			n += partTokens(tk, p)
		}
	}
	return n
}

// TestPCAutoBlocksMatchCountPrompt: on every request recorded with tools,
// schemas and call history (testdata/real/openai/tools/ and openrouter/
// tools/), the automatic cache blocks add up to generate.CountPrompt's
// total, and the hasher walks exactly that many tokens (task 246).
func TestPCAutoBlocksMatchCountPrompt(t *testing.T) {
	checked := 0
	for _, prov := range []string{"openai", "openrouter"} {
		d, err := driver.New(prov, nil)
		if err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "real", prov, "tools", "*.json"))
		if len(files) == 0 {
			t.Fatalf("%s: no tools fixtures", prov)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx struct {
				Request struct {
					Path string `json:"path"`
					Body string `json:"body"`
				} `json:"request"`
			}
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			kind := driver.KindChat
			if strings.HasSuffix(fx.Request.Path, "/responses") {
				kind = driver.KindResponses
			}
			req, perr := d.ParseRequest(kind, []byte(fx.Request.Body), driver.MapHeaders{})
			if perr != nil {
				continue // a validation probe
			}
			req.Driver = prov
			tk := tokenizerFor(&config.ModelConfig{ID: req.Model})
			want := generate.CountPrompt(tk, req, pcImageTokens).Total
			blocks := pcAutoBlocks(tk, req)
			if got := blocksTokens(tk, blocks); got != want {
				t.Errorf("%s/%s: blocks %d tokens, CountPrompt %d", prov, filepath.Base(f), got, want)
				continue
			}
			if h := newPCHasher(nil, []int{want}, []int{want}).hashes(tk, blocks); len(h) != 1 {
				t.Errorf("%s/%s: the hasher did not reach %d tokens", prov, filepath.Base(f), want)
			}
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("only %d requests checked", checked)
	}
}

// TestPCFitPart: a fit part is exactly n tokens, its text cut or completed
// with synthetic tokens; different texts hash differently.
func TestPCFitPart(t *testing.T) {
	tk := tokenizerFor(&config.ModelConfig{ID: "gpt-4o-mini"})
	text := "namespace functions { type a = () => any; }"
	full := tk.Count(text)
	hash := func(s string, n int) string {
		var b pcBlock
		b.fit(s, "tool", n)
		if got := partTokens(tk, b.parts[0]); got != n {
			t.Fatalf("fit %d tokens, want %d", got, n)
		}
		h := newPCHasher(nil, []int{n}, []int{n}).hashes(tk, []pcBlock{b})
		if len(h) != 1 {
			t.Fatalf("fit %d: no hash", n)
		}
		return h[0]
	}
	cut, padded := hash(text, full-3), hash(text, full+5)
	if cut == padded || hash(text, full+5) != padded {
		t.Fatal("fit hashes: cut and padded must differ and be stable")
	}
	if hash("namespace functions { type b = () => any; }", full+5) == padded {
		t.Fatal("other tools, same hash")
	}
	if partTokens(tk, pcPart{fit: true, n: -1}) != 0 {
		t.Fatal("negative fit")
	}
}

// pcPrefixHashes hashes blocks at every multiple of 128 from 1024 up to
// their total, as the OpenAI rule does.
func pcPrefixHashes(tk generate.Tokenizer, blocks []pcBlock) []string {
	var cuts []int
	for l := 1024; l <= blocksTokens(tk, blocks); l += 128 {
		cuts = append(cuts, l)
	}
	return newPCHasher([]string{"openai", "gpt-4o-mini"}, cuts, cuts).hashes(tk, blocks)
}

// commonPrefix is how many leading hashes a and b share.
func commonPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

const pcLongText = "The quick brown fox jumps over the lazy dog. "

// TestPCToolsFirst: the tools open the OpenAI prompt (ADR 0063, task 303).
// Changing only the tools invalidates the whole prefix; changing only the
// last message keeps every cut before it; the total stays CountPrompt's.
func TestPCToolsFirst(t *testing.T) {
	tk := tokenizerFor(&config.ModelConfig{ID: "gpt-4o-mini"})
	long := strings.Repeat(pcLongText, 300)
	mk := func(name, last string) *core.Request {
		return &core.Request{
			Model:  "gpt-4o-mini",
			System: "You are terse.",
			Tools: []core.ToolDef{{
				Name: name, Description: "Weather of a city",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			}},
			Messages: []core.Message{
				{Role: "user", Content: long},
				{Role: "assistant", Content: "Noted."},
				{Role: "user", Content: last},
			},
		}
	}
	base := mk("weather", "Weather in Recife?")
	blocks := pcAutoBlocks(tk, base)
	if p := blocks[0].parts[0]; !p.fit || p.syn != "tool" {
		t.Fatalf("first block %+v, want the tools", p)
	}
	if got, want := blocksTokens(tk, blocks), generate.CountPrompt(tk, base, pcImageTokens).Total; got != want {
		t.Fatalf("blocks %d tokens, CountPrompt %d", got, want)
	}
	h := pcPrefixHashes(tk, blocks)
	if len(h) < 3 {
		t.Fatalf("only %d cuts", len(h))
	}
	if n := commonPrefix(h, pcPrefixHashes(tk, pcAutoBlocks(tk, mk("forecast", "Weather in Recife?")))); n != 0 {
		t.Fatalf("other tools share %d cuts", n)
	}
	other := pcPrefixHashes(tk, pcAutoBlocks(tk, mk("weather", "And in Natal, tomorrow morning?")))
	if n := commonPrefix(h, other); n < len(h)-1 {
		t.Fatalf("other last message shares %d of %d cuts", n, len(h))
	}
	schema := &core.Request{
		Model: "gpt-4o-mini", Messages: base.Messages,
		ResponseFormat: "json_schema", ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	if p := pcAutoBlocks(tk, schema)[0].parts[0]; !p.fit || p.syn != "tool" {
		t.Fatalf("json_schema block %+v, want first", p)
	}
}

// TestPCHashSeed: h0 starts from "wwz-pc/2" (README 2.4, ADR 0063), so
// entries of the old order never hit.
func TestPCHashSeed(t *testing.T) {
	var b pcBlock
	b.raw("abcd")
	got := newPCHasher([]string{"openai", "m"}, []int{1}, []int{1}).hashes(nil, []pcBlock{b})
	h0 := sha256.Sum256([]byte("wwz-pc/2\x00openai\x00m"))
	seg := append(h0[:], 4)
	h1 := sha256.Sum256(append(seg, "abcd"...))
	if want := hex.EncodeToString(h1[:16]); len(got) != 1 || got[0] != want {
		t.Fatalf("hash %v, want %s", got, want)
	}
}

// TestPCGeminiToolsInvalidate: on Gemini the declarations follow the
// systemInstruction (gtok template); changing them keeps only the cuts
// inside the system and changing the last Content keeps those before it.
func TestPCGeminiToolsInvalidate(t *testing.T) {
	tk := gtok.Tokenizer{}
	long := strings.Repeat(pcLongText, 300)
	mk := func(name, last string) *core.Request {
		return &core.Request{
			System:   long,
			Tools:    []core.ToolDef{{Name: name, Description: "Weather of a city"}},
			Messages: []core.Message{{Role: "user", Content: long}, {Role: "user", Content: last}},
		}
	}
	base := mk("weather", "Recife?")
	blocks := pcTemplateBlocks(tk, base, gtok.Template{})
	if got, want := blocksTokens(tk, blocks), gtok.CountPrompt(tk, base).Total; got != want {
		t.Fatalf("blocks %d tokens, CountPrompt %d", got, want)
	}
	h := pcPrefixHashes(tk, blocks)
	inSystem := 0
	for l := 1024; l <= tk.Count(long); l += 128 {
		inSystem++
	}
	if n := commonPrefix(h, pcPrefixHashes(tk, pcTemplateBlocks(tk, mk("forecast", "Recife?"), gtok.Template{}))); n != inSystem {
		t.Fatalf("other tools share %d cuts, want %d (the system's)", n, inSystem)
	}
	if n := commonPrefix(h, pcPrefixHashes(tk, pcTemplateBlocks(tk, mk("weather", "And Natal, tomorrow?"), gtok.Template{}))); n < len(h)-1 {
		t.Fatalf("other last Content shares %d of %d cuts", n, len(h))
	}
}
