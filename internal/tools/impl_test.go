package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// run executes name with args through a forced success on session "t".
func run(t *testing.T, e *Executor, name, args string) Result {
	t.Helper()
	return mustCall(t, e, CallRequest{Session: "t", Name: name, ForceOutcome: OutcomeSuccess, Arguments: json.RawMessage(args)})
}

func wantOK(t *testing.T, r Result) string {
	t.Helper()
	if r.IsError || r.Outcome != OutcomeSuccess {
		t.Fatalf("want success, got %+v", r)
	}
	return r.Content
}

func wantNatural(t *testing.T, r Result, contains string) {
	t.Helper()
	if !r.IsError || r.Outcome != OutcomeNatural || !strings.Contains(r.Content, contains) {
		t.Fatalf("want natural error containing %q, got %+v", contains, r)
	}
}

func TestArgumentDecoding(t *testing.T) {
	e, _, _ := newExec(t, nil)
	// OpenAI sends arguments as a JSON string.
	wantOK(t, run(t, e, ToolReadFile, `"{\"path\":\"go.mod\"}"`))
	wantOK(t, run(t, e, ToolListDir, ``))
	wantOK(t, run(t, e, ToolListDir, `"  "`))
	wantNatural(t, run(t, e, ToolListDir, `"{bad`), "EINVAL")
	wantNatural(t, run(t, e, ToolListDir, `[1]`), "must be a JSON object")
	wantNatural(t, run(t, e, ToolListDir, `{"path":1}`), "EINVAL: invalid arguments for list_dir")
	wantNatural(t, run(t, e, ToolListDir, `{"extra":true}`), "unknown field")
	wantNatural(t, run(t, e, ToolListDir, `{} {}`), "trailing data")
	wantNatural(t, run(t, e, ToolListDir, `{"path":`), "EINVAL")
}

func TestReadFile(t *testing.T) {
	e, _, _ := newExec(t, nil)
	run(t, e, ToolWriteFile, `{"path":"f.txt","content":"l0\nl1\nl2\nl3\n"}`)
	if got := wantOK(t, run(t, e, ToolReadFile, `{"path":"/f.txt"}`)); got != "l0\nl1\nl2\nl3\n" {
		t.Fatalf("full: %q", got)
	}
	if got := wantOK(t, run(t, e, ToolReadFile, `{"path":"f.txt","offset":1,"limit":2}`)); got != "l1\nl2\n" {
		t.Fatalf("window: %q", got)
	}
	if got := wantOK(t, run(t, e, ToolReadFile, `{"path":"f.txt","offset":9}`)); got != "" {
		t.Fatalf("past end: %q", got)
	}
	run(t, e, ToolWriteFile, `{"path":"empty","content":""}`)
	if got := wantOK(t, run(t, e, ToolReadFile, `{"path":"empty"}`)); got != "" {
		t.Fatalf("empty: %q", got)
	}
	wantNatural(t, run(t, e, ToolReadFile, `{}`), "missing required field 'path'")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"f.txt","offset":-1}`), "'offset' must be >= 0")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"f.txt","limit":0}`), "'limit' must be between 1 and 2000")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"f.txt","limit":2001}`), "'limit'")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"f.txt","limit":1.5}`), "EINVAL")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"x"}`), "ENOENT: no such file or directory, open 'x'")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"../../etc/passwd"}`), "path outside sandbox")
	wantNatural(t, run(t, e, ToolReadFile, `{"path":"internal"}`), "EISDIR")
}

func TestWriteFile(t *testing.T) {
	e, _, _ := newExec(t, nil)
	if got := wantOK(t, run(t, e, ToolWriteFile, `{"path":"./a/../b/c.txt","content":"héllo"}`)); got != "ok: 6 bytes escritos em b/c.txt" {
		t.Fatalf("write: %q", got)
	}
	wantNatural(t, run(t, e, ToolWriteFile, `{"content":"x"}`), "'path'")
	wantNatural(t, run(t, e, ToolWriteFile, `{"path":"x"}`), "'content'")
	wantNatural(t, run(t, e, ToolWriteFile, `{"path":"/../x","content":"x"}`), "path outside sandbox")
	wantNatural(t, run(t, e, ToolWriteFile, `{"path":"x","content":"x","mode":1}`), "unknown field")
	big, _ := json.Marshal(map[string]string{"path": "big", "content": strings.Repeat("x", MaxFileBytes+1)})
	wantNatural(t, run(t, e, ToolWriteFile, string(big)), "no space left on device")
}

