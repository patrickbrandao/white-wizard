package openai

import (
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var (
	_ driver.Declarer       = (*Driver)(nil)
	_ driver.EngineDeclarer = (*Driver)(nil)
)

// Declaration: OpenAI dialect models, mounted first by default. The real
// OpenAI never returns the reasoning text (ADR 0046), so reasoning_field
// accepts only hidden. ModelExtra are the keys of a GET /v1/models item.
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects:        []string{config.DialectOpenAI},
		ToolMisbehavior: true,
		DefaultMount:    1,
		Options:         []config.DriverOption{{Name: "reasoning_field", Default: "hidden", Values: []string{"hidden"}}},
		ModelExtra:      []string{"id", "object", "created", "owned_by", "shutdown_date"},
	}
}

// EngineDeclaration: the automatic prompt cache and the content filter as
// a 200 that ends in finish_reason content_filter (specs 006, 007, 009).
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{PromptCache: automaticCache, ContentFilterAsFinish: true}
}

func automaticCache(*config.ModelConfig, core.Endpoint) driver.CacheStyle {
	return driver.CacheAutomatic
}
