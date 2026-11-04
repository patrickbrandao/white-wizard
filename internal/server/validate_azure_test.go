package server

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestValidateAzure: the v1 Responses API of Azure runs OpenAI's checks
// (task 295); the unknown deployment is a bare ErrModelNotFound for the
// driver's DeploymentNotFound; chat and an api-version the v1 family
// refuses are left to the driver.
func TestValidateAzure(t *testing.T) {
	cfg, err := config.Parse([]byte("models:\n  - {id: dep-1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := &validation{cfg: cfg, protocol: "openai"}
	q := func(s string) driver.MapHeaders { return driver.MapHeaders{driver.QueryHeader: s} }
	cases := []struct {
		name string
		kind driver.Kind
		h    driver.MapHeaders
		body string
		want core.ErrorKind // "" = nil
		code string
	}{
		{"chat is the driver's", driver.KindChat, nil, `{"foo":1}`, "", ""},
		{"api-version of the deployment routes", driver.KindResponses, q("api-version=2024-10-21"), `{`, "", ""},
		{"bad json", driver.KindResponses, nil, `{`, core.ErrInvalidRequest, "invalid_json"},
		{"unknown field", driver.KindResponses, q("api-version=v1"), `{"model":"dep-1","input":"x","foo":1}`, core.ErrInvalidRequest, "unknown_parameter"},
		{"unknown deployment", driver.KindResponses, q("api-version=preview"), `{"model":"nope","input":"x"}`, core.ErrModelNotFound, ""},
		{"conversation", driver.KindResponses, nil, `{"model":"dep-1","input":"x","conversation":"c"}`, core.ErrNotFound, ""},
		{"valid", driver.KindResponses, nil, `{"model":"dep-1","input":"x"}`, "", ""},
	}
	for _, c := range cases {
		var h driver.Headers
		if c.h != nil {
			h = c.h
		}
		e := validateAs(t, "azure", v, c.kind, c.body, h)
		switch {
		case c.want == "" && e != nil:
			t.Errorf("%s: %+v, want nil", c.name, e)
		case c.want != "" && (e == nil || e.Kind != c.want || e.Code != c.code):
			t.Errorf("%s: %+v, want %s %s", c.name, e, c.want, c.code)
		case c.want == core.ErrModelNotFound && e.Message != "":
			t.Errorf("%s: message %q, want the driver's", c.name, e.Message)
		}
	}
}
