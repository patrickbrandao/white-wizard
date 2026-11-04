package tokenize

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

type golden struct {
	Generator string `json:"generator"`
	Cases     []struct {
		Text   string   `json:"text"`
		O200k  []uint32 `json:"o200k_base"`
		Cl100k []uint32 `json:"cl100k_base"`
	} `json:"cases"`
}

// loadGolden reads the pairs text -> tokens generated outside the module
// with the reference tiktoken (encode_ordinary) from the official files.
func loadGolden(t testing.TB) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 500 {
		t.Fatalf("only %d golden cases", len(g.Cases))
	}
	return g
}

// TestGolden: Encode, Count and Split agree with tiktoken on every golden
// fixture, for both encodings (spec 007, ADR 0053).
func TestGolden(t *testing.T) {
	g := loadGolden(t)
	for _, enc := range []*Encoding{O200k, Cl100k} {
		for i, c := range g.Cases {
			want := c.O200k
			if enc == Cl100k {
				want = c.Cl100k
			}
			if got := enc.Encode(c.Text); !slices.Equal(got, want) {
				t.Errorf("%s case %d %q:\n got %v\nwant %v", enc.Name(), i, c.Text, got, want)
				continue
			}
			if got := enc.Count(c.Text); got != len(want) {
				t.Errorf("%s case %d %q: Count %d, want %d", enc.Name(), i, c.Text, got, len(want))
			}
			if got := enc.Count(c.Text); got != len(want) { // cached
				t.Errorf("%s case %d: cached Count %d, want %d", enc.Name(), i, got, len(want))
			}
			parts := enc.Split(c.Text)
			if strings.Join(parts, "") != c.Text || len(parts) > len(want) {
				t.Errorf("%s case %d %q: Split %q", enc.Name(), i, c.Text, parts)
			}
			for _, p := range parts {
				if p == "" || !utf8.ValidString(p) {
					t.Errorf("%s case %d: Split piece %q is not valid UTF-8", enc.Name(), i, p)
				}
			}
		}
	}
}

// TestProbe: the drift probe is 6 o200k tokens (13 prompt_tokens with the
// chat template, as the real OpenAI answered).
func TestProbe(t *testing.T) {
	if n := O200k.Count("Responda só: ok"); n != 6 {
		t.Fatalf("probe = %d tokens, want 6", n)
	}
}

// TestSplitUTF8: tokens that end inside a character are joined; ASCII text
// splits on every token boundary.
func TestSplitUTF8(t *testing.T) {
	text := "🎉🎉🎉 olá"
	parts := O200k.Split(text)
	for _, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("piece %q", p)
		}
	}
	if strings.Join(parts, "") != text {
		t.Fatalf("join %q", parts)
	}
	if got := O200k.Split("hello world, again"); !slices.Equal(got, []string{"hello", " world", ",", " again"}) {
		t.Fatalf("split %q", got)
	}
	if O200k.Split("") != nil {
		t.Fatal("empty text has no pieces")
	}
}

// TestMergeHeapEqualsLinear: the heap merge used for long pieces picks the
// same pairs as tiktoken's quadratic scan.
func TestMergeHeapEqualsLinear(t *testing.T) {
	r := rand.New(rand.NewPCG(196, 1))
	alpha := []string{"a", "b", "ab", "the", "ing", "é", "日", "x", "!", "zz"}
	for _, enc := range []*Encoding{O200k, Cl100k} {
		enc.load()
		for range 300 {
			var b strings.Builder
			for b.Len() < 2+r.IntN(maxLinear) {
				b.WriteString(alpha[r.IntN(len(alpha))])
			}
			s := b.String()
			lin := enc.mergeLinear(s, nil)
			heap := enc.mergeHeap(s)
			if len(lin) != len(heap) {
				t.Fatalf("%s %q: linear %d parts, heap %d", enc.Name(), s, len(lin), len(heap))
			}
			for k := range lin {
				if lin[k].start != heap[k].start {
					t.Fatalf("%s %q: boundaries differ at %d", enc.Name(), s, k)
				}
			}
		}
	}
}

func TestGet(t *testing.T) {
	for _, name := range []string{"o200k_base", "cl100k_base"} {
		e, err := Get(name)
		if err != nil || e.Name() != name || e.Load() != nil {
			t.Fatalf("Get(%q) = %v, %v", name, e, err)
		}
	}
	if _, err := Get("p50k_base"); err == nil {
		t.Fatal("p50k_base is not embedded")
	}
}

// TestBrokenTable: a missing or corrupt table fails Load and panics on use.
func TestBrokenTable(t *testing.T) {
	e := &Encoding{name: "missing", next: nextO200k}
	if e.Load() == nil {
		t.Fatal("missing table loaded")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Count on a broken table did not panic")
		}
	}()
	e.Count("x")
}

