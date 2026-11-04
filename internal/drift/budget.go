package drift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Spend is the accumulated real spend of one provider in the budget file.
type Spend struct {
	USD     float64   `json:"usd"`
	Calls   int       `json:"calls"`
	Updated time.Time `json:"updated"`
}

// DefaultBudgetFile follows ADR 0014; DefaultBudgetUSD is the per-provider
// cap of ADR 0042.
const (
	DefaultBudgetFile = "tmp/real-spend.json"
	DefaultBudgetUSD  = 20.0
)

// UnknownCallUSD is charged per paid call of a model without a price.
const UnknownCallUSD = 0.01

// price is USD per million tokens.
type price struct{ in, out float64 }

// prices of the probe models (public list prices, 2026-10; the OpenCode one
// matches the list_cost of the real answer), so the recorded spend is not
// the UnknownCallUSD overestimate.
var prices = map[string]price{
	"gpt-4o-mini":               {in: 0.15, out: 0.60},
	"gpt-4o-mini-2024-07-18":    {in: 0.15, out: 0.60},
	"gpt-4.1-nano":              {in: 0.10, out: 0.40},
	"claude-haiku-4-5":          {in: 1.00, out: 5.00},
	"claude-haiku-4-5-20251001": {in: 1.00, out: 5.00},
	"openai/gpt-4.1-nano":       {in: 0.10, out: 0.40},
	"deepseek-v4.1-flash":       {in: 0.15, out: 0.60},
	// Gemini 3.x Flash-Lite list price (3.5 not published yet: the 3.1
	// one, an overestimate next to 2.5's 0.10/0.40).
	"gemini-3.5-flash-lite": {in: 0.25, out: 1.50},
}

// Estimated usage when a priced answer carries no usage block.
const (
	estimatedInput  = 32
	estimatedOutput = ProbeMaxToks
)

// CostUSD estimates the cost of one paid call of model. ok reports whether
// the answer carried usage; without it the probe size is assumed.
func CostUSD(model string, in, out int, ok bool) float64 {
	p, known := prices[model]
	if !known {
		return UnknownCallUSD
	}
	if !ok {
		in, out = estimatedInput, estimatedOutput
	}
	return (float64(in)*p.in + float64(out)*p.out) / 1e6
}

// ReadBudget reads the budget file; a missing file is an empty budget.
func ReadBudget(path string) (map[string]Spend, error) {
	m := map[string]Spend{}
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("drift: orçamento: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("drift: orçamento %s: %w", path, err)
	}
	return m, nil
}

// AddSpend adds usd and one call to provider in the budget file and
// rewrites it atomically (temporary file + rename in the same directory).
func AddSpend(path, provider string, usd float64, now time.Time) (Spend, error) {
	m, err := ReadBudget(path)
	if err != nil {
		return Spend{}, err
	}
	s := m[provider]
	s.USD += usd
	s.Calls++
	s.Updated = now.UTC()
	m[provider] = s
	data, _ := json.MarshalIndent(m, "", "  ") // map of plain structs: cannot fail
	if err := writeAtomic(path, append(data, '\n')); err != nil {
		return Spend{}, fmt.Errorf("drift: orçamento: %w", err)
	}
	return s, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// usageBlock is the usage of either dialect.
type usageBlock struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

type usageDoc struct {
	Usage   *usageBlock `json:"usage"`
	Message *struct {
		Usage *usageBlock `json:"usage"`
	} `json:"message"`
	// Responses API: usage of response.completed (or .incomplete).
	Response *struct {
		Usage *usageBlock `json:"usage"`
	} `json:"response"`
	// Gemini: usageMetadata on the body and on every chunk; the thoughts
	// are billed as output.
	Gemini *struct {
		Prompt     int `json:"promptTokenCount"`
		Candidates int `json:"candidatesTokenCount"`
		Thoughts   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

// usageOf extracts token usage from a JSON body or from the data lines of
// an event stream (OpenAI final chunk, Anthropic message_start and
// message_delta), keeping the largest count seen on each side.
func usageOf(body []byte, stream bool) (in, out int, ok bool) {
	docs := [][]byte{body}
	if stream {
		docs = sseData(body)
	}
	for _, d := range docs {
		var u usageDoc
		if json.Unmarshal(d, &u) != nil {
			continue
		}
		if g := u.Gemini; g != nil {
			ok = true
			in, out = max(in, g.Prompt), max(out, g.Candidates+g.Thoughts)
		}
		for _, b := range []*usageBlock{u.Usage, messageUsage(u), responseUsage(u)} {
			if b == nil {
				continue
			}
			ok = true
			in = max(in, b.PromptTokens, b.InputTokens)
			out = max(out, b.CompletionTokens, b.OutputTokens)
		}
	}
	return in, out, ok
}

func responseUsage(u usageDoc) *usageBlock {
	if u.Response == nil {
		return nil
	}
	return u.Response.Usage
}

func messageUsage(u usageDoc) *usageBlock {
	if u.Message == nil {
		return nil
	}
	return u.Message.Usage
}
