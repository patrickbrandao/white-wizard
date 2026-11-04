package engine

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/tokenize"
)

// The tokenizers a model can count with (models[].tokenizer, spec 003, ADR
// 0053) and a prose generator over each. The BPE tables load on first use.
var (
	simulated     generate.Tokenizer = generate.Simulated{}
	simulatedText                    = generate.NewGenerator(simulated, nil)
	o200kText                        = generate.NewGenerator(tokenize.O200k, nil)
	cl100kText                       = generate.NewGenerator(tokenize.Cl100k, nil)
	claudeText                       = generate.NewGenerator(claudeTokens{}, nil)
)

// generatorFor is the default prose generator of the model's tokenizer.
func generatorFor(m *config.ModelConfig) *generate.Generator {
	switch m.Encoding() {
	case "o200k_base":
		return o200kText
	case "cl100k_base":
		return cl100kText
	}
	return simulatedText
}

// tokenizerFor is the model's tokenizer: BPE for the OpenAI models under
// auto, simulated for the providers without a public tokenizer.
func tokenizerFor(m *config.ModelConfig) generate.Tokenizer {
	return generatorFor(m).Tokenizer
}

// generatorOf is the prose generator a request counts with: a model
// without a tokenizer of its own (auto, not a BPE) counts with the
// generator the driver declares (Gemini's calibration, task 265); an
// explicit simulated or tiktoken tokenizer is kept. On a driver that
// declares ClaudeTokens a Claude 4.7 or later counts its output as it
// counts the prompt (claudeTokens, task 342).
func generatorOf(m *config.ModelConfig, decl driver.EngineDecl) *generate.Generator {
	g := generatorFor(m)
	if g == simulatedText && m.Tokenizer != config.TokenizerSimulated && decl.Text != nil {
		return decl.Text
	}
	if g == simulatedText && decl.ClaudeTokens && finerClaude(m) {
		return claudeText
	}
	return g
}

// countPrompt counts the prompt with the template the driver declares
// (Gemini's, measured with countTokens) or the chat template of
// generate.CountPrompt.
func countPrompt(tk generate.Tokenizer, req *core.Request, tpl driver.PromptTemplate) generate.PromptBreakdown {
	if _, ok := tk.(claudeTokens); ok {
		// The Claude template scales Simulated's counts itself.
		tk = simulated
	}
	if tpl != nil {
		return tpl.CountPrompt(tk, req)
	}
	return generate.CountPrompt(tk, req, 85)
}

// claudeTokens counts as Claude 4.7 and later do (spec 007, task 301: ~32%
// more tokens than Claude 4.5 in the same text, whose counts Simulated
// approximates): Simulated's tokens, one of them split in two at a
// character boundary at each 32/100 step of the running count (the next
// token of two or more characters when that one is a single character). Text of n
// simulated tokens counts about 1.32 n, as the prompt template counts it
// (generate.countAnthropicPrompt), and Split agrees with Count, so the
// max_tokens cut and the stream use the same tokens (task 342).
type claudeTokens struct{}

// Each yields the tokens of text in order; their concatenation is text.
func (claudeTokens) Each(text string, fn func(tok string)) {
	i, owed := 0, 0
	generate.EachToken(simulated, text, func(tok string) {
		owed += (i+1)*32/100 - i*32/100
		i++
		if owed > 0 {
			if cut := midRune(tok); cut > 0 {
				owed--
				fn(tok[:cut])
				fn(tok[cut:])
				return
			}
		}
		fn(tok)
	})
}

func (c claudeTokens) Count(text string) int {
	n := 0
	c.Each(text, func(string) { n++ })
	return n
}

func (c claudeTokens) Split(text string) []string {
	var out []string
	c.Each(text, func(tok string) { out = append(out, tok) })
	return out
}

// midRune is the character boundary nearest past the middle of tok; 0 when
// tok is a single character.
func midRune(tok string) int {
	cut := len(tok) / 2
	for cut < len(tok) && !utf8.RuneStart(tok[cut]) {
		cut++
	}
	if cut == 0 || cut == len(tok) {
		return 0
	}
	return cut
}

// finerClaude reports whether the model is a Claude 4.7 or later by its
// id, version or an alias ("claude-opus-4-7", "claude-haiku-5-5-20260101").
func finerClaude(m *config.ModelConfig) bool {
	for _, name := range append([]string{m.ID, m.Version}, m.Aliases...) {
		if major, minor, ok := claudeVersion(name); ok && (major > 4 || major == 4 && minor >= 7) {
			return true
		}
	}
	return false
}

// claudeVersion reads "claude-<line>-<major>[-<minor>][-<date>]" as
// generate.claudeFamilyOf does; other ids (claude-3-5-sonnet-…) are not
// read.
func claudeVersion(name string) (major, minor int, ok bool) {
	rest, ok := strings.CutPrefix(strings.ToLower(name), "claude-")
	if !ok {
		return 0, 0, false
	}
	line, rest, _ := strings.Cut(rest, "-")
	if line == "" || line[0] >= '0' && line[0] <= '9' {
		return 0, 0, false
	}
	parts := strings.Split(rest, "-")
	num := func(i int) int {
		if i >= len(parts) || len(parts[i]) == 0 || len(parts[i]) > 2 {
			return -1
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return -1
		}
		return n
	}
	if major = num(0); major < 0 {
		return 0, 0, false
	}
	return major, max(0, num(1)), true
}
