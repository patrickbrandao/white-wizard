package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// call is one execution in progress; the session lock is held.
type call struct {
	s    *session
	tc   toolCfg
	args json.RawMessage
}

// natural builds a natural-error result.
func natural(format string, a ...any) (string, bool) {
	return fmt.Sprintf(format, a...), true
}

func ok(s string) (string, bool) { return s, false }

func invalidArgs(tool string, err error) (string, bool) {
	return natural("EINVAL: invalid arguments for %s: %v", tool, err)
}

func missingArg(tool, field string) (string, bool) {
	return natural("EINVAL: invalid arguments for %s: missing required field '%s'", tool, field)
}

func rangeArg(tool, field, want string) (string, bool) {
	return natural("EINVAL: invalid arguments for %s: '%s' must be %s", tool, field, want)
}

// decodeArgs accepts a JSON object or a JSON string holding one; empty = {}.
// Unknown fields and trailing data are rejected (additionalProperties: false).
func decodeArgs(raw json.RawMessage, dst any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		raw = bytes.TrimSpace([]byte(s))
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if raw[0] != '{' {
		return errors.New("arguments must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the arguments object")
	}
	return nil
}

func (c *call) run(name string) (string, bool) {
	switch name {
	case ToolReadFile:
		return c.readFile()
	case ToolWriteFile:
		return c.writeFile()
	case ToolEditFile:
		return c.editFile()
	case ToolListDir:
		return c.listDir()
	case ToolSearchCode:
		return c.searchCode()
	case ToolRunCommand:
		return c.runCommand()
	case ToolWebSearch:
		return c.webSearch()
	default: // ToolFetchURL
		return c.fetchURL()
	}
}

// invalidPayload returns content that breaks the promised format: truncated
// JSON (run_command, web_search), non-UTF-8 bytes or empty text, drawn
// evenly.
func (c *call) invalidPayload(name string) string {
	variants := 2
	jsonTool := name == ToolRunCommand || name == ToolWebSearch
	if jsonTool {
		variants = 3
	}
	switch c.s.rng.IntN(variants) {
	case 0:
		return "\xff\xfe\xfdinvalid\xc3"
	case 1:
		return ""
	default:
		content, _ := c.run(name)
		return content[:len(content)/2]
	}
}

func (c *call) readFile() (string, bool) {
	var a struct {
		Path   *string `json:"path"`
		Offset *int    `json:"offset"`
		Limit  *int    `json:"limit"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolReadFile, err)
	}
	if a.Path == nil {
		return missingArg(ToolReadFile, "path")
	}
	off, lim := 0, 2000
	if a.Offset != nil {
		if *a.Offset < 0 {
			return rangeArg(ToolReadFile, "offset", ">= 0")
		}
		off = *a.Offset
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > 2000 {
			return rangeArg(ToolReadFile, "limit", "between 1 and 2000")
		}
		lim = *a.Limit
	}
	data, msg := c.s.fs.read(*a.Path)
	if msg != "" {
		return natural("%s", msg)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if off >= len(lines) {
		return ok("")
	}
	return ok(strings.Join(lines[off:min(off+lim, len(lines))], ""))
}

func (c *call) writeFile() (string, bool) {
	var a struct {
		Path    *string `json:"path"`
		Content *string `json:"content"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolWriteFile, err)
	}
	if a.Path == nil {
		return missingArg(ToolWriteFile, "path")
	}
	if a.Content == nil {
		return missingArg(ToolWriteFile, "content")
	}
	if msg := c.s.fs.write(*a.Path, []byte(*a.Content)); msg != "" {
		return natural("%s", msg)
	}
	rp, _ := resolve(*a.Path)
	return ok(fmt.Sprintf("ok: %d bytes escritos em %s", len(*a.Content), display(rp)))
}

func (c *call) editFile() (string, bool) {
	var a struct {
		Path       *string `json:"path"`
		Old        *string `json:"old"`
		New        *string `json:"new"`
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolEditFile, err)
	}
	for _, req := range []struct {
		v    *string
		name string
	}{{a.Path, "path"}, {a.Old, "old"}, {a.New, "new"}} {
		if req.v == nil {
			return missingArg(ToolEditFile, req.name)
		}
	}
	if *a.Old == "" {
		return rangeArg(ToolEditFile, "old", "non-empty")
	}
	data, msg := c.s.fs.read(*a.Path)
	if msg != "" {
		return natural("%s", msg)
	}
	text := string(data)
	k := strings.Count(text, *a.Old)
	if k == 0 {
		return natural("old string not found in '%s'", *a.Path)
	}
	if a.ReplaceAll {
		text = strings.ReplaceAll(text, *a.Old, *a.New)
	} else {
		text, k = strings.Replace(text, *a.Old, *a.New, 1), 1
	}
	if msg := c.s.fs.write(*a.Path, []byte(text)); msg != "" {
		return natural("%s", msg)
	}
	rp, _ := resolve(*a.Path)
	return ok(fmt.Sprintf("ok: %d substituições em %s", k, display(rp)))
}

func (c *call) listDir() (string, bool) {
	var a struct {
		Path      *string `json:"path"`
		Recursive bool    `json:"recursive"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolListDir, err)
	}
	p := "."
	if a.Path != nil {
		p = *a.Path
	}
	entries, msg := c.s.fs.list(p, a.Recursive)
	if msg != "" {
		return natural("%s", msg)
	}
	return ok(strings.Join(entries, "\n"))
}

const maxSearchLines = 50

func (c *call) searchCode() (string, bool) {
	var a struct {
		Pattern *string `json:"pattern"`
		Path    *string `json:"path"`
	}
	if err := decodeArgs(c.args, &a); err != nil {
		return invalidArgs(ToolSearchCode, err)
	}
	if a.Pattern == nil {
		return missingArg(ToolSearchCode, "pattern")
	}
	if *a.Pattern == "" {
		return rangeArg(ToolSearchCode, "pattern", "non-empty")
	}
	p := "."
	if a.Path != nil {
		p = *a.Path
	}
	lines, msg := c.s.fs.search(*a.Pattern, p, maxSearchLines)
	if msg != "" {
		return natural("%s", msg)
	}
	if len(lines) == 0 {
		return ok("no matches")
	}
	return ok(strings.Join(lines, "\n"))
}
