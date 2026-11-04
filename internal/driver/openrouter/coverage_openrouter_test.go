package openrouter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

func covDriver(t *testing.T) *Driver {
	t.Helper()
	drv, err := driver.New("openrouter", nil)
	if err != nil {
		t.Fatal(err)
	}
	return drv.(*Driver)
}

func TestRegisterRejectsUnknownReasoningField(t *testing.T) {
	if _, err := driver.New("openrouter", map[string]string{"reasoning_field": "thoughts"}); err == nil {
		t.Error("accepted reasoning_field=thoughts")
	}
}

func TestParseRequestUnknownKind(t *testing.T) {
	d := covDriver(t)
	_, e := d.ParseRequest(driver.KindModels, nil, driver.MapHeaders{})
	if e == nil || e.Kind != core.ErrNotFound {
		t.Errorf("err = %v", e)
	}
}

type fakeHeaders struct{ m map[string]string }

func (f fakeHeaders) Get(k string) string { return f.m[k] }

func TestKeyInfoLimitsAndLabel(t *testing.T) {
	credits := 5.0
	spent := 2.0
	cases := []struct {
		name      string
		key       *config.KeyConfig
		kb        *driver.Balance
		limit     any
		reset     any
		remaining any
		expires   any
	}{
		{"no key", nil, nil, nil, nil, nil, nil},
		{"key without limits", &config.KeyConfig{Key: "k"}, nil, nil, nil, nil, nil},
		{"expiry", &config.KeyConfig{Key: "k", ExpiresAt: "2030-01-01"}, nil, nil, nil, nil, "2030-01-01"},
		{"credits", &config.KeyConfig{Limits: &config.Limits{CreditsUSD: &credits}}, nil, 5.0, nil, 5.0, nil},
		{"credits spent", &config.KeyConfig{Limits: &config.Limits{CreditsUSD: &credits}}, &driver.Balance{CreditsUSD: &spent}, 5.0, nil, 2.0, nil},
		{"monthly", &config.KeyConfig{Limits: &config.Limits{SpendMonthlyUSD: 10}}, &driver.Balance{SpendUSD: 4}, 10.0, "monthly", 6.0, nil},
		{"limits without caps", &config.KeyConfig{Limits: &config.Limits{}}, nil, nil, nil, nil, nil},
	}
	for _, c := range cases {
		o := keyInfo(driver.MapHeaders{}, driver.ExtraContext{Key: c.key, KeyBalance: c.kb})
		get := func(k string) any { v, _ := o.Get(k); return v }
		if get("limit") != c.limit || get("limit_reset") != c.reset || get("limit_remaining") != c.remaining || get("expires_at") != c.expires {
			t.Errorf("%s: limit=%v reset=%v remaining=%v expires=%v", c.name, get("limit"), get("limit_reset"), get("limit_remaining"), get("expires_at"))
		}
	}
}

