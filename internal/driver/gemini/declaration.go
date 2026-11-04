package gemini

import (
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/gemini/gtok"
)

var (
	_ driver.Declarer       = (*Driver)(nil)
	_ driver.EngineDeclarer = (*Driver)(nil)
	_ driver.PromptTemplate = gtok.Template{}
)

// Declaration: Gemini dialect models, not mounted by default, no option
// (the reasoning format is fixed, ADR 0046). ModelExtra are the keys of a
// GET /v1beta/models item.
func (d *Driver) Declaration() config.DriverDecl {
	return config.DriverDecl{
		Dialects: []string{config.DialectGemini},
		ModelExtra: []string{
			"name", "version", "displayName", "description", "inputTokenLimit", "outputTokenLimit",
			"supportedGenerationMethods", "temperature", "topP", "topK", "maxTemperature", "thinking",
		},
	}
}

// EngineDeclaration: the calibrated tokenizer and the prompt template
// measured with countTokens (gtok, task 265) and the implicit prompt
// cache; the content filter is the driver's promptFeedback (spec 006).
func (d *Driver) EngineDeclaration() driver.EngineDecl {
	return driver.EngineDecl{Text: gtok.Text, Template: gtok.Template{}, PromptCache: implicitCache}
}

func implicitCache(*config.ModelConfig, core.Endpoint) driver.CacheStyle {
	return driver.CacheImplicit
}
