// Package scenario implements sequential test templates: a scenario is an
// ordered list of steps; the step for a request is chosen from the
// conversation state derived from the message history (and from decodable
// tool-call ids), so the server stays stateless.
package scenario

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/patrickbrandao/white-wizard/internal/config"
)

// Scenario is one YAML file.
type Scenario struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Match       *Match            `yaml:"match"`
	Vars        map[string]string `yaml:"vars"`
	Steps       []Step            `yaml:"steps"`
	OnNoMatch   string            `yaml:"on_no_match"`  // fallback | error | repeat_last
	OnExhausted string            `yaml:"on_exhausted"` // repeat_last | error | fallback
	StepLimit   int               `yaml:"step_limit"`
	Fallback    string            `yaml:"fallback"` // random | echo
	// ToolCallIDFormat is encoded (default: ids carry scenario and step) or
	// native (the driver's real id format; expect.last_step is unavailable).
	ToolCallIDFormat string `yaml:"tool_call_id_format"`
	compiled         map[string]*regexp.Regexp
}

// Match decides whether a scenario applies to a request.
type Match struct {
	ToolsInclude       []string          `yaml:"tools_include"`
	Model              string            `yaml:"model"`
	Driver             string            `yaml:"driver"`
	UserMessageMatches string            `yaml:"user_message_matches"`
	SystemMatches      string            `yaml:"system_matches"`
	Header             map[string]string `yaml:"header"`
}

// Expect is the condition for a step.
type Expect struct {
	Turn                  *int              `yaml:"turn"`
	TurnGte               *int              `yaml:"turn_gte"`
	ToolResults           *int              `yaml:"tool_results"`
	ToolResultsGte        *int              `yaml:"tool_results_gte"`
	LastTool              string            `yaml:"last_tool"`
	LastStep              string            `yaml:"last_step"` // decoded from tool call id
	LastToolResultMatches string            `yaml:"last_tool_result_matches"`
	ToolResultFor         string            `yaml:"tool_result_for"`
	UserMessageMatches    string            `yaml:"user_message_matches"`
	LatestMessageRole     string            `yaml:"latest_message_role"`
	SemanticMatch         *SemanticMatch    `yaml:"semantic_match"`
	Model                 string            `yaml:"model"`
	Header                map[string]string `yaml:"header"`
	Always                bool              `yaml:"always"`
}

// Step is one response template.
type Step struct {
	ID              string                 `yaml:"id"`
	Expect          *Expect                `yaml:"expect"`
	Reasoning       any                    `yaml:"reasoning"` // "literal" | {tokens: "40..120"} | {tokens: 80}
	Content         any                    `yaml:"content"`   // "literal/template" | {tokens: "50..200"}
	ToolCalls       []ToolCallSpec         `yaml:"tool_calls"`
	Parallel        *bool                  `yaml:"parallel"`
	FinishReason    string                 `yaml:"finish_reason"`
	Error           string                 `yaml:"error"`
	ErrorMessage    string                 `yaml:"error_message"`
	Latency         *config.LatencyProfile `yaml:"latency"`
	Speed           float64                `yaml:"speed"`
	Stall           *Stall                 `yaml:"stall"`
	CutAfterTokens  int                    `yaml:"cut_after_tokens"`
	FailFirst       *FailFirst             `yaml:"fail_first"`
	Repeat          *Repeat                `yaml:"repeat"`
	Goto            string                 `yaml:"goto"`
	Weight          float64                `yaml:"weight"`
	Headers         map[string]string      `yaml:"headers"` // extra response headers
	ToolMisbehavior string                 `yaml:"tool_misbehavior"`
	Moderation      string                 `yaml:"moderation"` // finish_reason | http_403 | refusal
	Timeline        []TimelineEvent        `yaml:"timeline"`
}

// ToolCallSpec describes a tool call to emit.
type ToolCallSpec struct {
	Name string `yaml:"name"` // literal, any, any_read, any_write
	// Args: a map (templated values), the string "schema" (generate from the
	// tool's schema) or "schema+" (generate then overlay Overlay).
	Args    any            `yaml:"args"`
	Overlay map[string]any `yaml:"overlay"`
	ID      string         `yaml:"id"`
}

// Stall pauses the stream after N tokens for a duration.
type Stall struct {
	AfterTokens int    `yaml:"after_tokens"`
	Duration    string `yaml:"duration"`
	MS          int    `yaml:"ms"`
}

// FailFirst makes the step fail its first N attempts.
type FailFirst struct {
	Times        int    `yaml:"times"`
	Error        string `yaml:"error"`
	RetryAfterMS int    `yaml:"retry_after_ms"`
	Message      string `yaml:"message"`
}

// Repeat repeats the step N turns before advancing.
type Repeat struct {
	Times int `yaml:"times"`
}

