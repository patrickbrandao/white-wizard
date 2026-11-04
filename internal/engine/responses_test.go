package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

const responsesEngineCfg = `
server: {seed: 7}
keys:
  - {key: k-a, label: a, account: Acme}
  - {key: k-b, label: b, account: Other}
models:
  - id: gpt-4o-mini
    protocols: [openai]
    errors: none
    latency: {ttft: 0ms, itl: 0ms, clock: logical}
  - id: no-cache
    protocols: [openai]
    errors: none
    latency: {ttft: 0ms, itl: 0ms, clock: logical}
    prompt_cache: {kind: none}
`

// respCall sends one /v1/responses body with key and returns status and body.
func respCall(t *testing.T, e *engine.Engine, key, body string) (int, string) {
	t.Helper()
	d, err := driver.New("openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := core.NewMemWriter()
	h := driver.MapHeaders{"authorization": "Bearer " + key, "x-wwz-response": "Hello there!"}
	e.Handle(context.Background(), engine.Input{Driver: d, Kind: driver.KindResponses, Method: "POST", Path: "/v1/responses", Headers: h, Body: []byte(body), ClientIP: "10.0.0.1"}, w)
	return w.StatusCode(), string(w.Body())
}

func respID(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.ID == "" {
		t.Fatalf("no id in %s", body)
	}
	return r.ID
}

func chainBody(model, prev string) string {
	return `{"model":"` + model + `","input":"Repeat it.","previous_response_id":"` + prev + `"}`
}

// TestResponsesPreviousAccount: a record is found only by its own account
// (README 2.5); the chained call adds the recorded conversation.
func TestResponsesPreviousAccount(t *testing.T) {
	e := newEngine(t, responsesEngineCfg)
	st, body := respCall(t, e, "k-a", `{"model":"gpt-4o-mini","input":"Say hi in two words.","instructions":"Be brief."}`)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	id := respID(t, body)
	if st, body = respCall(t, e, "k-b", chainBody("gpt-4o-mini", id)); st != 400 || !strings.Contains(body, `"code": "previous_response_not_found"`) {
		t.Errorf("other account: %d %s", st, body)
	}
	st, body = respCall(t, e, "k-a", chainBody("gpt-4o-mini", id))
	// 10 (Say hi...) + 7 (Hello there!) + 7 (Repeat it.) + 3; the
	// instructions are not inherited (previous_response_instructions).
	if st != 200 || !strings.Contains(body, `"input_tokens": 27,`) {
		t.Errorf("same account: %d %s", st, body)
	}
}

// plainStore hides the optional interfaces of the store it wraps.
type plainStore struct{ limits.Store }

// failingRegistry fails every registry call.
type failingRegistry struct{ *limits.Memory }

var errRegistry = errors.New("registry down")

func (failingRegistry) ResponsePut(context.Context, string, limits.ResponseRecord) error {
	return errRegistry
}

func (failingRegistry) ResponseGet(context.Context, string, string, time.Time) (*limits.ResponseRecord, error) {
	return nil, errRegistry
}

// TestResponsesWithoutRegistry: a store without the registry, or one that
// fails, finds nothing (previous_response_not_found) and records nothing,
// and the call itself is served.
func TestResponsesWithoutRegistry(t *testing.T) {
	for name, store := range map[string]limits.Store{"plain": plainStore{limits.NewMemory()}, "failing": failingRegistry{limits.NewMemory()}} {
		e := newEngine(t, responsesEngineCfg)
		e.Store = store
		st, body := respCall(t, e, "k-a", `{"model":"gpt-4o-mini","input":"x"}`)
		if st != 200 {
			t.Fatalf("%s: %d %s", name, st, body)
		}
		if st, body = respCall(t, e, "k-a", chainBody("gpt-4o-mini", respID(t, body))); st != 400 {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
}

// TestResponsesStoreFalseAndStream: store: false records nothing
// (previous_response_unstored); a streamed response is recorded when it
// ends and can be chained.
func TestResponsesStoreFalseAndStream(t *testing.T) {
	e := newEngine(t, responsesEngineCfg)
	_, body := respCall(t, e, "k-a", `{"model":"gpt-4o-mini","input":"x","store":false}`)
	if st, b := respCall(t, e, "k-a", chainBody("gpt-4o-mini", respID(t, body))); st != 400 {
		t.Errorf("store false: %d %s", st, b)
	}
	_, body = respCall(t, e, "k-a", `{"model":"gpt-4o-mini","input":"x","stream":true}`)
	i := strings.Index(body, `"id":"resp_`)
	if i < 0 {
		t.Fatalf("stream: %s", body)
	}
	id := body[i+6 : i+6+55]
	if st, b := respCall(t, e, "k-a", chainBody("no-cache", id)); st != 200 {
		t.Errorf("chained after stream: %d %s", st, b)
	}
}

// TestResponsesChainedCacheLong: a conversation over the cache minimum is
// read back by the chained call up to the largest 128-step cut at least one
// step below it; a model without cache reads nothing.
func TestResponsesChainedCacheLong(t *testing.T) {
	e := newEngine(t, responsesEngineCfg)
	long := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel. ", 150)
	for model, cached := range map[string]bool{"gpt-4o-mini": true, "no-cache": false} {
		_, body := respCall(t, e, "k-a", `{"model":"`+model+`","input":"`+long+`"}`)
		var r struct {
			Usage struct {
				Input   int `json:"input_tokens"`
				Details struct {
					Cached int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		}
		_, body = respCall(t, e, "k-a", chainBody(model, respID(t, body)))
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		if got := r.Usage.Details.Cached; (got > 0) != cached || got%128 != 0 || (cached && got > r.Usage.Input-128) {
			t.Errorf("%s: cached %d of %d", model, got, r.Usage.Input)
		}
	}
}
