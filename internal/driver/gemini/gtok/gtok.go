// Package gtok counts tokens the way the Gemini API does (task 265): a
// tokenizer calibrated against countTokens and the prompt template the real
// renders a GenerateContentRequest into. The Gemini tokenizer is not public
// (a SentencePiece vocabulary of ~262k pieces), so Tokenizer approximates
// it: on a corpus of 63 texts (prose in English and Portuguese, Markdown,
// Go, JSON and YAML; tmp/t265, 2026-10-31) the mean error is 4.9% and the
// sum is 2.3% short. The template, measured with countTokens on
// gemini-3.5-flash-lite (the same on 3.1-flash-lite and 3.8-flash), is
// exact given the text counts.
//
// It is its own package so the driver's declaration to the engine (Text,
// Template; task 402) and the driver's own counts (cachedContents) share
// it.
package gtok

import (
	"unicode"
	"unicode/utf8"
)

// Calibrated sizes (tmp/t265/calib): letters per piece of an ASCII word and
// of a word with non-ASCII letters, punctuation and CJK characters per
// token, newlines per token.
const (
	wordPiece   = 10
	nonASCII    = 4
	punctPiece  = 2
	cjkPiece    = 2
	newlinePerT = 3
)

// Tokenizer approximates the Gemini tokenizer: a word (letters and marks)
// is one piece per wordPiece letters (nonASCII when it has a non-ASCII
// letter) with a single leading space attached; every digit is a token;
// punctuation runs cost one token per punctPiece characters, with a single
// leading space attached; a longer run of spaces is one token (its last
// space joins the next word); newline runs cost one per newlinePerT; CJK
// one per cjkPiece characters.
type Tokenizer struct{}

type class uint8

const (
	cWord class = iota + 1
	cDigit
	cSpace
	cNewline
	cPunct
	cCJK
)

func classOf(r rune) class {
	if r < utf8.RuneSelf {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return cWord
		case r >= '0' && r <= '9':
			return cDigit
		case r == ' ':
			return cSpace
		case r == '\n':
			return cNewline
		}
		return cPunct
	}
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana):
		return cCJK
	case unicode.IsDigit(r):
		return cDigit
	case unicode.IsLetter(r), unicode.IsMark(r):
		return cWord
	}
	return cPunct
}

// Each calls fn with every token of text, in order, as substrings of text;
// their concatenation is text.
func (Tokenizer) Each(text string, fn func(tok string)) {
	start := 0 // start of the pending token (a space that joins the next)
	i := 0
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		c := classOf(r)
		j := i + size
		for j < len(text) {
			r2, s2 := utf8.DecodeRuneInString(text[j:])
			if classOf(r2) != c {
				break
			}
			j += s2
		}
		switch c {
		case cSpace:
			next := class(0)
			if j < len(text) {
				r2, _ := utf8.DecodeRuneInString(text[j:])
				next = classOf(r2)
			}
			end := j
			if next == cWord || next == cPunct {
				end = j - 1 // the last space joins the next token
			}
			if end > start {
				fn(text[start:end])
			}
			start = end
		case cWord:
			n := wordPiece
			for _, r := range text[i:j] {
				if r >= utf8.RuneSelf {
					n = nonASCII
					break
				}
			}
			start = chunks(text, start, i, j, n, fn)
		case cDigit:
			start = chunks(text, start, i, j, 1, fn)
		case cPunct:
			start = chunks(text, start, i, j, punctPiece, fn)
		case cNewline:
			start = chunks(text, start, i, j, newlinePerT, fn)
		case cCJK:
			start = chunks(text, start, i, j, cjkPiece, fn)
		}
		i = j // every run ends flushed: a trailing space run has no next token
	}
}

// chunks emits text[i:j] in pieces of n runes, the first one starting at
// start (a pending space); it returns the new start.
func chunks(text string, start, i, j, n int, fn func(string)) int {
	k := 0
	for p := i; p < j; {
		_, s := utf8.DecodeRuneInString(text[p:])
		p += s
		k++
		if k == n || p == j {
			fn(text[start:p])
			start, k = p, 0
		}
	}
	return start
}

// Count is the number of tokens Each yields.
func (t Tokenizer) Count(text string) int {
	n := 0
	t.Each(text, func(string) { n++ })
	return n
}

// Split returns the tokens of text.
func (t Tokenizer) Split(text string) []string {
	var out []string
	t.Each(text, func(s string) { out = append(out, s) })
	return out
}
