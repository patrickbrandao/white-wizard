package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// GenerationModes are the values generation.mode accepts (spec 003, spec
// 007, ADR 0068); "" resolves to random.
var GenerationModes = []string{"random", "echo", "lorem", "dataset", "template", "json"}

// validateGenerations checks generation.mode (and dataset_path with mode
// dataset) of every named profile and every inline model profile, citing the
// field path (task 381). Inline profiles that fail to decode were already
// reported by validateInline.
func (c *Config) validateGenerations() error {
	for _, name := range slices.Sorted(maps.Keys(c.Profiles.Generation)) {
		if err := validateGeneration("profiles.generation."+name, c.Profiles.Generation[name]); err != nil {
			return err
		}
	}
	for i := range c.Models {
		m := &c.Models[i]
		if !isInline(m.Generation) {
			continue
		}
		g, err := c.GenerationFor(m)
		if err != nil {
			return fmt.Errorf("config: model %q inline generation: %w", m.ID, err)
		}
		if err := validateGeneration(fmt.Sprintf("models[%s].generation", m.ID), g); err != nil {
			return err
		}
	}
	return nil
}

func validateGeneration(path string, g GenerationProfile) error {
	if g.Mode != "" && !slices.Contains(GenerationModes, g.Mode) {
		return fmt.Errorf("config: %s.mode: unknown mode %q (want random, echo, lorem, dataset, template or json)", path, g.Mode)
	}
	if g.Mode != "dataset" {
		return nil // dataset_path with another mode is accepted and ignored
	}
	if g.DatasetPath == "" {
		return fmt.Errorf("config: %s.dataset_path: required with mode dataset", path)
	}
	if _, err := ReadDataset(g.DatasetPath); err != nil {
		return fmt.Errorf("config: %s.dataset_path: %w", path, err)
	}
	return nil
}

// DatasetMessage is one turn of a dataset prompt.
type DatasetMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// DatasetEntry is one line of a generation.dataset_path file: the history
// it answers and the response text (spec 007, ADR 0068).
type DatasetEntry struct {
	Prompt   []DatasetMessage
	Response string
}

// maxDatasetLine bounds one JSONL line (16 MiB).
const maxDatasetLine = 16 << 20

// ReadDataset reads a JSONL dataset: one {"prompt": ..., "response": ...}
// object per line, prompt being a string (one user turn) or a list of
// {"role", "content"} turns; blank lines are skipped. A file that cannot be
// read, a malformed line or a file without entries is an error.
func ReadDataset(path string) ([]DatasetEntry, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxDatasetLine)
	var out []DatasetEntry
	for line := 1; sc.Scan(); line++ {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		e, err := parseDatasetLine(b)
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no entries", path)
	}
	return out, nil
}

func parseDatasetLine(b []byte) (DatasetEntry, error) {
	var raw struct {
		Prompt   json.RawMessage `json:"prompt"`
		Response *string         `json:"response"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return DatasetEntry{}, err
	}
	if raw.Response == nil || *raw.Response == "" {
		return DatasetEntry{}, errors.New(`"response" must be a non-empty string`)
	}
	e := DatasetEntry{Response: *raw.Response}
	var s string
	if err := json.Unmarshal(raw.Prompt, &s); err == nil && s != "" {
		e.Prompt = []DatasetMessage{{Role: "user", Content: s}}
		return e, nil
	}
	if err := json.Unmarshal(raw.Prompt, &e.Prompt); err != nil || len(e.Prompt) == 0 {
		return DatasetEntry{}, errors.New(`"prompt" must be a string or a non-empty list of {"role", "content"}`)
	}
	return e, nil
}
