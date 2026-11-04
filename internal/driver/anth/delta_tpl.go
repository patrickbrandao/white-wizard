package anth

import "github.com/patrickbrandao/white-wizard/internal/driver/oai"

// Delta types of content_block_delta, as indexes of Stream.deltas.
const (
	deltaText = iota
	deltaThinking
	deltaInputJSON
)

// deltaKinds names each delta type and its text field.
var deltaKinds = [3]struct{ typ, field string }{
	deltaText:      {"text_delta", "text"},
	deltaThinking:  {"thinking_delta", "thinking"},
	deltaInputJSON: {"input_json_delta", "partial_json"},
}

// newTemplate builds the delta templates (a seam for the tests, which
// compare against rendering every delta).
var newTemplate = oai.NewFrameTemplate

// deltaTpl is the content_block_delta of one delta type at one block index,
// rendered once with sentinel text (oai.FrameTemplate): the deltas of a
// block cost no Obj and no marshalling.
type deltaTpl struct {
	index int
	t     *oai.FrameTemplate
	built bool
}

// delta writes the content_block_delta of kind at block index.
func (s *Stream) delta(kind, index int, text string) error {
	d := &s.deltas[kind]
	if text != "" && (!d.built || d.index != index) {
		*d = deltaTpl{index: index, built: true}
		d.t = newTemplate(func(text, _ string) ([]byte, error) {
			return s.deltaJSON(kind, index, text)
		}, false, !jsonFormat.RawHTML)
	}
	if text == "" || d.t == nil {
		b, err := s.deltaJSON(kind, index, text)
		if err != nil {
			return err
		}
		return s.frame("content_block_delta", b)
	}
	s.data = d.t.Append(s.data[:0], text, nil)
	return s.frame("content_block_delta", s.data)
}

// deltaJSON renders a content_block_delta event the general way.
func (s *Stream) deltaJSON(kind, index int, text string) ([]byte, error) {
	k := deltaKinds[kind]
	return jsonFormat.Marshal(s.h.shape(PartEvent, oai.O("type", "content_block_delta", "index", index, "delta", oai.O("type", k.typ, k.field, text))))
}
