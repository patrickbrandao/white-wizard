package tools

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{".", "", true},
		{"/", "", true},
		{"README.md", "README.md", true},
		{"./a//b/./c", "a/b/c", true},
		{"/internal/x.go", "internal/x.go", true},
		{"a/../b", "b", true},
		{"a/..", "", true},
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"/../etc/passwd", "", false},
		{"a/../../b", "", false},
		{"/a/b/../../..", "", false},
		{"..\\..\\x", "", false},
		{"a\x00b", "", false},
		{"~/.ssh/id_rsa", "", false},
	}
	for _, tc := range cases {
		got, ok := resolve(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("resolve(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSeedTreeDeterministic(t *testing.T) {
	seen := map[int]bool{}
	for seed := int64(1); seed <= 200; seed++ {
		a := seedTree(seed, ProfileCoder)
		b := seedTree(seed, ProfileCoder)
		if !maps.EqualFunc(a.files, b.files, func(x, y []byte) bool { return string(x) == string(y) }) || a.size != b.size {
			t.Fatalf("seed %d: not deterministic", seed)
		}
		n := len(a.files)
		if n < 12 || n > 30 {
			t.Fatalf("seed %d: %d files", seed, n)
		}
		seen[n] = true
		size := 0
		for _, k := range []string{"README.md", "go.mod", "Makefile", "docs/architecture.md"} {
			if _, ok := a.files[k]; !ok {
				t.Fatalf("seed %d: missing %s", seed, k)
			}
		}
		hasCmd, hasInternal := false, false
		for k, v := range a.files {
			size += len(v)
			hasCmd = hasCmd || strings.HasPrefix(k, "cmd/")
			hasInternal = hasInternal || strings.HasPrefix(k, "internal/")
		}
		if !hasCmd || !hasInternal || size != a.size {
			t.Fatalf("seed %d: cmd %v internal %v size %d/%d", seed, hasCmd, hasInternal, size, a.size)
		}
	}
	if !seen[12] || !seen[30] {
		t.Errorf("file counts should span 12..30, saw %v", slices.Sorted(maps.Keys(seen)))
	}
	if equalTrees(seedTree(1, ProfileCoder), seedTree(2, ProfileCoder)) {
		t.Error("different seeds should give different trees")
	}
	if equalTrees(seedTree(1, ProfileCoder), seedTree(1, ProfilePesquisa)) {
		t.Error("profile is part of the tree seed")
	}
}

func equalTrees(a, b *vfs) bool {
	return maps.EqualFunc(a.files, b.files, func(x, y []byte) bool { return string(x) == string(y) })
}

func newFS(files map[string]string) *vfs {
	fs := &vfs{files: map[string][]byte{}}
	for k, v := range files {
		fs.files[k] = []byte(v)
		fs.size += len(v)
	}
	return fs
}

func TestVFSWriteLimits(t *testing.T) {
	fs := newFS(map[string]string{"a/b.txt": "x", "f": "y"})
	if msg := fs.write("../x", nil); !strings.Contains(msg, "path outside sandbox") {
		t.Errorf("outside: %q", msg)
	}
	if msg := fs.write("a", nil); !strings.HasPrefix(msg, "EISDIR") {
		t.Errorf("dir: %q", msg)
	}
	if msg := fs.write("/", nil); !strings.HasPrefix(msg, "EISDIR") {
		t.Errorf("root: %q", msg)
	}
	if msg := fs.write("f/g", nil); !strings.HasPrefix(msg, "ENOTDIR") {
		t.Errorf("parent is file: %q", msg)
	}
	if msg := fs.write("big", make([]byte, MaxFileBytes+1)); !strings.HasPrefix(msg, "ENOSPC") {
		t.Errorf("file limit: %q", msg)
	}
	if msg := fs.write("ok", make([]byte, MaxFileBytes)); msg != "" {
		t.Errorf("at file limit: %q", msg)
	}
	if fs.size != 2+MaxFileBytes {
		t.Errorf("size %d", fs.size)
	}
	if msg := fs.write("ok", []byte("z")); msg != "" || fs.size != 3 {
		t.Errorf("overwrite: %q size %d", msg, fs.size)
	}
	if msg := fs.write("empty", []byte{}); msg != "" {
		t.Errorf("empty: %q", msg)
	}

	full := newFS(nil)
	for i := range MaxSessionBytes / MaxFileBytes {
		if msg := full.write("d/"+strconv.Itoa(i), make([]byte, MaxFileBytes)); msg != "" {
			t.Fatalf("fill %d: %q", i, msg)
		}
	}
	if msg := full.write("one-more", []byte("x")); !strings.HasPrefix(msg, "ENOSPC") {
		t.Errorf("session bytes: %q", msg)
	}

	many := newFS(nil)
	for i := range MaxFiles {
		many.files["f"+strconv.Itoa(i)] = nil
	}
	if msg := many.write("new", []byte("x")); !strings.HasPrefix(msg, "ENOSPC") {
		t.Errorf("file count: %q", msg)
	}
	if msg := many.write("f0", []byte("x")); msg != "" {
		t.Errorf("overwrite at file count: %q", msg)
	}
}

func TestVFSListAndSearch(t *testing.T) {
	fs := newFS(map[string]string{
		"README.md":       "hello\nworld",
		"a/b/c.go":        "package c\n  func Hello() {}\n",
		"a/d.go":          "x " + strings.Repeat("y", 300) + " hello",
		"z/empty.txt":     "",
		"a/b/e/f/deep.md": "hello deep",
	})
	got, msg := fs.list(".", false)
	if msg != "" || strings.Join(got, ",") != "README.md,a/,z/" {
		t.Errorf("list root: %v %q", got, msg)
	}
	got, _ = fs.list("a", true)
	if strings.Join(got, ",") != "b/,b/c.go,b/e/,b/e/f/,b/e/f/deep.md,d.go" {
		t.Errorf("list recursive: %v", got)
	}
	got, _ = fs.list("/a/b", false)
	if strings.Join(got, ",") != "c.go,e/" {
		t.Errorf("list a/b: %v", got)
	}
	for p, prefix := range map[string]string{"README.md": "ENOTDIR", "nope": "ENOENT", "../..": "EACCES"} {
		if _, msg := fs.list(p, false); !strings.HasPrefix(msg, prefix) {
			t.Errorf("list %s: %q", p, msg)
		}
	}

	hits, msg := fs.search("ello", ".", 50)
	if msg != "" || len(hits) != 4 {
		t.Fatalf("search: %v %q", hits, msg)
	}
	if hits[0] != "README.md:1: hello" || hits[1] != "a/b/c.go:2: func Hello() {}" {
		t.Errorf("search lines: %v", hits)
	}
	for _, h := range hits {
		if strings.HasPrefix(h, "a/d.go:1: ") && len(h) != len("a/d.go:1: ")+200 {
			t.Errorf("long line not truncated: %d", len(h))
		}
	}
	if hits, _ := fs.search("ello", ".", 2); len(hits) != 2 {
		t.Errorf("limit: %v", hits)
	}
	if hits, msg := fs.search("hello", "README.md", 50); msg != "" || len(hits) != 1 {
		t.Errorf("search file: %v %q", hits, msg)
	}
	if hits, msg := fs.search("x", "z/empty.txt", 50); msg != "" || len(hits) != 0 {
		t.Errorf("search empty file: %v %q", hits, msg)
	}
	for p, prefix := range map[string]string{"nope": "ENOENT", "/../x": "EACCES"} {
		if _, msg := fs.search("x", p, 50); !strings.HasPrefix(msg, prefix) {
			t.Errorf("search %s: %q", p, msg)
		}
	}

	if _, msg := fs.read("a"); !strings.HasPrefix(msg, "EISDIR") {
		t.Errorf("read dir: %q", msg)
	}
	if _, msg := fs.read("nope"); msg != "ENOENT: no such file or directory, open 'nope'" {
		t.Errorf("read missing: %q", msg)
	}
	if _, msg := fs.read("../../etc/passwd"); msg != "EACCES: path outside sandbox, open '../../etc/passwd'" {
		t.Errorf("read outside: %q", msg)
	}
	if display("") != "." || display("a") != "a" {
		t.Error("display")
	}
}
