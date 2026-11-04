package core_test

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

func TestExtraRawBodyMatchesChaos(t *testing.T) {
	if core.ExtraRawBody != chaos.ExtraRawBody {
		t.Fatalf("core.ExtraRawBody %q != chaos.ExtraRawBody %q", core.ExtraRawBody, chaos.ExtraRawBody)
	}
}

func TestRawBody(t *testing.T) {
	cases := []struct {
		name   string
		e      *core.Error
		body   string
		ct     string
		wantOK bool
	}{
		{"nil error", nil, "", "", false},
		{"no extra", &core.Error{Kind: core.ErrServerError}, "", "", false},
		{"empty string", &core.Error{Extra: map[string]any{core.ExtraRawBody: ""}}, "", "", false},
		{"wrong type", &core.Error{Extra: map[string]any{core.ExtraRawBody: 42}}, "", "", false},
		{"json string", &core.Error{Extra: map[string]any{core.ExtraRawBody: ` {"x": 1} `}}, ` {"x": 1} `, core.RawBodyJSON, true},
		{"json bytes", &core.Error{Extra: map[string]any{core.ExtraRawBody: []byte(`[1,2]`)}}, `[1,2]`, core.RawBodyJSON, true},
		{"text", &core.Error{Extra: map[string]any{core.ExtraRawBody: "upstream exploded {"}}, "upstream exploded {", core.RawBodyText, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ct, ok := tc.e.RawBody()
			if ok != tc.wantOK || string(b) != tc.body || ct != tc.ct {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", b, ct, ok, tc.body, tc.ct, tc.wantOK)
			}
		})
	}
}

func TestRenderBody(t *testing.T) {
	plain := &core.Error{Kind: core.ErrServerError}
	if b, ct := plain.RenderBody(map[string]any{"a": 1}, "application/json"); string(b) != `{"a":1}` || ct != "application/json" {
		t.Fatalf("marshal: %q %q", b, ct)
	}
	if b, ct := plain.RenderBody([]byte("<html>"), "text/html"); string(b) != "<html>" || ct != "text/html" {
		t.Fatalf("bytes passthrough: %q %q", b, ct)
	}
	raw := &core.Error{Extra: map[string]any{core.ExtraRawBody: "oops"}}
	if b, ct := raw.RenderBody(map[string]any{"a": 1}, "application/json"); string(b) != "oops" || ct != core.RawBodyText {
		t.Fatalf("raw override: %q %q", b, ct)
	}
}
