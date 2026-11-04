package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestParseEvent(t *testing.T) {
	data := []byte(`{"t":"2026-10-30T12:00:00.123456789Z","type":"request","instance":"wwz-1","request_id":"req_1",
		"test_id":"carga","driver":"openai","model":"gpt-5-sim","entity":{"kind":"key","id":"k1"},"key":"k1",
		"ip":"203.0.113.7","status":200,"sample":0.5,"fields":{"prompt_tokens":120}}`)
	e, err := ParseEvent("1730289600123-0", data)
	if err != nil {
		t.Fatal(err)
	}
	want := Event{
		StreamID: "1730289600123-0", At: time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC), Type: EventRequest,
		Instance: "wwz-1", RequestID: "req_1", TestID: "carga", Driver: "openai", Model: "gpt-5-sim",
		EntityKind: "key", EntityID: "k1", Status: 200, Sample: 0.5, Data: data,
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("got %+v\nwant %+v", e, want)
	}
	b, err := ParseEvent("2-0", []byte(`{"t":"2026-10-30T12:00:01Z","type":"block","instance":"i","entity":{"kind":"ip","id":"1.2.3.4"},"error":"x"}`))
	if err != nil || b.EntityKind != "ip" || b.Error != "x" {
		t.Fatalf("block %+v %v", b, err)
	}
	n, err := ParseEvent("3-0", []byte(`{"t":"2026-10-30T12:00:01Z","type":"unban","instance":"i"}`))
	if err != nil || n.EntityKind != "" {
		t.Fatalf("no entity %+v %v", n, err)
	}
	for _, bad := range []string{`{`, `{"type":"request"}`, `{"t":"2026-10-30T12:00:01Z"}`} {
		if _, err := ParseEvent("x", []byte(bad)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestEventsInsertQuery(t *testing.T) {
	s, c, _ := open(t)
	base := c.now()
	evs := []Event{
		{StreamID: "1-0", At: base.Add(-3 * time.Minute), Type: EventRequest, Driver: "openai", Model: "m1", EntityKind: "key", EntityID: "k1", Status: 200, Instance: "i1", RequestID: "r1", TestID: "t1", Data: json.RawMessage(`{"fields":{}}`)},
		{StreamID: "2-0", At: base.Add(-2 * time.Minute), Type: EventError, Driver: "anthropic", Model: "m2", Status: 429, Error: "rate_limit", Sample: 0.5},
		{StreamID: "3-0", At: base.Add(-1 * time.Minute), Type: EventBlock, EntityKind: "key", EntityID: "k1", Sample: 7},
	}
	n, err := s.Events.InsertBatch(ctx, evs)
	if err != nil || n != 3 {
		t.Fatalf("insert n=%d err=%v", n, err)
	}
	// Idempotent by stream id: replaying the batch (plus one new) adds one.
	n, err = s.Events.InsertBatch(ctx, append(evs, Event{StreamID: "4-0", At: base, Type: EventStrike}))
	if err != nil || n != 1 {
		t.Fatalf("replay n=%d err=%v", n, err)
	}
	if n, err := s.Events.InsertBatch(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty batch %d %v", n, err)
	}
	if total, _ := s.Events.Count(ctx, EventFilter{}); total != 4 {
		t.Fatalf("count %d", total)
	}

	all, err := s.Events.Query(ctx, EventFilter{})
	if err != nil || len(all) != 4 || all[0].StreamID != "4-0" || all[3].StreamID != "1-0" {
		t.Fatalf("query all %v %v", ids(all), err)
	}
	first := all[3]
	if first.Driver != "openai" || first.Status != 200 || first.Sample != 1 || first.Instance != "i1" ||
		first.RequestID != "r1" || first.TestID != "t1" || !first.At.Equal(base.Add(-3*time.Minute)) ||
		string(first.Data) != `{"fields":{}}` {
		t.Fatalf("row %+v", first)
	}
	if all[2].Sample != 0.5 || all[2].Error != "rate_limit" || all[1].Sample != 1 || string(all[0].Data) != "{}" {
		t.Fatalf("sample/data normalization %+v %+v %+v", all[2], all[1], all[0])
	}
	cases := []struct {
		f    EventFilter
		want []string
	}{
		{EventFilter{Types: []string{EventRequest, EventError}}, []string{"2-0", "1-0"}},
		{EventFilter{Types: []string{EventBlock}}, []string{"3-0"}},
		{EventFilter{Since: base.Add(-2 * time.Minute), Until: base}, []string{"3-0", "2-0"}},
		{EventFilter{Driver: "anthropic"}, []string{"2-0"}},
		{EventFilter{Model: "m1"}, []string{"1-0"}},
		{EventFilter{EntityKind: "key", EntityID: "k1"}, []string{"3-0", "1-0"}},
		{EventFilter{TestID: "t1"}, []string{"1-0"}},
		{EventFilter{Limit: 2}, []string{"4-0", "3-0"}},
	}
	for i, tc := range cases {
		got, err := s.Events.Query(ctx, tc.f)
		if err != nil || !reflect.DeepEqual(ids(got), tc.want) {
			t.Errorf("case %d: %v %v", i, ids(got), err)
		}
		if cnt, _ := s.Events.Count(ctx, EventFilter{Types: tc.f.Types, Driver: tc.f.Driver}); cnt == 0 {
			t.Errorf("case %d: count 0", i)
		}
	}

	for _, bad := range [][]Event{
		{{Type: EventRequest}},
		{{StreamID: "9-0"}},
		{{StreamID: "9-0", Type: EventRequest, Data: json.RawMessage("nope")}},
	} {
		if _, err := s.Events.InsertBatch(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("bad batch %+v: %v", bad, err)
		}
	}
	// A rejected batch is atomic.
	if _, err := s.Events.InsertBatch(ctx, []Event{{StreamID: "8-0", Type: EventRequest}, {StreamID: "9-0"}}); err == nil {
		t.Fatal("bad batch accepted")
	}
	if _, err := s.Events.Query(ctx, EventFilter{Types: []string{EventRequest}, Since: base}); err != nil {
		t.Fatal(err)
	}
	if cnt, _ := s.Events.Count(ctx, EventFilter{}); cnt != 4 {
		t.Fatalf("partial batch committed: %d", cnt)
	}

	failOn(t, s, "events", "INSERT")
	if _, err := s.Events.InsertBatch(ctx, []Event{{StreamID: "10-0", Type: EventRequest}}); err == nil {
		t.Fatal("insert failure ignored")
	}
	raw(t, s, "DROP TABLE events")
	if _, err := s.Events.InsertBatch(ctx, []Event{{StreamID: "10-0", Type: EventRequest}}); err == nil {
		t.Fatal("prepare failure ignored")
	}
}

func TestEventsScanError(t *testing.T) {
	s, _, _ := open(t)
	raw(t, s, `INSERT INTO events (stream_id, at, type, data) VALUES ('x', 'não é número', 'request', '{}')`)
	if _, err := s.Events.Query(ctx, EventFilter{}); err == nil {
		t.Fatal("scan error ignored")
	}
}

func ids(evs []Event) []string {
	out := []string{}
	for _, e := range evs {
		out = append(out, e.StreamID)
	}
	return out
}

func f64(v float64) *float64 { return &v }

func TestUsage(t *testing.T) {
	s, c, _ := open(t)
	base := c.now().Truncate(time.Minute)
	mk := func(id string, at time.Time, typ, drv, model string, sample float64, data string) Event {
		return Event{StreamID: id, At: at, Type: typ, Driver: drv, Model: model, Sample: sample, EntityKind: "key", EntityID: "ent-" + id, Data: json.RawMessage(data)}
	}
	evs := []Event{
		mk("1", base, EventRequest, "openai", "gpt", 1,
			`{"key":"k1","ip":"1.1.1.1","fields":{"prompt_tokens":100,"output_tokens":10,"cached_tokens":50,"cost_usd":0.01,"account":"acme","tool_calls":{"search":2,"calc":1}}}`),
		mk("2", base.Add(30*time.Second), EventRequest, "openai", "gpt", 0.5,
			`{"key":"k1","ip":"2.2.2.2","fields":{"prompt_tokens":20,"output_tokens":5,"cached_tokens":0,"reasoning_tokens":7,"tool_calls":["search",{"name":"calc"},{"x":1},3]}}`),
		mk("3", base.Add(time.Minute), EventRequest, "anthropic", "claude", 1,
			`{"ip":"1.1.1.1","fields":{"prompt_tokens":1,"tool_calls":4}}`),
		mk("4", base.Add(time.Minute), EventError, "anthropic", "claude", 1, `{"key":"k2","fields":{"tool_calls":1.5}}`),
		mk("5", base.Add(time.Minute), EventBlock, "anthropic", "claude", 1, `{"fields":{"prompt_tokens":999}}`),
	}
	if _, err := s.Events.InsertBatch(ctx, evs); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Events.Usage(ctx, UsageQuery{GroupBy: GroupDriver})
	if err != nil {
		t.Fatal(err)
	}
	want := []UsageRow{
		{
			Group: "openai", Requests: 3, PromptTokens: f64(140), OutputTokens: f64(20), ReasoningTokens: f64(14),
			CachedTokens: f64(50), CostUSD: f64(0.01), ToolCalls: f64(11), CacheHitRate: f64(1.0 / 3),
		},
		{Group: "anthropic", Requests: 1, Errors: 1, PromptTokens: f64(1), ToolCalls: f64(5.5)},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("by driver:\n got %s\nwant %s", dump(rows), dump(want))
	}

	groups := func(q UsageQuery) []string {
		t.Helper()
		rows, err := s.Events.Usage(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, fmt.Sprintf("%s=%g/%g", r.Group, r.Requests, r.Errors))
		}
		return out
	}
	for _, tc := range []struct {
		q    UsageQuery
		want []string
	}{
		{UsageQuery{GroupBy: GroupModel}, []string{"gpt=3/0", "claude=1/1"}},
		{UsageQuery{GroupBy: GroupKey}, []string{"k1=3/0", "ent-3=1/0", "k2=0/1"}},
		{UsageQuery{GroupBy: GroupAccount}, []string{"=3/1", "acme=1/0"}},
		{UsageQuery{GroupBy: GroupIP}, []string{"1.1.1.1=2/0", "2.2.2.2=2/0", "=0/1"}},
		{UsageQuery{GroupBy: GroupEntity, Limit: 1}, []string{"key/ent-2=2/0"}},
		{UsageQuery{GroupBy: GroupTest}, []string{"=4/1"}},
		{UsageQuery{GroupBy: GroupDriver, Driver: "anthropic", Model: "claude", TestID: ""}, []string{"anthropic=1/1"}},
		{UsageQuery{GroupBy: GroupDriver, Since: base.Add(time.Minute), Until: base.Add(2 * time.Minute)}, []string{"anthropic=1/1"}},
		{UsageQuery{GroupBy: GroupDriver, TestID: "nenhum"}, nil},
	} {
		if got := groups(tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %v want %v", tc.q, got, tc.want)
		}
	}
	if _, err := s.Events.Usage(ctx, UsageQuery{GroupBy: "planeta"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad group: %v", err)
	}

	series, err := s.Events.UsageSeries(ctx, UsageQuery{}, time.Minute)
	if err != nil || len(series) != 2 || !series[0].At.Equal(base) || series[0].Requests != 3 ||
		!series[1].At.Equal(base.Add(time.Minute)) || series[1].Errors != 1 || series[1].OutputTokens != nil {
		t.Fatalf("series %+v %v", series, err)
	}
	if _, err := s.Events.UsageSeries(ctx, UsageQuery{}, time.Microsecond); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad step: %v", err)
	}

	tools, err := s.Events.ToolCallsByName(ctx, UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	wantTools := []ToolCallCount{{"calc", 3}, {"search", 4}}
	// search: 2 (obj, w=1) + 1*2 (array, w=2) = 4; calc: 1 + 2 = 3.
	if !reflect.DeepEqual(tools, []ToolCallCount{wantTools[1], wantTools[0]}) {
		t.Fatalf("tools %+v", tools)
	}
	if tools, err := s.Events.ToolCallsByName(ctx, UsageQuery{Limit: 1, Driver: "openai"}); err != nil || len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("tools limit %+v %v", tools, err)
	}
}

func dump(rows []UsageRow) string {
	out := ""
	for _, r := range rows {
		out += fmt.Sprintf("{%s req=%g err=%g", r.Group, r.Requests, r.Errors)
		for name, p := range map[string]*float64{
			"prompt": r.PromptTokens, "output": r.OutputTokens, "reasoning": r.ReasoningTokens,
			"cached": r.CachedTokens, "cost": r.CostUSD, "tools": r.ToolCalls, "hit": r.CacheHitRate,
		} {
			if p != nil {
				out += fmt.Sprintf(" %s=%g", name, *p)
			}
		}
		out += "} "
	}
	return out
}

// TestUsageByGroupValue: tool calls by group and the series of one
// key/account/ip are filtered and aggregated in SQL, so a volume far above
// what the API used to scan in Go comes back exact.
func TestUsageByGroupValue(t *testing.T) {
	s, c, _ := open(t)
	base := c.now().Truncate(time.Minute)
	const n = 6000
	evs := make([]Event, 0, n)
	for i := range n {
		key := fmt.Sprintf("k%d", i%3)
		data := fmt.Sprintf(`{"key":%q,"fields":{"prompt_tokens":2,"tool_calls":{"search":1}}}`, key)
		if i%3 == 2 {
			data = `{"fields":{"account":"acme","tool_calls":["calc",{"name":"search"}]}}`
		}
		evs = append(evs, Event{
			StreamID: fmt.Sprint(i), At: base.Add(time.Duration(i%120) * time.Second), Type: EventRequest,
			Driver: "openai", Sample: 1, Data: json.RawMessage(data),
		})
	}
	if _, err := s.Events.InsertBatch(ctx, evs); err != nil {
		t.Fatal(err)
	}

	tools, err := s.Events.ToolCallsByGroup(ctx, UsageQuery{GroupBy: GroupKey})
	want := map[string]map[string]float64{"k0": {"search": n / 3}, "k1": {"search": n / 3}, "": {"search": n / 3, "calc": n / 3}}
	if err != nil || !reflect.DeepEqual(tools, want) {
		t.Fatalf("tools by key %v %v", tools, err)
	}
	tools, err = s.Events.ToolCallsByGroup(ctx, UsageQuery{GroupBy: GroupAccount, Value: "acme"})
	if err != nil || !reflect.DeepEqual(tools, map[string]map[string]float64{"acme": {"search": n / 3, "calc": n / 3}}) {
		t.Fatalf("tools acme %v %v", tools, err)
	}
	rows, err := s.Events.Usage(ctx, UsageQuery{GroupBy: GroupKey, Value: "k1"})
	if err != nil || len(rows) != 1 || rows[0].Group != "k1" || rows[0].Requests != n/3 {
		t.Fatalf("usage k1 %+v %v", rows, err)
	}

	// 4 buckets of 10 s from base+60s: events from 60 s to 119 s, the last
	// bucket absorbs everything after base+90s.
	pts, err := s.Events.UsageBuckets(ctx, UsageQuery{GroupBy: GroupKey, Value: "k0", Since: base.Add(time.Minute)}, 10*time.Second, 4)
	if err != nil || len(pts) != 4 || !pts[0].At.Equal(base.Add(time.Minute)) || !pts[3].At.Equal(base.Add(90*time.Second)) {
		t.Fatalf("buckets %+v %v", pts, err)
	}
	var total float64
	for _, p := range pts {
		total += p.Requests
	}
	// k0 = seconds multiple of 3, 50 events each: 20 seconds in [60, 120).
	if total != 1000 || pts[0].Requests != 200 || pts[1].Requests != 150 || pts[3].Requests != 500 || *pts[0].PromptTokens != 400 {
		t.Fatalf("buckets k0: total %v %+v", total, pts)
	}
	series, err := s.Events.UsageSeries(ctx, UsageQuery{GroupBy: GroupIP, Value: "nenhum"}, time.Minute)
	if err != nil || len(series) != 0 {
		t.Fatalf("series ip %+v %v", series, err)
	}

	bad := map[string]func() error{
		"buckets sem início": func() error { _, err := s.Events.UsageBuckets(ctx, UsageQuery{}, time.Second, 4); return err },
		"buckets n=0": func() error {
			_, err := s.Events.UsageBuckets(ctx, UsageQuery{Since: base}, time.Second, 0)
			return err
		},
		"series sem grupo": func() error { _, err := s.Events.UsageSeries(ctx, UsageQuery{Value: "x"}, time.Second); return err },
		"tools sem grupo":  func() error { _, err := s.Events.ToolCallsByGroup(ctx, UsageQuery{}); return err },
	}
	for name, f := range bad {
		if err := f(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_ = s.Close()
	if _, err := s.Events.ToolCallsByGroup(ctx, UsageQuery{GroupBy: GroupKey}); err == nil {
		t.Fatal("tools com banco fechado")
	}
}

// TestUsageInputBreakdown: fields.input_breakdown is summed with weight
// 1/sample only over the request events that carry it, and tool_calls
// missing from a 0.4.0 event (with cost_usd) counts as 0 (ADR 0032).
func TestUsageInputBreakdown(t *testing.T) {
	s, c, _ := open(t)
	at := c.now()
	const ib = `"input_breakdown":{"system":40,"tool_schemas":30,"user":20,"assistant":10,"tool_calls":8,"tool_results":12,"images":0,"cached":64,"total":120}`
	ev := func(id, typ, drv string, sample float64, fields string) Event {
		return Event{StreamID: id, At: at, Type: typ, Driver: drv, Sample: sample, Data: json.RawMessage(`{"fields":{` + fields + `}}`)}
	}
	evs := []Event{
		ev("1", EventRequest, "openai", 1, `"cost_usd":0,`+ib),
		ev("2", EventRequest, "openai", 0.5, `"cost_usd":0.5,`+ib),
		// Without the breakdown: counts in requests, not in the breakdown.
		ev("3", EventRequest, "openai", 1, `"cost_usd":0`),
		// Partial object: the missing keys add nothing.
		ev("4", EventRequest, "openai", 1, `"input_breakdown":{"user":5,"total":5}`),
		// Errors never enter the breakdown.
		ev("5", EventError, "openai", 1, ib),
		// Pre-0.4.0 event: no cost_usd, no tool_calls, no breakdown.
		ev("6", EventRequest, "anthropic", 1, `"prompt_tokens":3`),
	}
	if _, err := s.Events.InsertBatch(ctx, evs); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events.Usage(ctx, UsageQuery{GroupBy: GroupDriver})
	if err != nil || len(rows) != 2 {
		t.Fatalf("usage %+v %v", rows, err)
	}
	o, a := rows[0], rows[1]
	if o.Group != "openai" || o.ToolCalls == nil || *o.ToolCalls != 0 || o.CostUSD == nil || *o.CostUSD != 1 {
		t.Fatalf("openai %s", dump(rows))
	}
	if o.InputBreakdownRequests != 4 {
		t.Fatalf("breakdown requests %g", o.InputBreakdownRequests)
	}
	got := map[string]any{}
	for i, p := range o.InputBreakdown.ptrs() {
		if *p == nil {
			got[InputBreakdownKeys[i]] = nil
		} else {
			got[InputBreakdownKeys[i]] = **p
		}
	}
	want := map[string]any{
		"system": 120.0, "tool_schemas": 90.0, "user": 65.0, "assistant": 30.0, "tool_calls": 24.0,
		"tool_results": 36.0, "images": 0.0, "cached": 192.0, "total": 365.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("breakdown %v", got)
	}
	if a.Group != "anthropic" || a.ToolCalls != nil || a.InputBreakdownRequests != 0 || a.InputBreakdown != (InputBreakdown{}) {
		t.Fatalf("anthropic %+v", a)
	}
	pts, err := s.Events.UsageSeries(ctx, UsageQuery{GroupBy: GroupDriver, Value: "openai"}, time.Hour)
	if err != nil || len(pts) != 1 || pts[0].InputBreakdownRequests != 4 || *pts[0].InputBreakdown.Total != 365 {
		t.Fatalf("series %+v %v", pts, err)
	}
}
