package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/metrics"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// Seams for tests: paths the engine and the registered drivers never take.
var (
	jsonMarshal  = json.Marshal
	newDriver    = driver.New
	engineHandle = func(e *engine.Engine, ctx context.Context, in engine.Input, w core.ResponseWriter) {
		e.Handle(ctx, in, w)
	}
)

// simulateFlags are the wwz simulate options.
type simulateFlags struct {
	config       string
	scenario     string
	scenariosDir string
	turns        int
	model        string
	driver       string
	seed         int64
	prompt       string
	tools        string
	json         bool
	set          map[string]bool
}

// Defaults of wwz simulate.
const (
	simDefaultTurns  = 6
	simDefaultDriver = "openai"
	simDefaultPrompt = "Please run the task."
	simDefaultSeed   = 1
	simDefaultDir    = "scenarios"
)

func parseSimulateFlags(args []string, stderr io.Writer) (*simulateFlags, error) {
	f := &simulateFlags{set: map[string]bool{}}
	fs := flag.NewFlagSet("wwz simulate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage: wwz simulate -scenario <name|file.yaml> [flags]\n\nRuns a scenario in memory (no server, store memory, no latency) and prints\neach response in the driver's wire format, feeding simulated tool results back.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	configPathFlag(fs, &f.config)
	fs.StringVar(&f.scenario, "scenario", "", "scenario name (from -scenarios-dir / the configuration) or path to a scenario YAML file")
	fs.StringVar(&f.scenariosDir, "scenarios-dir", "", "directory of scenario files (default: scenarios.dir, else ./"+simDefaultDir+")")
	fs.IntVar(&f.turns, "turns", simDefaultTurns, "number of requests (turns) to simulate")
	fs.StringVar(&f.model, "model", "", "model id (default: wwz-fast, else the first model the driver serves)")
	fs.StringVar(&f.driver, "driver", simDefaultDriver, "wire format: openai | anthropic | opencode | openrouter")
	fs.Int64Var(&f.seed, "seed", 0, "random seed (default: server.seed, else 1); same seed, same output")
	fs.StringVar(&f.prompt, "prompt", simDefaultPrompt, "first user message")
	fs.StringVar(&f.tools, "tools", "", "comma-separated extra tools to offer besides the ones the scenario names")
	fs.BoolVar(&f.json, "json", false, "print one JSON object per turn (turn, status, step, headers, body)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	f.config = resolveConfigPath(fs, f.config)
	if f.scenario == "" {
		return nil, errors.New("-scenario is required")
	}
	if f.turns < 1 {
		return nil, fmt.Errorf("-turns must be at least 1, got %d", f.turns)
	}
	return f, nil
}

// runSimulate executes wwz simulate. Exit 0 when every turn ran (HTTP errors
// injected by the scenario are part of the output), 1 when the setup fails,
// 2 on bad usage.
func runSimulate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f, err := parseSimulateFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz simulate:", err)
		return exitUsage
	}
	sim, err := newSimulation(f)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz simulate:", err)
		return exitError
	}
	if err := sim.run(ctx, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "wwz simulate:", err)
		return exitError
	}
	return exitOK
}

// simulation is one in-memory conversation against the engine.
type simulation struct {
	f     *simulateFlags
	eng   *engine.Engine
	drv   driver.Driver
	kind  driver.Kind
	path  string
	model string
	seed  int64
	scn   *scenario.Scenario
	tools []toolDef
	conv  string
}

// toolDef is a tool offered to the simulated model.
type toolDef struct {
	Name   string
	Schema map[string]any
}

