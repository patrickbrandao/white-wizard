package config

import (
	"strings"
	"testing"
	"time"
)

func mustCron(t *testing.T, expr string, loc *time.Location) *Cron {
	t.Helper()
	c, err := ParseCron(expr, loc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestParseCronErrors(t *testing.T) {
	cases := map[string]string{
		"* * * * *":           "want 6 fields",
		"60 * * * * *":        "second: value 60 out of range 0-59",
		"* x * * * *":         `minute: invalid value "x"`,
		"* * 5-2 * * *":       `hour: range "5-2" is backwards`,
		"* * * 0 * *":         "day of month: value 0 out of range 1-31",
		"* * * * 1-x *":       `month: invalid value "x"`,
		"* * * * * 8":         "day of week: value 8 out of range 0-7",
		"*/0 * * * * *":       `second: invalid step "0"`,
		"*/a * * * * *":       `second: invalid step "a"`,
		"* * * * x-3 *":       `month: invalid value "x"`,
		"1,2,99 * * * * *":    "value 99 out of range",
		"* * * 1-40/2 * *":    "value 40 out of range",
		"* * * * * 1-2,x/3 *": "want 6 fields",
	}
	for expr, want := range cases {
		_, err := ParseCron(expr, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseCron(%q) = %v, want error containing %q", expr, err, want)
		}
	}
}

func TestParseCronFields(t *testing.T) {
	c := mustCron(t, "*/15 1-3,10 0-23/6 ? 2/5 7", nil)
	if c.sec != 1|1<<15|1<<30|1<<45 {
		t.Fatalf("sec %b", c.sec)
	}
	if c.min != 1<<1|1<<2|1<<3|1<<10 {
		t.Fatalf("min %b", c.min)
	}
	if c.hour != 1|1<<6|1<<12|1<<18 {
		t.Fatalf("hour %b", c.hour)
	}
	if !c.domStar || c.dowStar || c.dow != 1 {
		t.Fatalf("dom star %v dow star %v dow %b", c.domStar, c.dowStar, c.dow)
	}
	if c.month != 1<<2|1<<7|1<<12 {
		t.Fatalf("month %b", c.month)
	}
	if c.loc != time.UTC {
		t.Fatal("nil location must be UTC")
	}
}

func TestPrevBit(t *testing.T) {
	if prevBit(0b1010, -1) != -1 || prevBit(0b1010, 0) != -1 || prevBit(0b1010, 2) != 1 || prevBit(0b1010, 59) != 3 {
		t.Fatal("prevBit")
	}
}

func TestCronPrev(t *testing.T) {
	cases := []struct {
		expr, now, earliest, want string
	}{
		// seconds, minutes and hours
		{"*/30 * * * * *", "2026-10-31T12:00:44Z", "2026-10-31T12:00:00Z", "2026-10-31T12:00:30Z"},
		{"0 */15 * * * *", "2026-10-31T12:44:59Z", "2026-10-31T12:00:00Z", "2026-10-31T12:30:00Z"},
		{"0 0 3 * * *", "2026-10-31T02:59:59Z", "2026-10-29T00:00:00Z", "2026-10-30T03:00:00Z"},
		{"0 0 3 * * *", "2026-10-31T03:00:00Z", "2026-10-31T00:00:00Z", "2026-10-31T03:00:00Z"},
		// the second boundary inside the current minute
		{"10,50 * * * * *", "2026-10-31T12:05:20Z", "2026-10-31T12:00:00Z", "2026-10-31T12:05:10Z"},
		{"50 * * * * *", "2026-10-31T12:05:20Z", "2026-10-31T12:00:00Z", "2026-10-31T12:04:50Z"},
		// day of month OR day of week when both are restricted
		{"0 0 0 1 * 6", "2026-10-30T12:00:00Z", "2026-10-01T00:00:00Z", "2026-10-24T00:00:00Z"},
		{"0 0 0 13 * 6", "2026-10-15T12:00:00Z", "2026-10-01T00:00:00Z", "2026-10-13T00:00:00Z"},
		// only day of week restricted; month restriction
		{"0 0 0 * * 1", "2026-10-31T12:00:00Z", "2026-10-01T00:00:00Z", "2026-10-26T00:00:00Z"},
		{"0 0 0 * 9 *", "2026-10-31T12:00:00Z", "2026-09-01T00:00:00Z", "2026-09-30T00:00:00Z"},
		// nothing in range
		{"0 0 3 * * *", "2026-10-31T02:59:59Z", "2026-10-30T03:00:01Z", ""},
		{"0 0 0 1 1 *", "2026-10-31T12:00:00Z", "2026-10-01T00:00:00Z", ""},
	}
	for _, tc := range cases {
		c := mustCron(t, tc.expr, nil)
		got, ok := c.Prev(utc(tc.now), utc(tc.earliest))
		if tc.want == "" {
			if ok {
				t.Errorf("%s at %s: got %s, want none", tc.expr, tc.now, got)
			}
			continue
		}
		if !ok || !got.Equal(utc(tc.want)) {
			t.Errorf("%s at %s: got %s %v, want %s", tc.expr, tc.now, got, ok, tc.want)
		}
	}
}

// TestCronPrevTimezoneAndDST: the schedule runs on the wall clock of its
// zone, and a firing inside a DST gap (02:30 on the spring-forward day in
// New York) is skipped rather than reported in the future.
func TestCronPrevTimezoneAndDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	c := mustCron(t, "0 0 3 * * *", ny)
	got, ok := c.Prev(utc("2026-10-31T08:00:00Z"), utc("2026-10-30T00:00:00Z"))
	if !ok || !got.Equal(utc("2026-10-31T07:00:00Z")) { // 03:00 EDT
		t.Fatalf("got %s %v", got, ok)
	}
	gap := mustCron(t, "0 30 2 * * *", ny)
	// 2026-03-08 03:10 EDT = 07:10 UTC; 02:30 that day does not exist.
	now := utc("2026-03-08T07:10:00Z")
	if got, ok := gap.Prev(now, now.Add(-time.Hour)); ok {
		t.Fatalf("DST gap: got %s", got)
	}
	got, ok = gap.Prev(now, now.Add(-48*time.Hour))
	if !ok || !got.Equal(utc("2026-03-07T07:30:00Z")) { // 02:30 EST the day before
		t.Fatalf("DST gap fallback: got %s %v", got, ok)
	}
}

func TestOutageWindow(t *testing.T) {
	cases := []struct {
		name, sched string
		dur         time.Duration
		now, end    string
	}{
		{"seconds inside", "*/30 * * * * *", 5 * time.Second, "2026-10-31T12:00:33.500Z", "2026-10-31T12:00:35Z"},
		{"seconds outside", "*/30 * * * * *", 5 * time.Second, "2026-10-31T12:00:35Z", ""},
		{"minutes inside", "0 */10 * * * *", time.Minute, "2026-10-31T12:20:59.999Z", "2026-10-31T12:21:00Z"},
		{"minutes outside", "0 */10 * * * *", time.Minute, "2026-10-31T12:21:00Z", ""},
		{"hours inside", "0 0 */6 * * *", time.Hour, "2026-10-31T06:30:00Z", "2026-10-31T07:00:00Z"},
		{"hours across midnight", "0 30 23 * * *", time.Hour, "2026-11-01T00:10:00Z", "2026-11-01T00:30:00Z"},
		{"hours outside", "0 0 */6 * * *", time.Hour, "2026-10-31T07:00:00Z", ""},
		{"overlapping windows", "*/10 * * * * *", 30 * time.Second, "2026-10-31T12:00:25Z", "2026-10-31T12:00:50Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Outage{Schedule: tc.sched, Duration: tc.dur}
			if err := o.Compile(); err != nil {
				t.Fatal(err)
			}
			end, ok := o.Window(utc(tc.now))
			if tc.end == "" {
				if ok {
					t.Fatalf("active until %s, want inactive", end)
				}
				return
			}
			if !ok || !end.Equal(utc(tc.end)) {
				t.Fatalf("got %s %v, want %s", end, ok, tc.end)
			}
		})
	}
}

