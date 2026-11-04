package anthropic

import (
	"github.com/patrickbrandao/white-wizard/internal/driver/anth"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// refusalDetails is stop_details of a refusal (spec 006, "Recusa como
// sucesso"; docs "Refusals and fallback"): {type, category, explanation}.
// The simulated refusal maps to no named policy area, so category and
// explanation are null, the documented permanent form for that case;
// recommended_model only comes with fallbacks (beta), which the driver does
// not implement.
func refusalDetails() oai.Obj {
	return oai.O("type", "refusal", "category", nil, "explanation", nil)
}

// shapeStopDetails is the anthropic driver's anth.Hooks.Shape. The dialect
// writes stop_details: null, the real value for every stop_reason except
// refusal; a refusal gets its object in the message (body and
// message_start) and in the message_delta of the stream.
func shapeStopDetails(p anth.Part, o oai.Obj) oai.Obj {
	switch p {
	case anth.PartMessage:
		if isRefusal(o) {
			o.Set("stop_details", refusalDetails())
		}
	case anth.PartEvent:
		if t, _ := o.Get("type"); t != "message_delta" {
			return o
		}
		d, _ := o.Get("delta")
		if delta, ok := d.(oai.Obj); ok && isRefusal(delta) {
			delta.Set("stop_details", refusalDetails())
		}
	}
	return o
}

func isRefusal(o oai.Obj) bool {
	r, _ := o.Get("stop_reason")
	return r == "refusal"
}
