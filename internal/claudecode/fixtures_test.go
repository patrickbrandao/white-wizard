package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

const fixtureDir = "../../testdata/real/claude-code/2.1.308"

// manifestRow matches a row of MANIFEST.md: | `file.raw` | … | `sha256` |.
var manifestRow = regexp.MustCompile("^\\| `([^`]+\\.raw)` \\|.*\\| `([0-9a-f]{64})` \\|$")

// manifest returns file → sha256 from MANIFEST.md.
func manifest(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "MANIFEST.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if m := manifestRow.FindSubmatch(line); m != nil {
			rows[string(m[1])] = string(m[2])
		}
	}
	return rows
}

type fixture struct {
	name string
	raw  []byte
	head Head
	body []byte
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*.raw"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var out []fixture
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		h, body, err := ParseHead(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, fixture{name: filepath.Base(f), raw: raw, head: h, body: body})
	}
	return out
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	for _, f := range fixtures(t) {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("fixture %s not found", name)
	return fixture{}
}

// TestRealFixturesClaudeCode is the acceptance test of spec 020 for the
// package: every fixture is rebuilt byte for byte from its inputs, its body
// is its own JSON.stringify, and Verify accepts every inference request.
func TestRealFixturesClaudeCode(t *testing.T) {
	rows := manifest(t)
	fs := fixtures(t)
	var names []string
	for _, f := range fs {
		names = append(names, f.name)
	}
	var listed []string
	for name := range rows {
		listed = append(listed, name)
	}
	slices.Sort(names)
	slices.Sort(listed)
	if !slices.Equal(names, listed) {
		t.Fatalf("MANIFEST.md lists %v, the directory has %v", listed, names)
	}
	types := map[Type]int{}
	for _, f := range fs {
		t.Run(f.name, func(t *testing.T) {
			sum := sha256.Sum256(f.raw)
			if got := hex.EncodeToString(sum[:]); got != rows[f.name] {
				t.Fatalf("sha256 %s, MANIFEST.md says %s", got, rows[f.name])
			}
			r, err := Extract(f.head, f.body)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			types[r.Type]++
			head, body, err := Build(r)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			got := append(head, body...)
			if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != rows[f.name] {
				i := firstDiff(got, f.raw)
				t.Fatalf("Build differs at byte %d: got %q, want %q", i,
					got[i:min(len(got), i+60)], f.raw[i:min(len(f.raw), i+60)])
			}
			if r.Type == TypeHello {
				if reason, _, ok := Verify(f.head, f.body); ok || reason != ReasonQuery {
					t.Fatalf("Verify of the probe = %q, %v; want query", reason, ok)
				}
				return
			}
			v, err := Parse(f.body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !bytes.Equal(Marshal(v), f.body) {
				t.Fatal("JSON.stringify of the ordered decode differs from the body")
			}
			if reason, detail, ok := Verify(f.head, f.body); !ok {
				t.Fatalf("Verify refused: %s: %s", reason, detail)
			}
		})
	}
	for _, typ := range []Type{TypeHello, TypeTurn, TypeTurnSafeguards, TypeQuota, TypeCountTokens} {
		if types[typ] == 0 {
			t.Errorf("no fixture of type %s", typ)
		}
	}
}
