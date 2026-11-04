package openrouter

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var _ driver.Declarer = (*Driver)(nil)

// Declaration: OpenAI and Anthropic dialect models, every model on every
// route (the gateway normalizes, ADR 0048), mounted third by default; the
// name was once a models[].protocols value. reasoning_field imitates the
// upstream model the gateway serves (ADR 0046). ModelExtra are the keys of
// a GET /api/v1/models item.
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects:        []string{config.DialectOpenAI, config.DialectAnthropic},
		AllRoutes:       true,
		LegacyProtocol:  true,
		ToolMisbehavior: true,
		DefaultMount:    3,
		Options:         []config.DriverOption{{Name: "reasoning_field", Default: "reasoning", Values: config.ReasoningFields()}},
		ModelExtra: []string{
			"id", "canonical_slug", "alias_target", "hugging_face_id", "name", "created", "description",
			"context_length", "architecture", "pricing", "top_provider", "per_request_limits",
			"supported_parameters", "default_parameters", "supported_voices", "knowledge_cutoff",
			"expiration_date", "links", "endpoints", "benchmarks", "reasoning",
		},
	}
}

var _ driver.EngineDeclarer = (*Driver)(nil)

// EngineDeclaration: the prompt cache of the upstream the slug's author
// names (openai, anthropic, google; none for the others); the content
// filter is the driver's 403 (spec 006).
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{PromptCache: upstreamCache}
}

func upstreamCache(m *config.ModelConfig, _ core.Endpoint) driver.CacheStyle {
	author, _, _ := strings.Cut(m.ID, "/")
	switch author {
	case "openai":
		return driver.CacheAutomatic
	case "anthropic":
		return driver.CacheBreakpoints
	case "google":
		return driver.CacheImplicit
	}
	return driver.CacheNone
}
