package gtok

import (
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
)

// Text is the prose generator over Tokenizer: the text of a Gemini model
// under tokenizer auto (task 265), which the driver declares to the engine.
var Text = generate.NewGenerator(Tokenizer{}, nil)

// Template is the Gemini prompt template as the engine sees it
// (driver.PromptTemplate): the count of CountPrompt and the cost of each
// part, for the prompt cache's layout.
type Template struct{}

// CountPrompt is CountPrompt.
func (Template) CountPrompt(tk generate.Tokenizer, req *core.Request) generate.PromptBreakdown {
	return CountPrompt(tk, req)
}

// Declaration is Declaration.
func (Template) Declaration(tk generate.Tokenizer, t core.ToolDef) int { return Declaration(tk, t) }

// Call is Call.
func (Template) Call(tk generate.Tokenizer, name, args string) int { return Call(tk, name, args) }

// Response is Response.
func (Template) Response(tk generate.Tokenizer, name, response string) int {
	return Response(tk, name, response)
}

// ContentOverhead is ContentOverhead.
func (Template) ContentOverhead() int { return ContentOverhead }

// MediaTokens is what images and audio parts cost.
func (Template) MediaTokens(images, audio int) int { return images*ImageTokens + audio*AudioTokens }
