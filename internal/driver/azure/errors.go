package azure

import (
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Error texts of Azure OpenAI (spec 006 16.6). They come from the Microsoft
// Q&A and GitHub reports cited there: all "not conferred" (ADR 0043).
const (
	msgInvalidKey = "Access denied due to invalid subscription key or wrong API endpoint. " +
		"Make sure to provide a valid key for an active subscription and use a correct regional API endpoint for your resource."
	msgMissingKey      = "Access denied due to missing subscription key. Make sure to include subscription key when making requests to an API."
	msgDeploymentGone  = "The API deployment for this resource does not exist. If you created the deployment within the last 5 minutes, please wait a moment and try again."
	msgResourceMissing = "Resource not found"
	msgContentFilter   = "The response was filtered due to the prompt triggering Azure OpenAI's content management policy. " +
		"Please modify your prompt and retry. To learn more about our content filtering policies please read our documentation: https://go.microsoft.com/fwlink/?linkid=2198766"
	msgPermission = "Principal does not have access to API/Operation."
	msgServer     = "Backend returned unexpected response. Please contact Microsoft for help."
	msgBusy       = "The service is temporarily unable to process your request. Please try again later."
	msgTimeout    = "The operation was timeout."
	msgBadGateway = "Bad gateway."
	// The OpenAI texts the service passes through from the model backend.
	msgBadBody = "We could not parse the JSON body of your request. (HINT: This likely means you aren't using your HTTP library correctly. " +
		"The OpenAI API expects a JSON payload, but what was sent was not valid JSON.)"
	msgContext = "This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens. Please reduce the length of the messages."
)

// errorFormat is the byte layout of the error bodies: compact (not
// conferred).
var errorFormat = oai.Format{RawHTML: true}

// azureEnvelope is Azure's own {"error": {"code", "message"}}.
func azureEnvelope(code, msg string) oai.Obj {
	return oai.O("error", oai.O("code", code, "message", msg))
}

// openAIEnvelope is the OpenAI-shaped body the service passes through from
// the model backend: {"error": {"message", "type", "param", "code"}}.
func openAIEnvelope(msg string, typ, param, code any) oai.Obj {
	return oai.O("error", oai.O("message", msg, "type", typ, "param", param, "code", code))
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// orDefault is msg, or def when msg is empty.
func orDefault(msg, def string) string {
	if msg == "" {
		return def
	}
	return msg
}

// errorBody maps an error to Azure's answer: status, body, content type
// and headers (spec 006 16.6). apim-request-id goes on every error.
func errorBody(req *core.Request, e *core.Error) (int, any, string, map[string]string) {
	hdr := map[string]string{"apim-request-id": requestID(req)}
	var (
		status int
		body   oai.Obj
	)
	switch e.Kind {
	case core.ErrInvalidRequest:
		status, body = 400, openAIEnvelope(orDefault(e.Message, msgBadBody), "invalid_request_error", nilIfEmpty(e.Param), nilIfEmpty(e.Code))
	case core.ErrContextLength:
		status, body = 400, openAIEnvelope(orDefault(e.Message, msgContext), "invalid_request_error", "messages", "context_length_exceeded")
	case core.ErrContentFilter:
		status = 400
		body = oai.O("error", oai.O(
			"message", orDefault(e.Message, msgContentFilter), "type", nil, "param", "prompt", "code", "content_filter", "status", 400,
			"innererror", oai.O("code", "ResponsibleAIPolicyViolation", "content_filter_result", promptFiltered()),
		))
	case core.ErrInvalidAPIKey:
		status, body = 401, azureEnvelope("401", msgInvalidKey)
		if !hasCredential(req) {
			body = azureEnvelope("401", msgMissingKey)
		}
	case core.ErrPermission, core.ErrGeoBlocked, core.ErrAccountSuspended, core.ErrAccountBanned:
		status, body = 403, azureEnvelope("PermissionDenied", orDefault(e.Message, msgPermission))
	case core.ErrModelNotFound:
		status, body = 404, azureEnvelope("DeploymentNotFound", msgDeploymentGone)
	case core.ErrNotFound:
		status, body = 404, azureEnvelope("404", msgResourceMissing)
	case core.ErrMethodNotAllowed:
		status, body = 405, azureEnvelope("405", orDefault(e.Message, "Method not allowed"))
	case core.ErrRequestTooLarge:
		status, body = 413, openAIEnvelope(orDefault(e.Message, "Request entity too large"), "invalid_request_error", nil, "request_too_large")
	case core.ErrRateLimit, core.ErrSlowDown, core.ErrQuota, core.ErrSpendLimit, core.ErrBudget:
		status, body = 429, azureEnvelope("429", rateLimitMessage(req, e))
		if e.RetryAfter > 0 {
			// Standard deployments send retry-after-ms only; retry-after
			// is the provisioned (PTU) header (spec 006 16.6).
			hdr["retry-after-ms"] = strconv.FormatInt(e.RetryAfter.Milliseconds(), 10)
		}
	case core.ErrBadGateway:
		status, body = 502, azureEnvelope("BadGateway", orDefault(e.Message, msgBadGateway))
	case core.ErrOverloaded:
		status, body = 503, azureEnvelope("ServiceUnavailable", orDefault(e.Message, msgBusy))
		if e.RetryAfter > 0 {
			hdr["retry-after"] = strconv.FormatInt(oai.CeilSeconds(e.RetryAfter), 10)
		}
	case core.ErrGatewayTimeout:
		status, body = 504, azureEnvelope("Timeout", orDefault(e.Message, msgTimeout))
	default:
		status, body = 500, azureEnvelope("InternalServerError", orDefault(e.Message, msgServer))
	}
	if e.Status > 0 {
		status = e.Status
	}
	b, _ := errorFormat.Marshal(body)
	return status, b, "application/json", hdr
}

// rateLimitMessage is the 429 text: the operation, the api-version, call
// (requests) or token rate limit and the wait in whole seconds.
func rateLimitMessage(req *core.Request, e *core.Error) string {
	unit := "call"
	if lim, _ := e.Extra["limit"].(string); lim == "tpm" || lim == "itpm" || lim == "otpm" || lim == "tpd" || lim == "tokens" {
		unit = "token"
	}
	secs := max(oai.CeilSeconds(e.RetryAfter), 1)
	return "Requests to the " + operationOf(req) + " Operation under Azure OpenAI API version " + apiVersionOf(req) +
		" have exceeded " + unit + " rate limit of your current OpenAI S0 pricing tier. Please retry after " +
		strconv.FormatInt(secs, 10) + " seconds. Please go here: https://aka.ms/oai/quotaincrease if you would like to further increase the default rate limit."
}

// hasCredential reports whether the request presented a key at all.
func hasCredential(req *core.Request) bool {
	if req == nil {
		return true
	}
	return req.Header("api-key") != "" || req.Header("authorization") != ""
}

// streamError is the mid-stream error chunk (the OpenAI one; not conferred).
func streamError(e *core.Error) any { return oai.OpenAIStreamError(e) }
