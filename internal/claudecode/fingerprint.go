package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"unicode/utf8"
)

// fingerprintSalt is the salt of the cc_version fingerprint.
const fingerprintSalt = "59cf53e54c78"

// fingerprintIndices are the UTF-16 indices of the text that enter the hash.
var fingerprintIndices = [3]int{4, 7, 20}

// Fingerprint returns the 3 lowercase hex characters Claude Code appends to
// cc_version: the start of sha256("59cf53e54c78" + t[4] + t[7] + t[20] +
// version) over UTF-8, where t[i] is the UTF-16 code unit of index i of
// text, "0" when the index does not exist and U+FFFD when the unit is half
// of a surrogate pair (as Node's Buffer encodes a lone surrogate).
func Fingerprint(text, version string) string {
	var picked [3]string
	for i := range picked {
		picked[i] = "0"
	}
	unit := 0
	for i := 0; i < len(text) && unit <= fingerprintIndices[2]; {
		r, size := utf8.DecodeRuneInString(text[i:])
		units := 1
		if r == utf8.RuneError && size == 1 {
			// A WTF-8 lone surrogate (or an invalid byte) is one unit.
			if _, ok := wtf8Surrogate(text[i:]); ok {
				size = 3
			}
		} else if r >= 0x10000 {
			units = 2
		}
		for u := range units {
			for k, idx := range fingerprintIndices {
				if unit+u != idx {
					continue
				}
				if units == 2 || r == utf8.RuneError {
					picked[k] = "�"
				} else {
					picked[k] = text[i : i+size]
				}
			}
		}
		unit += units
		i += size
	}
	h := sha256.New()
	h.Write([]byte(fingerprintSalt))
	for _, s := range picked {
		h.Write([]byte(s))
	}
	h.Write([]byte(version))
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0])[:2])[:3]
}

// FirstUserText returns the text the fingerprint is computed from: the
// content of the first message with role "user" when it is a string, or the
// text of its first "text" block; "" when there is none.
func FirstUserText(messages []Value) string {
	for _, m := range messages {
		role, _ := m.Get("role")
		if role.Str() != "user" {
			continue
		}
		content, _ := m.Get("content")
		if content.Kind() == KindString {
			return content.Str()
		}
		for _, b := range content.Elems() {
			if t, _ := b.Get("type"); t.Str() == "text" {
				text, _ := b.Get("text")
				return text.Str()
			}
		}
		return ""
	}
	return ""
}

// FingerprintTexts returns the texts the fingerprint may have been computed
// from, as seen in the API body. Claude Code hashes the first user message
// of its transcript that is not meta (function uF of the 2.1.308 binary:
// e.find(s => s.type === "user" && !s.isMeta), then the content string or
// its first "text" block, "" when there is none). The meta messages before
// it are merged into the same API message as earlier text blocks, so the
// prompt is some text block of the first API message with role "user",
// not necessarily the first one.
//
// The meta blocks cannot be told apart in the body (task 457, real
// traffic): the attribution reminder and CLAUDE.md start with
// <system-reminder>, but a /context run before the prompt adds the meta
// blocks "<command-name>/context…", "<local-command-stdout>…" and "##
// Context Usage…" (no tag), while a !command adds "<bash-input>…", which
// is not meta and is the one hashed. So the candidates are the content
// string, or the text of each "text" block of that message, plus "" when
// it also has a block of another type (a prompt with only an image has no
// text). Nothing else of the body enters: a fingerprint that matches no
// candidate is still refused.
func FingerprintTexts(messages []Value) []string {
	for _, m := range messages {
		if role, _ := m.Get("role"); role.Str() != "user" {
			continue
		}
		content, _ := m.Get("content")
		if content.Kind() == KindString {
			return []string{content.Str()}
		}
		var texts []string
		other := false
		for _, b := range content.Elems() {
			if t, _ := b.Get("type"); t.Str() != "text" {
				other = true
				continue
			}
			text, _ := b.Get("text")
			texts = append(texts, text.Str())
		}
		if other || len(texts) == 0 {
			texts = append(texts, "")
		}
		return texts
	}
	return []string{""}
}

// fingerprints returns the distinct fingerprints of texts, in order.
func fingerprints(texts []string, version string) []string {
	var fps []string
	for _, t := range texts {
		if fp := Fingerprint(t, version); !slices.Contains(fps, fp) {
			fps = append(fps, fp)
		}
	}
	return fps
}