// TestOutageWindowMemo: the latest firing is cached for the current second;
// a different second or duration recomputes it, and an uncompiled outage
// compiles on the fly.
func TestOutageWindowMemo(t *testing.T) {
	o := Outage{Schedule: "*/30 * * * * *", Duration: 5 * time.Second}
	if _, ok := o.Window(utc("2026-10-31T12:00:01Z")); !ok {
		t.Fatal("uncompiled outage must still evaluate")
	}
	if err := o.Compile(); err != nil {
		t.Fatal(err)
	}
	now := utc("2026-10-31T12:00:04Z")
	if _, ok := o.Window(now); !ok {
		t.Fatal("inside")
	}
	m := o.rt.memo.Load()
	if _, ok := o.Window(now.Add(500 * time.Millisecond)); !ok || o.rt.memo.Load() != m {
		t.Fatal("same second must reuse the memo")
	}
	o.Duration = 2 * time.Second
	if _, ok := o.Window(now.Add(500 * time.Millisecond)); ok || o.rt.memo.Load() == m {
		t.Fatal("new duration must recompute")
	}
	o.Duration = 0
	if _, ok := o.Window(now); ok {
		t.Fatal("zero duration is never active")
	}
	bad := Outage{Schedule: "nope", Duration: time.Second}
	if _, ok := bad.Window(now); ok {
		t.Fatal("invalid schedule is never active")
	}
	if bad.Compile() == nil {
		t.Fatal("Compile must fail")
	}
	badTZ := Outage{Schedule: "* * * * * *", Duration: time.Second, Timezone: "Mars/Olympus"}
	if badTZ.Compile() == nil {
		t.Fatal("Compile must fail on timezone")
	}
}

