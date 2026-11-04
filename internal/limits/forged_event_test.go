package limits

import (
	"context"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

// TestFinishForgedRequestEvent: the refusal of the strict client
// verification (spec 020) finishes without subjects: only the error event
// is written, with fields.reason and without denied (asyncapi).
func TestFinishForgedRequestEvent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	err := m.Finish(ctx, FinishRequest{RequestID: "r1", Now: time.Now(), Status: 403, ErrorKind: core.ErrForgedRequest, Driver: "anthropic", Endpoint: "messages", Reason: "header_order"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := m.Events(ctx, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %+v", err, evs)
	}
	ev := evs[0]
	if ev.Type != "error" || ev.Error != "forged_request" || ev.Status != 403 || ev.Fields["reason"] != "header_order" || ev.Fields["denied"] != nil ||
		ev.Fields["prompt_tokens"] != 0 || ev.Fields["output_tokens"] != 0 || ev.Entity != nil {
		t.Errorf("event %+v", ev)
	}
	// Without Reason, no fields.reason.
	if ev := (&FinishRequest{ErrorKind: core.ErrServerError}).event(time.Now()); ev.Fields["reason"] != nil {
		t.Errorf("reason without Reason: %+v", ev.Fields)
	}
}
