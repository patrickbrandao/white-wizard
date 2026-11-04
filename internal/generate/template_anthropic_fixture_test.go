package generate_test

import (
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// probeCounts maps the text of every probe_* fixture (a lone user message)
// to its real count: the probe minus a user message without text (the
// count of "x" minus 1; "x" is one token in every Claude tokenizer).
func probeCounts(t *testing.T, fxs map[string]*toolFixture) map[string]int {
	t.Helper()
	x := fxs["x"]
	if x == nil {
		x = fxs["base_x"]
	}
	if x == nil {
		t.Fatal("no x fixture")
	}
	frame := x.tokens - 1
	counts := map[string]int{"x": 1}
	for name, fx := range fxs {
		if !strings.HasPrefix(name, "probe_") {
			continue
		}
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(fx.Request.Body), &body) != nil || len(body.Messages) != 1 {
			t.Fatalf("%s: bad probe", name)
		}
		counts[body.Messages[0].Content] = fx.tokens - frame
	}
	return counts
}

// TestRealFixturesAnthropicTemplate checks the Claude template (task 264)
// against count_tokens of every model in testdata/real/anthropic/template/:
// with the real counts of the texts (probes), CountPrompt gives the real
// input_tokens of each case exactly: system, turns, joins, prefill, tools
// and the tool-use history.
func TestRealFixturesAnthropicTemplate(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "anthropic", "template", "claude-*"))
	if err != nil || len(dirs) < 14 {
		t.Fatalf("model dirs: %d %v", len(dirs), err)
	}
	total := 0
	for _, dir := range dirs {
		fxs := loadFixtureDir(t, dir)
		ora := oracle{t: t, counts: probeCounts(t, fxs)}
		names := make([]string, 0, len(fxs))
		for name := range fxs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if strings.HasPrefix(name, "probe_") {
				continue
			}
			fx := fxs[name]
			req := parseAnthropic(t, fx)
			got := generate.CountPrompt(ora, req, 0)
			if got.Total != fx.tokens {
				t.Errorf("%s/%s: %d, real %d (%+v)", filepath.Base(dir), name, got.Total, fx.tokens, got)
			}
			total++
		}
	}
	if total < 300 {
		t.Fatalf("cases: %d", total)
	}
}

// TestRealFixturesClaudeTokenizer measures the simulated tokenizer against
// count_tokens of 12 texts on each Claude generation
// (testdata/real/anthropic/tokenizer/, task 301): the text of a probe costs
// its total minus the frame of a lone "x" (1 token). Calibrated by
// generation (ADR 0053), the mean error per text stays under 16% and the
// sum of the texts within 4% of the real one; Claude 4.7 on counts ~32%
// more than 4.5, and unscaled the simulated missed it by 24% (26.6% per
// text).
func TestRealFixturesClaudeTokenizer(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "..", "testdata", "real", "anthropic", "tokenizer", "claude-*"))
	if err != nil || len(dirs) < 11 {
		t.Fatalf("model dirs: %d %v", len(dirs), err)
	}
	for _, dir := range dirs {
		fxs := loadFixtureDir(t, dir)
		x := fxs["x"]
		frameReal := x.tokens - 1
		frameSim := generate.CountPrompt(generate.Simulated{}, parseAnthropic(t, x), 0).Total - 1
		var errSum float64
		realSum, simSum, n := 0, 0, 0
		for name, fx := range fxs {
			if name == "x" {
				continue
			}
			real := fx.tokens - frameReal
			sim := generate.CountPrompt(generate.Simulated{}, parseAnthropic(t, fx), 0).Total - frameSim
			errSum += math.Abs(float64(sim-real)) / float64(real)
			realSum += real
			simSum += sim
			n++
		}
		mean := 100 * errSum / float64(n)
		total := 100 * float64(simSum-realSum) / float64(realSum)
		t.Logf("%s: %d texts, mean error %.1f%%, sum %d vs real %d (%+.1f%%)", filepath.Base(dir), n, mean, simSum, realSum, total)
		if n != 12 || mean > 16 || math.Abs(total) > 4 {
			t.Errorf("%s: %d texts, mean error %.1f%%, sum %+.1f%%", filepath.Base(dir), n, mean, total)
		}
	}
}