func TestOutageAccessors(t *testing.T) {
	r := 50.0
	o := Outage{}
	if o.Kind() != "overloaded" || o.EffectiveRate() != 100 || !o.AppliesTo("openai") {
		t.Fatal("defaults")
	}
	o = Outage{Type: "bad_gateway", Rate: &r, Drivers: []string{"anthropic"}}
	if o.Kind() != "bad_gateway" || o.EffectiveRate() != 50 || o.AppliesTo("openai") || !o.AppliesTo("anthropic") {
		t.Fatal("explicit values")
	}
}

func TestLoadLocationCache(t *testing.T) {
	if l, err := loadLocation(""); err != nil || l != time.UTC {
		t.Fatal("empty = UTC")
	}
	a, err := loadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := loadLocation("UTC")
	if a != b {
		t.Fatal("location must be cached")
	}
	if _, err := loadLocation("Mars/Olympus"); err == nil {
		t.Fatal("unknown zone")
	}
}

const outageYAML = `
errors:
  outages:
    - {schedule: "*/30 * * * * *", duration: 5s, type: overloaded}
    - {schedule: "0 */10 * * * *", duration: 1m, type: server_error, rate: 50, drivers: [openai]}
    - {schedule: "0 0 */6 * * *", duration: 1h, type: bad_gateway, timezone: America/Sao_Paulo}
profiles:
  errors:
    down:
      rate: 0
      outages:
        - {schedule: "0 0 3 * * *", duration: 2m}
models:
  - id: m1
    errors: down
  - id: m2
    errors:
      rate: 0
      outages:
        - {schedule: "0 * * * * *", duration: 5s}
`

func TestParseOutages(t *testing.T) {
	c, err := Parse([]byte(outageYAML))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Errors.Outages); n != 3 {
		t.Fatalf("root outages %d", n)
	}
	o := c.Errors.Outages[1]
	if o.Duration != time.Minute || o.EffectiveRate() != 50 || o.Kind() != "server_error" || o.Drivers[0] != "openai" || o.rt == nil {
		t.Fatalf("outage %+v", o)
	}
	if c.Errors.Outages[2].rt.cron.loc.String() != "America/Sao_Paulo" {
		t.Fatal("timezone")
	}
	p := c.Profiles.Errors["down"]
	if len(p.Outages) != 1 || p.Outages[0].Kind() != "overloaded" || p.Outages[0].rt == nil {
		t.Fatalf("profile outages %+v", p.Outages)
	}
	m, err := c.ErrorsFor(&c.Models[1])
	if err != nil || len(m.Outages) != 1 || m.Outages[0].rt == nil {
		t.Fatalf("inline outages %+v %v", m.Outages, err)
	}
}

func TestValidateOutages(t *testing.T) {
	RegisterErrorTypes(func(s string) bool { return s == "overloaded" || s == "server_error" })
	defer RegisterErrorTypes(nil)
	cases := []struct{ yaml, want string }{
		{"errors:\n  outages:\n    - {duration: 5s}\n", "errors.outages[0]: schedule is required"},
		{"errors:\n  outages:\n    - {schedule: '* * * * *', duration: 5s}\n", "errors.outages[0]: schedule: cron \"* * * * *\": want 6 fields"},
		{"errors:\n  outages:\n    - {schedule: '61 * * * * *', duration: 5s}\n", "second: value 61 out of range"},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 5s, timezone: Mars/Olympus}\n", `errors.outages[0]: timezone: invalid timezone "Mars/Olympus"`},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *'}\n", "errors.outages[0]: duration must be positive"},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: -1s}\n", "duration must be positive"},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, rate: 101}\n", "rate must be between 0 and 100"},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, rate: -1}\n", "rate must be between 0 and 100"},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, type: nope}\n", `unknown error type "nope"`},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, drivers: [gemini]}\n", `drivers: unknown driver "gemini"`},
		{"errors:\n  outages:\n    - {schedule: '* * * * * *', duration: 1s, bogus: 1}\n", "bogus"},
		{"profiles:\n  errors:\n    x:\n      outages:\n        - {schedule: bad, duration: 1s}\n", "profiles.errors.x.outages[0]: schedule"},
		{"models:\n  - id: m\n    errors:\n      outages:\n        - {schedule: bad, duration: 1s}\n", "models[m].errors.outages[0]: schedule"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.yaml, err, tc.want)
		}
	}
	// Outages built in code (no Compile) are validated too.
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Errors.Outages = []Outage{{Schedule: "* * * * * 9", Duration: time.Second}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "day of week") {
		t.Fatalf("validate: %v", err)
	}
	c.Errors.Outages = []Outage{{Schedule: "* * * * * *", Duration: time.Second, Type: "server_error"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	RegisterErrorTypes(nil)
	c.Errors.Outages[0].Type = "anything"
	if err := c.Validate(); err != nil {
		t.Fatalf("without a registered checker any type passes: %v", err)
	}
}
