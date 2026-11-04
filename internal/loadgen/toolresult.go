package loadgen

import "strings"

// ToolResultError is the content of a tool result sent as an error. It
// matches none of the success words the scenarios look for (ok, written,
// success, saved, done), so a step like file-agent-flow's edit-failed is
// reached.
const ToolResultError = "operation failed: permission denied"

// ToolResults invents the results of the tool calls in conversation mode
// (spec 015, "Modo conversa"): "ok" for tools whose name starts with write,
// edit, create or delete, a lorem paragraph for the others, and an error
// with probability ErrorRate. The output depends only on Seed and the
// position of the call, so a run with a fixed seed sends the same results.
type ToolResults struct {
	Seed int64
	// ErrorRate is the percent (0–100) of results sent as errors.
	ErrorRate float64
}

// writePrefixes mark the tools whose result is "ok".
var writePrefixes = []string{"write", "edit", "create", "delete"}

// IsWriteTool reports a tool whose invented result is "ok".
func IsWriteTool(name string) bool {
	n := strings.ToLower(name)
	for _, p := range writePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// key mixes the position of a call into one seed.
func (t ToolResults) key(conv int64, turn, idx int) int64 {
	return t.Seed ^ conv*0x5851f42d4c957f2d ^ int64(turn)*0x14057b7ef767814f ^ int64(idx)*0x2545f4914f6cdd1d
}

// For returns the result of the idx-th tool call of turn (both from 0) of
// conversation conv.
func (t ToolResults) For(conv int64, turn, idx int, name string) (content string, isError bool) {
	k := t.key(conv, turn, idx)
	if t.ErrorRate > 0 && float64(Draw(k, 0, 10000)) < t.ErrorRate*100 {
		return ToolResultError, true
	}
	if IsWriteTool(name) {
		return "ok", false
	}
	return lorem(k, 24+Draw(k, 1, 24)), false
}

var loremWords = strings.Fields(`lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod
tempor incididunt ut labore et magna aliqua enim ad minim veniam quis nostrud exercitation ullamco
laboris nisi aliquip ex ea commodo consequat duis aute irure in reprehenderit voluptate velit esse
cillum fugiat nulla pariatur excepteur sint occaecat cupidatat non proident sunt culpa qui officia
deserunt mollit anim id est laborum`)

// lorem returns n words drawn from key, capitalized and ending with a dot.
func lorem(key int64, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		w := loremWords[Draw(key, int64(i)+2, len(loremWords))]
		if i == 0 {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		b.WriteString(w)
	}
	b.WriteByte('.')
	return b.String()
}
