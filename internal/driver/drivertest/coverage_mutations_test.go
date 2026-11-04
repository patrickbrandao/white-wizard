package drivertest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var sweepDrivers = []string{"openai", "anthropic", "openrouter", "opencode", "gemini", "azure"}

func mustNew0(name string) driver.Driver {
	d, err := driver.New(name, nil)
	if err != nil {
		panic(err)
	}
	return d
}

func mustNew(t *testing.T, name string) driver.Driver {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// catalog lists the mutations of one driver: what it writes (observed in a
// clean run) broken one member at a time, and its declared surface (routes,
// credential, policy, parsed request) broken one field at a time.
func catalog(t *testing.T, name string) []*mutation {
	t.Helper()
	obs := newObservation()
	if !runSuite(wrap(mustNew(t, name), &mutation{name: "observe", observe: obs})) {
		t.Fatalf("%s: the unmodified driver fails the suite", name)
	}
	// Only the values two runs agree on: ids, instants and the like vary.
	again := newObservation()
	if !runSuite(wrap(mustNew(t, name), &mutation{name: "observe", observe: again})) {
		t.Fatalf("%s: the unmodified driver fails the suite", name)
	}
	for v := range obs.values {
		if !again.values[v] || digits(v) >= 8 {
			delete(obs.values, v)
		}
	}
	var out []*mutation
	for _, k := range sortedKeys(obs.keys) {
		out = append(out, &mutation{name: "delete member " + k, json: deleteKey(k)})
	}
	for _, v := range sortedKeys(obs.values) {
		out = append(out, &mutation{name: "replace value " + v, json: replaceValue(v)})
	}
	hs := make([]string, 0, len(obs.headers))
	for h := range obs.headers {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	for _, h := range hs {
		out = append(out, &mutation{name: "drop header " + h, header: func(k, v string) (string, bool) { return v, strings.EqualFold(k, h) }})
	}
	codes := make([]int, 0, len(obs.statuses))
	for c := range obs.statuses {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		out = append(out, &mutation{name: fmt.Sprintf("status %d becomes %d", c, c+1), status: func(got int) int {
			if got == c {
				return c + 1
			}
			return got
		}})
	}
	for i := 0; i < 16; i++ {
		out = append(out,
			&mutation{name: fmt.Sprintf("drop frame %d", i), frame: func(j int, p []byte) [][]byte { return keepIf(j != i, p) }},
			&mutation{name: fmt.Sprintf("repeat frame %d", i), frame: func(j int, p []byte) [][]byte {
				if j == i {
					return [][]byte{p, p}
				}
				return [][]byte{p}
			}},
			&mutation{name: fmt.Sprintf("garble frame %d", i), frame: func(j int, p []byte) [][]byte {
				if j == i {
					return [][]byte{[]byte("data: {garbled\n\n")}
				}
				return [][]byte{p}
			}},
			&mutation{name: fmt.Sprintf("cut stream at frame %d", i), frame: func(j int, p []byte) [][]byte { return keepIf(j < i, p) }},
		)
	}
	for _, f := range requestFields() {
		out = append(out, &mutation{name: "zero request field " + f, req: zeroField(f)})
	}
	for _, f := range policyFields() {
		out = append(out, &mutation{name: "zero policy field " + f, policy: zeroPolicyField(f)})
	}
	for i := range mustNew(t, name).Routes() {
		out = append(out,
			&mutation{name: fmt.Sprintf("drop route %d", i), routes: func(rs []driver.Route) []driver.Route { return append(rs[:i], rs[i+1:]...) }},
			&mutation{name: fmt.Sprintf("wrong kind on route %d", i), routes: func(rs []driver.Route) []driver.Route { rs[i].Kind = "wrong"; return rs }},
			&mutation{name: fmt.Sprintf("wrong method on route %d", i), routes: func(rs []driver.Route) []driver.Route { rs[i].Method = "PATCH"; return rs }},
		)
	}
	out = append(out,
		&mutation{name: "no credential", auth: func(driver.Credential) driver.Credential { return driver.Credential{} }},
		&mutation{name: "wrong scheme", auth: func(c driver.Credential) driver.Credential { c.Scheme = "wrong"; return c }},
		&mutation{name: "no ids", hideIDs: true},
		&mutation{name: "constant ids", id: func(_, _ string) string { return "same" }},
		&mutation{name: "empty ids", id: func(_, _ string) string { return "" }},
		&mutation{name: "prefixed ids", id: func(_, v string) string { return "zz-" + v }},
		&mutation{name: "no model namer", hideNamer: true},
	)
	out = append(out, extraMutations(name, hs)...)
	return out
}

func keepIf(keep bool, p []byte) [][]byte {
	if keep {
		return [][]byte{p}
	}
	return nil
}

// digits counts the digits of v: values with many are instants and counters.
func digits(v string) int {
	n := 0
	for _, r := range v {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// goldenPath lists the mutations of a driver that the suite tolerates (it
// still passes): the members, headers and fields it does not check. The
// list is a snapshot of the suite's reach: a check that stops catching its
// mutation, or a new check that starts to, changes it and fails the test.
// WWZ_UPDATE_GOLDEN=1 rewrites the files after a deliberate change.
func goldenPath(name string) string {
	return filepath.Join("testdata", "tolerated_"+name+".txt")
}

// TestBrokenDriversFailTheContract runs the contract suite against each
// driver broken one thing at a time and demands that it fails, except for
// the recorded tolerated mutations.
func TestBrokenDriversFailTheContract(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the suite once per mutation")
	}
	if raceEnabled {
		t.Skip("serial sweep of thousands of suite runs: minutes under -race, with no concurrency to check")
	}
	for _, name := range sweepDrivers {
		t.Run(name, func(t *testing.T) {
			var tolerated []string
			for _, m := range catalog(t, name) {
				if os.Getenv("WWZ_TRACE") != "" {
					fmt.Fprintln(os.Stderr, "MUT", name, m.name)
				}
				if runSuite(wrap(mustNew(t, name), m)) {
					tolerated = append(tolerated, m.name)
				}
			}
			got := strings.Join(tolerated, "\n") + "\n"
			if os.Getenv("WWZ_UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(goldenPath(name), []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath(name))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("tolerated mutations changed (WWZ_UPDATE_GOLDEN=1 to accept):\n%s", diffLines(string(want), got))
			}
		})
	}
}

func diffLines(want, got string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		w[l] = true
	}
	var b strings.Builder
	for _, l := range strings.Split(got, "\n") {
		g[l] = true
		if !w[l] {
			b.WriteString("  now tolerated: " + l + "\n")
		}
	}
	for l := range w {
		if !g[l] {
			b.WriteString("  now caught: " + l + "\n")
		}
	}
	return b.String()
}
