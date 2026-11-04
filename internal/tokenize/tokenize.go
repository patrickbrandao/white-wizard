// Package tokenize is the byte-level BPE of OpenAI's tiktoken for the
// o200k_base and cl100k_base encodings (spec 007, ADR 0053), without a new
// dependency: the ranks are embedded compact and gzip-compressed (generated
// by gen_tables.go from the official files, sha256 checked), the
// pre-tokenizer is written by hand for each encoding's pattern, and a rank
// table is loaded only by the first call that uses it.
//
// Text is encoded like tiktoken's encode_ordinary: special tokens such as
// <|endoftext|> count as plain text.
package tokenize

//go:generate go run gen_tables.go

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"unicode/utf8"
)

//go:embed data/*.bin.gz
var tables embed.FS

// Encoding is one BPE vocabulary with its pre-tokenizer. It implements
// generate.Tokenizer (Count, Split) and the Each extension. The zero value
// is not usable; use O200k, Cl100k or Get.
type Encoding struct {
	name string
	next func(s string, i int) int // end of the pre-token starting at i

	once    sync.Once
	loadErr error
	ranks   map[string]uint32
	cache   *countCache
}

var (
	// O200k is o200k_base (gpt-4o, gpt-4.1, gpt-5, o-series, gpt-oss).
	O200k = &Encoding{name: "o200k_base", next: nextO200k}
	// Cl100k is cl100k_base (gpt-4, gpt-4-turbo, gpt-3.5-turbo, embeddings).
	Cl100k = &Encoding{name: "cl100k_base", next: nextCl100k}
)

// Get returns the encoding by name ("o200k_base" or "cl100k_base").
func Get(name string) (*Encoding, error) {
	switch name {
	case O200k.name:
		return O200k, nil
	case Cl100k.name:
		return Cl100k, nil
	}
	return nil, fmt.Errorf("tokenize: unknown encoding %q", name)
}

// Name is the tiktoken encoding name.
func (e *Encoding) Name() string { return e.name }

// cacheMin is the smallest text whose count goes through the cache: shorter
// ones tokenize faster than the cache lookup.
const cacheMin = 64

// cacheEntries is the size of each encoding's count cache (ADR 0053).
const cacheEntries = 64 << 10

// Load reads the rank table now instead of on the first call that needs it.
func (e *Encoding) Load() error {
	e.once.Do(func() {
		e.ranks, e.loadErr = readTable(tables, "data/"+e.name+".bin.gz")
		e.cache = newCountCache(cacheEntries)
	})
	return e.loadErr
}

func (e *Encoding) load() {
	if err := e.Load(); err != nil {
		panic(err) // the tables are embedded and tested: a broken build
	}
}

// tableMagic opens the compact table written by gen_tables.go.
const tableMagic = "WWZBPE1\n"

// readTable decodes a compact table: magic, count, then length+bytes per
// rank. The token strings share one backing string.
func readTable(fsys fs.FS, path string) (map[string]uint32, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("tokenize: %s: %w", path, err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("tokenize: %s: %w", path, err)
	}
	return parseTable(raw)
}

var errTable = errors.New("tokenize: corrupt rank table")

func parseTable(raw []byte) (map[string]uint32, error) {
	if !bytes.HasPrefix(raw, []byte(tableMagic)) {
		return nil, errTable
	}
	all := string(raw) // one copy; every token is a substring of it
	i := len(tableMagic)
	n, w := binary.Uvarint(raw[i:])
	if w <= 0 || n == 0 || n > 1<<24 {
		return nil, errTable
	}
	i += w
	ranks := make(map[string]uint32, n)
	for r := range uint32(n) {
		l, w := binary.Uvarint(raw[i:])
		if w <= 0 || l == 0 || uint64(len(raw)-i-w) < l { //nolint:gosec // G115: i+w <= len(raw)
			return nil, errTable
		}
		i += w
		end := i + int(l) //nolint:gosec // G115: l fits in the rest of raw (checked above)
		ranks[all[i:end]] = r
		i = end
	}
	if i != len(raw) || len(ranks) != int(n) {
		return nil, errTable
	}
	return ranks, nil
}

// Encode returns the token ids of text (tiktoken encode_ordinary).
func (e *Encoding) Encode(text string) []uint32 {
	e.load()
	var out []uint32
	var buf [maxLinear + 1]part
	for i := 0; i < len(text); {
		j := e.next(text, i)
		piece := text[i:j]
		if r, ok := e.ranks[piece]; ok {
			out = append(out, r)
		} else {
			parts := e.merge(piece, buf[:0])
			for k := 0; k+1 < len(parts); k++ {
				out = append(out, e.ranks[piece[parts[k].start:parts[k+1].start]])
			}
		}
		i = j
	}
	return out
}

// Count returns the number of tokens of text. Texts of cacheMin bytes or
// more go through the encoding's LRU cache (load generators repeat prompts).
func (e *Encoding) Count(text string) int {
	e.load()
	if len(text) < cacheMin {
		return e.count(text)
	}
	if n, ok := e.cache.get(text); ok {
		return n
	}
	n := e.count(text)
	e.cache.put(text, n)
	return n
}

func (e *Encoding) count(text string) int {
	n := 0
	var buf [maxLinear + 1]part
	for i := 0; i < len(text); {
		j := e.next(text, i)
		if _, ok := e.ranks[text[i:j]]; ok {
			n++
		} else {
			n += len(e.merge(text[i:j], buf[:0])) - 1
		}
		i = j
	}
	return n
}

// Each calls fn with the tokens of text, in order, as substrings of text.
// Tokens that end inside a UTF-8 character are joined with the next ones,
// so every piece is valid UTF-8 (streaming never splits a character).
func (e *Encoding) Each(text string, fn func(tok string)) {
	e.load()
	start := 0 // start of the pending output piece
	var buf [maxLinear + 1]part
	cut := func(at int) {
		if at > start && (at == len(text) || utf8.RuneStart(text[at])) {
			fn(text[start:at])
			start = at
		}
	}
	for i := 0; i < len(text); {
		j := e.next(text, i)
		if _, ok := e.ranks[text[i:j]]; !ok {
			parts := e.merge(text[i:j], buf[:0])
			for k := 1; k+1 < len(parts); k++ {
				cut(i + int(parts[k].start))
			}
		}
		cut(j)
		i = j
	}
}

// Split returns the pieces Each produces; their concatenation is text.
func (e *Encoding) Split(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	e.Each(text, func(tok string) { out = append(out, tok) })
	return out
}
