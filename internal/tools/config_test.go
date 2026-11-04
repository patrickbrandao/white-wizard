package tools

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const specExample = `
listen: ":9200"
seed: 0
timeout: 30s
max_sessions: 100000
session_ttl: 30m
latency: {min: 5ms, max: 50ms}
outcomes: {error: 0, timeout: 0, invalid_payload: 0}
tools:
  run_command: {fail_rate: 10, latency: {min: 200ms, max: 2s}}
  web_search: {latency: {min: 100ms, max: 800ms}}
  fetch_url: {latency: {min: 50ms, max: 500ms}, outcomes: {error: 2}}
`

func TestParseSpecExample(t *testing.T) {
	c, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9200" || c.Timeout != 30*time.Second || c.MaxSessions != 100000 || c.SessionTTL != 30*time.Minute {
		t.Fatalf("top: %+v", c)
	}
	fu := c.effective(ToolFetchURL)
	if fu.rates.Error != 2 || fu.latency != (Range{50 * time.Millisecond, 500 * time.Millisecond}) {
		t.Fatalf("fetch_url: %+v", fu)
	}
	rc := c.effective(ToolRunCommand)
	if rc.failRate != 10 || rc.latency.Max != 2*time.Second {
		t.Fatalf("run_command: %+v", rc)
	}
	rf := c.effective(ToolReadFile)
	if rf.latency != (Range{5 * time.Millisecond, 50 * time.Millisecond}) || rf.failRate != 0 {
		t.Fatalf("read_file: %+v", rf)
	}
}