func newSimulation(f *simulateFlags) (*simulation, error) {
	cfg, err := loadConfig(f.config)
	if err != nil {
		return nil, err
	}
	// In memory, no auth, no latency: simulate shows content, not timing.
	cfg.Server.Auth = "none"
	cfg.Store.Kind = "memory"
	t := true
	cfg.Server.RequestHeaders = &t

	reg := scenario.NewRegistry()
	scn, err := findScenario(reg, f, cfg)
	if err != nil {
		return nil, err
	}

	var opts map[string]string
	for _, m := range cfg.Drivers {
		if m.Name == f.driver {
			opts = m.Options
		}
	}
	drv, err := newDriver(f.driver, opts)
	if err != nil {
		return nil, err
	}
	kind := driver.KindChat
	if drv.Protocol() == "anthropic" {
		kind = driver.KindMessages
	}
	path := ""
	for _, rt := range drv.Routes() {
		if rt.Kind == kind && path == "" {
			path = rt.Path
		}
	}
	if path == "" {
		return nil, fmt.Errorf("driver %q has no %s route", f.driver, kind)
	}

	model := f.model
	if model == "" {
		model = defaultModel(cfg, drv.Protocol(), engine.Dialect(kind))
	}
	if m := cfg.FindModel(model); m == nil || !m.ServedOn(drv.Protocol(), engine.Dialect(kind)) {
		return nil, fmt.Errorf("model %q is not configured for driver %q", model, f.driver)
	}

	seed := cfg.Server.Seed
	if f.set["seed"] {
		seed = f.seed
	}
	if seed == 0 {
		seed = simDefaultSeed
	}
	cfg.Server.Seed = seed

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := engine.NewMetrics(metrics.New(cfg.Metrics.MaxSeries), cfg.Metrics.Buckets)
	eng := engine.New(cfg, limits.NewMemory(), reg, m, log)
	return &simulation{
		f: f, eng: eng, drv: drv, kind: kind, path: path, model: model, seed: seed, scn: scn,
		tools: scenarioTools(scn, splitList(f.tools)),
		conv:  "sim-" + scn.Name + "-" + strconv.FormatInt(seed, 10),
	}, nil
}

// findScenario loads the scenario named by -scenario into reg: a file path,
// else a name looked up in -scenarios-dir (or scenarios.dir/files, or
// ./scenarios).
func findScenario(reg *scenario.Registry, f *simulateFlags, cfg *config.Config) (*scenario.Scenario, error) {
	ref := f.scenario
	if strings.HasSuffix(ref, ".yaml") || strings.HasSuffix(ref, ".yml") || strings.ContainsRune(ref, os.PathSeparator) {
		data, err := os.ReadFile(ref) //nolint:gosec // G304: the operator names the scenario file
		if err != nil {
			return nil, err
		}
		s, err := scenario.ParseYAML(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		reg.Add(s)
		return s, nil
	}
	var files []string
	switch {
	case f.scenariosDir != "":
		files = []string{f.scenariosDir}
	case cfg.Scenarios.Dir != "" || len(cfg.Scenarios.Files) > 0:
		files, _ = scenarioFiles(nil, cfg)
	default:
		files = []string{simDefaultDir}
	}
	paths, err := scenarioFiles(files, nil)
	if err != nil {
		// The implicit ./scenarios may be absent (the built-ins remain);
		// a directory the operator named must exist.
		if !errors.Is(err, fs.ErrNotExist) || f.scenariosDir != "" || cfg.Scenarios.Dir != "" || len(cfg.Scenarios.Files) > 0 {
			return nil, err
		}
		paths = nil
	}
	// Later sources replace earlier ones: files/dir win over the built-ins.
	var found *scenario.Scenario
	for _, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // G304: files of the configured scenarios directory
		if err != nil {
			continue
		}
		s, err := scenario.ParseYAML(data)
		if err != nil {
			continue
		}
		if s.Name == ref {
			found = s
		}
	}
	if found == nil && cfg.Scenarios.UseBuiltin() {
		built, err := builtinScenarios()
		if err != nil {
			return nil, err
		}
		for _, s := range built {
			if s.Name == ref {
				found = s
			}
		}
	}
	if found != nil {
		reg.Add(found)
		return found, nil
	}
	return nil, fmt.Errorf("scenario %q not found in %s", ref, strings.Join(files, ", "))
}

// defaultModel is wwz-fast when the driver serves it in dialect, else the
// first model it does (ADR 0048).
func defaultModel(cfg *config.Config, driverName, dialect string) string {
	if m := cfg.FindModel("wwz-fast"); m != nil && m.ServedOn(driverName, dialect) {
		return m.ID
	}
	for i := range cfg.Models {
		if cfg.Models[i].ServedOn(driverName, dialect) {
			return cfg.Models[i].ID
		}
	}
	return ""
}

