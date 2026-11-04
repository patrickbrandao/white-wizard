package openrouter_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
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

// TestValidateRequest: the gateway's own checks on Chat Completions (the
// cases of internal/server's TestValidationGateways, task 388).
func TestValidateRequest(t *testing.T) {
	v := validator(t, "openrouter")
	const hi = `"messages":[{"role":"user","content":"hi"}]`
	for _, c := range []struct{ body, msg string }{
		{`{"model":`, `Input required: specify "prompt" or "messages"`},
		{`{"model":"m","messages":[]}`, `Input required: specify "prompt" or "messages"`},
		{`{"model":5,` + hi + `}`, "No models provided"},
		{`{"model":"m","top_p":"x","temperature":"x",` + hi + `}`, "temperature: Invalid input: expected number, received string"},
		{`{"model":"m","top_p":[],` + hi + `}`, "top_p: Invalid input: expected number, received array"},
		{`{"model":"m","temperature":{},` + hi + `}`, "temperature: Invalid input: expected number, received object"},
		{`{"model":"m","temperature":true,` + hi + `}`, "temperature: Invalid input: expected number, received boolean"},
		{`{"model":"m","stream":"x",` + hi + `}`, "stream: Invalid input"},
		{`{"model":"nope","stream":"x",` + hi + `}`, "nope is not a valid model ID"},
	} {
		e := v.ValidateRequest(driver.KindChat, []byte(c.body), nil, models{})
		if e == nil || e.Message != c.msg || e.Kind != core.ErrInvalidRequest {
			t.Errorf("%s: %+v, want %q", c.body, e, c.msg)
		}
	}
	// A prompt stands in for messages, models for model; an unknown model
	// without a type error is the engine's; other kinds are not checked.
	for _, body := range []string{`{"model":"m","prompt":"hi"}`, `{"models":["m"],"messages":[{}]}`, `{"model":"nope",` + hi + `}`} {
		if e := v.ValidateRequest(driver.KindChat, []byte(body), nil, models{}); e != nil {
			t.Errorf("%s: %+v", body, e)
		}
	}
	if e := v.ValidateRequest(driver.KindMessages, []byte(`x`), nil, models{}); e != nil {
		t.Errorf("messages: %+v", e)
	}
}

func TestDeclaration(t *testing.T) {
	d := declaration(t, "openrouter")
	if d.DefaultMount != 3 || !d.AllRoutes || !d.LegacyProtocol || !d.ToolMisbehavior || len(d.Dialects) != 2 {
		t.Fatalf("%+v", d)
	}
	if len(d.Options) != 1 || strings.Join(d.Options[0].Values, ",") != strings.Join(config.ReasoningFields(), ",") {
		t.Fatalf("options %+v", d.Options)
	}
}
