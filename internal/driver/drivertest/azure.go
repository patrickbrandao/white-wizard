package drivertest

import (
	"regexp"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// Azure OpenAI (spec 006 16.6, ADR 0043): the Chat Completions dialect of
// OpenAI with Azure's credential (api-key), its error envelope ({"error":
// {"code", "message"}} for its own errors, OpenAI's for what the model
// backend refuses), apim-request-id instead of x-request-id and the content
// filter annotations: prompt_filter_results on the body and on a first
// stream event with empty id, object and model, and content_filter_results
// on every choice. Everything comes from the documentation (no real key).

var (
	credAzure = map[string]string{"api-key": "wwz-azure-test-key"}
	reUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	idsAzure  = &IDFormat{Response: reChatcmpl, ToolCall: reCall, Request: reUUID}
)

// The Azure texts of spec 006 16.6.
const (
	azureMsgInvalidKey = "Access denied due to invalid subscription key or wrong API endpoint. " +
		"Make sure to provide a valid key for an active subscription and use a correct regional API endpoint for your resource."
	azureMsgDeployment = "The API deployment for this resource does not exist. If you created the deployment within the last 5 minutes, please wait a moment and try again."
)

func azureErrorCases() []ErrorCase {
	return []ErrorCase{
		{Name: "invalid_request", Err: core.Error{Kind: core.ErrInvalidRequest, Message: "Unrecognized request argument supplied: foo"}, Status: 400, Fields: map[string]any{"error.type": "invalid_request_error", "error.code": nil, "error.param": nil, "error.message": "Unrecognized request argument supplied: foo"}},
		{Name: "context_length", Err: core.Error{Kind: core.ErrContextLength, Message: "too long"}, Status: 400, Fields: map[string]any{"error.code": "context_length_exceeded"}},
		{Name: "content_filter", Err: core.Error{Kind: core.ErrContentFilter}, Status: 400, Fields: map[string]any{"error.code": "content_filter", "error.param": "prompt", "error.status": 400, "error.innererror.code": "ResponsibleAIPolicyViolation", "error.innererror.content_filter_result.hate.filtered": true}},
		{Name: "invalid_api_key", Err: core.Error{Kind: core.ErrInvalidAPIKey}, Status: 401, Fields: map[string]any{"error.code": "401"}},
		{Name: "permission", Err: core.Error{Kind: core.ErrPermission}, Status: 403, Fields: map[string]any{"error.code": "PermissionDenied"}},
		{Name: "deployment_not_found", Err: core.Error{Kind: core.ErrModelNotFound}, Status: 404, Fields: map[string]any{"error.code": "DeploymentNotFound", "error.message": azureMsgDeployment}},
		{Name: "resource_not_found", Err: core.Error{Kind: core.ErrNotFound}, Status: 404, Fields: map[string]any{"error.code": "404", "error.message": "Resource not found"}},
		{
			Name: "rate_limit_tokens", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 1500 * time.Millisecond, Extra: lim("tpm")}, Status: 429,
			Fields:  map[string]any{"error.code": "429", "error.message": "Requests to the ChatCompletions_Create Operation under Azure OpenAI API version 2024-10-21 have exceeded token rate limit of your current OpenAI S0 pricing tier. Please retry after 2 seconds. Please go here: https://aka.ms/oai/quotaincrease if you would like to further increase the default rate limit."},
			Headers: map[string]string{"retry-after-ms": "1500", "x-ratelimit-limit-requests": "100", "apim-request-id": "*"}, NoHeaders: []string{"retry-after"}, WithRateLimit: true,
		},
		{Name: "rate_limit_requests", Err: core.Error{Kind: core.ErrRateLimit, RetryAfter: 2 * time.Second, Extra: lim("rpm")}, Status: 429, Fields: map[string]any{"error.code": "429"}, Headers: map[string]string{"retry-after-ms": "2000"}},
		{Name: "server_error", Err: core.Error{Kind: core.ErrServerError}, Status: 500, Fields: map[string]any{"error.code": "InternalServerError"}},
		{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, Status: 503, Fields: map[string]any{"error.code": "ServiceUnavailable"}, Headers: map[string]string{"retry-after": "3"}},
		{Name: "gateway_timeout", Err: core.Error{Kind: core.ErrGatewayTimeout}, Status: 504, Fields: map[string]any{"error.code": "Timeout"}},
	}
}

