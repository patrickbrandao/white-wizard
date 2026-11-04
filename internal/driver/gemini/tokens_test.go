package gemini

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
)

// The countTokens recordings of task 265 (testdata/real/gemini/count/):
// the template probes and the calibration corpus.
type countProbes struct {
	Probes []struct {
		ID          string `json:"id"`
		Model       string `json:"model"`
		Body        string `json:"body"`
		TotalTokens any    `json:"totalTokens"`
	} `json:"probes"`
	Texts []struct {
		ID          string `json:"id"`
		Text        string `json:"text"`
		TotalTokens int    `json:"totalTokens"`
	} `json:"texts"`
}

func loadCount(t *testing.T, name string) countProbes {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/real/gemini/count/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var p countProbes
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// textTokens are the real counts of the texts the probes use beyond
// one-token words (corpus and c1 probes: countTokens minus the content's
// overhead), so the template is checked apart from the tokenizer.
var textTokens = map[string]int{
	"Responda só: ok": 6, "Seja breve.": 4, "Hello world": 2, "The quick brown fox jumps over the lazy dog.": 10,
	"xxxxxxxxxx": 2, "Clima atual de uma cidade": 6, "a b c d": 4, "Recife": 2,
}

// joinedParts are the probes with several text parts in one Content: the
// real counts each part apart and the driver joins them, which merges at
// most one token per boundary.
var joinedParts = map[string]bool{"u2parts": true, "sys2parts": true, "thought": true}

// realTok counts with the real counts of textTokens and one token per word
// otherwise (the probes' words are one token each in the real).
type realTok struct{}

func (realTok) Count(s string) int {
	if n, ok := textTokens[s]; ok {
		return n
	}
	return gtok.Tokenizer{}.Count(s)
}

func (realTok) Split(s string) []string { return gtok.Tokenizer{}.Split(s) }

// TestRealCountTemplate: with the real text counts, the template gives the
// real totalTokens: exact for contents, system, calls, responses and media;
// function declarations within 2 tokens (their JSON goes through the
// approximate tokenizer); several text parts in a Content within 1.
func TestRealCountTemplate(t *testing.T) {
	d := &Driver{}
	for _, p := range loadCount(t, "template").Probes {
		want, ok := p.TotalTokens.(float64)
		if !ok {
			t.Fatalf("%s: %v", p.ID, p.TotalTokens)
		}
		h := driver.MapHeaders{"x-wwz-path": "/v1beta/models/" + p.Model + ":countTokens", "x-goog-api-key": "k"}
		req, e := d.ParseRequest(driver.KindGenerate, []byte(p.Body), h)
		if e != nil {
			t.Fatalf("%s: %v", p.ID, e)
		}
		got := gtok.CountPrompt(realTok{}, req).Total
		tol := 0
		if len(req.Tools) > 0 {
			tol = 2
		}
		if joinedParts[p.ID] {
			tol = 1
		}
		if math.Abs(float64(got)-want) > float64(tol) {
			t.Errorf("%s: %d tokens, real %.0f", p.ID, got, want)
		}
	}
}

// TestRealCountCorpus: the calibrated tokenizer against the corpus: mean
// error under 6%, sum within 3%.
func TestRealCountCorpus(t *testing.T) {
	var sumAbs, sumGot, sumReal float64
	texts := loadCount(t, "corpus").Texts
	for _, x := range texts {
		real := float64(x.TotalTokens - gtok.ContentOverhead)
		got := float64(gtok.Tokenizer{}.Count(x.Text))
		sumAbs += math.Abs(got-real) / real
		sumGot += got
		sumReal += real
	}
	mean, total := sumAbs/float64(len(texts)), (sumGot-sumReal)/sumReal
	t.Logf("mean |e| %.1f%%, sum %+.1f%%", mean*100, total*100)
	if mean > 0.06 || math.Abs(total) > 0.03 {
		t.Errorf("mean |e| %.1f%%, sum %+.1f%%", mean*100, total*100)
	}
}