// TimelineEvent is an explicit event in a response timeline.
type TimelineEvent struct {
	Type   string `yaml:"type"` // text | reasoning | tool_call | usage | ping | error | finish | delay
	Text   string `yaml:"text"`
	Tokens string `yaml:"tokens"`
	Name   string `yaml:"name"`
	Args   any    `yaml:"args"`
	Reason string `yaml:"reason"`
	Error  string `yaml:"error"`
	MS     int    `yaml:"ms"`
}

// TokenRange parses "40..120", "80" or {tokens: ...} forms.
type TokenRange struct {
	Min, Max  int
	Literal   string
	IsLiteral bool
}

// clampInt converts a decoded YAML unsigned integer to int, saturating at
// math.MaxInt instead of wrapping to a negative value.
func clampInt(u uint64) int {
	if u > math.MaxInt {
		return math.MaxInt
	}
	return int(u)
}

func parseTokenRange(v any) (TokenRange, error) {
	switch x := v.(type) {
	case nil:
		return TokenRange{}, nil
	case string:
		return TokenRange{Literal: x, IsLiteral: true}, nil
	case int:
		return TokenRange{Min: x, Max: x}, nil
	case uint64:
		return TokenRange{Min: clampInt(x), Max: clampInt(x)}, nil
	case float64:
		return TokenRange{Min: int(x), Max: int(x)}, nil
	case map[string]any:
		if t, ok := x["tokens"]; ok {
			switch tv := t.(type) {
			case string:
				parts := strings.SplitN(tv, "..", 2)
				lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err != nil {
					return TokenRange{}, fmt.Errorf("tokens %q: %w", tv, err)
				}
				hi := lo
				if len(parts) == 2 {
					hi, err = strconv.Atoi(strings.TrimSpace(parts[1]))
					if err != nil {
						return TokenRange{}, fmt.Errorf("tokens %q: %w", tv, err)
					}
				}
				return TokenRange{Min: lo, Max: hi}, nil
			case int:
				return TokenRange{Min: tv, Max: tv}, nil
			case uint64:
				return TokenRange{Min: clampInt(tv), Max: clampInt(tv)}, nil
			case float64:
				return TokenRange{Min: int(tv), Max: int(tv)}, nil
			}
		}
		if t, ok := x["text"]; ok {
			if s, ok := t.(string); ok {
				return TokenRange{Literal: s, IsLiteral: true}, nil
			}
		}
		return TokenRange{}, fmt.Errorf("unsupported form %v", x)
	}
	return TokenRange{}, fmt.Errorf("unsupported form %T", v)
}

