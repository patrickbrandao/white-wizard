package tools

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

type commandResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (r commandResult) json() string {
	b, _ := json.Marshal(r) // plain struct of strings and an int: cannot fail
	return string(b)
}

// runCommand simulates a shell: test runners give plausible output (failing
// with probability fail_rate), ls/cat/grep/pwd answer from the virtual file
// system and anything else is "command not found". Nothing is executed.
func (c *call) runCommand() (string, bool) {
	var a struct {
		Command   *string `json:"command"`
		TimeoutMS *int    `json:"timeout_ms"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolRunCommand, err)
	}
	if a.Command == nil {
		return missingArg(ToolRunCommand, "command")
	}
	if a.TimeoutMS != nil && *a.TimeoutMS < 0 {
		return rangeArg(ToolRunCommand, "timeout_ms", ">= 0")
	}
	f := strings.Fields(*a.Command)
	if len(f) == 0 {
		return rangeArg(ToolRunCommand, "command", "non-empty")
	}
	var r commandResult
	switch {
	case len(f) >= 2 && f[0] == "go" && f[1] == "test",
		len(f) >= 2 && f[0] == "npm" && f[1] == "test",
		f[0] == "pytest":
		r = c.testRun(f[0])
	case f[0] == "pwd":
		r.Stdout = "/\n"
	case f[0] == "ls":
		r = c.shLs(operands(f[1:]))
	case f[0] == "cat":
		r = c.shCat(operands(f[1:]))
	case f[0] == "grep":
		r = c.shGrep(operands(f[1:]))
	default:
		r = commandResult{ExitCode: 127, Stderr: f[0] + ": command not found\n"}
	}
	return ok(r.json())
}

// operands drops flags (-x, --x).
func operands(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

func (c *call) shLs(args []string) commandResult {
	p := "."
	if len(args) > 0 {
		p = args[0]
	}
	entries, msg := c.s.fs.list(p, false)
	if msg != "" {
		if strings.HasPrefix(msg, "ENOTDIR") {
			return commandResult{Stdout: p + "\n"}
		}
		return commandResult{ExitCode: 2, Stderr: fmt.Sprintf("ls: cannot access '%s': No such file or directory\n", p)}
	}
	return commandResult{Stdout: lines(entries)}
}

func (c *call) shCat(args []string) commandResult {
	var r commandResult
	var out strings.Builder
	for _, p := range args {
		data, msg := c.s.fs.read(p)
		switch {
		case msg == "":
			out.Write(data)
		case strings.HasPrefix(msg, "EISDIR"):
			r.ExitCode = 1
			r.Stderr += fmt.Sprintf("cat: %s: Is a directory\n", p)
		default:
			r.ExitCode = 1
			r.Stderr += fmt.Sprintf("cat: %s: No such file or directory\n", p)
		}
	}
	r.Stdout = out.String()
	return r
}

func (c *call) shGrep(args []string) commandResult {
	if len(args) == 0 {
		return commandResult{ExitCode: 2, Stderr: "usage: grep PATTERN [PATH]\n"}
	}
	p := "."
	if len(args) > 1 {
		p = args[1]
	}
	found, msg := c.s.fs.search(args[0], p, maxSearchLines)
	switch {
	case msg != "":
		return commandResult{ExitCode: 2, Stderr: fmt.Sprintf("grep: %s: No such file or directory\n", p)}
	case len(found) == 0:
		return commandResult{ExitCode: 1}
	}
	return commandResult{Stdout: lines(found)}
}

func lines(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return strings.Join(ss, "\n") + "\n"
}

// testRun produces plausible test-runner output; it fails with probability
// fail_rate (percent).
func (c *call) testRun(runner string) commandResult {
	r := c.s.rng
	fail := r.Float64()*100 < c.tc.failRate
	pkgs, _ := c.s.fs.list("internal", false)
	module := strings.TrimSpace(strings.TrimPrefix(firstLine(c.s.fs.files["go.mod"]), "module "))
	n := 3 + r.IntN(20)
	secs := func() string { return fmt.Sprintf("%.3fs", 0.01+r.Float64()*2) }
	var b strings.Builder
	switch runner {
	case "go":
		for i, p := range pkgs {
			pkg := module + "/internal/" + strings.TrimSuffix(p, "/")
			if fail && i == len(pkgs)-1 {
				name := "Test" + capitalize(lorem[r.IntN(len(lorem))])
				fmt.Fprintf(&b, "--- FAIL: %s (0.00s)\n    %s_test.go:%d: expected %d, got %d\nFAIL\nFAIL\t%s\t%s\n", name, strings.TrimSuffix(p, "/"), 10+r.IntN(90), r.IntN(10), 10+r.IntN(10), pkg, secs())
				b.WriteString("FAIL\n")
				continue
			}
			fmt.Fprintf(&b, "ok  \t%s\t%s\n", pkg, secs())
		}
	case "npm":
		fmt.Fprintf(&b, "\n> %s@1.0.0 test\n> jest\n\n", lastPart(module))
		if fail {
			fmt.Fprintf(&b, "FAIL src/%s.test.js\n  ● %s\n\nTests:       1 failed, %d passed, %d total\n", lorem[r.IntN(len(lorem))], sentence(r, 4), n-1, n)
		} else {
			fmt.Fprintf(&b, "PASS src/%s.test.js\n\nTests:       %d passed, %d total\n", lorem[r.IntN(len(lorem))], n, n)
		}
		fmt.Fprintf(&b, "Time:        %s\n", secs())
	default: // pytest
		b.WriteString("============================= test session starts ==============================\n")
		fmt.Fprintf(&b, "collected %d items\n\n", n)
		if fail {
			fmt.Fprintf(&b, "FAILED tests/test_%s.py::test_%s - AssertionError\n", lorem[r.IntN(len(lorem))], lorem[r.IntN(len(lorem))])
			fmt.Fprintf(&b, "========================= 1 failed, %d passed in %s =========================\n", n-1, secs())
		} else {
			fmt.Fprintf(&b, "============================== %d passed in %s ==============================\n", n, secs())
		}
	}
	res := commandResult{Stdout: b.String()}
	if fail {
		res.ExitCode = 1
	}
	return res
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return s
}

func lastPart(s string) string {
	return s[strings.LastIndex(s, "/")+1:]
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// webSearch returns results deterministic by (seed, query); no network.
func (c *call) webSearch() (string, bool) {
	var a struct {
		Query      *string `json:"query"`
		MaxResults *int    `json:"max_results"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolWebSearch, err)
	}
	if a.Query == nil {
		return missingArg(ToolWebSearch, "query")
	}
	if strings.TrimSpace(*a.Query) == "" {
		return rangeArg(ToolWebSearch, "query", "non-empty")
	}
	n := 5
	if a.MaxResults != nil {
		if *a.MaxResults < 1 || *a.MaxResults > 10 {
			return rangeArg(ToolWebSearch, "max_results", "between 1 and 10")
		}
		n = *a.MaxResults
	}
	r := newRNG(c.s.seed, ToolWebSearch, *a.Query)
	slug := slugify(*a.Query)
	out := struct {
		Results []searchResult `json:"results"`
	}{Results: make([]searchResult, n)}
	for i := range out.Results {
		out.Results[i] = searchResult{
			Title:   capitalize(words(r, 3+r.IntN(5))),
			URL:     fmt.Sprintf("https://example.invalid/%s/%d-%s", slug, i+1, strings.ReplaceAll(words(r, 2), " ", "-")),
			Snippet: sentence(r, 15+r.IntN(15)),
		}
	}
	b, _ := json.Marshal(out) // strings only: cannot fail
	return ok(string(b))
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, ch := range strings.ToLower(s) {
		switch {
		case ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "q"
	}
	return out
}

