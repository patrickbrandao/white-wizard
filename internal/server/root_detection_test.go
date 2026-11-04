package server

import (
	"slices"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/azure"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openrouter"
)

// TestRootDetectionSixSDKs (spec 005, "na raiz sem prefixo, detecção certa
// para cada SDK"): the six drivers mounted at the root without a prefix,
// in both orders, so the config order cannot pick the answer; each request
// carries the headers and path the official SDK sends and lands on its
// driver. The shared paths (/v1/models, served by openai and anthropic)
// are told apart by Sniff.
func TestRootDetectionSixSDKs(t *testing.T) {
	names := []string{"openai", "anthropic", "gemini", "azure", "openrouter", "opencode"}
	cases := []struct {
		sdk, method, path string
		h                 driver.MapHeaders
		want              string
		kind              driver.Kind
	}{
		{
			"openai", "POST", "/v1/chat/completions",
			driver.MapHeaders{"authorization": "Bearer sk-proj-abc"},
			"openai", driver.KindChat,
		},
		{
			"openai", "GET", "/v1/models",
			driver.MapHeaders{"authorization": "Bearer sk-proj-abc"},
			"openai", driver.KindModels,
		},
		{
			"anthropic", "POST", "/v1/messages",
			driver.MapHeaders{"x-api-key": "sk-ant-abc", "anthropic-version": "2023-06-01"},
			"anthropic", driver.KindMessages,
		},
		{
			"anthropic", "GET", "/v1/models",
			driver.MapHeaders{"x-api-key": "sk-ant-abc", "anthropic-version": "2023-06-01"},
			"anthropic", driver.KindModels,
		},
		{
			"google-genai", "POST", "/v1beta/models/gemini-2.5-flash:generateContent",
			driver.MapHeaders{"x-goog-api-key": "AIzaabc", "x-goog-api-client": "google-genai-sdk/1.0"},
			"gemini", driver.KindGenerate,
		},
		{
			"AzureOpenAI", "POST", "/openai/deployments/dep-1/chat/completions",
			driver.MapHeaders{"api-key": "azkey", driver.QueryHeader: "api-version=2024-10-21"},
			"azure", driver.KindChat,
		},
		{
			"openai (OpenRouter base_url)", "POST", "/api/v1/chat/completions",
			driver.MapHeaders{"authorization": "Bearer sk-or-v1-abc"},
			"openrouter", driver.KindChat,
		},
		{
			"openai (OpenCode base_url)", "POST", "/zen/go/v1/chat/completions",
			driver.MapHeaders{"authorization": "Bearer oc_sk_abc", "x-opencode-session": "ses_1"},
			"opencode", driver.KindChat,
		},
	}
	reversed := slices.Clone(names)
	slices.Reverse(reversed)
	for _, order := range [][]string{names, reversed} {
		cfg := &config.Config{}
		for _, n := range order {
			cfg.Drivers = append(cfg.Drivers, config.DriverMount{Name: n})
		}
		r, err := buildRouter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			m, _ := r.lookup(c.method, c.path, c.h)
			if m == nil || m.drv.Name() != c.want || m.kind != c.kind {
				t.Errorf("order %v, %s %s %s: got %+v, want %s %s", order, c.sdk, c.method, c.path, m, c.want, c.kind)
			}
		}
	}
}
