package drivertest_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// extraMutations breaks what the observation-based catalog cannot: things
// the driver should not write (headers, members), whole bodies, the outcome
// of ParseRequest, every StreamWriter method, extra frames and perturbed
// request fields.
func extraMutations(name string, observed []string) []*mutation {
	var out []*mutation
	for _, h := range []string{
		"retry-after", "x-request-id", "request-id", "x-ratelimit-limit-requests",
		"anthropic-ratelimit-requests-limit", "x-ratelimit-reset-requests", "x-gemini-service-tier",
		"x-wwz-unknown",
	} {
		for _, v := range []string{"0", "wwz-other"} {
			out = append(out, &mutation{
				name:       fmt.Sprintf("add header %s %s", h, v),
				addHeaders: [][2]string{{h, v}},
			})
		}
	}
	for _, h := range observed {
		for _, v := range []string{"wwz-other", "0", "text/html"} {
			out = append(out, &mutation{
				name: fmt.Sprintf("header %s becomes %q", h, v),
				header: func(k, old string) (string, bool) {
					if strings.EqualFold(k, h) {
						return v, false
					}
					return old, false
				},
			})
		}
	}
	for _, k := range []string{"annotations", "obfuscation", "metadata", "request_id", "reasoning", "reasoning_content", "signature", "refusal", "error", "citations", "usage"} {
		for _, v := range []string{`"wwz-other"`, `{}`} {
			out = append(out,
				&mutation{name: fmt.Sprintf("add member %s=%s", k, v), json: addKey(k, v, true)},
				&mutation{name: fmt.Sprintf("add root member %s=%s", k, v), json: addKey(k, v, false)},
			)
		}
	}
	out = append(out,
		&mutation{name: "rename reasoning_content", json: renameKey("reasoning_content", "reasoning")},
		&mutation{name: "rename reasoning", json: renameKey("reasoning", "reasoning_content")},
		&mutation{name: "garbled body", body: func([]byte) []byte { return []byte("{garbled") }},
		&mutation{name: "empty body", body: func([]byte) []byte { return nil }},
		&mutation{name: "html body", body: func([]byte) []byte { return []byte("<html>x</html>") }},
		&mutation{name: "wrong content length", badLen: true},
		&mutation{name: "write fails", failWrite: true},
		&mutation{name: "parse fails", parse: func(_ driver.Kind, _ *core.Request, _ *core.Error) (*core.Request, *core.Error) {
			return nil, &core.Error{Kind: core.ErrInvalidRequest, Message: "wwz"}
		}},
		&mutation{name: "parse returns nothing", parse: func(driver.Kind, *core.Request, *core.Error) (*core.Request, *core.Error) {
			return nil, nil
		}},
		&mutation{name: "parse accepts everything", parse: func(_ driver.Kind, r *core.Request, e *core.Error) (*core.Request, *core.Error) {
			if e != nil {
				return &core.Request{}, nil
			}
			return r, nil
		}},
		&mutation{name: "parse error kind", parse: func(_ driver.Kind, r *core.Request, e *core.Error) (*core.Request, *core.Error) {
			if e != nil {
				c := *e
				c.Kind = core.ErrOverloaded
				return r, &c
			}
			return r, nil
		}},
		&mutation{name: "parse error message", parse: func(_ driver.Kind, r *core.Request, e *core.Error) (*core.Request, *core.Error) {
			if e != nil {
				c := *e
				c.Message = ""
				return r, &c
			}
			return r, nil
		}},
		&mutation{name: "comment before first frame", frame: func(i int, p []byte) [][]byte {
			if i == 0 {
				return [][]byte{[]byte(": wwz\n\n"), p}
			}
			return [][]byte{p}
		}},
		&mutation{name: "bogus line frame", frame: func(i int, p []byte) [][]byte {
			if i == 1 {
				return [][]byte{[]byte("bogus: line\n\n"), p}
			}
			return [][]byte{p}
		}},
		&mutation{name: "comment after every frame", frame: func(_ int, p []byte) [][]byte { return [][]byte{p, []byte(": wwz\n\n")} }},
		&mutation{name: "done after every frame", frame: func(_ int, p []byte) [][]byte { return [][]byte{p, []byte("data: [DONE]\n\n")} }},
		&mutation{name: "ping after every frame", frame: func(_ int, p []byte) [][]byte {
			return [][]byte{p, []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")}
		}},
		&mutation{name: "event line on every frame", frame: func(_ int, p []byte) [][]byte {
			if strings.HasPrefix(string(p), "data: ") {
				return [][]byte{append([]byte("event: wwz\n"), p...)}
			}
			return [][]byte{p}
		}},
		&mutation{name: "two data lines", frame: func(_ int, p []byte) [][]byte {
			if strings.HasPrefix(string(p), "event: ") {
				return [][]byte{[]byte(strings.Replace(string(p), "\ndata: ", "\ndata: \ndata: ", 1))}
			}
			return [][]byte{p}
		}},
		&mutation{name: "no blank line at end", frame: func(_ int, p []byte) [][]byte { return [][]byte{[]byte(strings.TrimSuffix(string(p), "\n"))} }},
		&mutation{name: "id not reproducible", id: func() func(string, string) string {
			var n atomic.Int64
			return func(_, v string) string { return fmt.Sprintf("%s%d", v, n.Add(1)) }
		}()},
	)
	for _, m := range []string{"Start", "ReasoningDelta", "TextDelta", "ToolCallStart", "ToolArgsDelta", "ToolCallEnd", "Ping", "Finish", "Error"} {
		out = append(out, &mutation{name: "stream " + m + " fails", failStream: m})
	}
	for _, c := range []int{200, 302} {
		for _, from := range []int{400, 401, 403, 404, 429, 500, 503, 529} {
			out = append(out, &mutation{name: fmt.Sprintf("status %d becomes %d", from, c), status: func(got int) int {
				if got == from {
					return c
				}
				return got
			}})
		}
	}
	for _, f := range requestFields() {
		out = append(out, &mutation{name: "perturb request field " + f, req: perturbField(f)})
	}
	out = append(out, deeperMutations(name, mustNew0(name).Routes())...)
	return out
}

// addKey adds a member to the root object, or to every object.
func addKey(key, raw string, all bool) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		add := func(n *jnode) {
			if n.kind == 'o' {
				n.keys = append(n.keys, key)
				n.kids = append(n.kids, &jnode{kind: 's', raw: []byte(raw)})
				changed = true
			}
		}
		if !all {
			add(root)
			return changed
		}
		var objs []*jnode
		var rec func(n *jnode)
		rec = func(n *jnode) {
			objs = append(objs, n)
			for _, k := range n.kids {
				rec(k)
			}
		}
		rec(root)
		for _, o := range objs {
			add(o)
		}
		return changed
	}
}

func renameKey(from, to string) func(*jnode) bool {
	return func(root *jnode) bool {
		changed := false
		root.walk(func(p *jnode, i int, key string, _ *jnode) {
			if key == from {
				p.keys[i] = to
				changed = true
			}
		})
		return changed
	}
}

// perturbField gives a core.Request field a wrong, non-zero value.
func perturbField(name string) func(*core.Request) {
	return func(r *core.Request) {
		f := reflect.ValueOf(r).Elem().FieldByName(name)
		switch f.Kind() {
		case reflect.String:
			f.SetString("wwz-other")
		case reflect.Bool:
			f.SetBool(!f.Bool())
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Float32, reflect.Float64:
			f.SetFloat(f.Float() + 1)
		case reflect.Slice:
			if f.Len() > 0 {
				f.Set(f.Slice(0, f.Len()-1))
			} else {
				f.Set(reflect.MakeSlice(f.Type(), 1, 1))
			}
		case reflect.Pointer:
			if f.IsNil() {
				f.Set(reflect.New(f.Type().Elem()))
			} else {
				f.Set(reflect.Zero(f.Type()))
			}
		default:
			f.Set(reflect.Zero(f.Type()))
		}
	}
}
