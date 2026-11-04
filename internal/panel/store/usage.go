package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Usage groupings (spec 016, "Uso da inferência falsa").
const (
	GroupDriver  = "driver"
	GroupModel   = "model"
	GroupKey     = "key"
	GroupAccount = "account"
	GroupIP      = "ip"
	GroupEntity  = "entity"
	GroupTest    = "test"
)

// groupExpr maps a grouping to its SQL expression. key/account/ip prefer the
// top-level event field, then fields.<name>, then the entity when it has
// that kind.
var groupExpr = map[string]string{
	GroupDriver:  "driver",
	GroupModel:   "model",
	GroupKey:     idExpr("key"),
	GroupAccount: idExpr("account"),
	GroupIP:      idExpr("ip"),
	GroupEntity:  "entity_kind || '/' || entity_id",
	GroupTest:    "test_id",
}

func idExpr(kind string) string {
	return fmt.Sprintf("coalesce(json_extract(data, '$.%[1]s'), json_extract(data, '$.fields.%[1]s'), CASE WHEN entity_kind = '%[1]s' THEN entity_id END)", kind)
}

// UsageQuery selects the request/error events aggregated by Usage.
type UsageQuery struct {
	GroupBy string    // one of the Group* constants
	Since   time.Time // at >= Since (zero = no bound)
	Until   time.Time // at < Until (zero = no bound)
	Driver  string
	Model   string
	TestID  string
	Limit   int // <= 0 = all groups
	// Value, when not empty, keeps only the events whose GroupBy value is
	// Value; the filter runs in SQL (json_extract), never in Go.
	Value string
}

func (q UsageQuery) filter() EventFilter {
	return EventFilter{
		Types: []string{EventRequest, EventError}, Since: q.Since, Until: q.Until,
		Driver: q.Driver, Model: q.Model, TestID: q.TestID,
	}
}

// group returns the SQL expression of q.GroupBy (required when need or
// when Value is set) and the conditions of q, including the Value filter.
func (q UsageQuery) group(need bool) (string, []string, []any, error) {
	conds, args := q.filter().conds()
	if !need && q.Value == "" {
		return "", conds, args, nil
	}
	expr, ok := groupExpr[q.GroupBy]
	if !ok {
		return "", nil, nil, fmt.Errorf("%w: agrupamento %q", ErrInvalidInput, q.GroupBy)
	}
	if q.Value != "" {
		conds = append(conds, "("+expr+") = ?")
		args = append(args, q.Value)
	}
	return expr, conds, args, nil
}

// UsageRow is one group of Usage. Every count is weighted by 1/sample.
// Pointer fields are nil when no event of the group carried that field, so
// the screen shows "—" instead of zero (spec 016).
type UsageRow struct {
	Group           string // "" when the events had no value for the grouping
	Requests        float64
	Errors          float64
	PromptTokens    *float64
	OutputTokens    *float64
	ReasoningTokens *float64
	CachedTokens    *float64
	CostUSD         *float64
	ToolCalls       *float64 // fields.tool_calls: count, array (length) or {name: count}
	CacheHitRate    *float64 // share of requests with cached_tokens > 0 among those that report it
	// InputBreakdown sums each key of fields.input_breakdown (I16, ADR 0032)
	// over the request events that carry it; every key is nil when none does.
	InputBreakdown InputBreakdown
	// InputBreakdownRequests counts (weighted) the request events that carry
	// fields.input_breakdown.
	InputBreakdownRequests float64
}

// InputBreakdown is the input composition of a group, in the order of
// InputBreakdownKeys.
type InputBreakdown struct {
	System, ToolSchemas, User, Assistant, ToolCalls, ToolResults, Images, Cached, Total *float64
}

// InputBreakdownKeys are the keys of fields.input_breakdown (asyncapi
// InputBreakdown), in the order of the InputBreakdown fields.
var InputBreakdownKeys = []string{"system", "tool_schemas", "user", "assistant", "tool_calls", "tool_results", "images", "cached", "total"}

func (b *InputBreakdown) ptrs() []**float64 {
	return []**float64{&b.System, &b.ToolSchemas, &b.User, &b.Assistant, &b.ToolCalls, &b.ToolResults, &b.Images, &b.Cached, &b.Total}
}

const weight = "(1.0 / sample)"

func fieldSum(name string) string {
	return fmt.Sprintf("sum(%s * json_extract(data, '$.fields.%s'))", weight, name)
}

