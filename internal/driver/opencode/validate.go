package opencode

import (
	"encoding/json"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	js "github.com/patrickbrandao/white-wizard/internal/driver/jsonscan"
)

var _ driver.RequestValidator = (*Driver)(nil)

// The gateway forwards unknown fields and most value errors to the upstream
// provider, so it refuses only what it checks itself; those answers have
// their own envelopes, sent byte for byte (recorded on 2026-10-31 in
// testdata/real/opencode/validation/).

// ValidateRequest refuses, on Chat Completions:
//   - a body that is not JSON: 400 with an empty body and no content type;
//   - model not a non-empty string: the gateway's OpenAI-style envelope.
//
// Everything else is the upstream's answer and depends on the model.
//
// On Responses the driver runs the portable route's checks
// (checkResponses), but the real refuses an unknown model before most of
// them (order_unknown_model_vs_unknown_field, task 267).
func (d *Driver) ValidateRequest(kind driver.Kind, body []byte, _ driver.Headers, models driver.Models) *core.Error {
	switch kind {
	case driver.KindChat:
	case driver.KindResponses:
		return responsesModel(models, body)
	default:
		return nil
	}
	if !json.Valid(body) {
		return driver.RawError("invalid JSON", driver.RawReply{Status: 400})
	}
	fs, _ := js.Members(body)
	model, _ := js.Lookup(fs, "model")
	if s, _ := js.StringValue(model); s == "" {
		const msg = "`model` must be a non-empty string"
		b := `{"error":{"message":` + js.Quote(msg) + `,"type":"invalid_request_error","param":"model","code":null}}`
		return driver.RawError(msg, driver.RawReply{Status: 400, ContentType: "application/json", Body: []byte(b)})
	}
	return nil
}

// responsesModel answers a Responses body whose model is a non-empty string
// the engine does not serve as the engine does ("Model is unavailable."); a
// body that is not JSON and a model that is not a non-empty string stay
// with the driver's checks, which come first.
func responsesModel(models driver.Models, body []byte) *core.Error {
	if !json.Valid(body) {
		return nil
	}
	fs, _ := js.Members(body)
	model, _ := js.Lookup(fs, "model")
	if s, _ := js.StringValue(model); s != "" && !models.KnownModel(driver.KindResponses, s) {
		return &core.Error{Kind: core.ErrModelNotFound, Status: 400}
	}
	return nil
}
