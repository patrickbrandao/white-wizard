package engine

import (
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// silentDriver writes an error without setting a status.
type silentDriver struct{ driver.Driver }

func (silentDriver) WriteError(w core.ResponseWriter, _ *core.Request, _ *core.Error, _ *core.RateLimitState) {
	_, _ = w.Write([]byte("x"))
}

// Task 281: writeError counts the status the driver sent (OpenCode's
// unknown model is a 400, not the kind's 404) and statusOf's when the
// driver sets none.
func TestWriteErrorStatus(t *testing.T) {
	e := &Engine{}
	oc, err := driver.New("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	er := &core.Error{Kind: core.ErrModelNotFound}
	w := core.NewMemWriter()
	if got := e.writeError(w, oc, &core.Request{Model: "nope"}, er, nil); got != 400 || w.StatusCode() != 400 {
		t.Errorf("opencode: counted %d, sent %d", got, w.StatusCode())
	}
	if got := e.writeError(core.NewMemWriter(), silentDriver{oc}, nil, er, nil); got != 404 {
		t.Errorf("silent driver: %d, want statusOf's 404", got)
	}
}
