package openrouter

import (
	"encoding/json"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	js "github.com/patrickbrandao/white-wizard/internal/driver/jsonscan"
)

var _ driver.RequestValidator = (*Driver)(nil)

// The gateway forwards unknown fields and most value errors to the upstream
// provider, so it refuses only what it checks itself; those answers have
// their own envelope (recorded on 2026-10-31 in
// testdata/real/openrouter/validation/).

// ValidateRequest refuses, on Chat Completions:
//   - messages missing, not an array or empty (a body that is not JSON
//     counts as {}): `Input required: specify "prompt" or "messages"`;
//   - model not a non-empty string and no models list: "No models provided";
//   - an unknown model, before the types: "<model> is not a valid model ID"
//     (auth-order/validkey_unknown_model_wrong_type_*, ADR 0053); without a
//     type error the engine refuses the model itself;
//   - temperature and top_p not numbers, stream not a boolean (zod).
//
// The answers are GatewayError, which adds the generation id and the CORS
// list (shape/error_validation.json).
func (d *Driver) ValidateRequest(kind driver.Kind, body []byte, _ driver.Headers, models driver.Models) *core.Error {
	if kind != driver.KindChat {
		return nil
	}
	var fs []js.Field
	if json.Valid(body) {
		fs, _ = js.Members(body)
	}
	_, prompt := js.Present(fs, "prompt")
	if v, _ := js.Lookup(fs, "messages"); !prompt && len(js.Elements(v)) == 0 {
		return GatewayError(`Input required: specify "prompt" or "messages"`)
	}
	model, _ := js.Lookup(fs, "model")
	list, _ := js.Lookup(fs, "models")
	if s, _ := js.StringValue(model); s == "" && len(js.Elements(list)) == 0 {
		return GatewayError("No models provided")
	}
	terr := samplingTypes(fs)
	if terr == nil {
		return nil
	}
	if s, _ := js.StringValue(model); s != "" && !models.KnownModel(kind, s) {
		return GatewayError(s + " is not a valid model ID")
	}
	return terr
}

// samplingTypes is the first type error of the sampling fields, or nil.
func samplingTypes(fs []js.Field) *core.Error {
	for _, name := range []string{"temperature", "top_p"} {
		if v, ok := js.Present(fs, name); ok && !js.IsNumber(js.KindOf(v)) {
			return GatewayError(name + ": Invalid input: expected number, received " + zodType(v))
		}
	}
	if v, ok := js.Present(fs, "stream"); ok && js.KindOf(v) != js.Bool {
		return GatewayError("stream: Invalid input")
	}
	return nil
}
