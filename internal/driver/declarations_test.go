package driver_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// declGolden is the copy of the six declarations internal/config's tests
// register (they cannot import the drivers, which import config).
const declGolden = "../config/testdata/driver_decls.json"

// TestDeclarationsMatchConfigFixture: what the drivers declare is what the
// configuration tests assume (WWZ_UPDATE_GOLDEN=1 rewrites the copy).
func TestDeclarationsMatchConfigFixture(t *testing.T) {
	got := map[string]config.DriverDecl{}
	for _, name := range []string{"anthropic", "azure", "gemini", "openai", "opencode", "openrouter"} {
		d, err := driver.New(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		x, ok := driver.As[driver.Declarer](d)
		if !ok {
			t.Fatalf("%s declares nothing", name)
		}
		got[name] = x.Declaration()
	}
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if os.Getenv("WWZ_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(declGolden, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(declGolden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(b) {
		t.Fatalf("declarations changed (WWZ_UPDATE_GOLDEN=1 to accept):\n%s", b)
	}
}