const toolCallsExpr = `CASE json_type(data, '$.fields.tool_calls')
	WHEN 'integer' THEN json_extract(data, '$.fields.tool_calls')
	WHEN 'real' THEN json_extract(data, '$.fields.tool_calls')
	WHEN 'array' THEN json_array_length(data, '$.fields.tool_calls')
	WHEN 'object' THEN (SELECT total(value) FROM json_each(data, '$.fields.tool_calls'))
	ELSE CASE WHEN json_extract(data, '$.fields.cost_usd') IS NOT NULL THEN 0 END
	END`

// hasBreakdown selects the request events that carry fields.input_breakdown.
const hasBreakdown = "type = 'request' AND json_type(data, '$.fields.input_breakdown') = 'object'"

func breakdownCols() string {
	out := ""
	for _, k := range InputBreakdownKeys {
		out += fmt.Sprintf(", sum(CASE WHEN %s THEN %s * json_extract(data, '$.fields.input_breakdown.%s') END)", hasBreakdown, weight, k)
	}
	return out + ", total(CASE WHEN " + hasBreakdown + " THEN " + weight + " END)"
}

var usageCols = "sum(CASE WHEN type = 'request' THEN " + weight + " ELSE 0 END), " +
	"sum(CASE WHEN type = 'error' THEN " + weight + " ELSE 0 END), " +
	fieldSum("prompt_tokens") + ", " + fieldSum("output_tokens") + ", " + fieldSum("reasoning_tokens") + ", " +
	fieldSum("cached_tokens") + ", " + fieldSum("cost_usd") + ", " +
	"sum(" + weight + " * " + toolCallsExpr + "), " +
	"sum(CASE WHEN json_extract(data, '$.fields.cached_tokens') > 0 THEN " + weight + " ELSE 0 END) / " +
	"sum(CASE WHEN json_extract(data, '$.fields.cached_tokens') IS NOT NULL THEN " + weight + " END)" +
	breakdownCols()

func scanUsage(rows *sql.Rows, lead ...any) (UsageRow, error) {
	var u UsageRow
	ptrs := append([]**float64{&u.PromptTokens, &u.OutputTokens, &u.ReasoningTokens, &u.CachedTokens, &u.CostUSD, &u.ToolCalls, &u.CacheHitRate},
		u.InputBreakdown.ptrs()...)
	nums := make([]sql.NullFloat64, len(ptrs))
	dst := append(append([]any(nil), lead...), &u.Requests, &u.Errors)
	for i := range nums {
		dst = append(dst, &nums[i])
	}
	dst = append(dst, &u.InputBreakdownRequests)
	err := rows.Scan(dst...)
	for i, p := range ptrs {
		if nums[i].Valid {
			v := nums[i].Float64
			*p = &v
		}
	}
	return u, err
}

// Usage aggregates request and error events by q.GroupBy, largest groups
// first. An unknown grouping returns ErrInvalidInput.
func (r *Events) Usage(ctx context.Context, q UsageQuery) ([]UsageRow, error) {
	expr, conds, args, err := q.group(true)
	if err != nil {
		return nil, err
	}
	sqlq := "SELECT coalesce(" + expr + ", '') AS g, " + usageCols + " FROM events" + whereSQL(conds) +
		" GROUP BY g ORDER BY 2 DESC, 3 DESC, g"
	if q.Limit > 0 {
		sqlq += " LIMIT ?"
		args = append(args, q.Limit)
	}
	return queryAll(ctx, r.s.r, func(rows *sql.Rows) (UsageRow, error) {
		var g string
		u, err := scanUsage(rows, &g)
		u.Group = g
		return u, err
	}, sqlq, args...)
}

// UsagePoint is one bucket of UsageSeries.
type UsagePoint struct {
	At time.Time // bucket start
	UsageRow
}

// UsageSeries aggregates the same events in buckets of step (>= 1 ms)
// aligned to the epoch, oldest first; q.Limit is ignored and q.GroupBy only
// matters with q.Value. Buckets without events are omitted.
func (r *Events) UsageSeries(ctx context.Context, q UsageQuery, step time.Duration) ([]UsagePoint, error) {
	return r.series(ctx, q, step, 0)
}

// UsageBuckets is UsageSeries with n buckets starting at q.Since (required):
// bucket i covers [Since+i*step, Since+(i+1)*step) and events after the
// last bucket fall into it. Buckets without events are omitted.
func (r *Events) UsageBuckets(ctx context.Context, q UsageQuery, step time.Duration, n int) ([]UsagePoint, error) {
	if q.Since.IsZero() || n < 1 {
		return nil, fmt.Errorf("%w: buckets sem início ou n < 1", ErrInvalidInput)
	}
	return r.series(ctx, q, step, n)
}

