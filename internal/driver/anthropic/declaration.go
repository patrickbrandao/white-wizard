package anthropic

import (
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var (
	_ driver.Declarer       = (*Driver)(nil)
	_ driver.EngineDeclarer = (*Driver)(nil)
)

// Declaration: Anthropic dialect models, mounted second by default.
// require_version accepts only true, the real API always demands
// anthropic-version (ADR 0068); oauth_key and oauth_plan turn on the
// subscription and client_verification the strict client check (ADR
// 0074). ModelExtra are the keys of a GET
// /v1/models item.
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects:        []string{config.DialectAnthropic},
		ToolMisbehavior: true,
		DefaultMount:    2,
		Options: []config.DriverOption{
			{Name: "require_version", Default: "true", Values: []string{"true"}},
			// The subscription (OAuth) of Claude Code (ADR 0074, spec 006
			// 16.2.1): the keys[] entry it stands for and the plan its
			// profile routes report.
			{Name: "oauth_key", Check: config.OptionKeyLabel},
			{Name: "oauth_plan", Default: planNames[0], Values: planNames},
			// The strict client verification (spec 020): off is the real
			// API; claude-code refuses every client but Claude Code.
			{Name: "client_verification", Default: verifyOff, Values: []string{verifyOff, verifyClaudeCode}},
		},
		ModelExtra: []string{
			"type", "id", "display_name", "created_at", "line", "max_input_tokens", "max_tokens",
			"capabilities", "lifecycle", "deprecated_at", "retires_at",
		},
	}
}

// malformedFrame is a content_block_delta whose JSON is cut.
var malformedFrame = []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"\n\n")

// EngineDeclaration: the Claude 4.7+ tokens (task 342), the cache_control
// breakpoints, the content filter as a 200 that ends in stop_reason
// refusal (specs 006, 007, 009), the refusal before any output with empty
// content and the malformed frame in the Messages
// stream's shape.
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{
		ClaudeTokens:          true,
		PromptCache:           breakpointCache,
		ContentFilterAsFinish: true,
		RefusalEmpty:          true,
		MalformedFrame:        malformedFrame,
	}
}

func breakpointCache(*config.ModelConfig, core.Endpoint) driver.CacheStyle {
	return driver.CacheBreakpoints
}
