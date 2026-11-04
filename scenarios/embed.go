// Package scenarios embeds the built-in scenarios of the wwz binary (spec
// 011, ADR 0048). The YAML files in this directory are the source; the
// directory also works as scenarios.dir.
package scenarios

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed *.yaml
var files embed.FS

// File is one embedded scenario file.
type File struct {
	Name string // file name, e.g. "refusal.yaml"
	Data []byte
}

// Files lists the embedded scenario files sorted by name.
func Files() []File {
	entries, err := files.ReadDir(".")
	if err != nil {
		panic(fmt.Sprintf("scenarios: %v", err)) // embed.FS never fails on "."
	}
	var out []File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := files.ReadFile(e.Name())
		if err != nil {
			panic(fmt.Sprintf("scenarios: %v", err))
		}
		out = append(out, File{Name: e.Name(), Data: data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
