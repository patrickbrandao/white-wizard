package oaicheck

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Moved from internal/server with the validator (task 388); the HTTP cases
// stay in internal/server/validate_test.go.

func TestValidatorBranches(t *testing.T) {
	ok := map[string][]byte{
		"string": []byte(`"x"`), "enum": []byte(`"auto"`), "array": []byte(`[]`), "stop": []byte(`"s"`),
	}
	if oaiString("user", ok["string"]) != nil || oaiEnum("x")("service_tier", ok["enum"]) != nil ||
		oaiArray("x")("modalities", ok["array"]) != nil || oaiStop("stop", ok["stop"]) != nil || oaiStop("stop", ok["array"]) != nil {
		t.Fatal("valid values refused")
	}
	if got := oaiGot([]byte(`{}`)); got != "an object" {
		t.Fatalf("oaiGot object: %s", got)
	}
	if err := Validate(nil, driver.KindModels, nil); err != nil {
		t.Fatalf("models: %+v", err)
	}
}

func TestOAIToolsTolerant(t *testing.T) {
	for _, body := range []string{`[1]`, `[{"function":{"strict":true}}]`, `[{"function":1}]`} {
		if err := oaiTools("tools", []byte(body)); err != nil {
			t.Errorf("%s: %+v", body, err)
		}
	}
	if err := oaiResponseFormat("response_format", []byte(`{"type":"text"}`)); err != nil {
		t.Errorf("text format: %+v", err)
	}
}
