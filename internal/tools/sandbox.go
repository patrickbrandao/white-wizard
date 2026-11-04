package tools

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
)

// Sandbox limits (spec 017).
const (
	MaxFileBytes    = 64 << 10
	MaxSessionBytes = 4 << 20
	MaxFiles        = 1000
)

// vfs is the virtual file system of a session. Keys are cleaned paths
// relative to the root, without a leading slash ("internal/x/x.go"); the
// root is "". Directories are implicit: a directory exists while some file
// lives under it.
type vfs struct {
	files map[string][]byte
	size  int
}

// resolve normalises p against the root "/". Absolute paths are rooted at
// the sandbox root; ".." above the root, backslashes, NUL bytes and "~"
// leave the sandbox (ok = false).
func resolve(p string) (string, bool) {
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "~") {
		return "", false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	return strings.Join(stack, "/"), true
}

func display(rp string) string {
	if rp == "" {
		return "."
	}
	return rp
}

func errOutside(op, p string) string {
	return fmt.Sprintf("EACCES: path outside sandbox, %s '%s'", op, p)
}

func errNoent(op, p string) string {
	return fmt.Sprintf("ENOENT: no such file or directory, %s '%s'", op, p)
}

func errIsDir(op, p string) string {
	return fmt.Sprintf("EISDIR: illegal operation on a directory, %s '%s'", op, p)
}

func errNotDir(op, p string) string {
	return fmt.Sprintf("ENOTDIR: not a directory, %s '%s'", op, p)
}

func errNoSpace(op, p string) string {
	return fmt.Sprintf("ENOSPC: no space left on device, %s '%s'", op, p)
}