// scenarioTools lists the tools to offer: match.tools_include, every literal
// tool a step calls, the extra -tools and, for the generic names (any,
// any_read, any_write), a matching tool when -tools offers none.
func scenarioTools(s *scenario.Scenario, extra []string) []toolDef {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] && !strings.Contains(n, "{{") {
			seen[n] = true
			names = append(names, n)
		}
	}
	if s.Match != nil {
		for _, n := range s.Match.ToolsInclude {
			add(n)
		}
	}
	generic := map[string]bool{}
	for _, st := range s.Steps {
		for _, tc := range st.ToolCalls {
			switch tc.Name {
			case "any", "any_read", "any_write":
				generic[tc.Name] = true
			default:
				add(tc.Name)
			}
		}
		for _, ev := range st.Timeline {
			if ev.Type == "tool_call" {
				add(ev.Name)
			}
		}
	}
	for _, n := range extra {
		add(n)
	}
	// A generic call gets its own tool unless -tools offers one: reusing a
	// tool the scenario names (web_search for any_read) would make the
	// conversation state look like that step again.
	has := func(words ...string) bool {
		for _, n := range extra {
			for _, w := range words {
				if strings.Contains(strings.ToLower(n), w) {
					return true
				}
			}
		}
		return false
	}
	if generic["any_read"] && !has("read", "get", "list", "search", "fetch") {
		add("read_file")
	}
	if generic["any_write"] && !has("write", "create", "edit", "update", "delete", "put", "set") {
		add("write_file")
	}
	if generic["any"] && len(names) == 0 {
		add("read_file")
	}
	out := make([]toolDef, 0, len(names))
	for _, n := range names {
		out = append(out, toolDef{Name: n, Schema: toolSchema(n)})
	}
	return out
}

// toolSchema is a plausible JSON Schema for a tool, guessed from its name.
func toolSchema(name string) map[string]any {
	str := map[string]any{"type": "string"}
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "search"):
		return map[string]any{"type": "object", "properties": map[string]any{"query": str, "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}}, "required": []any{"query"}}
	case strings.Contains(n, "fetch"):
		return map[string]any{"type": "object", "properties": map[string]any{"url": str}, "required": []any{"url"}}
	case strings.Contains(n, "edit"):
		return map[string]any{"type": "object", "properties": map[string]any{"path": str, "old_text": str, "new_text": str}, "required": []any{"path", "old_text", "new_text"}}
	case strings.Contains(n, "write"), strings.Contains(n, "create"):
		return map[string]any{"type": "object", "properties": map[string]any{"path": str, "content": str}, "required": []any{"path", "content"}}
	default:
		return map[string]any{"type": "object", "properties": map[string]any{"path": str}, "required": []any{"path"}}
	}
}

// simCall is a tool call read back from a response.
type simCall struct {
	ID, Name, Args string
}

