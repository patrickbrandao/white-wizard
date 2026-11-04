package generate

import (
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// Dataset is a loaded generation.dataset_path file (mode dataset, spec 007,
// ADR 0068): responses indexed by the hash of the normalized history. It is
// read-only after LoadDataset and shared by every request.
type Dataset struct {
	responses []string
	byKey     map[uint64]int // HistoryKey → first entry with that history
}

// Len is the number of entries.
func (d *Dataset) Len() int { return len(d.responses) }

// Response returns the entry whose prompt has the same normalized history as
// msgs; without a match, an entry drawn with rng (the request seed), so the
// same seed picks the same entry.
func (d *Dataset) Response(rng *RNG, msgs []core.Message) (text string, matched bool) {
	if i, ok := d.byKey[HistoryKey(msgs)]; ok {
		return d.responses[i], true
	}
	return d.responses[rng.IntN(len(d.responses))], false
}

// NewDataset indexes entries read by config.ReadDataset.
func NewDataset(entries []config.DatasetEntry) *Dataset {
	d := &Dataset{responses: make([]string, len(entries)), byKey: make(map[uint64]int, len(entries))}
	msgs := []core.Message{}
	for i, e := range entries {
		d.responses[i] = e.Response
		msgs = msgs[:0]
		for _, m := range e.Prompt {
			msgs = append(msgs, core.Message{Role: m.Role, Content: m.Content})
		}
		k := HistoryKey(msgs)
		if _, dup := d.byKey[k]; !dup {
			d.byKey[k] = i
		}
	}
	return d
}

type datasetFile struct {
	size  int64
	mtime time.Time
	ds    *Dataset
}

var (
	datasetMu    sync.Mutex
	datasetCache = map[string]datasetFile{}
)

// LoadDataset reads and indexes a dataset once per file version: a path
// whose size and modification time did not change returns the same
// *Dataset. A file that became unreadable keeps the last good version.
func LoadDataset(path string) (*Dataset, error) {
	path = filepath.Clean(path)
	datasetMu.Lock()
	defer datasetMu.Unlock()
	cached, ok := datasetCache[path]
	st, err := os.Stat(path)
	if err != nil {
		if ok {
			return cached.ds, nil
		}
		return nil, err
	}
	if ok && cached.size == st.Size() && cached.mtime.Equal(st.ModTime()) {
		return cached.ds, nil
	}
	entries, err := config.ReadDataset(path)
	if err != nil {
		if ok {
			return cached.ds, nil
		}
		return nil, err
	}
	ds := NewDataset(entries)
	datasetCache[path] = datasetFile{size: st.Size(), mtime: st.ModTime(), ds: ds}
	return ds, nil
}

// HistoryKey hashes the normalized conversation (FNV-1a 64): only user and
// assistant turns with text count (system prompts, tool results and empty
// turns are skipped; Gemini's "model" is "assistant"), and the whitespace of
// each text is collapsed to single spaces and trimmed. It does not allocate.
func HistoryKey(msgs []core.Message) uint64 {
	h := uint64(14695981039346656037)
	for i := range msgs {
		role := msgs[i].Role
		switch role {
		case "user", "assistant":
		case "model":
			role = "assistant"
		default:
			continue
		}
		if !hasText(msgs[i].Content) {
			continue
		}
		h = fnvString(h, role)
		h = fnvByte(h, 0)
		h = fnvNormalized(h, msgs[i].Content)
		h = fnvByte(h, 1)
	}
	return h
}

func hasText(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

func fnvByte(h uint64, b byte) uint64 { return (h ^ uint64(b)) * 1099511628211 }

func fnvString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h = fnvByte(h, s[i])
	}
	return h
}

// fnvNormalized hashes s with runs of whitespace as one space and without
// leading or trailing whitespace.
func fnvNormalized(h uint64, s string) uint64 {
	space, started := false, false
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			space = started
		} else {
			if space {
				h = fnvByte(h, ' ')
				space = false
			}
			started = true
			h = fnvString(h, s[i:i+n])
		}
		i += n
	}
	return h
}