func TestEditFile(t *testing.T) {
	e, _, _ := newExec(t, nil)
	run(t, e, ToolWriteFile, `{"path":"e.txt","content":"aXbXc"}`)
	if got := wantOK(t, run(t, e, ToolEditFile, `{"path":"e.txt","old":"X","new":"-"}`)); got != "ok: 1 substituições em e.txt" {
		t.Fatalf("first: %q", got)
	}
	if got := wantOK(t, run(t, e, ToolReadFile, `{"path":"e.txt"}`)); got != "a-bXc" {
		t.Fatalf("content: %q", got)
	}
	run(t, e, ToolWriteFile, `{"path":"e.txt","content":"aXbXc"}`)
	if got := wantOK(t, run(t, e, ToolEditFile, `{"path":"e.txt","old":"X","new":"","replace_all":true}`)); got != "ok: 2 substituições em e.txt" {
		t.Fatalf("all: %q", got)
	}
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"e.txt","old":"Q","new":"z"}`), "old string not found in 'e.txt'")
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"e.txt","new":"z"}`), "'old'")
	wantNatural(t, run(t, e, ToolEditFile, `{"old":"a","new":"z"}`), "'path'")
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"e.txt","old":"a"}`), "'new'")
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"e.txt","old":"","new":"z"}`), "'old' must be non-empty")
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"nope","old":"a","new":"z"}`), "ENOENT")
	wantNatural(t, run(t, e, ToolEditFile, `{"path":1}`), "EINVAL")

	half := strings.Repeat("y", MaxFileBytes/2+1)
	run(t, e, ToolWriteFile, `{"path":"g.txt","content":"`+half+`"}`)
	wantNatural(t, run(t, e, ToolEditFile, `{"path":"g.txt","old":"`+half+`","new":"`+half+half+`"}`), "no space left on device")
}

func TestListDirAndSearch(t *testing.T) {
	e, _, _ := newExec(t, nil)
	root := wantOK(t, run(t, e, ToolListDir, `{}`))
	if !strings.Contains(root, "README.md\n") || !strings.Contains(root, "internal/") || strings.Contains(root, "internal/core") {
		t.Fatalf("root: %q", root)
	}
	rec := wantOK(t, run(t, e, ToolListDir, `{"path":".","recursive":true}`))
	if !strings.Contains(rec, "internal/") || !strings.Contains(rec, ".go") {
		t.Fatalf("recursive: %q", rec)
	}
	wantNatural(t, run(t, e, ToolListDir, `{"path":"README.md"}`), "ENOTDIR")
	wantNatural(t, run(t, e, ToolListDir, `{"path":"../"}`), "path outside sandbox")

	hits := wantOK(t, run(t, e, ToolSearchCode, `{"pattern":"func "}`))
	lines := strings.Split(hits, "\n")
	if len(lines) == 0 || len(lines) > maxSearchLines || !strings.Contains(lines[0], ".go:") {
		t.Fatalf("search: %q", hits)
	}
	if got := wantOK(t, run(t, e, ToolSearchCode, `{"pattern":"zzzz-not-there","path":"docs"}`)); got != "no matches" {
		t.Fatalf("none: %q", got)
	}
	wantNatural(t, run(t, e, ToolSearchCode, `{}`), "'pattern'")
	wantNatural(t, run(t, e, ToolSearchCode, `{"pattern":""}`), "non-empty")
	wantNatural(t, run(t, e, ToolSearchCode, `{"pattern":"x","path":"nope"}`), "ENOENT")
	wantNatural(t, run(t, e, ToolSearchCode, `{"pattern":1}`), "EINVAL")
}

func cmd(t *testing.T, e *Executor, command string) commandResult {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command})
	var r commandResult
	if err := json.Unmarshal([]byte(wantOK(t, run(t, e, ToolRunCommand, string(args)))), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunCommandShell(t *testing.T) {
	e, _, _ := newExec(t, nil)
	if r := cmd(t, e, "pwd"); r.Stdout != "/\n" || r.ExitCode != 0 {
		t.Fatalf("pwd: %+v", r)
	}
	if r := cmd(t, e, "ls -la"); !strings.Contains(r.Stdout, "README.md\n") {
		t.Fatalf("ls: %+v", r)
	}
	if r := cmd(t, e, "ls go.mod"); r.Stdout != "go.mod\n" {
		t.Fatalf("ls file: %+v", r)
	}
	if r := cmd(t, e, "ls nope"); r.ExitCode != 2 || !strings.Contains(r.Stderr, "cannot access 'nope'") {
		t.Fatalf("ls missing: %+v", r)
	}
	if r := cmd(t, e, "cat go.mod nope internal"); r.ExitCode != 1 || !strings.HasPrefix(r.Stdout, "module github.com/example/") ||
		!strings.Contains(r.Stderr, "cat: nope: No such file or directory") || !strings.Contains(r.Stderr, "cat: internal: Is a directory") {
		t.Fatalf("cat: %+v", r)
	}
	if r := cmd(t, e, "cat"); r.ExitCode != 0 || r.Stdout != "" {
		t.Fatalf("cat none: %+v", r)
	}
	if r := cmd(t, e, "grep -n module go.mod"); r.ExitCode != 0 || !strings.HasPrefix(r.Stdout, "go.mod:1: module") {
		t.Fatalf("grep: %+v", r)
	}
	if r := cmd(t, e, "grep zzzz-none"); r.ExitCode != 1 || r.Stdout != "" {
		t.Fatalf("grep none: %+v", r)
	}
	if r := cmd(t, e, "grep x nope"); r.ExitCode != 2 {
		t.Fatalf("grep missing: %+v", r)
	}
	if r := cmd(t, e, "grep"); r.ExitCode != 2 || !strings.Contains(r.Stderr, "usage") {
		t.Fatalf("grep usage: %+v", r)
	}
	if r := cmd(t, e, "rm -rf /"); r.ExitCode != 127 || r.Stderr != "rm: command not found\n" {
		t.Fatalf("unknown: %+v", r)
	}
	if r := cmd(t, e, "go build"); r.ExitCode != 127 {
		t.Fatalf("go build is not simulated: %+v", r)
	}
	wantNatural(t, run(t, e, ToolRunCommand, `{}`), "'command'")
	wantNatural(t, run(t, e, ToolRunCommand, `{"command":"   "}`), "non-empty")
	wantNatural(t, run(t, e, ToolRunCommand, `{"command":"pwd","timeout_ms":-1}`), "'timeout_ms'")
	wantNatural(t, run(t, e, ToolRunCommand, `{"command":1}`), "EINVAL")
	wantOK(t, run(t, e, ToolRunCommand, `{"command":"pwd","timeout_ms":1000}`))
	if got := lines(nil); got != "" {
		t.Fatalf("lines(nil) = %q", got)
	}
}

func TestRunCommandTests(t *testing.T) {
	for _, tc := range []struct {
		fail    float64
		exit    int
		command string
		marker  string
	}{
		{0, 0, "go test ./...", "ok  \t"},
		{100, 1, "go test ./...", "--- FAIL:"},
		{0, 0, "npm test", "passed"},
		{100, 1, "npm test", "1 failed"},
		{0, 0, "pytest -q", "passed in"},
		{100, 1, "pytest", "1 failed"},
	} {
		e, _, _ := newExec(t, func(c *Config) {
			rc := c.Tools[ToolRunCommand]
			rc.FailRate = &tc.fail
			c.Tools[ToolRunCommand] = rc
		})
		r := cmd(t, e, tc.command)
		if r.ExitCode != tc.exit || !strings.Contains(r.Stdout, tc.marker) {
			t.Errorf("%s fail=%v: %+v", tc.command, tc.fail, r)
		}
	}
	// Default fail_rate 10%: roughly one in ten runs fails.
	e, _, _ := newExec(t, nil)
	fails := 0
	for range 1000 {
		if cmd(t, e, "go test ./...").ExitCode != 0 {
			fails++
		}
	}
	if fails < 70 || fails > 130 {
		t.Fatalf("fail_rate 10%%: %d/1000", fails)
	}
}

func TestWebSearch(t *testing.T) {
	e, _, _ := newExec(t, nil)
	var out struct {
		Results []searchResult `json:"results"`
	}
	content := wantOK(t, run(t, e, ToolWebSearch, `{"query":"Go: context & cancel!"}`))
	if err := json.Unmarshal([]byte(content), &out); err != nil || len(out.Results) != 5 {
		t.Fatalf("default: %v %q", err, content)
	}
	for _, r := range out.Results {
		if !strings.HasPrefix(r.URL, "https://example.invalid/go-context-cancel/") || r.Title == "" || r.Snippet == "" {
			t.Fatalf("result: %+v", r)
		}
	}
	if again := wantOK(t, run(t, e, ToolWebSearch, `{"query":"Go: context & cancel!"}`)); again != content {
		t.Fatal("same (seed, query) must give the same results")
	}
	if other := wantOK(t, run(t, e, ToolWebSearch, `{"query":"other"}`)); other == content {
		t.Fatal("different query, same results")
	}
	content = wantOK(t, run(t, e, ToolWebSearch, `{"query":"x","max_results":10}`))
	_ = json.Unmarshal([]byte(content), &out)
	if len(out.Results) != 10 {
		t.Fatalf("max_results: %d", len(out.Results))
	}
	wantNatural(t, run(t, e, ToolWebSearch, `{}`), "'query'")
	wantNatural(t, run(t, e, ToolWebSearch, `{"query":" "}`), "non-empty")
	wantNatural(t, run(t, e, ToolWebSearch, `{"query":"x","max_results":11}`), "'max_results'")
	wantNatural(t, run(t, e, ToolWebSearch, `{"query":"x","max_results":0}`), "'max_results'")
	wantNatural(t, run(t, e, ToolWebSearch, `{"query":1}`), "EINVAL")

	for in, want := range map[string]string{
		"!!!":                    "q",
		"Hello   World":          "hello-world",
		strings.Repeat("ab", 40): strings.Repeat("ab", 20),
		"çã":                     "q",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q want %q", in, got, want)
		}
	}
}

func TestFetchURL(t *testing.T) {
	e, _, _ := newExec(t, nil)
	sizes := map[int]bool{}
	for i := range 50 {
		u := "https://example.com/page/" + strings.Repeat("x", i)
		text := wantOK(t, run(t, e, ToolFetchURL, `{"url":"`+u+`"}`))
		if len(text) < 200 || len(text) > 4000 || !utf8.ValidString(text) {
			t.Fatalf("size %d", len(text))
		}
		sizes[len(text)] = true
		if again := wantOK(t, run(t, e, ToolFetchURL, `{"url":"`+u+`"}`)); again != text {
			t.Fatal("not deterministic")
		}
	}
	if len(sizes) < 10 {
		t.Fatalf("sizes should vary: %v", sizes)
	}
	if got := wantOK(t, run(t, e, ToolFetchURL, `{"url":"http://a/b","max_bytes":10}`)); len(got) != 10 {
		t.Fatalf("max_bytes: %q", got)
	}
	if got := wantOK(t, run(t, e, ToolFetchURL, `{"url":"http://a","max_bytes":100000}`)); len(got) < 200 {
		t.Fatalf("large max_bytes: %d", len(got))
	}
	for _, bad := range []string{"ftp://x", "https://", "http:///x", "https://a b", "x"} {
		wantNatural(t, run(t, e, ToolFetchURL, `{"url":"`+bad+`"}`), "'url'")
	}
	wantNatural(t, run(t, e, ToolFetchURL, `{}`), "'url'")
	wantNatural(t, run(t, e, ToolFetchURL, `{"url":"http://a","max_bytes":0}`), "'max_bytes'")
	wantNatural(t, run(t, e, ToolFetchURL, `{"url":1}`), "EINVAL")
}
