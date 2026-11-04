package anthropic_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
)

const refusalDetailsJSON = `"stop_details":{"type":"refusal","category":null,"explanation":null}`

// TestStopDetailsRefusalBody: a refusal carries stop_details {type,
// category, explanation} in the body, in the real place after
// stop_sequence; any other stop_reason keeps stop_details null (spec 006,
// task 403).
func TestStopDetailsRefusalBody(t *testing.T) {
	for _, tc := range []struct {
		reason core.FinishReason
		want   string
	}{
		{core.FinishRefusal, `"stop_reason":"refusal","stop_sequence":null,` + refusalDetailsJSON + `,"usage":`},
		{core.FinishContentFilter, `"stop_reason":"refusal","stop_sequence":null,` + refusalDetailsJSON + `,"usage":`},
		{core.FinishStop, `"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"usage":`},
		{core.FinishLength, `"stop_reason":"max_tokens","stop_sequence":null,"stop_details":null,"usage":`},
	} {
		w := core.NewMemWriter()
		resp := &core.Response{ID: "msg_1", Model: "m", FinishReason: tc.reason}
		if err := newDriver(t).WriteResponse(w, probeReq(false), resp, nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(w.Body()), tc.want) {
			t.Errorf("%s: %s", tc.reason, w.Body())
		}
	}
}

// TestStopDetailsRefusalStream: message_start keeps stop_details null (no
// stop_reason yet); the message_delta of a refusal carries the object, in
// the real place between stop_sequence and container.
func TestStopDetailsRefusalStream(t *testing.T) {
	for _, tc := range []struct {
		reason core.FinishReason
		want   string
	}{
		{core.FinishRefusal, `"delta":{"stop_reason":"refusal","stop_sequence":null,` + refusalDetailsJSON + `,"container":null}`},
		{core.FinishStop, `"delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"container":null}`},
	} {
		w := core.NewMemWriter()
		sw := newDriver(t).StreamWriter(w, probeReq(true), nil)
		if err := sw.Start(&core.Response{ID: "msg_1", Model: "m"}); err != nil {
			t.Fatal(err)
		}
		if err := sw.TextDelta("partial"); err != nil {
			t.Fatal(err)
		}
		if err := sw.Finish(tc.reason, core.Usage{}, true); err != nil {
			t.Fatal(err)
		}
		body := string(w.Body())
		start, _, _ := strings.Cut(body, "event: content_block_start")
		if !strings.Contains(start, `"stop_reason":null,"stop_sequence":null,"stop_details":null`) {
			t.Errorf("%s message_start: %s", tc.reason, start)
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: %s", tc.reason, body)
		}
	}
}
