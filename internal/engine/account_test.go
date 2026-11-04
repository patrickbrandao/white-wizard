package engine_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/openai"
	"github.com/patrickbrandao/white-wizard/internal/engine"
)

// Task 199 (ADR 0049): the driver sees the key as presented and the account
// and project of its keys[] entry, also on errors raised before the body is
// parsed.

const accountCfg = `
server: {seed: 42}
profiles:
  latency:
    fast: {ttft: 0ms, itl: 0ms}
models:
  - {id: gpt-4o-mini, latency: fast}
keys:
  - {key: k-acme-1, label: a1, account: Acme Corp, project: Web}
  - {key: k-acme-2, label: a2, account: Acme Corp, project: Web}
  - {key: k-acme-3, label: a3, account: Acme Corp, project: Batch}
`

const chatBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

func TestAccountHeadersFollowKeysAccount(t *testing.T) {
	e := newEngine(t, accountCfg)
	hdr := func(key string, extra ...string) map[string]string {
		h := map[string]string{"Authorization": "Bearer " + key}
		for i := 0; i+1 < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
		w := call(t, e, "openai", "/v1/chat/completions", h, chatBody)
		if w.StatusCode() != 200 {
			t.Fatalf("%s: status %d: %s", key, w.StatusCode(), w.Body())
		}
		return w.Headers()
	}
	a1, a2, a3 := hdr("k-acme-1"), hdr("k-acme-2"), hdr("k-acme-3")
	if a1["openai-organization"] != "acme-corp" {
		t.Errorf("openai-organization = %q, want the account slug acme-corp", a1["openai-organization"])
	}
	if a1["openai-organization"] != a3["openai-organization"] {
		t.Errorf("same account, different organization: %q %q", a1["openai-organization"], a3["openai-organization"])
	}
	if a1["openai-project"] != a2["openai-project"] {
		t.Errorf("same account and project, different openai-project: %q %q", a1["openai-project"], a2["openai-project"])
	}
	if a1["openai-project"] == a3["openai-project"] {
		t.Errorf("different projects share openai-project %q", a1["openai-project"])
	}
	if p := a1["openai-project"]; !strings.HasPrefix(p, "proj_") || len(p) != 29 {
		t.Errorf("openai-project = %q, want proj_ + 24", p)
	}
	if got := hdr("k-acme-1", "openai-project", "proj_client")["openai-project"]; got != "proj_client" {
		t.Errorf("client openai-project not echoed: %q", got)
	}
}

// TestAccountWithoutKeyEntry: with auth none and a key outside keys[] the
// values still derive from the presented key (no account to use).
func TestAccountWithoutKeyEntry(t *testing.T) {
	e := newEngine(t, "server: {seed: 42, auth: none}\nprofiles: {latency: {fast: {ttft: 0ms, itl: 0ms}}}\nmodels: [{id: gpt-4o-mini, latency: fast}]\n")
	w := call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer sk-loose"}, chatBody)
	if w.StatusCode() != 200 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	if org := w.Headers()["openai-organization"]; !strings.HasPrefix(org, "user-") {
		t.Errorf("openai-organization = %q, want the key-derived user-…", org)
	}
}

const maskedKey = "sk-inval********rift"

func check401Masked(t *testing.T, w *core.MemWriter) {
	t.Helper()
	if w.StatusCode() != 401 {
		t.Fatalf("status %d: %s", w.StatusCode(), w.Body())
	}
	if !strings.Contains(string(w.Body()), maskedKey) {
		t.Errorf("401 body without the masked key: %s", w.Body())
	}
	raw, err := base64.StdEncoding.DecodeString(w.Headers()["x-error-json"])
	if err != nil || !strings.Contains(string(raw), maskedKey) {
		t.Errorf("x-error-json without the masked key: %q (%v)", raw, err)
	}
}

// TestBadKey401MaskedBeforeParse: /v1/models and an unparsable body refuse
// a bad key with the key masked, like the chat route.
func TestBadKey401MaskedBeforeParse(t *testing.T) {
	e := newEngine(t, accountCfg)
	bad := map[string]string{"Authorization": "Bearer sk-invalid-wwz-drift"}
	t.Run("chat", func(t *testing.T) {
		check401Masked(t, call(t, e, "openai", "/v1/chat/completions", bad, chatBody))
	})
	t.Run("parse", func(t *testing.T) {
		check401Masked(t, call(t, e, "openai", "/v1/chat/completions", bad, `{not json`))
	})
	for _, c := range []struct {
		kind driver.Kind
		path string
	}{{driver.KindModels, "/v1/models"}, {driver.KindModel, "/v1/models/gpt-4o-mini"}} {
		t.Run(string(c.kind), func(t *testing.T) {
			drv, err := driver.New("openai", nil)
			if err != nil {
				t.Fatal(err)
			}
			w := core.NewMemWriter()
			in := engine.Input{Driver: drv, Kind: c.kind, Method: "GET", Path: c.path, Headers: driver.MapHeaders{"authorization": bad["Authorization"]}, PathParam: "gpt-4o-mini"}
			e.Handle(context.Background(), in, w)
			check401Masked(t, w)
		})
	}
	t.Run("missing key", func(t *testing.T) {
		w := call(t, e, "openai", "/v1/chat/completions", nil, `{not json`)
		if w.StatusCode() != 401 || strings.Contains(string(w.Body()), "********") {
			t.Errorf("status %d body %s", w.StatusCode(), w.Body())
		}
	})
}