// fetchURL returns 200–4000 bytes of text deterministic by (seed, url); no
// network.
func (c *call) fetchURL() (string, bool) {
	var a struct {
		URL      *string `json:"url"`
		MaxBytes *int    `json:"max_bytes"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolFetchURL, err)
	}
	if a.URL == nil {
		return missingArg(ToolFetchURL, "url")
	}
	if !validURL(*a.URL) {
		return rangeArg(ToolFetchURL, "url", "an http or https URL")
	}
	if a.MaxBytes != nil && *a.MaxBytes < 1 {
		return rangeArg(ToolFetchURL, "max_bytes", ">= 1")
	}
	r := newRNG(c.s.seed, ToolFetchURL, *a.URL)
	size := 200 + r.IntN(3801)
	text := page(r, size)
	if a.MaxBytes != nil && *a.MaxBytes < len(text) {
		text = text[:*a.MaxBytes]
	}
	return ok(text)
}

func validURL(u string) bool {
	rest, found := strings.CutPrefix(u, "https://")
	if !found {
		if rest, found = strings.CutPrefix(u, "http://"); !found {
			return false
		}
	}
	host, _, _ := strings.Cut(rest, "/")
	return host != "" && !strings.ContainsAny(u, " \t\r\n")
}

// page is lorem text of exactly size bytes (ASCII).
func page(r *rand.Rand, size int) string {
	var b strings.Builder
	b.WriteString(capitalize(words(r, 4)) + "\n\n")
	for b.Len() < size {
		b.WriteString(paragraph(r) + "\n\n")
	}
	return b.String()[:size]
}
