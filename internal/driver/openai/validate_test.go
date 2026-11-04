package openai_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
)

// models knows every model but those named nope*.
type models struct{}

func (models) KnownModel(_ driver.Kind, name string) bool { return !strings.HasPrefix(name, "nope") }

func (models) Model(k driver.Kind, name string) (known, retired bool) {
	return models{}.KnownModel(k, name), false
}

func validator(t *testing.T, name string) driver.RequestValidator {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := driver.As[driver.RequestValidator](d)
	if !ok {
		t.Fatalf("%s has no validator", name)
	}
	return v
}

func declaration(t *testing.T, name string) config.DriverDecl {
	t.Helper()
	d, err := driver.New(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	x, ok := driver.As[driver.Declarer](d)
	if !ok {
		t.Fatalf("%s declares nothing", name)
	}
	return x.Declaration()
}

// TestValidateRequest: the driver runs the OpenAI checks (oaicheck) on
// Chat Completions, legacy completions and Responses; the cases are in
// oaicheck and, over HTTP, in internal/server.
func TestValidateRequest(t *testing.T) {
	v := validator(t, "openai")
	if e := v.ValidateRequest(driver.KindChat, []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"x":1}`), nil, models{}); e == nil || e.Message != "Unrecognized request argument supplied: x" {
		t.Fatalf("chat: %+v", e)
	}
	if e := v.ValidateRequest(driver.KindModels, []byte(`x`), nil, models{}); e != nil {
		t.Fatalf("models: %+v", e)
	}
}

func TestDeclaration(t *testing.T) {
	d := declaration(t, "openai")
	if d.DefaultMount != 1 || !d.ToolMisbehavior || d.AllRoutes || len(d.Dialects) != 1 || d.Dialects[0] != config.DialectOpenAI {
		t.Fatalf("%+v", d)
	}
	if len(d.Options) != 1 || d.Options[0].Name != "reasoning_field" || strings.Join(d.Options[0].Values, ",") != "hidden" {
		t.Fatalf("options %+v", d.Options)
	}
}
