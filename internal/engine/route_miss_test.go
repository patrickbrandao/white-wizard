package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/gemini"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

func handle(t *testing.T, e *engine.Engine, drvName string, in engine.Input) *core.MemWriter {
	t.Helper()
	drv, err := driver.New(drvName, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.Driver = drv
	w := core.NewMemWriter()
	e.Handle(context.Background(), in, w)
	return w
}

// TestRouteMiss: KindRouteMiss goes to the driver's RouteMisser with the
// key's refusal; a driver without one answers its own 404 (task 235).
func TestRouteMiss(t *testing.T) {
	e := newEngine(t, fastCfg+"keys: [{key: k1}]\n")
	miss := driver.RouteMiss{Method: "DELETE", Path: "/v1/models", Allow: []string{"GET"}, Kind: driver.KindModels}
	anth := driver.MapHeaders{"x-api-key": "k1", "anthropic-version": "2023-06-01"}
	w := handle(t, e, "anthropic", engine.Input{Kind: driver.KindRouteMiss, Method: "DELETE", Path: "/v1/models", Headers: anth, Miss: miss})
	if w.StatusCode() != 405 || !strings.Contains(string(w.Body()), "Method Not Allowed") {
		t.Fatalf("anthropic: %d %s", w.StatusCode(), w.Body())
	}
	bad := driver.MapHeaders{"x-api-key": "nope", "anthropic-version": "2023-06-01"}
	w = handle(t, e, "anthropic", engine.Input{Kind: driver.KindRouteMiss, Method: "DELETE", Path: "/v1/models", Headers: bad, Miss: miss})
	if w.StatusCode() != 401 {
		t.Fatalf("anthropic bad key: %d %s", w.StatusCode(), w.Body())
	}
	w = handle(t, e, "openai", engine.Input{Kind: driver.KindRouteMiss, Method: "GET", Path: "/v1/nope", Miss: driver.RouteMiss{Method: "GET", Path: "/v1/nope"}})
	if w.StatusCode() != 404 {
		t.Fatalf("openai: %d %s", w.StatusCode(), w.Body())
	}
}

// TestAfterModel: Gemini's backend checks come after the model, so an
// unknown or retired model is the 404 and a served one the backend's 400
// (fixture candidates_model_unknown, task 235).
func TestAfterModel(t *testing.T) {
	e := newEngine(t, fastCfg+`keys: [{key: k1}]
models:
  - {id: gemini-ok, latency: fast}
  - {id: gemini-old, latency: fast, retires_at: "2020-01-01"}
`)
	body := `{"contents":[{"parts":[{"text":"a"}]}],"generationConfig":{"candidateCount":2}}`
	for model, want := range map[string]int{"gemini-nope": 404, "gemini-old": 404, "gemini-ok": 400} {
		path := "/v1beta/models/" + model + ":generateContent"
		h := driver.MapHeaders{"x-goog-api-key": "k1", "x-wwz-path": path}
		w := handle(t, e, "gemini", engine.Input{Kind: driver.KindGenerate, Method: "POST", Path: path, Headers: h, Body: []byte(body)})
		if w.StatusCode() != want {
			t.Errorf("%s: %d %s", model, w.StatusCode(), w.Body())
		}
	}
}
