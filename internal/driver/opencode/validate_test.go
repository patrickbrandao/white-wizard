package opencode_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
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

// TestValidateRequest: the gateway's own checks (the cases of
// internal/server's TestValidationGateways and
// TestOpenCodeResponsesOrder, task 388).
func TestValidateRequest(t *testing.T) {
	v := validator(t, "opencode")
	if e := v.ValidateRequest(driver.KindChat, []byte(`{"model": "x", `), nil, models{}); e == nil {
		t.Fatal("bad json accepted")
	} else if r, ok := driver.RawReplyOf(e); !ok || r.Status != 400 || len(r.Body) != 0 || r.ContentType != "" {
		t.Fatalf("bad json: %+v", r)
	}
	e := v.ValidateRequest(driver.KindChat, []byte(`{"model":5}`), nil, models{})
	want := "{\"error\":{\"message\":\"`model` must be a non-empty string\",\"type\":\"invalid_request_error\",\"param\":\"model\",\"code\":null}}"
	if r, ok := driver.RawReplyOf(e); !ok || r.Status != 400 || string(r.Body) != want || r.ContentType != "application/json" {
		t.Fatalf("model: %+v", r)
	}
	if e := v.ValidateRequest(driver.KindChat, []byte(`{"model":"m","foo":1}`), nil, models{}); e != nil {
		t.Fatalf("unknown field is the upstream's: %+v", e)
	}
	if e := v.ValidateRequest(driver.KindResponses, []byte(`{"model":"nope","input":"hi"}`), nil, models{}); e == nil || e.Kind != core.ErrModelNotFound || e.Status != 400 {
		t.Fatalf("responses unknown model: %+v", e)
	}
	for _, body := range []string{`{"model":"x","input":"hi"}`, `{`, `{"model":5}`} {
		if e := v.ValidateRequest(driver.KindResponses, []byte(body), nil, models{}); e != nil {
			t.Errorf("responses %s: %+v", body, e)
		}
	}
	if e := v.ValidateRequest(driver.KindMessages, []byte(`x`), nil, models{}); e != nil {
		t.Errorf("messages: %+v", e)
	}
}

func TestDeclaration(t *testing.T) {
	d := declaration(t, "opencode")
	if d.DefaultMount != 4 || !d.AllRoutes || !d.LegacyProtocol || !d.ToolMisbehavior || len(d.Dialects) != 3 || len(d.Options) != 4 {
		t.Fatalf("%+v", d)
	}
}
