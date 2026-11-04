package drift

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// Provider names, in report order.
const (
	ProviderOpenAI     = "openai"
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
	ProviderOpenCode   = "opencode"
	ProviderGemini     = "gemini"
	// ProviderOpenCodeGoogle is the Gemini route of OpenCode Go
	// (<base>/google/v1beta/models/{model}:generateContent, task 241): the
	// same key and session as opencode, probed apart because the dialect
	// and the shapes are Google's.
	ProviderOpenCodeGoogle = "opencode-google"
	// ProviderAzure has no public base (https://{resource}.openai.azure.com)
	// and no real key (ADR 0043): it is probed only against the base given
	// by --base-url (a fake provider) and never by default.
	ProviderAzure = "azure"
)

// ProviderNames lists the providers wwz-drift knows, in report order.
var ProviderNames = []string{ProviderOpenAI, ProviderAnthropic, ProviderOpenRouter, ProviderOpenCode, ProviderGemini, ProviderOpenCodeGoogle, ProviderAzure}

// DefaultProviders are probed when --providers is not given: every known
// provider but azure, which has no real base (ADR 0043).
var DefaultProviders = []string{ProviderOpenAI, ProviderAnthropic, ProviderOpenRouter, ProviderOpenCode, ProviderGemini, ProviderOpenCodeGoogle}

// Probe names, in report order.
const (
	ProbeModels     = "models"
	ProbeChat       = "chat"
	ProbeChatStream = "chat_stream"
	ProbeErrorAuth  = "error_auth"
	ProbeErrorModel = "error_model"
	// ProbeResponses and ProbeResponsesStream are POST .../responses
	// without and with stream (ADR 0055, task 239), on the providers with
	// a Responses route (Provider.ResponsesPath); the others report them
	// skipped/not_supported.
	ProbeResponses       = "responses"
	ProbeResponsesStream = "responses_stream"
)

// ProbeNames lists the probes, in report order.
var ProbeNames = []string{ProbeModels, ProbeChat, ProbeChatStream, ProbeErrorAuth, ProbeErrorModel, ProbeResponses, ProbeResponsesStream}

// Probe constants (spec 017).
const (
	InvalidKey    = "sk-invalid-wwz-drift"
	MissingModel  = "wwz-drift-modelo-inexistente"
	DefaultWWZKey = "wwz-key-drift"
	ProbePrompt   = "Responda só: ok"
	ProbeMaxToks  = 16
	// OpenCodeMaxToks leaves room for the reasoning of the OpenCode model.
	OpenCodeMaxToks = 256
	// AnthropicVersion is the anthropic-version header sent to Anthropic.
	AnthropicVersion = "2023-06-01"
	// OpenCodeUserAgent identifies the client to OpenCode Go, which
	// rejects the generic SDK agents (spec 006).
	OpenCodeUserAgent = "opencode/latest/0.0.0/wwz-drift"
)

// Dialects of the chat probes.
const (
	dialectOpenAI    = "openai"
	dialectAnthropic = "anthropic"
	// dialectGemini puts the model and the method in the path
	// (models/{model}:generateContent) and the key in x-goog-api-key.
	dialectGemini = "gemini"
	// dialectAzure is the OpenAI body on the deployment route: the model
	// (deployment) in the path, api-version in the query, the key in api-key.
	dialectAzure = "azure"
)

// AzureAPIVersion is the api-version of the Azure probes (latest GA, spec 006).
const AzureAPIVersion = "2024-10-21"

// modelSlot is replaced by the probe model in Provider.ChatPath (azure).
const modelSlot = "{model}"

// Provider describes how to probe one real provider and its wwz driver.
type Provider struct {
	Name string
	// Driver is the wwz driver behind the provider when it is not Name
	// (opencode-google is a route of the opencode driver).
	Driver string
	// EnvKey is the variable (and keys-file entry) holding the real key.
	EnvKey string
	// EnvAlt are other names of the same key, tried after EnvKey
	// (GEMENI_API_KEY, the spelling of the keys file).
	EnvAlt []string
	// BaseURL is the real base (spec 006); paths are appended to it.
	BaseURL string
	// WWZPrefix is the base of the driver on the wwz data plane (mount
	// prefix of the embedded wwz.yaml plus the driver's own base, spec 004).
	WWZPrefix  string
	ModelsPath string
	ChatPath   string
	// ResponsesPath is the Responses API route ("" = the provider has
	// none and the responses probes are skipped/not_supported).
	ResponsesPath string
	Dialect       string
	// DefaultModel is the probe model, the same on the real provider and on
	// wwz: fixed, cheap, alive and listed in /models on both sides (ADR 0049).
	// The embedded wwz.yaml serves it.
	DefaultModel string
	// StreamUsage adds stream_options.include_usage to the stream probe.
	StreamUsage bool
	// Session sends the OpenCode User-Agent and x-opencode-session.
	Session bool
	// MaxTokens overrides ProbeMaxToks. The OpenCode model reasons before
	// it answers, and with 16 tokens the real answer sometimes ends in
	// reasoning (finish_reason length, no content) and sometimes does not,
	// which would be reported as drift at random.
	MaxTokens int
}