func (r *Events) series(ctx context.Context, q UsageQuery, step time.Duration, n int) ([]UsagePoint, error) {
	if step < time.Millisecond {
		return nil, fmt.Errorf("%w: passo %v", ErrInvalidInput, step)
	}
	_, conds, args, err := q.group(false)
	if err != nil {
		return nil, err
	}
	stepMS := step.Milliseconds()
	bucket := fmt.Sprintf("(at / %d) * %d", stepMS, stepMS)
	if n > 0 {
		origin := ms(q.Since)
		bucket = fmt.Sprintf("%d + min((at - %d) / %d, %d) * %d", origin, origin, stepMS, n-1, stepMS)
	}
	sqlq := fmt.Sprintf("SELECT %s AS b, %s FROM events%s GROUP BY b ORDER BY b", bucket, usageCols, whereSQL(conds))
	return queryAll(ctx, r.s.r, func(rows *sql.Rows) (UsagePoint, error) {
		var b int64
		u, err := scanUsage(rows, &b)
		return UsagePoint{At: fromMS(b), UsageRow: u}, err
	}, sqlq, args...)
}

// ToolCallCount is one row of ToolCallsByName.
type ToolCallCount struct {
	Name  string
	Calls float64 // weighted by 1/sample
}

// ToolCallsByName counts tool calls by name from fields.tool_calls of
// request events, accepting {name: count}, [name, ...] or [{name: ...}, ...].
func (r *Events) ToolCallsByName(ctx context.Context, q UsageQuery) ([]ToolCallCount, error) {
	f := q.filter()
	f.Types = []string{EventRequest}
	conds, args := f.conds()
	conds = append(conds, "json_type(data, '$.fields.tool_calls') IN ('object', 'array')")
	sqlq := `SELECT name, sum(calls) AS c FROM (
		SELECT CASE WHEN json_type(e.data, '$.fields.tool_calls') = 'object' THEN j.key
			WHEN j.type = 'text' THEN j.value
			WHEN j.type = 'object' THEN json_extract(j.value, '$.name') END AS name,
			(1.0 / e.sample) * CASE WHEN json_type(e.data, '$.fields.tool_calls') = 'object' THEN j.value ELSE 1 END AS calls
		FROM (SELECT * FROM events` + whereSQL(conds) + `) e, json_each(e.data, '$.fields.tool_calls') j
	) WHERE name IS NOT NULL GROUP BY name ORDER BY c DESC, name`
	if q.Limit > 0 {
		sqlq += " LIMIT ?"
		args = append(args, q.Limit)
	}
	return queryAll(ctx, r.s.r, func(rows *sql.Rows) (ToolCallCount, error) {
		var t ToolCallCount
		err := rows.Scan(&t.Name, &t.Calls)
		return t, err
	}, sqlq, args...)
}

// ToolCallsByGroup counts tool calls by group value (q.GroupBy, required)
// and name over the request and error events of q, with the same formats
// as ToolCallsByName; q.Limit is ignored. The whole aggregation runs in SQL.
func (r *Events) ToolCallsByGroup(ctx context.Context, q UsageQuery) (map[string]map[string]float64, error) {
	expr, conds, args, err := q.group(true)
	if err != nil {
		return nil, err
	}
	conds = append(conds, "json_type(data, '$.fields.tool_calls') IN ('object', 'array')")
	sqlq := `SELECT g, name, sum(calls) FROM (
		SELECT e.g, CASE WHEN json_type(e.data, '$.fields.tool_calls') = 'object' THEN j.key
			WHEN j.type = 'text' THEN j.value
			WHEN j.type = 'object' THEN json_extract(j.value, '$.name') END AS name,
			(1.0 / e.sample) * CASE WHEN json_type(e.data, '$.fields.tool_calls') = 'object' THEN j.value ELSE 1 END AS calls
		FROM (SELECT data, sample, coalesce(` + expr + `, '') AS g FROM events` + whereSQL(conds) + `) e,
			json_each(e.data, '$.fields.tool_calls') j
	) WHERE name IS NOT NULL GROUP BY g, name`
	type row struct {
		g, name string
		calls   float64
	}
	rows, err := queryAll(ctx, r.s.r, func(rows *sql.Rows) (row, error) {
		var x row
		err := rows.Scan(&x.g, &x.name, &x.calls)
		return x, err
	}, sqlq, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, x := range rows {
		if out[x.g] == nil {
			out[x.g] = map[string]float64{}
		}
		out[x.g][x.name] = x.calls
	}
	return out, nil
}
