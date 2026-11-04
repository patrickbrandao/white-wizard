package anth

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestForgedRequestError: the refusal of the strict client verification
// (spec 020) is a 403 forged_request_error with the driver's message, the
// x-wwz-client-verification header with the reason, request-id and
// x-should-retry: false, and no account headers (the credential was not
// looked at).
func TestForgedRequestError(t *testing.T) {
	h := APIHooks()
	w := core.NewMemWriter()
	e := &core.Error{Kind: core.ErrForgedRequest, Message: "Client verification failed: x (header_order: y).", Extra: map[string]any{
		core.ExtraVerificationReason: "header_order", core.ExtraVerificationDetail: "y",
	}}
	WriteError(&h, w, &core.Request{RequestID: "req_011CForged"}, e, nil)
	want := `{"type":"error","error":{"type":"forged_request_error","message":"Client verification failed: x (header_order: y)."},"request_id":"req_011CForged"}`
	if w.StatusCode() != 403 || string(w.Body()) != want {
		t.Fatalf("%d %s", w.StatusCode(), w.Body())
	}
	hd := w.Headers()
	if hd["x-wwz-client-verification"] != "failed; reason=header_order" || hd["request-id"] != "req_011CForged" ||
		hd["x-should-retry"] != "false" || hd["Content-Type"] != "application/json" {
		t.Errorf("headers %v", hd)
	}
	if _, ok := hd["anthropic-organization-id"]; ok {
		t.Errorf("account headers on a forged request: %v", hd)
	}
	// Another 403 still carries them.
	w = core.NewMemWriter()
	WriteError(&h, w, &core.Request{RequestID: "req_011CPerm"}, &core.Error{Kind: core.ErrPermission}, nil)
	if _, ok := w.Headers()["anthropic-organization-id"]; !ok {
		t.Errorf("permission error without account headers: %v", w.Headers())
	}
	// Without a message, a generic one.
	status, body, _, hdr := AnthropicErrorBody(&core.Request{}, &core.Error{Kind: core.ErrForgedRequest})
	inner := body.(map[string]any)["error"].(map[string]any)
	if status != 403 || inner["message"] != "Client verification failed: the client is forging requests." || hdr["x-wwz-client-verification"] != "failed; reason=" {
		t.Errorf("%d %v %v", status, inner, hdr)
	}
}
