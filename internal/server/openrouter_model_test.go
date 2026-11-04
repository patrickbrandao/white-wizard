package server

import (
	"encoding/json"
	"testing"
)

// TestOpenRouterSingleModelRoute: GET /api/v1/model/{author}/{slug} is the
// real single-model route (ADR 0040); /api/v1/models/{...} serves no model
// (ADR 0046). Misses carry the real API's texts (checked in task 163).
func TestOpenRouterSingleModelRoute(t *testing.T) {
	h := start(t, fastCfg+`
models:
  - {id: wwz-fast, aliases: [fast-alias], latency: fast}
  - {id: anthropic/claude-x, latency: fast}
keys:
  - {key: sk-or-v1-x, label: x}
drivers:
  - {name: openrouter, prefix: /openrouter}
`, nil)
	auth := map[string]string{"Authorization": "Bearer sk-or-v1-x"}
	const notFound = `{"error":{"message":"Not Found","code":404}}`
	for path, want := range map[string]string{
		"/openrouter/api/v1/model/white-wizard/wwz-fast":           "white-wizard/wwz-fast",
		"/openrouter/api/v1/model/anthropic/claude-x":              "anthropic/claude-x",
		"/openrouter/api/v1/models/white-wizard/wwz-fast":          notFound,
		"/openrouter/api/v1/models/wwz-fast":                       notFound,
		"/openrouter/api/v1/model/white-wizard/nope":               `{"error":{"message":"Model not found: white-wizard/nope","code":404}}`,
		"/openrouter/api/v1/model/wwz-fast":                        notFound,
		"/openrouter/api/v1/model/white-wizard/fast-alias":         `{"error":{"message":"Model not found: white-wizard/fast-alias","code":404}}`,
		"/openrouter/api/v1/model/white-wizard/anthropic/claude-x": notFound,
	} {
		resp := h.do("GET", path, auth, "")
		if want[0] == '{' {
			if resp.StatusCode() != 404 || string(resp.Body()) != want {
				t.Errorf("GET %s: %d %s, want 404 %s", path, resp.StatusCode(), resp.Body(), want)
			}
			continue
		}
		var got struct{ Data struct{ ID string } }
		if resp.StatusCode() != 200 || json.Unmarshal(resp.Body(), &got) != nil || got.Data.ID != want {
			t.Errorf("GET %s: %d %s, want data.id %q", path, resp.StatusCode(), resp.Body(), want)
		}
	}
}