func TestParseTable(t *testing.T) {
	good := tableMagic + "\x02\x01a\x02bc"
	ranks, err := parseTable([]byte(good))
	if err != nil || ranks["a"] != 0 || ranks["bc"] != 1 {
		t.Fatalf("%v %v", ranks, err)
	}
	for _, bad := range []string{
		"", "WWZBPE0\n\x01\x01a", tableMagic, tableMagic + "\x00",
		tableMagic + "\x02\x01a", tableMagic + "\x01\x05ab", tableMagic + "\x01\x00",
		tableMagic + "\x01\x01a!", tableMagic + "\x02\x01a\x01a",
	} {
		if _, err := parseTable([]byte(bad)); err == nil {
			t.Errorf("parseTable(%q) accepted", bad)
		}
	}
}

func TestReadTableErrors(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(tableMagic + "\x01\x01a"))
	w.Close()
	truncated := gz.Bytes()[:gz.Len()-6]
	fsys := fstest.MapFS{"plain": {Data: []byte("not gzip")}, "cut.gz": {Data: truncated}, "ok.gz": {Data: gz.Bytes()}}
	for _, name := range []string{"missing", "plain", "cut.gz"} {
		if _, err := readTable(fsys, name); err == nil {
			t.Errorf("readTable(%s) accepted", name)
		}
	}
	if r, err := readTable(fsys, "ok.gz"); err != nil || r["a"] != 0 {
		t.Fatalf("ok.gz: %v %v", r, err)
	}
}

// TestCacheLRU: the least recently used entry is evicted; touched ones stay.
func TestCacheLRU(t *testing.T) {
	c := newCountCache(cacheShards) // one entry per shard
	texts := make([]string, 0, 200)
	for i := range 200 {
		texts = append(texts, strings.Repeat("w", i+1))
	}
	for i, s := range texts {
		c.put(s, i)
	}
	hits := 0
	for i, s := range texts {
		if n, ok := c.get(s); ok {
			hits++
			if n != i {
				t.Fatalf("%d: count %d", i, n)
			}
		}
	}
	if hits == 0 || hits > cacheShards {
		t.Fatalf("%d hits with %d slots", hits, cacheShards)
	}
	c = newCountCache(3 * cacheShards)
	sh := func(s string) *cacheShard { _, x := c.key(s); return x }
	// Three texts on the same shard plus a fourth: the untouched one goes.
	var same []string
	for i := 0; len(same) < 4; i++ {
		s := strings.Repeat("q", i+1)
		if len(same) == 0 || sh(s) == sh(same[0]) {
			same = append(same, s)
		}
	}
	c.put(same[0], 0)
	c.put(same[1], 1)
	c.put(same[2], 2)
	c.get(same[1])     // from the middle of the list
	c.get(same[0])     // most recent now
	c.put(same[1], 11) // update in place
	c.put(same[3], 3)  // evicts same[2]
	if _, ok := c.get(same[2]); ok {
		t.Fatal("LRU entry not evicted")
	}
	for i, want := range map[int]int{0: 0, 1: 11, 3: 3} {
		if n, ok := c.get(same[i]); !ok || n != want {
			t.Fatalf("same[%d] = %d %v", i, n, ok)
		}
	}
	if newCountCache(0) == nil {
		t.Fatal("zero size")
	}
}

func benchText() string {
	return strings.Repeat("O roteador de modelos recebe milhares de requisições por minuto e decide, em milissegundos, para qual provedor encaminhar cada chamada; it's 9:41 and p99 is 310ms. ", 8)[:1024]
}

// BenchmarkO200k1KB is the uncached cost of a 1 KB prompt (meta ≤ 20
// µs/KB, ADR 0053).
func BenchmarkO200k1KB(b *testing.B) {
	text := benchText()
	O200k.load()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		O200k.count(text)
	}
}

// TestConcurrentCount: the lazy load and the cache are safe for concurrent
// use (run with -race).
func TestConcurrentCount(t *testing.T) {
	enc := &Encoding{name: "cl100k_base", next: nextCl100k} // fresh: loads under contention
	text := benchText()
	done := make(chan int)
	for g := range 8 {
		go func() {
			n := 0
			for i := range 200 {
				n += enc.Count(text[:len(text)-(g+i)%100])
			}
			done <- n
		}()
	}
	for range 8 {
		<-done
	}
	if enc.Count(text) != Cl100k.count(text) {
		t.Fatal("cached count differs")
	}
}

func BenchmarkCl100k1KB(b *testing.B) {
	text := benchText()
	Cl100k.load()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		Cl100k.count(text)
	}
}

// BenchmarkO200k1KBCached is the same prompt through the LRU.
func BenchmarkO200k1KBCached(b *testing.B) {
	text := benchText()
	O200k.Count(text)
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		O200k.Count(text)
	}
}

func BenchmarkO200kLoad(b *testing.B) {
	for b.Loop() {
		if _, err := readTable(tables, "data/o200k_base.bin.gz"); err != nil {
			b.Fatal(err)
		}
	}
}
