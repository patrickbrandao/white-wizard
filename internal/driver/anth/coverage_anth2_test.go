package anth

import (
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func TestWriteErrorKeepsRetryAfter(t *testing.T) {
	h := DefaultHooks()
	w := core.NewMemWriter()
	WriteError(&h, w, &core.Request{}, &core.Error{Kind: core.ErrOverloaded, RetryAfter: 3 * time.Second}, nil)
	if w.Headers()["retry-after"] != "3" {
		t.Errorf("headers = %v", w.Headers())
	}
}

func TestMarshalFailuresSurface(t *testing.T) {
	h := DefaultHooks()
	h.Shape = func(p Part, o oai.Obj) oai.Obj {
		if p == PartMessage {
			return append(o, oai.Field{Key: "bad", Val: make(chan int)})
		}
		return o
	}
	if err := WriteMessage(&h, core.NewMemWriter(), &core.Request{}, &core.Response{}, nil); err == nil {
		t.Error("WriteMessage swallowed the marshal error")
	}

	h = DefaultHooks()
	h.ErrorBody = func(*core.Request, *core.Error) (int, any, string, map[string]string) {
		return 500, map[string]any{"type": "error", "error": map[string]any{"type": "x", "message": make(chan int)}}, "application/json", nil
	}
	s := NewStream(&h, core.NewMemWriter(), &core.Request{}, nil)
	if err := s.Error(&core.Error{}); err == nil {
		t.Error("Error swallowed the marshal error")
	}

	// A delta whose event cannot be marshalled: no template, same error.
	h = DefaultHooks()
	h.Shape = func(p Part, o oai.Obj) oai.Obj {
		if p == PartEvent {
			if typ, _ := o.Get("type"); typ == "content_block_delta" {
				return append(o, oai.Field{Key: "bad", Val: make(chan int)})
			}
		}
		return o
	}
	s = NewStream(&h, core.NewMemWriter(), &core.Request{}, nil)
	if err := s.Start(&core.Response{ID: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TextDelta("x"); err == nil {
		t.Error("delta swallowed the marshal error")
	}
}