// simTurn is the --json output of one turn.
type simTurn struct {
	Turn    int               `json:"turn"`
	Status  int               `json:"status"`
	Step    string            `json:"step,omitempty"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// run plays the turns. Each response is printed, then the conversation grows:
// tool calls get a simulated success result, a text answer gets a new user
// message, and an error response is retried with the same history.
func (s *simulation) run(ctx context.Context, out io.Writer) error {
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "user", "content": s.f.prompt})
	for turn := 1; turn <= s.f.turns; turn++ {
		body, err := jsonMarshal(s.requestBody(msgs))
		if err != nil {
			return err
		}
		w := core.NewMemWriter()
		engineHandle(s.eng, ctx, engine.Input{Driver: s.drv, Kind: s.kind, Method: "POST", Path: s.path, Headers: s.headers(), Body: body, ClientIP: "127.0.0.1"}, w)
		if err := ctx.Err(); err != nil {
			return err
		}
		status, hdr, raw := w.StatusCode(), w.Headers(), w.Body()
		if err := s.print(out, turn, status, hdr, raw); err != nil {
			return err
		}
		if status != 200 {
			continue // retry: same history, next attempt
		}
		text, calls, err := s.parseResponse(raw)
		if err != nil {
			return fmt.Errorf("turn %d: %w", turn, err)
		}
		msgs = append(msgs, s.assistantMessage(text, calls)...)
		if len(calls) == 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": "Continue."})
		}
	}
	return nil
}

// headers are the request headers: credentials the drivers expect plus the
// x-wwz-* overrides that pin the scenario, the seed and zero latency.
func (s *simulation) headers() driver.MapHeaders {
	return driver.MapHeaders{
		"content-type":          "application/json",
		"authorization":         "Bearer wwz-simulate",
		"x-api-key":             "wwz-simulate",
		"anthropic-version":     "2023-06-01",
		"x-opencode-session":    s.conv,
		"x-wwz-conversation-id": s.conv,
		"x-wwz-scenario":        s.scn.Name,
		"x-wwz-seed":            strconv.FormatInt(s.seed, 10),
		"x-wwz-delay-ms":        "0",
	}
}

// requestBody builds the chat request in the driver's dialect.
func (s *simulation) requestBody(msgs []map[string]any) map[string]any {
	tools := make([]any, 0, len(s.tools))
	for _, t := range s.tools {
		desc := "Simulated tool " + t.Name
		if s.kind == driver.KindMessages {
			tools = append(tools, map[string]any{"name": t.Name, "description": desc, "input_schema": t.Schema})
		} else {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": desc, "parameters": t.Schema}})
		}
	}
	b := map[string]any{"model": s.model, "messages": msgs, "max_tokens": 1024}
	if len(tools) > 0 {
		b["tools"] = tools
	}
	return b
}

// assistantMessage turns a response into history: the assistant turn and,
// for tool calls, the simulated results.
func (s *simulation) assistantMessage(text string, calls []simCall) []map[string]any {
	result := func(c simCall) string {
		return fmt.Sprintf(`{"ok":true,"tool":%q,"result":"done"}`, c.Name)
	}
	if s.kind == driver.KindMessages {
		var content []any
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		var results []any
		for _, c := range calls {
			var input any = map[string]any{}
			_ = json.Unmarshal([]byte(c.Args), &input)
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input})
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": c.ID, "content": result(c)})
		}
		if len(content) == 0 {
			content = append(content, map[string]any{"type": "text", "text": "."})
		}
		out := []map[string]any{{"role": "assistant", "content": content}}
		if len(results) > 0 {
			out = append(out, map[string]any{"role": "user", "content": results})
		}
		return out
	}
	am := map[string]any{"role": "assistant", "content": text}
	if len(calls) > 0 {
		tcs := make([]any, 0, len(calls))
		for _, c := range calls {
			tcs = append(tcs, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Args}})
		}
		am["tool_calls"] = tcs
	}
	out := []map[string]any{am}
	for _, c := range calls {
		out = append(out, map[string]any{"role": "tool", "tool_call_id": c.ID, "content": result(c)})
	}
	return out
}

// parseResponse reads the text and tool calls of a non-streaming response.
func (s *simulation) parseResponse(raw []byte) (string, []simCall, error) {
	if s.kind == driver.KindMessages {
		var r struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", nil, fmt.Errorf("decode response: %w", err)
		}
		var text strings.Builder
		var calls []simCall
		for _, b := range r.Content {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				calls = append(calls, simCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
			}
		}
		return text.String(), calls, nil
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", nil, fmt.Errorf("decode response: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", nil, errors.New("response has no choices")
	}
	msg := r.Choices[0].Message
	text := ""
	if msg.Content != nil {
		text = *msg.Content
	}
	calls := make([]simCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		calls = append(calls, simCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	return text, calls, nil
}

// print writes one turn: a header line and the indented body, or a JSON line.
func (s *simulation) print(out io.Writer, turn, status int, hdr map[string]string, raw []byte) error {
	wwz := map[string]string{}
	for k, v := range hdr {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-wwz-") || lk == "retry-after" {
			wwz[lk] = v
		}
	}
	if s.f.json {
		body := json.RawMessage(raw)
		if !json.Valid(raw) {
			b, _ := json.Marshal(string(raw))
			body = b
		}
		b, err := jsonMarshal(simTurn{Turn: turn, Status: status, Step: wwz["x-wwz-step"], Headers: wwz, Body: body})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", b)
		return err
	}
	keys := make([]string, 0, len(wwz))
	for k := range wwz {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var line strings.Builder
	fmt.Fprintf(&line, "--- turn %d/%d status=%d", turn, s.f.turns, status)
	for _, k := range keys {
		fmt.Fprintf(&line, " %s=%s", strings.TrimPrefix(k, "x-wwz-"), wwz[k])
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(raw)
	}
	_, err := fmt.Fprintf(out, "%s\n%s\n", line.String(), bytes.TrimRight(pretty.Bytes(), "\n"))
	return err
}
