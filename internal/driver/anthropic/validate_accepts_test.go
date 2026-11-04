package anthropic

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestValidateAnthropicAccepts: valid values of every checked field pass
// (the HTTP twin is internal/server's TestValidationAlwaysOn).
func TestValidateAnthropicAccepts(t *testing.T) {
	d := &Driver{}
	for _, body := range []string{
		`{"model":"m","max_tokens":10,"system":"s","top_k":5,"temperature":1,"top_p":0.5,"metadata":{"user_id":"u"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
		`{"model":"m","max_tokens":0,"container":"c","service_tier":"auto","stream":true,"system":[],` + hiMsgs + `}`,
		`{"model":"m","max_tokens":1,"tools":[{"name":"t","input_schema":{}},{"type":"custom","name":"u","input_schema":{}},1],` + hiMsgs + `}`,
	} {
		if e := d.ValidateRequest(driver.KindMessages, []byte(body), anthHeaders, nil); e != nil {
			t.Errorf("%s: %+v", body, e)
		}
	}
	// The version header is checked after the JSON, before every field.
	if e := d.ValidateRequest(driver.KindMessages, []byte(`{}`), driver.MapHeaders{}, nil); e == nil || e.Message != "anthropic-version: header is required" {
		t.Fatalf("no version: %+v", e)
	}
}

// TestDeclaration: what the driver declares to internal/config (spec 005).
func TestDeclaration(t *testing.T) {
	decl := (&Driver{}).Declaration()
	if len(decl.Dialects) != 1 || decl.Dialects[0] != config.DialectAnthropic || decl.DefaultMount != 2 || len(decl.Options) != 4 {
		t.Fatalf("%+v", decl)
	}
}