func TestDefaultsEqualSpecExample(t *testing.T) {
	c, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultConfig()
	for _, tool := range catalog {
		want := c.effective(tool.Name)
		if tool.Name == ToolFetchURL {
			want.rates.Error = 0
		}
		if got := d.effective(tool.Name); got != want {
			t.Errorf("%s: default %+v, example %+v", tool.Name, got, want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	c, err := Parse([]byte(`
seed: -7
latency: {max: 80ms}
outcomes: {timeout: 10, error: 5.5, invalid_payload: 1}
tools:
  read_file: {latency: {min: 1ms, max: 2ms}, outcomes: {timeout: 0}}
  run_command: {latency: {min: 1s}, outcomes: {invalid_payload: 20}}
  edit_file: {outcomes: {error: 1}}
  write_file: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Seed != -7 || c.Latency != (Range{5 * time.Millisecond, 80 * time.Millisecond}) {
		t.Fatalf("top: %+v", c)
	}
	if got := c.effective(ToolReadFile).rates; got != (Rates{Error: 5.5, Timeout: 0, InvalidPayload: 1}) {
		t.Fatalf("read_file rates %+v", got)
	}
	if got := c.effective(ToolRunCommand); got.latency != (Range{time.Second, 2 * time.Second}) || got.rates.InvalidPayload != 20 || got.failRate != 10 {
		t.Fatalf("run_command %+v", got)
	}
	if got := c.effective(ToolEditFile).rates; got.Error != 1 || got.Timeout != 10 {
		t.Fatalf("edit_file %+v", got)
	}
	if got := c.effective(ToolWebSearch).rates; got != (Rates{Error: 5.5, Timeout: 10, InvalidPayload: 1}) {
		t.Fatalf("web_search %+v", got)
	}
}

func TestParseEmptyAndNilTools(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.Listen != DefaultListen {
		t.Fatalf("empty: %v %+v", err, c)
	}
	// tools section with Tools map cleared by a programmatic caller.
	p := &parser{}
	cfg := Config{}
	p.tools("tools", map[string]any{"read_file": map[string]any{}}, &cfg)
	if _, ok := cfg.Tools[ToolReadFile]; !ok || len(p.errs) != 0 {
		t.Fatalf("nil map: %+v %v", cfg.Tools, p.errs)
	}
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(error)
	walk = func(err error) {
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			for _, e := range j.Unwrap() {
				walk(e)
			}
			return
		}
		var fe *FieldError
		if !errors.As(err, &fe) {
			t.Fatalf("not a FieldError: %v", err)
		}
		out[fe.Field] = fe.Msg
	}
	walk(err)
	return out
}

func TestParseErrorsWithPath(t *testing.T) {
	cases := []struct {
		yaml  string
		field string
	}{
		{"foo: 1", "foo"},
		{"- 1", "(raiz)"},
		{"listen: 1", "listen"},
		{"seed: x", "seed"},
		{"seed: 18446744073709551615", "seed"},
		{"seed: 1.5", "seed"},
		{"timeout: 5", "timeout"},
		{"timeout: abc", "timeout"},
		{"timeout: 0s", "timeout"},
		{"max_sessions: 0", "max_sessions"},
		{"max_sessions: 9999999999", "max_sessions"},
		{"session_ttl: -1s", "session_ttl"},
		{"latency: 3", "latency"},
		{"latency: {min: 9ms, max: 1ms}", "latency"},
		{"latency: {min: -1ms}", "latency"},
		{"latency: {mid: 1ms}", "latency.mid"},
		{"outcomes: {error: 101}", "outcomes.error"},
		{"outcomes: {timeout: -1}", "outcomes.timeout"},
		{"outcomes: {invalid_payload: x}", "outcomes.invalid_payload"},
		{"outcomes: {error: 60, timeout: 50}", "outcomes"},
		{"outcomes: {boom: 1}", "outcomes.boom"},
		{"tools: []", "tools"},
		{"tools: {rm_rf: {}}", "tools.rm_rf"},
		{"tools: {read_file: 1}", "tools.read_file"},
		{"tools: {read_file: {fail_rate: 1}}", "tools.read_file.fail_rate"},
		{"tools: {read_file: {latency: {max: 1ns}}}", "tools.read_file.latency"},
		{"tools: {fetch_url: {outcomes: {error: 200}}}", "tools.fetch_url.outcomes.error"},
		{"tools: {run_command: {fail_rate: 101}}", "tools.run_command.fail_rate"},
		{"tools: {run_command: {fail_rate: -2}}", "tools.run_command.fail_rate"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%q: want error", tc.yaml)
			continue
		}
		fe := fieldErrors(t, err)
		if _, ok := fe[tc.field]; !ok {
			t.Errorf("%q: want field %s, got %v", tc.yaml, tc.field, err)
		}
	}
	if _, err := Parse([]byte("a: [")); err == nil || !strings.HasPrefix(err.Error(), "tools config:") {
		t.Errorf("syntax: %v", err)
	}
	if got := (&FieldError{"a.b", "x"}).Error(); got != "a.b: x" {
		t.Errorf("FieldError: %q", got)
	}
}

func TestValidateProgrammatic(t *testing.T) {
	nan := math.NaN()
	one := 1.0
	c := Config{
		Tools: map[string]ToolConfig{
			"bogus":        {},
			ToolWriteFile:  {FailRate: &one},
			ToolRunCommand: {FailRate: &nan},
			ToolSearchCode: {Outcomes: RateOverrides{Error: &nan}},
			ToolWebSearch:  {Latency: &Range{-1, 0}},
			ToolEditFile:   {},
			ToolFetchURL:   {},
			ToolReadFile:   {},
			ToolListDir:    {},
		},
	}
	fe := fieldErrors(t, c.Validate())
	for _, f := range []string{
		"listen", "timeout", "max_sessions", "session_ttl", "tools.bogus",
		"tools.write_file.fail_rate", "tools.run_command.fail_rate",
		"tools.search_code.outcomes.error", "tools.web_search.latency",
	} {
		if _, ok := fe[f]; !ok {
			t.Errorf("want %s in %v", f, fe)
		}
	}
	if _, err := New(c); err == nil {
		t.Fatal("New must validate")
	}
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte("listen: \":7000\"\nseed: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	c, err := Load(Flags{Config: path}, getenv)
	if err != nil || c.Listen != ":7000" || c.Seed != 3 || c.Token != "" {
		t.Fatalf("file: %v %+v", err, c)
	}
	env["WWZ_TOOLS_LISTEN"] = ":7001"
	env["WWZ_TOOLS_SEED"] = "4"
	env["WWZ_TOOLS_TOKEN"] = "tok"
	c, err = Load(Flags{Config: path}, getenv)
	if err != nil || c.Listen != ":7001" || c.Seed != 4 || c.Token != "tok" {
		t.Fatalf("env: %v %+v", err, c)
	}
	zero := int64(0)
	c, err = Load(Flags{Config: path, Listen: ":7002", Seed: &zero}, getenv)
	if err != nil || c.Listen != ":7002" || c.Seed != 0 {
		t.Fatalf("flag: %v %+v", err, c)
	}
	c, err = Load(Flags{}, func(string) string { return "" })
	if err != nil || c.Listen != DefaultListen || c.MaxSessions != DefaultMaxSessions {
		t.Fatalf("default: %v %+v", err, c)
	}

	env["WWZ_TOOLS_SEED"] = "x"
	if _, err := Load(Flags{}, getenv); err == nil || !strings.Contains(err.Error(), "WWZ_TOOLS_SEED") {
		t.Fatalf("bad env seed: %v", err)
	}
	if _, err := Load(Flags{Config: filepath.Join(dir, "missing.yaml")}, getenv); err == nil {
		t.Fatal("missing file: want error")
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("nope: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Flags{Config: bad}, getenv); err == nil {
		t.Fatal("bad file: want error")
	}
	delete(env, "WWZ_TOOLS_SEED")
	if _, err := Load(Flags{Listen: ""}, func(k string) string {
		if k == "WWZ_TOOLS_LISTEN" {
			return ""
		}
		return env[k]
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WWZ_TOOLS_LISTEN", ":7003")
	if c, err := Load(Flags{}, nil); err != nil || c.Listen != ":7003" {
		t.Fatalf("os env: %v %+v", err, c)
	}
}

func TestLoadValidateFails(t *testing.T) {
	// Validation runs once, after file, env and flags are merged.
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte("listen: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Flags{Config: path}, func(string) string { return "" }); err == nil {
		t.Fatal("want error")
	}
	if c, err := Load(Flags{Config: path, Listen: ":1"}, func(string) string { return "" }); err != nil || c.Listen != ":1" {
		t.Fatalf("flag fixes the file: %v", err)
	}
}