func TestPresentedKeyOfAndMask(t *testing.T) {
	// A Headers that is not a MapHeaders has no account slots: the configured key wins.
	h := fakeHeaders{m: map[string]string{}}
	if got := presentedKeyOf(h, driver.ExtraContext{Key: &config.KeyConfig{Key: "configured"}}); got != "configured" {
		t.Errorf("presentedKeyOf = %q", got)
	}
	if got := presentedKeyOf(h, driver.ExtraContext{}); got != "" {
		t.Errorf("presentedKeyOf = %q", got)
	}
	if headerMap(h) != nil {
		t.Error("headerMap of a foreign Headers")
	}
	mh := driver.MapHeaders{}
	driver.SetAccount(mh, driver.Account{Key: "engine-key"})
	if got := presentedKeyOf(mh, driver.ExtraContext{Key: &config.KeyConfig{Key: "configured"}}); got != "engine-key" {
		t.Errorf("presentedKeyOf = %q", got)
	}
	for in, want := range map[string]string{
		"sk-or-v1-0123456789abcdef": "sk-or-v1-012...def",
		"sk-or":                     "sk-...",
		"abc":                       "...",
		"":                          "...",
	} {
		if got := maskKey(in); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPresentedKeyFromHeaders(t *testing.T) {
	if presentedKey(nil) != "" {
		t.Error("nil request")
	}
	if got := presentedKey(&core.Request{Headers: map[string]string{"authorization": "Bearer abc "}}); got != "abc" {
		t.Errorf("bearer = %q", got)
	}
	if got := presentedKey(&core.Request{Headers: map[string]string{"x-api-key": " k2 "}}); got != "k2" {
		t.Errorf("x-api-key = %q", got)
	}
}

func TestGatewayErrorIsAnUnmetadatedRaw400(t *testing.T) {
	d := covDriver(t)
	e := GatewayError(`bad "thing"`)
	status, body, ct, hdr := d.errorBody(&core.Request{RequestID: "gen-1"}, e)
	if status != 400 || ct != "application/json" || hdr["x-generation-id"] == "" {
		t.Errorf("%d %s %v", status, ct, hdr)
	}
	var m map[string]any
	if err := json.Unmarshal(body.([]byte), &m); err != nil {
		t.Fatal(err)
	}
	if m["error"].(map[string]any)["message"] != `bad "thing"` || m["user_id"] != "user_wwz" {
		t.Errorf("body = %v", m)
	}
}

func TestErrorBodyKindsOutsideTheFastPaths(t *testing.T) {
	d := covDriver(t)
	// An auth error with an explicit status goes through the generic switch.
	status, body, _, _ := d.errorBody(&core.Request{}, &core.Error{Kind: core.ErrInvalidAPIKey, Status: 401})
	if status != 401 || !strings.Contains(string(mustJSON(t, body)), "No cookie auth credentials found") {
		t.Errorf("%d %v", status, body)
	}
	// A kind without a mapped error_type is "unmapped"; no model names "this model".
	_, body, _, _ = d.errorBody(&core.Request{}, &core.Error{Kind: core.ErrorKind("zzz")})
	_, body2, _, _ := d.errorBody(&core.Request{}, &core.Error{Kind: core.ErrServerError, Code: "custom_code"})
	if !strings.Contains(string(mustJSON(t, body2)), "custom_code") {
		t.Errorf("code not used: %v", body2)
	}
	if !strings.Contains(string(mustJSON(t, body)), "unmapped") {
		t.Errorf("body = %v", body)
	}
	if got := openRouterDefaultMessage(&core.Request{}, &core.Error{Kind: core.ErrContentFilter}, ""); !strings.Contains(got, "this model requires moderation") {
		t.Errorf("message = %q", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	switch b := v.(type) {
	case []byte:
		return b
	case json.RawMessage:
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStreamErrorChunkForEveryBodyShape(t *testing.T) {
	d := covDriver(t)
	for _, e := range []*core.Error{
		{Kind: core.ErrServerError, Message: "boom"}, // oai.Obj body
		{Kind: core.ErrInvalidAPIKey},                // raw JSON body
		GatewayError("raw"),                          // []byte body
		{Kind: core.ErrBadGateway, Status: 502},      // another Obj
	} {
		chunk, ok := d.streamError(e).(oai.Obj)
		if !ok {
			t.Fatalf("chunk type %T", d.streamError(e))
		}
		if _, has := chunk.Get("error"); !has {
			t.Errorf("no error member: %v", chunk)
		}
	}
}

func TestAnthErrorBodyNonMapPassesThrough(t *testing.T) {
	d := covDriver(t)
	status, body, ct, _ := d.anthErrorBody(&core.Request{RequestID: "r"}, &core.Error{Kind: core.ErrBadGateway})
	if status != 502 || ct != "text/html" {
		t.Errorf("%d %s", status, ct)
	}
	if _, ok := body.([]byte); !ok {
		t.Errorf("body %T", body)
	}
}

func TestShapeHelpers(t *testing.T) {
	o := oai.O("a", 1)
	if got := shapeChat(oai.Part("unknown"), o); len(got) != 1 {
		t.Errorf("unknown part changed: %v", got)
	}
	got := insertAfter(oai.O("a", 1, "b", 2), "missing", oai.Field{Key: "c", Val: 3})
	if len(got) != 3 || got[2].Key != "c" {
		t.Errorf("insertAfter = %v", got)
	}
	got = insertAfter(oai.O("a", 1, "b", 2), "a", oai.Field{Key: "c", Val: 3})
	if got[1].Key != "c" {
		t.Errorf("insertAfter = %v", got)
	}
}

func TestStreamKeepAliveHaltsAndFailures(t *testing.T) {
	h := oai.Hooks{}
	// A halted stream does not tick again.
	s := newStream(&h, core.NewMemWriter(), &core.Request{}, nil)
	s.halt()
	s.tick()
	if strings.Contains(string(s.rw.(*core.MemWriter).Body()), "PROCESSING") {
		t.Error("halted stream sent a keep-alive")
	}
	// A failing client halts the stream; the open step also reports the error.
	fw := &failAfter{MemWriter: core.NewMemWriter(), n: 0}
	s = newStream(&h, fw, &core.Request{}, nil)
	s.halt()
	s.mu.Lock()
	err := s.comment()
	s.mu.Unlock()
	if err == nil {
		t.Error("comment swallowed the write error")
	}
	s.halted = false
	s.tick()
	if !s.halted {
		t.Error("a failing keep-alive did not halt the stream")
	}
	// A stream the engine never started stops after keepAliveFor.
	s = newStream(&h, core.NewMemWriter(), &core.Request{}, nil)
	s.halt()
	s.halted = false
	s.deadline = time.Now().Add(-time.Second)
	s.timer = time.NewTimer(time.Hour)
	s.tick()
	if !s.halted {
		t.Error("an abandoned stream kept ticking")
	}
}
