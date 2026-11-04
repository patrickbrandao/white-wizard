package generate

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/config"
)

// Sentences is the built-in corpus for random mode. Neutral, varied length,
// no proper nouns, so no router can accidentally key on content.
var Sentences = []string{
	"The request has been processed and the result is summarised below.",
	"Based on the information provided, the most reasonable approach is to proceed step by step.",
	"There are several factors to consider before reaching a conclusion.",
	"The configuration appears valid, although a few parameters could be tuned for better throughput.",
	"Each option has trade-offs in latency, cost and operational complexity.",
	"A simple baseline is often the best starting point for comparison.",
	"The data suggests a steady trend with occasional short-lived spikes.",
	"Further validation would help confirm the assumption behind this estimate.",
	"The service responded within the expected time window.",
	"Consider adding retries with exponential backoff for transient failures.",
	"The schema matches the expected structure and all required fields are present.",
	"Caching the intermediate result avoids recomputing it on every call.",
	"This summary covers the main points without going into implementation detail.",
	"Performance under load depends mostly on connection reuse and payload size.",
	"The next step is to compare the measured numbers with the target thresholds.",
	"No anomalies were detected in the sample that was analysed.",
	"Splitting the work into smaller units makes the pipeline easier to monitor.",
	"The output format follows the specification exactly.",
	"Timeouts should be set slightly above the observed p99 latency.",
	"Everything requested has been completed and verified.",
}

// LoremWords is the lorem-ipsum vocabulary.
var LoremWords = strings.Fields("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua enim ad minim veniam quis nostrud exercitation ullamco laboris nisi aliquip ex ea commodo consequat duis aute irure in reprehenderit voluptate velit esse cillum fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt culpa qui officia deserunt mollit anim id est laborum")

// ReasoningPhrases open thinking content.
var ReasoningPhrases = []string{
	"Let me think through this carefully.",
	"First, I need to understand what is being asked.",
	"The user wants a concrete answer, so I should check the constraints.",
	"There are a few possible interpretations; the most likely one is the simplest.",
	"I should verify whether a tool call is needed before answering.",
	"The available tools cover this case, so calling one is reasonable.",
	"Let me outline the steps and then produce the response.",
	"This looks consistent with the previous turn.",
	"I will keep the answer concise and structured.",
	"Double-checking the edge cases before finalising.",
}

// Generator produces text of a target token length.
type Generator struct {
	Tokenizer Tokenizer
	Sentences []string
}

// NewGenerator builds a generator; custom sentences override the corpus.
func NewGenerator(tk Tokenizer, custom []string) *Generator {
	s := Sentences
	if len(custom) > 0 {
		s = custom
	}
	return &Generator{Tokenizer: tk, Sentences: s}
}

// Text builds random prose cut at exactly target tokens. The sentences are
// summed without a separator token, because the space before a sentence
// belongs to its first token (Simulated); a tokenizer that counts the joins
// only makes the text longer before the cut.
func (g *Generator) Text(r *RNG, target int) string {
	if target <= 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for n < target {
		s := g.Sentences[r.IntN(len(g.Sentences))]
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
		n += g.Tokenizer.Count(s) // the separator joins the next token
	}
	return TrimToTokens(g.Tokenizer, b.String(), target)
}

// Lorem builds lorem ipsum of exactly target tokens (words).
func (g *Generator) Lorem(r *RNG, target int) string {
	if target <= 0 {
		return ""
	}
	words := make([]string, 0, target)
	for i := 0; i < target; i++ {
		w := LoremWords[r.IntN(len(LoremWords))]
		if i == 0 || (i%12 == 0) {
			w = strings.ToUpper(w[:1]) + w[1:]
			if i > 0 {
				words[len(words)-1] += "."
			}
		}
		words = append(words, w)
	}
	return strings.Join(words, " ") + "."
}

// Reasoning builds thinking text of about target tokens.
func (g *Generator) Reasoning(r *RNG, target int) string {
	if target <= 0 {
		return ""
	}
	var b strings.Builder
	n := 0
	for n < target {
		s := ReasoningPhrases[r.IntN(len(ReasoningPhrases))]
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
		n += g.Tokenizer.Count(s) // the separator joins the next token
	}
	return TrimToTokens(g.Tokenizer, b.String(), target)
}

// TrimToTokens cuts text to at most max tokens on a token boundary.
func TrimToTokens(tk Tokenizer, text string, max int) string {
	if s, ok := tk.(Simulated); ok && utf8.ValidString(text) {
		// Each yields Split's tokens as substrings of text: the cut is a
		// prefix, found without building the token list (task 270).
		n, end := 0, 0
		s.Each(text, func(tok string) {
			if n < max {
				end += len(tok)
			}
			n++
		})
		if n <= max {
			return text
		}
		return text[:end]
	}
	toks := tk.Split(text)
	if len(toks) <= max {
		return text
	}
	if end, ok := prefixOf(text, toks[:max]); ok {
		return text[:end]
	}
	return strings.Join(toks[:max], "")
}

// prefixOf reports whether toks, concatenated, are a prefix of text, and
// its length: the join is then text[:end], without allocating.
func prefixOf(text string, toks []string) (end int, ok bool) {
	for _, t := range toks {
		if len(text)-end < len(t) || text[end:end+len(t)] != t {
			return 0, false
		}
		end += len(t)
	}
	return end, true
}

// SampleOutputTokens draws an output length from the profile distribution.
func SampleOutputTokens(r *RNG, d *config.TokenDist, maxTokens int) int {
	n := 100
	if d != nil {
		switch d.Dist {
		case "fixed":
			n = d.Value
		case "uniform":
			n = r.Between(d.Min, d.Max)
		case "normal":
			v := r.NormFloat64()*d.StdDev + d.Mean
			n = int(math.Max(1, math.Round(v)))
		default: // histogram
			if len(d.Buckets) > 0 {
				total := 0.0
				for _, b := range d.Buckets {
					if len(b) == 2 {
						total += b[1]
					}
				}
				x := r.Float64() * total
				acc := 0.0
				n = int(d.Buckets[len(d.Buckets)-1][0])
				for _, b := range d.Buckets {
					if len(b) != 2 {
						continue
					}
					acc += b[1]
					if x <= acc {
						// Spread inside the bucket: 50%..100% of its nominal size.
						base := int(b[0])
						n = base/2 + r.IntN(base/2+1)
						break
					}
				}
			}
		}
	}
	if n < 1 {
		n = 1
	}
	if maxTokens > 0 && n > maxTokens {
		n = maxTokens
	}
	return n
}

// SampleReasoningTokens draws a thinking length; 0 means no reasoning.
func SampleReasoningTokens(r *RNG, rt *config.ReasoningTokens, effort string, budget int) int {
	if rt == nil {
		return 0
	}
	if !r.Chance(rt.Probability) {
		return 0
	}
	n := r.Between(rt.Min, rt.Max)
	switch effort {
	case "minimal":
		n = n / 4
	case "low":
		n = n / 2
	case "high":
		n = n * 2
	case "xhigh", "max":
		n = n * 3
	}
	if budget > 0 && n > budget {
		n = budget
	}
	if n < 1 {
		n = 1
	}
	return n
}