func azureProfile() Profile {
	chat := Surface{
		Envelope: EnvAzure, RateLimit: RLOpenAI, ReasoningField: "hidden",
		Errors: azureErrorCases(), IDs: idsAzure, SuccessHeaders: []string{"apim-request-id"},
		RetryAfter:    []RetryAfterCase{{Name: "overloaded", Err: core.Error{Kind: core.ErrOverloaded, RetryAfter: 2500 * time.Millisecond}, Want: "3"}},
		ContentFilter: true,
	}
	return Profile{
		Routes: concat(
			routes("POST", "chat", "/openai/deployments/*", "/openai/v1/chat/completions"),
			routes("POST", "responses", "/openai/v1/responses"),
			routes("GET", "models", "/openai/models", "/openai/v1/models"),
			routes("GET", "model", "/openai/models/*", "/openai/v1/models/*"),
		),
		Credential: credAzure, Key: "wwz-azure-test-key",
		Foreign:             []map[string]string{credOpenAI, credAnthropic, credOpenCode, credOpenRouter, credGemini},
		Models:              "azure",
		ModelNotFoundStatus: 404,
		ModelNotFoundFields: map[string]any{"error.code": "404"},
		Policy:              driver.LimitPolicy{Entities: []string{"ip", "key", "project", "account", "deployment"}, TPMReserve: "max_tokens", ResetFormat: "go_duration"},
		Surfaces:            map[driver.Kind]Surface{driver.KindChat: chat},
		Responses:           &ResponsesProfile{EventLines: true, ID: reResp50},
	}
}

// checkAzureEnvelope checks an Azure error body: error.message a string
// and error.code present (a string, or null on what the model backend
// refuses); the key check carries the documented text.
func checkAzureEnvelope(t *testing.T, body map[string]any, status int, e *core.Error) {
	t.Helper()
	if _, ok := mustPath(body, "error.message").(string); !ok {
		t.Errorf("error.message must be a string: %v", body)
	}
	expectPresent(t, body, "error.code")
	if c, ok := mustPath(body, "error.code").(string); ok && c == "" {
		t.Errorf("error.code is empty: %v", body)
	}
	if e.Kind == core.ErrInvalidAPIKey && status == 401 {
		if m := str(body, "error.message"); m != azureMsgInvalidKey && m != "Access denied due to missing subscription key. Make sure to include subscription key when making requests to an API." {
			t.Errorf("401 message %q is not Azure's", m)
		}
	}
}

// checkPromptFilterFrame checks the first event of an Azure chat stream.
func checkPromptFilterFrame(t *testing.T, f frame) {
	t.Helper()
	if f.Data == "" || f.Data == "[DONE]" {
		t.Fatalf("first frame %+v, want the prompt_filter_results event", f)
	}
	ch := decodeData(t, f)
	expectPath(t, ch, "id", "")
	expectPath(t, ch, "object", "")
	expectPath(t, ch, "created", 0)
	expectPath(t, ch, "model", "")
	expectPath(t, ch, "usage", nil)
	if c := arr(ch, "choices"); c == nil || len(c) != 0 {
		t.Errorf("prompt filter event choices %v, want []", ch["choices"])
	}
	checkPromptFilterResults(t, ch)
}

// checkPromptFilterResults checks prompt_filter_results of a body or of
// the first stream event.
func checkPromptFilterResults(t *testing.T, body any) {
	t.Helper()
	expectPath(t, body, "prompt_filter_results.0.prompt_index", 0)
	for _, c := range []string{"hate", "self_harm", "sexual", "violence"} {
		expectPath(t, body, "prompt_filter_results.0.content_filter_results."+c+".filtered", false)
		expectPath(t, body, "prompt_filter_results.0.content_filter_results."+c+".severity", "safe")
	}
	expectPath(t, body, "prompt_filter_results.0.content_filter_results.jailbreak.detected", false)
}

// checkChoiceFilter checks content_filter_results of a choice: the four
// categories when it carries text, an empty object otherwise.
func checkChoiceFilter(t *testing.T, choice any, hasText bool) {
	t.Helper()
	v, ok := path(choice, "content_filter_results")
	if !ok {
		t.Errorf("choice without content_filter_results: %v", choice)
		return
	}
	m, _ := v.(map[string]any)
	if !hasText {
		if len(m) != 0 {
			t.Errorf("content_filter_results = %v, want {} on a choice without text", v)
		}
		return
	}
	for _, c := range []string{"hate", "self_harm", "sexual", "violence"} {
		expectPath(t, m, c+".severity", "safe")
	}
	expectPath(t, m, "protected_material_text.detected", false)
	expectPath(t, m, "protected_material_code.detected", false)
}
