package chaos

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// The registered checks make config.Parse refuse unknown names in
// errors.types and errors.mid_stream_types (task 151) and still accept the
// frame-level fault duplicate_frames.
func TestConfigRefusesUnknownProfileTypes(t *testing.T) {
	cases := []struct{ yaml, wantErr string }{
		{"profiles: {errors: {p: {types: [rate_limit, duplicate_frames], mid_stream_types: [stream_truncated]}}}", ""},
		// finish_length is in the mid-stream table of spec 009 (task 393).
		{"profiles: {errors: {p: {mid_stream_types: [finish_length]}}}", ""},
		{"profiles: {errors: {p: {types: [bogus]}}}", `profiles.errors.p.types[0]: unknown error type "bogus"`},
		{"profiles: {errors: {p: {mid_stream_types: [overloaded]}}}", `profiles.errors.p.mid_stream_types[0]: unknown mid-stream type "overloaded"`},
	}
	for _, tc := range cases {
		_, err := config.Parse([]byte(tc.yaml))
		if tc.wantErr == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("err = %v, want %q", err, tc.wantErr)
		}
	}
}
