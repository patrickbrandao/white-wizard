package generate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestHistoryKeyNormalized: system prompts, tool turns and empty turns do
// not count, "model" is "assistant" and whitespace is collapsed.
func TestHistoryKeyNormalized(t *testing.T) {
	base := []core.Message{{Role: "user", Content: "hi there"}, {Role: "assistant", Content: "hello"}}
	same := []core.Message{
		{Role: "system", Content: "be nice"},
		{Role: "user", Content: "  hi \n\t there  "},
		{Role: "tool", Content: "ignored"},
		{Role: "assistant", Content: "   "},
		{Role: "model", Content: "hello"},
	}
	if HistoryKey(base) != HistoryKey(same) {
		t.Fatal("normalized histories differ")
	}
	for _, other := range [][]core.Message{
		{{Role: "user", Content: "hi there"}},
		{{Role: "user", Content: "hithere"}, {Role: "assistant", Content: "hello"}},
		{{Role: "assistant", Content: "hi there"}, {Role: "assistant", Content: "hello"}},
		{{Role: "user", Content: "hi there hello"}},
	} {
		if HistoryKey(other) == HistoryKey(base) {
			t.Fatalf("collision: %+v", other)
		}
	}
	if HistoryKey(nil) != HistoryKey([]core.Message{{Role: "system", Content: "x"}}) {
		t.Fatal("empty histories differ")
	}
	if a := testing.AllocsPerRun(100, func() { HistoryKey(same) }); a != 0 {
		t.Fatalf("HistoryKey allocates: %v", a)
	}
}

// TestDatasetResponse: a matching history returns its entry (the first one
// when two entries share it); otherwise the seed picks an entry.
func TestDatasetResponse(t *testing.T) {
	d := NewDataset([]config.DatasetEntry{
		{Prompt: []config.DatasetMessage{{Role: "user", Content: "q"}}, Response: "first"},
		{Prompt: []config.DatasetMessage{{Role: "user", Content: " q "}}, Response: "dup"},
		{Prompt: []config.DatasetMessage{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}, Response: "multi"},
	})
	if d.Len() != 3 {
		t.Fatal(d.Len())
	}
	rng := NewRNG(1, "x")
	if got, ok := d.Response(rng, []core.Message{{Role: "user", Content: "q"}}); !ok || got != "first" {
		t.Fatal(got, ok)
	}
	if got, ok := d.Response(rng, []core.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}); !ok || got != "multi" {
		t.Fatal(got, ok)
	}
	miss := []core.Message{{Role: "user", Content: "nothing"}}
	a, ok := d.Response(NewRNG(7, "r"), miss)
	b, _ := d.Response(NewRNG(7, "r"), miss)
	if ok || a != b {
		t.Fatal(a, b, ok)
	}
}

// TestLoadDatasetCached: one *Dataset per file version; a changed file is
// read again, a file that disappears or breaks keeps the last good version,
// and a path never loaded reports the error.
func TestLoadDatasetCached(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.jsonl")
	write := func(body string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Unix(1_700_000_000, 0)
	write(`{"prompt":"q","response":"v1"}`, t0)
	d1, err := LoadDataset(p)
	if err != nil {
		t.Fatal(err)
	}
	if d2, _ := LoadDataset(p); d2 != d1 {
		t.Fatal("same file read twice")
	}
	write(`{"prompt":"q","response":"v2"}`, t0.Add(time.Second))
	d3, err := LoadDataset(p)
	if err != nil || d3 == d1 || d3.responses[0] != "v2" {
		t.Fatal(err, d3)
	}
	write(`broken`, t0.Add(2*time.Second))
	if d, err := LoadDataset(p); err != nil || d != d3 {
		t.Fatal("broken file must keep the last good version", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if d, err := LoadDataset(p); err != nil || d != d3 {
		t.Fatal("removed file must keep the last good version", err)
	}
	if _, err := LoadDataset(filepath.Join(dir, "never")); err == nil {
		t.Fatal("missing file")
	}
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(bad); err == nil {
		t.Fatal("malformed file")
	}
}
