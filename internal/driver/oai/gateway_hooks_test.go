package oai_test

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// TestGatewayHooks: NativeFinish names the native reason (null on every
// chunk before the finish, the final usage choice through Shape),
// NoRoleChunk drops the leading role chunk and Format.Prefix precedes the
// body (OpenRouter, task 174).
func TestGatewayHooks(t *testing.T) {
	native := func(_ *core.Request, fr core.FinishReason) any { return "native-" + string(fr) }
	var shaped []oai.Part
	h := oai.Hooks{
		NativeFinishReason: true, NativeFinish: native, NoRoleChunk: true, FinalChunkWithChoice: true,
		BodyFormat: oai.Format{Prefix: "\n  \n"},
		Shape: func(p oai.Part, o oai.Obj) oai.Obj {
			shaped = append(shaped, p)
			return o
		},
	}
	r := newRec()
	if err := oai.WriteChat(&h, r, chatReq(), resp(), nil); err != nil {
		t.Fatal(err)
	}
	if b := r.body.String(); !strings.HasPrefix(b, "\n  \n{") || !strings.Contains(b, `"native_finish_reason":"native-stop"`) {
		t.Fatalf("body %q", b)
	}

	w := newRec()
	s := newStream(h, w, chatReq())
	must(t, s.Start(resp()))
	must(t, s.TextDelta("hi"))
	must(t, s.Finish(core.FinishLength, core.Usage{}, true))
	d := w.data(t)
	if len(d) != 3 {
		t.Fatalf("%d chunks (no role chunk): %v", len(d), d)
	}
	if v, ok := dig(d[0], "choices", 0).(map[string]any)["native_finish_reason"]; !ok || v != nil {
		t.Fatalf("first chunk: native_finish_reason must be null: %v", d[0])
	}
	if dig(d[1], "choices", 0, "native_finish_reason") != "native-length" || dig(d[2], "choices", 0, "native_finish_reason") != "native-length" {
		t.Fatalf("finish and usage chunks: %v %v", d[1], d[2])
	}
	if dig(d[0], "choices", 0, "delta", "content") != "hi" {
		t.Fatalf("first delta: %v", d[0])
	}
	deltas := 0
	for _, p := range shaped {
		if p == oai.PartDelta {
			deltas++
		}
	}
	// The text delta twice (its template renders two sentinel chunks), the
	// finish and the usage chunk's empty delta.
	if deltas != 4 {
		t.Fatalf("PartDelta shaped %d times: %v", deltas, shaped)
	}
}

// TestFinishUsage: the closing chunk carries usage itself, with or without
// include_usage, and nothing follows but [DONE] (OpenCode, task 175).
func TestFinishUsage(t *testing.T) {
	for _, include := range []bool{false, true} {
		h := oai.Hooks{FinishUsage: true}
		w := newRec()
		s := newStream(h, w, chatReq())
		must(t, s.Start(resp()))
		must(t, s.TextDelta("hi"))
		must(t, s.Finish(core.FinishStop, core.Usage{PromptTokens: 3, CompletionTokens: 1}, include))
		d := w.data(t)
		if len(d) != 3 {
			t.Fatalf("include %v: %d chunks (role, text, finish): %v", include, len(d), d)
		}
		if dig(d[2], "choices", 0, "finish_reason") != "stop" || dig(d[2], "usage", "total_tokens") != 4.0 {
			t.Fatalf("include %v: closing chunk %v", include, d[2])
		}
	}
}