func (fs *vfs) isDir(rp string) bool {
	if rp == "" {
		return true
	}
	prefix := rp + "/"
	for k := range fs.files {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// read returns the file at p, or a natural error message.
func (fs *vfs) read(p string) ([]byte, string) {
	rp, ok := resolve(p)
	if !ok {
		return nil, errOutside("open", p)
	}
	if data, ok := fs.files[rp]; ok {
		return data, ""
	}
	if fs.isDir(rp) {
		return nil, errIsDir("read", p)
	}
	return nil, errNoent("open", p)
}

// write stores data at p, enforcing the sandbox limits.
func (fs *vfs) write(p string, data []byte) string {
	rp, ok := resolve(p)
	if !ok {
		return errOutside("open", p)
	}
	if fs.isDir(rp) {
		return errIsDir("open", p)
	}
	for i := range len(rp) {
		if rp[i] == '/' {
			if _, isFile := fs.files[rp[:i]]; isFile {
				return errNotDir("open", p)
			}
		}
	}
	old, exists := fs.files[rp]
	if len(data) > MaxFileBytes || fs.size-len(old)+len(data) > MaxSessionBytes || (!exists && len(fs.files) >= MaxFiles) {
		return errNoSpace("write", p)
	}
	fs.files[rp] = append([]byte(nil), data...)
	fs.size += len(data) - len(old)
	return ""
}

// under returns the sorted files below directory rp ("" = all).
func (fs *vfs) under(rp string) []string {
	prefix := ""
	if rp != "" {
		prefix = rp + "/"
	}
	var out []string
	for k := range fs.files {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// list returns the entries of directory p, relative to it, directories with
// a trailing "/", in lexicographic order.
func (fs *vfs) list(p string, recursive bool) ([]string, string) {
	rp, ok := resolve(p)
	if !ok {
		return nil, errOutside("scandir", p)
	}
	if _, isFile := fs.files[rp]; isFile {
		return nil, errNotDir("scandir", p)
	}
	if !fs.isDir(rp) {
		return nil, errNoent("scandir", p)
	}
	prefix := ""
	if rp != "" {
		prefix = rp + "/"
	}
	set := map[string]bool{}
	for _, k := range fs.under(rp) {
		rel := strings.TrimPrefix(k, prefix)
		for i := range len(rel) {
			if rel[i] == '/' {
				set[rel[:i+1]] = true
				if !recursive {
					break
				}
			}
		}
		if recursive || !strings.Contains(rel, "/") {
			set[rel] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, ""
}

// search finds pattern in the file or directory p: "file:line: text", at
// most limit lines.
func (fs *vfs) search(pattern, p string, limit int) ([]string, string) {
	rp, ok := resolve(p)
	if !ok {
		return nil, errOutside("scandir", p)
	}
	var files []string
	_, isFile := fs.files[rp]
	switch {
	case isFile:
		files = []string{rp}
	case fs.isDir(rp):
		files = fs.under(rp)
	default:
		return nil, errNoent("scandir", p)
	}
	var out []string
	for _, f := range files {
		for i, line := range strings.Split(string(fs.files[f]), "\n") {
			if !strings.Contains(line, pattern) {
				continue
			}
			line = strings.TrimSpace(line)
			if len(line) > 200 {
				line = line[:200]
			}
			out = append(out, fmt.Sprintf("%s:%d: %s", f, i+1, line))
			if len(out) == limit {
				return out, ""
			}
		}
	}
	return out, ""
}

var packages = []string{"api", "auth", "cache", "config", "core", "db", "httpx", "jobs", "logx", "metrics", "queue", "store", "util", "worker"}

var projects = []string{"atlas", "beacon", "comet", "delta", "ember", "falcon", "garnet", "harbor"}

// seedTree builds the fixed project tree of a session: 12–30 files
// (README.md, go.mod, Makefile, cmd/…, internal/…, docs/…), deterministic by
// (seed, profile).
func seedTree(seed int64, profile Profile) *vfs {
	r := newRNG(seed, "tree", string(profile))
	proj := projects[r.IntN(len(projects))]
	fs := &vfs{files: map[string][]byte{}}
	put := func(p, content string) {
		fs.files[p] = []byte(content)
		fs.size += len(content)
	}
	put("README.md", "# "+capitalize(proj)+"\n\n"+paragraph(r)+"\n\n## Usage\n\n```\ngo run ./cmd/"+proj+"\n```\n\n"+paragraph(r)+"\n")
	put("go.mod", "module github.com/example/"+proj+"\n\ngo 1.22\n")
	put("Makefile", ".PHONY: build test\n\nbuild:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n")
	put("cmd/"+proj+"/main.go", goMain(proj))
	put("docs/architecture.md", "# Architecture\n\n"+paragraph(r)+"\n\n"+paragraph(r)+"\n")

	pkgs := append([]string(nil), packages...)
	r.Shuffle(len(pkgs), func(i, j int) { pkgs[i], pkgs[j] = pkgs[j], pkgs[i] })
	var candidates []string
	docs := []string{"docs/usage.md", "docs/faq.md", "docs/changelog.md"}
	for i, pkg := range pkgs {
		candidates = append(candidates, "internal/"+pkg+"/"+pkg+".go", "internal/"+pkg+"/"+pkg+"_test.go")
		if i%4 == 1 && len(docs) > 0 {
			candidates = append(candidates, docs[0])
			docs = docs[1:]
		}
	}
	total := 12 + r.IntN(19)
	for _, p := range candidates[:total-len(fs.files)] {
		switch {
		case strings.HasSuffix(p, "_test.go"):
			put(p, goTest(r, p))
		case strings.HasSuffix(p, ".go"):
			put(p, goFile(r, p))
		default:
			title := capitalize(strings.TrimSuffix(p[len("docs/"):], ".md"))
			put(p, "# "+title+"\n\n"+paragraph(r)+"\n\n"+paragraph(r)+"\n")
		}
	}
	return fs
}

func pkgOf(p string) string {
	parts := strings.Split(p, "/")
	return parts[1]
}

func goMain(proj string) string {
	return "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {\n\tif err := run(); err != nil {\n\t\tfmt.Fprintln(os.Stderr, err)\n\t\tos.Exit(1)\n\t}\n}\n\nfunc run() error {\n\tfmt.Println(\"" + proj + " started\")\n\treturn nil\n}\n"
}

func goFile(r *rand.Rand, p string) string {
	pkg := pkgOf(p)
	var b strings.Builder
	fmt.Fprintf(&b, "// Package %s %s\npackage %s\n\nimport \"context\"\n", pkg, words(r, 8), pkg)
	for range 2 + r.IntN(4) {
		name := capitalize(lorem[r.IntN(len(lorem))]) + capitalize(lorem[r.IntN(len(lorem))])
		fmt.Fprintf(&b, "\n// %s %s.\nfunc %s(ctx context.Context) error {\n\t// TODO: %s\n\treturn ctx.Err()\n}\n", name, words(r, 6), name, words(r, 5))
	}
	return b.String()
}

func goTest(r *rand.Rand, p string) string {
	pkg := pkgOf(p)
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nimport \"testing\"\n", pkg)
	for range 1 + r.IntN(3) {
		name := "Test" + capitalize(lorem[r.IntN(len(lorem))])
		fmt.Fprintf(&b, "\nfunc %s(t *testing.T) {\n\tt.Log(%q)\n}\n", name, words(r, 4))
	}
	return b.String()
}
