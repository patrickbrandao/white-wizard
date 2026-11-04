package config

import (
	"strings"
	"testing"
)

func TestDriverOptions(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"openai hidden", "drivers: [{name: openai, options: {reasoning_field: hidden}}]", ""},
		{"azure hidden", "drivers: [{name: azure, options: {reasoning_field: hidden}}]", ""},
		{"openai reasoning_content", "drivers: [{name: openai, options: {reasoning_field: reasoning_content}}]", `option reasoning_field="reasoning_content" is invalid`},
		{"azure inline_tag", "drivers: [{name: azure, options: {reasoning_field: inline_tag}}]", "is invalid"},
		{"opencode inline_tag", "drivers: [{name: opencode, options: {reasoning_field: inline_tag, require_session: \"true\"}}]", ""},
		{"opencode require_session default", "drivers: [{name: opencode}]", ""},
		{"opencode require_session false", "drivers: [{name: opencode, options: {require_session: \"false\"}}]", `option require_session="false" is invalid (allowed: [true])`},
		{"openrouter reasoning_content", "drivers: [{name: openrouter, options: {reasoning_field: reasoning_content}}]", ""},
		{"openrouter bogus", "drivers: [{name: openrouter, options: {reasoning_field: thoughts}}]", "is invalid"},
		{"anthropic reasoning_field", "drivers: [{name: anthropic, options: {reasoning_field: hidden}}]", `unknown option "reasoning_field"`},
		{"anthropic require_version false", "drivers: [{name: anthropic, options: {require_version: \"false\"}}]", `option require_version="false" is invalid (allowed: [true])`},
		{"anthropic require_version true", "drivers: [{name: anthropic, options: {require_version: \"true\"}}]", ""},
		{"gemini any", "drivers: [{name: gemini, options: {reasoning_field: hidden}}]", "unknown option"},
		{"opts renamed", "drivers: [{name: openai}, {name: opencode, opts: {reasoning_field: hidden}}]", `config: drivers[1]: field "opts" renamed to "options"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
