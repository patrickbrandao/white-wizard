package opencode

import (
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var _ driver.Declarer = (*Driver)(nil)

// Declaration: OpenAI, Anthropic and Gemini dialect models, every model on
// every route (the gateway translates, ADR 0062), mounted fourth by
// default; the name was once a models[].protocols value. reasoning_field
// imitates the upstream model the gateway serves (ADR 0046);
// require_session accepts only true, the real gateway always answers
// MissingSessionID without x-opencode-session (ADR 0059). ModelExtra are
// the keys of a GET /zen/go/v1/models item. ADR 0072: the catalogs
// opencode (pay-as-you-go bases) and opencode-go (Go bases, with the
// allowance), the plans of tiers.<name>.plan, the reserved key public and
// free_requests_per_day (the daily count of the free models) and
// console_key (the keys[] label whose value signs the Console's tokens).
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects:        []string{config.DialectOpenAI, config.DialectAnthropic, config.DialectGemini},
		AllRoutes:       true,
		LegacyProtocol:  true,
		ToolMisbehavior: true,
		DefaultMount:    4,
		Options: []config.DriverOption{
			{Name: "reasoning_field", Default: "reasoning_content", Values: config.ReasoningFields()},
			{Name: "require_session", Default: "true", Values: []string{"true"}},
			{Name: "free_requests_per_day", Default: strconv.Itoa(defaultFreePerDay), Check: config.OptionPositiveInt},
			{Name: "console_key", Check: config.OptionKeyLabel},
		},
		Catalogs:     []config.CatalogDecl{{Name: catalogPAYG}, {Name: catalogGo, Allowance: true}},
		Plans:        []string{planGo, planGoPlus, planGoTeams, planNone},
		ReservedKeys: []string{publicKey},
		ModelExtra: []string{
			"id", "object", "created", "owned_by", "name", "canonical_id", "type", "description", "family",
			"open_weights", "knowledge", "release_date", "last_updated", "modalities", "capabilities",
			"limit", "cost", "api",
		},
	}
}

var _ driver.EngineDeclarer = (*Driver)(nil)

// EngineDeclaration: the prompt cache of the endpoint's native API
// (cache_control breakpoints on Messages, automatic elsewhere).
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{PromptCache: endpointCache}
}

func endpointCache(_ *config.ModelConfig, ep core.Endpoint) driver.CacheStyle {
	if ep == core.EndpointMessages {
		return driver.CacheBreakpoints
	}
	return driver.CacheAutomatic
}
