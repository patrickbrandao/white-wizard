package azure

import (
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// Content filter annotations (spec 006 16.6, "content filter annotations"
// and "content streaming" pages). The simulator never filters on its own:
// every category comes out safe and unfiltered, as the real annotates a
// harmless prompt and completion. Not conferred (ADR 0043).

func severity() oai.Obj { return oai.O("filtered", false, "severity", "safe") }

func detection() oai.Obj { return oai.O("filtered", false, "detected", false) }

// promptResults is content_filter_results of a prompt: the four harm
// categories and the jailbreak (Prompt Shields) detection.
func promptResults() oai.Obj {
	return oai.O("hate", severity(), "jailbreak", detection(), "self_harm", severity(), "sexual", severity(), "violence", severity())
}

// promptFiltered is innererror.content_filter_result of the 400
// content_filter: the prompt results with hate filtered at medium.
func promptFiltered() oai.Obj {
	r := promptResults()
	r.Set("hate", oai.O("filtered", true, "severity", "medium"))
	return r
}

// completionResults is content_filter_results of a choice that carries
// text: the four categories and the protected material detections.
func completionResults() oai.Obj {
	return oai.O(
		"hate", severity(), "protected_material_code", detection(), "protected_material_text", detection(),
		"self_harm", severity(), "sexual", severity(), "violence", severity(),
	)
}

// promptFilterResults is prompt_filter_results: one entry per prompt.
func promptFilterResults() []any {
	return []any{oai.O("prompt_index", 0, "content_filter_results", promptResults())}
}

// shape adds Azure's filter fields to the OpenAI objects: the prompt's
// results before choices, and each choice's results after its own fields.
// A choice or chunk without text (tool calls, the role chunk, the closing
// chunk) carries an empty object, as the real annotates only text.
func shape(part oai.Part, o oai.Obj) oai.Obj {
	switch part {
	case oai.PartChat:
		out := make(oai.Obj, 0, len(o)+1)
		for _, f := range o {
			if f.Key == "choices" {
				out = append(out, oai.Field{Key: "prompt_filter_results", Val: promptFilterResults()})
			}
			out = append(out, f)
		}
		return out
	case oai.PartChoice:
		return append(o, oai.Field{Key: "content_filter_results", Val: choiceResults(o)})
	case oai.PartChunkChoice:
		return append(o, oai.Field{Key: "content_filter_results", Val: chunkResults(o)})
	case oai.PartResponse:
		return append(o, oai.Field{Key: "content_filters", Val: responseFilters(o)})
	}
	return o
}

// responseFilters is content_filters of a Responses API object (the
// "Handle guardrails and content filtering" section of the Responses
// how-to): instead of prompt_filter_results and content_filter_results,
// one entry per filtered source, {source_type, blocked,
// content_filter_results}. The prompt always has one; the completion has
// one when the output carries text, as on Chat Completions. Placed last,
// since the documented sample shows only id and content_filters; offsets
// are not documented and stay out. Not conferred (ADR 0043).
func responseFilters(o oai.Obj) []any {
	out := []any{oai.O("source_type", "prompt", "blocked", false, "content_filter_results", promptResults())}
	if outputHasText(o) {
		out = append(out, oai.O("source_type", "completion", "blocked", false, "content_filter_results", completionResults()))
	}
	return out
}

// outputHasText reports whether a response object has a message whose
// content carries non-empty text (output_text or refusal).
func outputHasText(o oai.Obj) bool {
	items, _ := o.Get("output")
	list, _ := items.([]any)
	for _, it := range list {
		item, _ := it.(oai.Obj)
		if typ, _ := item.Get("type"); typ != "message" {
			continue
		}
		content, _ := item.Get("content")
		parts, _ := content.([]any)
		for _, p := range parts {
			part, _ := p.(oai.Obj)
			for _, k := range []string{"text", "refusal"} {
				if t, _ := part.Get(k); t != nil && t != "" {
					return true
				}
			}
		}
	}
	return false
}

// choiceResults are the results of a non-streaming choice: full when it
// carries text (message content or the legacy text), empty otherwise.
func choiceResults(o oai.Obj) oai.Obj {
	if t, ok := o.Get("text"); ok {
		if s, _ := t.(string); s != "" {
			return completionResults()
		}
		return oai.Obj{}
	}
	msg, _ := o.Get("message")
	m, _ := msg.(oai.Obj)
	if c, _ := m.Get("content"); c != nil {
		if s, _ := c.(string); s != "" {
			return completionResults()
		}
	}
	return oai.Obj{}
}

// chunkResults are the results of a chunk's choice: full on a text delta,
// empty on the role, tool-call and closing chunks.
func chunkResults(o oai.Obj) oai.Obj {
	if t, ok := o.Get("text"); ok {
		if s, _ := t.(string); s != "" {
			return completionResults()
		}
		return oai.Obj{}
	}
	d, _ := o.Get("delta")
	delta, _ := d.(oai.Obj)
	if c, _ := delta.Get("content"); c != nil {
		if s, _ := c.(string); s != "" {
			return completionResults()
		}
	}
	return oai.Obj{}
}

// promptFilterFrame is the first event of every chat stream: empty id,
// object and model, created 0, the prompt's results, no choices and usage
// null (spec 006 16.6).
var promptFilterFrame = func() []byte {
	o := oai.O("id", "", "object", "", "created", 0, "model", "", "prompt_filter_results", promptFilterResults(), "choices", []any{}, "usage", nil)
	b, _ := chunkFormat.Marshal(o)
	return append(append([]byte("data: "), b...), '\n', '\n')
}()

// preWriter writes a frame before the first byte of the body.
type preWriter struct {
	core.ResponseWriter
	pre  []byte
	done bool
}

func (p *preWriter) Write(b []byte) (int, error) {
	if !p.done {
		p.done = true
		if _, err := p.ResponseWriter.Write(p.pre); err != nil {
			return 0, err
		}
	}
	return p.ResponseWriter.Write(b)
}

// send writes the frame now, when nothing was written yet.
func (p *preWriter) send() error {
	if p.done {
		return nil
	}
	p.done = true
	if _, err := p.ResponseWriter.Write(p.pre); err != nil {
		return err
	}
	return p.Flush()
}

// stream is the OpenAI chunk writer behind the prompt filter frame. The
// frame goes out at Start, before the first chunk and before the silence
// of hidden reasoning, as the real annotates the prompt before it answers.
type stream struct {
	*oai.Stream
	pw  *preWriter
	req *core.Request
}

func (s *stream) Start(resp *core.Response) error {
	fixID(s.req, resp)
	if err := s.Stream.Start(resp); err != nil {
		return err
	}
	return s.pw.send()
}

// fixID gives the legacy completions their cmpl- id: the deployment route
// serves chat and completions under one route kind, so the engine mints a
// chat id for both.
func fixID(req *core.Request, resp *core.Response) {
	if req != nil && resp != nil && req.Endpoint == core.EndpointCompletions {
		if rest, ok := strings.CutPrefix(resp.ID, "chatcmpl-"); ok {
			resp.ID = "cmpl-" + rest
		}
	}
}
