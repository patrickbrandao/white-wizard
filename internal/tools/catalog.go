package tools

import (
	"encoding/json"
	"slices"
	"strings"
)

// Tool names of the catalogue (spec 017).
const (
	ToolReadFile   = "read_file"
	ToolWriteFile  = "write_file"
	ToolEditFile   = "edit_file"
	ToolListDir    = "list_dir"
	ToolSearchCode = "search_code"
	ToolRunCommand = "run_command"
	ToolWebSearch  = "web_search"
	ToolFetchURL   = "fetch_url"
)

// Tool is one catalogue entry.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Profiles    []Profile       `json:"profiles"`
}

// catalog is in catalogue order (coder tools, then pesquisa-only tools).
var catalog = []Tool{
	{
		Name:        ToolReadFile,
		Description: "Read a text file from the workspace. Returns lines offset..offset+limit.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"offset":{"type":"integer","minimum":0,"description":"First line to return (0-based)."},"limit":{"type":"integer","minimum":1,"maximum":2000,"description":"Maximum number of lines."}},"required":["path"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder, ProfilePesquisa},
	},
	{
		Name:        ToolWriteFile,
		Description: "Create or overwrite a file in the workspace.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"content":{"type":"string","description":"Full file content."}},"required":["path","content"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder},
	},
	{
		Name:        ToolEditFile,
		Description: "Replace text in a file. Fails if old is not found.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"old":{"type":"string","description":"Exact text to replace."},"new":{"type":"string","description":"Replacement text."},"replace_all":{"type":"boolean","description":"Replace every occurrence instead of the first."}},"required":["path","old","new"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder},
	},
	{
		Name:        ToolListDir,
		Description: "List a directory. Directories end with '/'.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path (default '.')."},"recursive":{"type":"boolean","description":"List every descendant."}},"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder},
	},
	{
		Name:        ToolSearchCode,
		Description: "Search files for a literal substring. Returns file:line: text, at most 50 lines.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Literal substring (not a regex)."},"path":{"type":"string","description":"File or directory to search (default '.')."}},"required":["pattern"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder},
	},
	{
		Name:        ToolRunCommand,
		Description: "Run a shell command in the workspace. Returns JSON with exit_code, stdout and stderr.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Command line."},"timeout_ms":{"type":"integer","minimum":0,"description":"Timeout in milliseconds."}},"required":["command"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfileCoder},
	},
	{
		Name:        ToolWebSearch,
		Description: "Search the web. Returns JSON with results (title, url, snippet).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query."},"max_results":{"type":"integer","minimum":1,"maximum":10,"description":"Number of results (default 5)."}},"required":["query"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfilePesquisa},
	},
	{
		Name:        ToolFetchURL,
		Description: "Fetch a web page and return its text.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"http or https URL."},"max_bytes":{"type":"integer","minimum":1,"description":"Truncate the text to this many bytes."}},"required":["url"],"additionalProperties":false}`),
		Profiles:    []Profile{ProfilePesquisa},
	},
}

func toolIndex(name string) int {
	return slices.IndexFunc(catalog, func(t Tool) bool { return t.Name == name })
}

// inProfile reports whether tool t belongs to profile p.
func (t *Tool) inProfile(p Profile) bool {
	return p == ProfileAll || slices.Contains(t.Profiles, p)
}

// Tools returns the catalogue entries of a profile ("" = all), in
// lexicographic order of name (cmd/wwz-tools/openapi.yaml, GET /v1/tools).
// The returned slice and its schemas must not be modified.
func Tools(profile Profile) ([]Tool, error) {
	p, err := ParseProfile(string(profile))
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(catalog))
	for i := range catalog {
		if catalog[i].inProfile(p) {
			out = append(out, catalog[i])
		}
	}
	slices.SortFunc(out, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type openaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiTool struct {
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Catalog returns the tools of a profile ("" = all) as a JSON array in the
// requested format ("" = raw): openai `[{"type":"function","function":{…}}]`,
// anthropic `[{"name","description","input_schema"}]` or raw
// `[{"name","description","schema","profiles"}]`. A profile without tools
// gives `[]`.
func Catalog(profile Profile, format Format) (json.RawMessage, error) {
	f, err := ParseFormat(string(format))
	if err != nil {
		return nil, err
	}
	ts, err := Tools(profile)
	if err != nil {
		return nil, err
	}
	var v any
	switch f {
	case FormatOpenAI:
		out := make([]openaiTool, len(ts))
		for i, t := range ts {
			out[i] = openaiTool{Type: "function", Function: openaiFunction{t.Name, t.Description, t.Schema}}
		}
		v = out
	case FormatAnthropic:
		out := make([]anthropicTool, len(ts))
		for i, t := range ts {
			out[i] = anthropicTool{t.Name, t.Description, t.Schema}
		}
		v = out
	default:
		v = ts
	}
	b, _ := json.Marshal(v) // fixed, valid schemas: cannot fail
	return b, nil
}
