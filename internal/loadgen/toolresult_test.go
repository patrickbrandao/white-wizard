package loadgen

import (
	"regexp"
	"strings"
	"testing"
)

func TestToolResults(t *testing.T) {
	tr := ToolResults{Seed: 42}
	for _, name := range []string{"write_file", "Edit_File", "create_issue", "delete_file"} {
		if c, e := tr.For(1, 0, 0, name); c != "ok" || e {
			t.Fatalf("%s: %q %v", name, c, e)
		}
	}
	a, _ := tr.For(1, 2, 3, "read_file")
	b, _ := tr.For(1, 2, 3, "read_file")
	c, _ := tr.For(2, 2, 3, "read_file")
	if a != b || a == c || !strings.HasSuffix(a, ".") || len(strings.Fields(a)) < 24 || a[0] < 'A' || a[0] > 'Z' {
		t.Fatalf("lorem %q %q", a, c)
	}
	if IsWriteTool("read_file") || !IsWriteTool("WRITE") {
		t.Fatal("IsWriteTool")
	}
	// The error text must not look like a success to the scenarios.
	if regexp.MustCompile(`(?i)ok|written|success|saved|done`).MatchString(ToolResultError) {
		t.Fatal(ToolResultError)
	}
	all := ToolResults{Seed: 1, ErrorRate: 100}
	half := ToolResults{Seed: 1, ErrorRate: 50}
	errs := 0
	for i := range int64(2000) {
		if c, e := all.For(i, 0, 0, "write_file"); !e || c != ToolResultError {
			t.Fatal("rate 100")
		}
		if _, e := half.For(i, 1, 0, "read_file"); e {
			errs++
		}
	}
	if errs < 850 || errs > 1150 {
		t.Fatalf("rate 50: %d of 2000", errs)
	}
}
