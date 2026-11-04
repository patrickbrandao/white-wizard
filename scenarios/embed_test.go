package scenarios

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

func TestEmbeddedScenarios(t *testing.T) {
	want := []string{"file-agent-flow", "long-reasoning", "multi-tool-parallel", "refusal", "retry-on-error", "search-then-answer"}
	files := Files()
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d", len(files), len(want))
	}
	for i, f := range files {
		s, err := scenario.ParseYAML(f.Data)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if s.Name != want[i] || f.Name != want[i]+".yaml" {
			t.Errorf("file %s name %s, want %s", f.Name, s.Name, want[i])
		}
		rep := s.Check(scenario.CheckOptions{})
		if len(rep.Errors) > 0 || len(rep.Warnings) > 0 {
			t.Errorf("%s: errors %v warnings %v", f.Name, rep.Errors, rep.Warnings)
		}
		disk, err := os.ReadFile(filepath.Clean(f.Name))
		if err != nil || string(disk) != string(f.Data) {
			t.Errorf("%s: embedded copy differs from the file", f.Name)
		}
	}
}
