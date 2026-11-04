package azure

import (
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

var (
	_ driver.Declarer       = (*Driver)(nil)
	_ driver.EngineDeclarer = (*Driver)(nil)
)

// Declaration: OpenAI dialect models, not mounted by default. Like OpenAI,
// reasoning_field accepts only hidden (ADR 0046). ModelExtra are the keys
// of a GET /openai/models item.
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects:   []string{config.DialectOpenAI},
		Options:    []config.DriverOption{{Name: "reasoning_field", Default: "hidden", Values: []string{"hidden"}}},
		ModelExtra: []string{"status", "capabilities", "lifecycle_status", "deprecation", "id", "created_at", "object"},
	}
}

// EngineDeclaration: OpenAI's automatic prompt cache; the content filter
// is the driver's 400 (spec 006).
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{PromptCache: automaticCache}
}

func automaticCache(*config.ModelConfig, core.Endpoint) driver.CacheStyle {
	return driver.CacheAutomatic
}
