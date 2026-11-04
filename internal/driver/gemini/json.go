package gemini

import (
	"bytes"

	"github.com/patrickbrandao/white-wizard/internal/driver/oai"
)

// The byte layouts of the Gemini API, conferred on 2026-10-31 (task 164,
// testdata/real/gemini/shape/):
//
//   - a JSON body is prettyPrint: two-space indent, "key": value, a final
//     "\n"; < and > are escaped as \u003c and \u003e, & and = are not,
//     non-ASCII goes out raw;
//   - an SSE event (alt=sse) is the same JSON with every newline and the
//     indentation after it removed ({"a": [{"b": 1}],"c": 2}), framed as
//     "data: " + JSON + "\r\n\r\n";
//   - streamGenerateContent without alt=sse is a JSON array delivered in
//     pieces: "[" + body + ",\r\n" + body + ... + "]", each body being the
//     prettyPrint JSON with its final "\n".

// prettyFormat renders the indented JSON; the angle brackets are escaped
// afterwards (encoding/json's HTML escaping would also escape &).
var prettyFormat = oai.Format{Indent: "  ", Newline: true, RawHTML: true}

// pretty renders v as a prettyPrint body.
func pretty(v any) []byte {
	b, _ := prettyFormat.Marshal(v) // Obj, []any, strings and numbers: cannot fail
	return escapeAngles(b)
}

// escapeAngles escapes < and >: in valid JSON they only occur inside
// strings, so a byte replacement is exact.
func escapeAngles(b []byte) []byte {
	if !bytes.ContainsAny(b, "<>") {
		return b
	}
	b = bytes.ReplaceAll(b, []byte("<"), []byte(`\u003c`))
	return bytes.ReplaceAll(b, []byte(">"), []byte(`\u003e`))
}

// line folds a prettyPrint body into the one-line form of an SSE event:
// every newline goes away with the indentation that follows it. A newline
// inside a string is escaped, so only the layout ones are removed.
func line(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			out = append(out, b[i])
			continue
		}
		for i+1 < len(b) && b[i+1] == ' ' {
			i++
		}
	}
	return out
}

// sseEvent is one SSE frame carrying v.
func sseEvent(v any) []byte {
	b := append([]byte("data: "), line(pretty(v))...)
	return append(b, "\r\n\r\n"...)
}
