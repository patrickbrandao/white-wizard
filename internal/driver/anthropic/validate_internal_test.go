package anthropic

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Moved from internal/server with the validator (task 388); the HTTP cases
// stay in internal/server/validate_test.go.

func TestSerdeError(t *testing.T) {
	cases := map[string]string{
		`{"a":[1,2]}`:          "",
		` {"a": -1.5e+3} `:     "",
		`"\u00e9\n"`:           "",
		``:                     "EOF while parsing a value at line 1 column 0",
		`{"a":1}` + "\n x":     "trailing characters at line 2 column 2",
		`{`:                    "EOF while parsing an object at line 1 column 1",
		`{"a"`:                 "EOF while parsing an object at line 1 column 4",
		`{"a" 1}`:              "expected `:` at line 1 column 6",
		`{"a":1 "b":2}`:        "expected `,` or `}` at line 1 column 8",
		`{"a":1,}`:             "trailing comma at line 1 column 8",
		`{1:2}`:                "key must be a string at line 1 column 2",
		`{"a":1,`:              "EOF while parsing a value at line 1 column 7",
		`[1,]`:                 "trailing comma at line 1 column 4",
		`[1 2]`:                "expected `,` or `]` at line 1 column 4",
		`[1,`:                  "EOF while parsing a value at line 1 column 3",
		`[`:                    "EOF while parsing a list at line 1 column 1",
		`nul`:                  "EOF while parsing a value at line 1 column 3",
		`nux`:                  "expected ident at line 1 column 3",
		`tru`:                  "EOF while parsing a value at line 1 column 3",
		`falsy`:                "expected ident at line 1 column 5",
		`-`:                    "EOF while parsing a value at line 1 column 1",
		`-x`:                   "invalid number at line 1 column 2",
		`1.`:                   "EOF while parsing a value at line 1 column 2",
		`1.x`:                  "invalid number at line 1 column 3",
		`1e`:                   "EOF while parsing a value at line 1 column 2",
		`1ex`:                  "invalid number at line 1 column 3",
		`"abc`:                 "EOF while parsing a string at line 1 column 4",
		`"a\`:                  "EOF while parsing a string at line 1 column 3",
		`"a\q"`:                "invalid escape at line 1 column 4",
		`"\u12`:                "EOF while parsing a string at line 1 column 5",
		`"\u12x4"`:             "invalid escape at line 1 column 6",
		"\"a\x01\"":            "control character (\\u0000-\\u001F) found while parsing a string at line 1 column 3",
		`{"a":x}`:              "expected value at line 1 column 6",
		`["a",{"b":[true]}] ]`: "trailing characters at line 1 column 20",
	}
	for in, want := range cases {
		if got := serdeError([]byte(in)); got != want {
			t.Errorf("%q:\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestValidatorBranches(t *testing.T) {
	// A body that is not an object, messages and blocks that are not
	// objects, and blocks of other types are left to the driver.
	for _, body := range []string{
		`[1]`,
		`{"model":"m","max_tokens":1,"messages":[1,{"role":"user","content":[1,{"type":"image","foo":1}]}]}`,
	} {
		if msg := anthropicCheck([]byte(body), anthropicFields[driver.KindMessages], nil); msg != "" {
			t.Errorf("%s: %s", body, msg)
		}
	}
	for in, want := range map[string]string{
		`[1 `:      "EOF while parsing a list at line 1 column 3",
		`[[1],x]`:  "expected value at line 1 column 6",
		`{"a":[x]`: "expected value at line 1 column 7",
		`{"a":1 `:  "EOF while parsing an object at line 1 column 7",
		`{"a`:      "EOF while parsing a string at line 1 column 3",
	} {
		if got := serdeError([]byte(in)); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
	if hasBeta(nil, "x") {
		t.Fatal("beta without headers")
	}
	d := &Driver{}
	if err := d.ValidateRequest(driver.KindModels, nil, nil, nil); err != nil {
		t.Fatalf("models: %+v", err)
	}
}