var providers = map[string]Provider{
	ProviderOpenAI: {
		Name: ProviderOpenAI, EnvKey: "OPENAI_API_KEY", BaseURL: "https://api.openai.com",
		WWZPrefix: "/openai", ModelsPath: "/v1/models", ChatPath: "/v1/chat/completions",
		ResponsesPath: "/v1/responses",
		Dialect:       dialectOpenAI, DefaultModel: "gpt-4o-mini", StreamUsage: true,
	},
	ProviderAnthropic: {
		Name: ProviderAnthropic, EnvKey: "ANTHROPIC_API_KEY", BaseURL: "https://api.anthropic.com",
		WWZPrefix: "/anthropic", ModelsPath: "/v1/models", ChatPath: "/v1/messages",
		Dialect: dialectAnthropic, DefaultModel: "claude-haiku-4-5-20251001",
	},
	ProviderOpenRouter: {
		Name: ProviderOpenRouter, EnvKey: "OPENROUTER_API_KEY", BaseURL: "https://openrouter.ai/api",
		WWZPrefix: "/api", ModelsPath: "/v1/models", ChatPath: "/v1/chat/completions",
		ResponsesPath: "/v1/responses",
		Dialect:       dialectOpenAI, DefaultModel: "openai/gpt-4.1-nano",
	},
	ProviderOpenCode: {
		Name: ProviderOpenCode, EnvKey: "OPENCODE_API_KEY", BaseURL: "https://opencode.ai/inference/go",
		WWZPrefix: "/inference/go", ModelsPath: "/v1/models", ChatPath: "/openai/v1/chat/completions",
		ResponsesPath: "/openai/v1/responses",
		Dialect:       dialectOpenAI, DefaultModel: "deepseek-v4.1-flash", Session: true, MaxTokens: OpenCodeMaxToks,
	},
	// Gemini (task 164): the listing asks for every model in one page, so
	// nextPageToken is absent on both sides; gemini-2.5-flash-lite is "no
	// longer available to new users" (observed 2026-10-31), so the probe
	// uses the cheapest current model.
	ProviderGemini: {
		Name: ProviderGemini, EnvKey: "GEMINI_API_KEY", EnvAlt: []string{"GEMENI_API_KEY"},
		BaseURL: "https://generativelanguage.googleapis.com", WWZPrefix: "/gemini",
		ModelsPath: "/v1beta/models?pageSize=1000", ChatPath: "/v1beta/models/",
		Dialect: dialectGemini, DefaultModel: "gemini-3.5-flash-lite",
	},
	// The Gemini route of OpenCode Go (task 156): the gateway has no models
	// listing there (GET .../google/v1beta/models is a 404 without body,
	// observed 2026-10-31), and that 404 is what the models probe compares.
	// The model is translated (not Vertex AI) and reasons, hence the 256
	// tokens of the opencode probes.
	ProviderOpenCodeGoogle: {
		Name: ProviderOpenCodeGoogle, Driver: ProviderOpenCode, EnvKey: "OPENCODE_API_KEY", BaseURL: "https://opencode.ai/inference/go/google",
		WWZPrefix: "/inference/go/google", ModelsPath: "/v1beta/models", ChatPath: "/v1beta/models/",
		Dialect: dialectGemini, DefaultModel: "deepseek-v4.1-flash", Session: true, MaxTokens: OpenCodeMaxToks,
	},
	// Azure OpenAI (task 156, ADR 0043): deployment route with the latest
	// GA api-version; the deployment is the model id, as in the embedded
	// wwz.yaml. No BaseURL: only --base-url gives one.
	ProviderAzure: {
		Name: ProviderAzure, EnvKey: "AZURE_OPENAI_API_KEY", WWZPrefix: "/azure",
		ModelsPath: "/openai/models?api-version=" + AzureAPIVersion,
		ChatPath:   "/openai/deployments/" + modelSlot + "/chat/completions?api-version=" + AzureAPIVersion,
		// The v1 API (no api-version, model in the body), the only one
		// with Responses in the driver (ADR 0054).
		ResponsesPath: "/openai/v1/responses",
		Dialect:       dialectAzure, DefaultModel: "gpt-4o-mini", StreamUsage: true,
	},
}

