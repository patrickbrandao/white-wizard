package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// coreDriverPrefix is where the driver contract lives; the packages below
// it are concrete drivers and the dialect helpers only drivers import.
const coreDriverPrefix = "github.com/patrickbrandao/white-wizard/internal/driver"

// coreDriverAllowed are the packages under internal/driver the core may
// import: the contract and ids (opaque ids, no provider).
var coreDriverAllowed = map[string]bool{
	coreDriverPrefix:          true,
	coreDriverPrefix + "/ids": true,
}

// TestCoreImportsNoConcreteDriver (R15, spec 005 "driver novo não altera o
// núcleo"): the non-test dependencies of internal/server, internal/config,
// internal/engine and internal/core include no concrete driver and no
// dialect package, the Gemini tokenizer included (task 402); the core only
// calls the driver interfaces (RequestValidator, Declarer, EngineDeclarer).
func TestCoreImportsNoConcreteDriver(t *testing.T) {
	for _, pkg := range []string{"../server", "../config", "../engine", "../core"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep+"/", coreDriverPrefix+"/") && !coreDriverAllowed[dep] {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}

// TestEngineNamesNoDriver (R15, task 402): no non-test file of
// internal/engine names a registered driver in a string literal; the
// tokenizer, the prompt template and cache, the content filter and the
// malformed frame come from the driver's EngineDecl.
func TestEngineNamesNoDriver(t *testing.T) {
	names := driver.Names()
	if len(names) < 6 {
		t.Fatalf("only %v registered", names)
	}
	files, err := filepath.Glob("../engine/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no engine files: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.Contains(string(src), `"`+name+`"`) {
				t.Errorf("%s names the driver %q", filepath.Base(f), name)
			}
		}
	}
}
