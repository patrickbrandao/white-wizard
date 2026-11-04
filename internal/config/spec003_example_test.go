package config

import (
	"os"
	"strings"
	"testing"
)

// The complete YAML example of spec 003 must parse in strict mode.
func TestSpec003ExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../specs/003-configuracao.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	start := strings.Index(doc, "```yaml\n")
	if start < 0 {
		t.Fatal("no yaml block in spec 003")
	}
	body := doc[start+len("```yaml\n"):]
	end := strings.Index(body, "\n```")
	if end < 0 {
		t.Fatal("unterminated yaml block")
	}
	cfg, err := Parse([]byte(body[:end]))
	if err != nil {
		t.Fatalf("spec 003 example: %v", err)
	}
	if !cfg.Scenarios.UseBuiltin() {
		t.Error("scenarios.builtin: true lost")
	}
	off, err := Parse([]byte("scenarios: {builtin: false}\n"))
	if err != nil || off.Scenarios.UseBuiltin() {
		t.Errorf("builtin false: %v", err)
	}
	if def, _ := Parse(nil); def == nil || !def.Scenarios.UseBuiltin() {
		t.Error("builtin must default to true")
	}
}