// LookupProvider returns the description of a provider.
func LookupProvider(name string) (Provider, bool) {
	p, ok := providers[name]
	return p, ok
}

// DriverName is the wwz driver that serves the provider.
func (p Provider) DriverName() string {
	if p.Driver != "" {
		return p.Driver
	}
	return p.Name
}

// paid reports whether a probe costs money on the real provider.
func paid(probe string) bool {
	return probe == ProbeChat || probe == ProbeChatStream || responsesProbe(probe)
}

// responsesProbe reports whether probe goes to the Responses route.
func responsesProbe(probe string) bool {
	return probe == ProbeResponses || probe == ProbeResponsesStream
}

// request is one HTTP call of a probe.
type request struct {
	method string
	path   string
	body   []byte
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatBody struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// responsesBody is the body of the responses probes: the same prompt and
// token cap as the chat probes (16 is also the API's minimum of
// max_output_tokens).
type responsesBody struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Stream          bool   `json:"stream,omitempty"`
}

// probeRequest builds the request of probe for model.
func (p Provider) probeRequest(probe, model string) request {
	if probe == ProbeModels {
		return request{method: http.MethodGet, path: p.ModelsPath}
	}
	if probe == ProbeErrorModel {
		model = MissingModel
	}
	if p.Dialect == dialectGemini {
		return p.geminiRequest(probe, model)
	}
	maxTokens := ProbeMaxToks
	if p.MaxTokens > 0 {
		maxTokens = p.MaxTokens
	}
	if responsesProbe(probe) {
		b := responsesBody{Model: model, Input: ProbePrompt, MaxOutputTokens: maxTokens, Stream: probe == ProbeResponsesStream}
		body, _ := json.Marshal(b) // plain struct: cannot fail
		return request{method: http.MethodPost, path: p.ResponsesPath, body: body}
	}
	b := chatBody{Model: model, Messages: []chatMessage{{Role: "user", Content: ProbePrompt}}, MaxTokens: maxTokens}
	if probe == ProbeChatStream {
		b.Stream = true
		if p.StreamUsage {
			b.StreamOptions = &streamOptions{IncludeUsage: true}
		}
	}
	body, _ := json.Marshal(b) // plain struct: cannot fail
	return request{method: http.MethodPost, path: strings.ReplaceAll(p.ChatPath, modelSlot, model), body: body}
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens"`
}

type geminiBody struct {
	Contents         []geminiContent `json:"contents"`
	GenerationConfig geminiConfig    `json:"generationConfig"`
}

// geminiRequest is a chat probe in the Gemini dialect: generateContent, or
// streamGenerateContent with alt=sse for the stream probe.
func (p Provider) geminiRequest(probe, model string) request {
	maxTokens := ProbeMaxToks
	if p.MaxTokens > 0 {
		maxTokens = p.MaxTokens
	}
	b := geminiBody{
		Contents:         []geminiContent{{Role: "user", Parts: []geminiPart{{Text: ProbePrompt}}}},
		GenerationConfig: geminiConfig{MaxOutputTokens: maxTokens},
	}
	body, _ := json.Marshal(b) // plain struct: cannot fail
	method := ":generateContent"
	if probe == ProbeChatStream {
		method = ":streamGenerateContent?alt=sse"
	}
	return request{method: http.MethodPost, path: p.ChatPath + model + method, body: body}
}

// headers returns the request headers for key.
func (p Provider) headers(key, session, userAgent string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	switch p.Dialect {
	case dialectGemini:
		h.Set("x-goog-api-key", key)
	case dialectAzure:
		h.Set("api-key", key)
	case dialectAnthropic:
		h.Set("x-api-key", key)
		h.Set("anthropic-version", AnthropicVersion)
	default:
		h.Set("Authorization", "Bearer "+key)
	}
	h.Set("User-Agent", userAgent)
	if p.Session {
		h.Set("User-Agent", OpenCodeUserAgent)
		h.Set("x-opencode-session", session)
	}
	return h
}

// sensitiveHeaders never reach --save-raw.
var sensitiveHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key", "Cookie", "Set-Cookie", "Proxy-Authorization"}

func isSensitive(name string) bool {
	return slices.Contains(sensitiveHeaders, http.CanonicalHeaderKey(name))
}