// ParseYAML parses a scenario document.
func ParseYAML(data []byte) (*Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalWithOptions(data, &s, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks the scenario and compiles its regexes.
func (s *Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("scenario: name is required")
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("scenario %q: at least one step is required", s.Name)
	}
	if s.OnNoMatch == "" {
		s.OnNoMatch = "fallback"
	}
	if s.OnExhausted == "" {
		s.OnExhausted = "repeat_last"
	}
	if s.StepLimit == 0 {
		s.StepLimit = 50
	}
	if s.ToolCallIDFormat == "" {
		s.ToolCallIDFormat = IDFormatEncoded
	}
	s.compiled = map[string]*regexp.Regexp{}
	compile := func(where, pat string) error {
		if pat == "" {
			return nil
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("scenario %q: %s: invalid regex %q: %w", s.Name, where, pat, err)
		}
		s.compiled[pat] = re
		return nil
	}
	if s.Match != nil {
		if err := compile("match.user_message_matches", s.Match.UserMessageMatches); err != nil {
			return err
		}
		if err := compile("match.system_matches", s.Match.SystemMatches); err != nil {
			return err
		}
	}
	ids := map[string]bool{}
	for i := range s.Steps {
		st := &s.Steps[i]
		if st.ID == "" {
			st.ID = fmt.Sprintf("step%d", i+1)
		}
		if ids[st.ID] {
			return fmt.Errorf("scenario %q: duplicate step id %q", s.Name, st.ID)
		}
		ids[st.ID] = true
		if st.Expect != nil {
			if err := compile("expect.last_tool_result_matches", st.Expect.LastToolResultMatches); err != nil {
				return err
			}
			if err := compile("expect.user_message_matches", st.Expect.UserMessageMatches); err != nil {
				return err
			}
			if sm := st.Expect.SemanticMatch; sm != nil {
				if err := sm.prepare(); err != nil {
					return fmt.Errorf("scenario %q step %q: expect.semantic_match: %w", s.Name, st.ID, err)
				}
			}
		}
		if _, err := parseTokenRange(st.Reasoning); err != nil {
			return fmt.Errorf("scenario %q step %q: reasoning: %w", s.Name, st.ID, err)
		}
		if _, err := parseTokenRange(st.Content); err != nil {
			return fmt.Errorf("scenario %q step %q: content: %w", s.Name, st.ID, err)
		}
		if st.ToolMisbehavior != "" {
			if _, ok := config.CanonicalToolMisbehavior(st.ToolMisbehavior); !ok {
				return fmt.Errorf("scenario %q step %q: tool_misbehavior: unknown kind %q", s.Name, st.ID, st.ToolMisbehavior)
			}
		}
		for j, tc := range st.ToolCalls {
			if tc.Name == "" {
				return fmt.Errorf("scenario %q step %q: tool_calls[%d].name is required", s.Name, st.ID, j)
			}
		}
	}
	for _, st := range s.Steps {
		if st.Goto != "" && !ids[st.Goto] {
			return fmt.Errorf("scenario %q step %q: goto unknown step %q", s.Name, st.ID, st.Goto)
		}
	}
	return nil
}

func (s *Scenario) re(pat string) *regexp.Regexp {
	if pat == "" {
		return nil
	}
	if re, ok := s.compiled[pat]; ok {
		return re
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil
	}
	if s.compiled == nil {
		s.compiled = map[string]*regexp.Regexp{}
	}
	s.compiled[pat] = re
	return re
}

// Tool call id formats (spec 011, ADR 0027).
const (
	IDFormatEncoded = "encoded"
	IDFormatNative  = "native"
)

// DefaultSemanticThreshold is the threshold of a semantic_match written as a
// bare string.
const DefaultSemanticThreshold = 0.5

// SemanticMatch is expect.semantic_match (I12): the cosine similarity between
// the word-frequency vectors of the last user message and Text. It is written
// either as the text alone or as {text, threshold}.
type SemanticMatch struct {
	Text      string
	Threshold float64
	// explicit is true when the YAML set threshold, so 0 is an error rather
	// than the default.
	explicit bool
	vec      map[string]float64
	norm     float64
}

// UnmarshalYAML accepts "text" or {text: "...", threshold: 0.7}.
func (m *SemanticMatch) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	*m = SemanticMatch{}
	switch x := raw.(type) {
	case string:
		m.Text = x
		return nil
	case map[string]any:
		for k, v := range x {
			switch k {
			case "text":
				s, ok := v.(string)
				if !ok {
					return fmt.Errorf("semantic_match.text must be a string, got %T", v)
				}
				m.Text = s
			case "threshold":
				f, ok := toFloat(v)
				if !ok {
					return fmt.Errorf("semantic_match.threshold must be a number, got %T", v)
				}
				m.Threshold, m.explicit = f, true
			default:
				return fmt.Errorf("semantic_match: unknown field %q", k)
			}
		}
		return nil
	}
	return fmt.Errorf("semantic_match must be a string or {text, threshold}, got %T", raw)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// prepare validates the threshold and pre-computes the vector of Text.
func (m *SemanticMatch) prepare() error {
	if m.Threshold == 0 && !m.explicit {
		m.Threshold = DefaultSemanticThreshold
	}
	if math.IsNaN(m.Threshold) || m.Threshold <= 0 || m.Threshold > 1 {
		return fmt.Errorf("threshold must be in (0, 1], got %v", m.Threshold)
	}
	if strings.TrimSpace(m.Text) == "" {
		return fmt.Errorf("text is required")
	}
	m.vec, m.norm = wordVector(m.Text)
	return nil
}

// Matches reports whether msg is at least Threshold similar to Text. The
// threshold and the vector of Text are settled at load (Validate); a value
// built in code without Validate uses the default threshold.
func (m *SemanticMatch) Matches(msg string) bool {
	t := m.Threshold
	if t == 0 && !m.explicit {
		t = DefaultSemanticThreshold
	}
	return t > 0 && m.Similarity(msg) >= t-1e-9 // tolerate rounding at t = 1
}

// Similarity is the cosine between the word vectors of msg and Text.
func (m *SemanticMatch) Similarity(msg string) float64 {
	ref, rn := m.vec, m.norm
	if ref == nil {
		ref, rn = wordVector(m.Text) // not validated: compute without caching
	}
	v, n := wordVector(msg)
	if n == 0 || rn == 0 {
		return 0
	}
	dot := 0.0
	for w, c := range v {
		dot += c * ref[w]
	}
	return dot / (n * rn)
}

// wordRe is a word: a run of letters and digits.
var wordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// wordVector counts the lower-cased words of s and returns the counts and
// their Euclidean norm.
func wordVector(s string) (map[string]float64, float64) {
	v := map[string]float64{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		v[w]++
	}
	sum := 0.0
	for _, c := range v {
		sum += c * c
	}
	return v, math.Sqrt(sum)
}
