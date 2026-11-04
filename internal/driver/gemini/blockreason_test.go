package gemini

import (
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// TestPromptBlocked: a content_filter error is a 200 with promptFeedback
// and no candidates, whole or streamed (task 382).
func TestPromptBlocked(t *testing.T) {
	d := &Driver{}
	mk := func(method, query string) *core.Request {
		return &core.Request{
			Model: "m", RequestID: "r1",
			Messages: []core.Message{{Role: "user", Content: "hello"}},
			Headers:  driver.MapHeaders{"x-wwz-path": "/v1beta/models/m:" + method, driver.QueryHeader: query},
		}
	}
	for _, c := range []struct{ method, query, prefix string }{
		{"generateContent", "", "{"},
		{"streamGenerateContent", "alt=sse", "data: "},
		{"streamGenerateContent", "", "[{"},
	} {
		w := core.NewMemWriter()
		d.WriteError(w, mk(c.method, c.query), &core.Error{Kind: core.ErrContentFilter}, nil)
		b := string(w.Body())
		if w.StatusCode() != 200 || !strings.HasPrefix(b, c.prefix) || strings.Contains(b, `"candidates"`) ||
			!strings.Contains(b, `"blockReason": "SAFETY"`) || !strings.Contains(b, `"promptTokenCount"`) {
			t.Errorf("%s %q: %d %s", c.method, c.query, w.StatusCode(), b)
		}
	}
	w := core.NewMemWriter()
	d.WriteError(w, nil, &core.Error{Kind: core.ErrContentFilter}, nil)
	if w.StatusCode() != 200 || !strings.Contains(string(w.Body()), `"promptTokenCount": 0`) {
		t.Errorf("nil request: %d %s", w.StatusCode(), w.Body())
	}
}
