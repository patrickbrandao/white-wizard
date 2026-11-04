package tokenize

import (
	"unicode"
	"unicode/utf8"
)

// Character classes of the tiktoken patterns. Go's regexp has no lookahead
// nor possessive quantifiers, so each pattern is matched by hand with the
// leftmost-first, backtracking semantics of the reference (fancy-regex).
const (
	cL     uint8 = 1 << iota // \p{L}
	cN                       // \p{N}
	cS                       // \s (White_Space)
	cNL                      // \r or \n
	cUpper                   // [\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]
	cLower                   // [\p{Ll}\p{Lm}\p{Lo}\p{M}]
)

var asciiClass [utf8.RuneSelf]uint8

func init() {
	for r := range rune(utf8.RuneSelf) {
		asciiClass[r] = classOf(r)
	}
}

func classOf(r rune) uint8 {
	var c uint8
	switch {
	case unicode.IsLetter(r):
		c = cL
		switch {
		case unicode.IsUpper(r) || unicode.IsTitle(r):
			c |= cUpper
		case unicode.IsLower(r):
			c |= cLower
		default: // Lm, Lo
			c |= cUpper | cLower
		}
	case unicode.IsMark(r):
		c = cUpper | cLower
	case unicode.IsNumber(r):
		c = cN
	case unicode.IsSpace(r):
		c = cS
		if r == '\r' || r == '\n' {
			c |= cNL
		}
	}
	return c
}

// at decodes the character at i: its class and size. An invalid byte is a
// one-byte symbol.
func at(s string, i int) (uint8, int) {
	if b := s[i]; b < utf8.RuneSelf {
		return asciiClass[b], 1
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	return classOf(r), n
}

// isPrefix is [^\r\n\p{L}\p{N}].
func isPrefix(c uint8) bool { return c&(cL|cN|cNL) == 0 }

// isSymbol is [^\s\p{L}\p{N}].
func isSymbol(c uint8) bool { return c&(cL|cN|cS) == 0 }

// run returns the end of the run of characters with any class bit in mask.
func run(s string, i int, mask uint8) int {
	for i < len(s) {
		c, n := at(s, i)
		if c&mask == 0 {
			break
		}
		i += n
	}
	return i
}

// numbers is \p{N}{1,3} at i (i when there is none).
func numbers(s string, i int) int {
	for k := 0; k < 3 && i < len(s); k++ {
		c, n := at(s, i)
		if c&cN == 0 {
			break
		}
		i += n
	}
	return i
}

// symbols is ` ?[^\s\p{L}\p{N}]+` followed by the run of tail bytes; i
// when it does not match.
func symbols(s string, i int, tail string) int {
	j := i
	if s[j] == ' ' && j+1 < len(s) {
		j++
	}
	for j < len(s) {
		c, n := at(s, j)
		if !isSymbol(c) {
			break
		}
		j += n
	}
	if j == i || s[j-1] == ' ' {
		return i // no symbol (a lone space is not one)
	}
	for j < len(s) && containsByte(tail, s[j]) {
		j++
	}
	return j
}

func containsByte(set string, b byte) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == b {
			return true
		}
	}
	return false
}

// spaces describes the whitespace run at i: its end, the end of its last
// \r or \n (0 when it has none), and the start of its last character.
func spaces(s string, i int) (end, afterNL, last int) {
	for end = i; end < len(s); {
		c, n := at(s, end)
		if c&cS == 0 {
			break
		}
		last = end
		end += n
		if c&cNL != 0 {
			afterNL = end
		}
	}
	return end, afterNL, last
}

// contraction is (?i:'s|'t|'re|'ve|'m|'ll|'d) at i: its end, or i. Case
// folding lets 's' match 'S' and 'ſ' (U+017F).
func contraction(s string, i int) int {
	if i+1 >= len(s) || s[i] != '\'' {
		return i
	}
	switch s[i+1] | 0x20 {
	case 's', 't', 'm', 'd':
		return i + 2
	case 'r', 'v':
		if i+2 < len(s) && s[i+2]|0x20 == 'e' {
			return i + 3
		}
	case 'l':
		if i+2 < len(s) && s[i+2]|0x20 == 'l' {
			return i + 3
		}
	}
	if s[i+1] == 0xC5 && i+2 < len(s) && s[i+2] == 0xBF { // ſ
		return i + 3
	}
	return i
}

// nextCl100k is cl100k_base:
//
//	'(?i:[sdmt]|ll|ve|re)|[^\r\n\p{L}\p{N}]?+\p{L}++|\p{N}{1,3}+|
//	 ?[^\s\p{L}\p{N}]++[\r\n]*+|\s++$|\s*[\r\n]|\s+(?!\S)|\s
func nextCl100k(s string, i int) int {
	if j := contraction(s, i); j > i {
		return j
	}
	c, n := at(s, i)
	switch {
	case c&cL != 0:
		return run(s, i+n, cL)
	case c&cN != 0:
		return numbers(s, i)
	}
	if isPrefix(c) && i+n < len(s) {
		if c2, n2 := at(s, i+n); c2&cL != 0 {
			return run(s, i+n+n2, cL)
		}
	}
	if j := symbols(s, i, "\r\n"); j > i {
		return j
	}
	// Whitespace (every other character matched above).
	end, afterNL, last := spaces(s, i)
	switch {
	case end == len(s):
		return end
	case afterNL > 0:
		return afterNL
	case last > i:
		return last
	}
	return i + n
}

// nextO200k is o200k_base:
//
//	[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?|
//	[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?|
//	\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+
func nextO200k(s string, i int) int {
	c, n := at(s, i)
	prefixed := isPrefix(c) && i+n < len(s)
	if c&(cUpper|cLower) != 0 || prefixed {
		// Each alternative tries the optional prefix first, then without it.
		if prefixed {
			if j := lowerWord(s, i+n); j > 0 {
				return contraction(s, j)
			}
		}
		if j := lowerWord(s, i); j > 0 {
			return contraction(s, j)
		}
		if prefixed {
			if j := upperWord(s, i+n); j > 0 {
				return contraction(s, j)
			}
		}
		if j := upperWord(s, i); j > 0 {
			return contraction(s, j)
		}
	}
	if c&cN != 0 {
		return numbers(s, i)
	}
	if j := symbols(s, i, "\r\n/"); j > i {
		return j
	}
	end, afterNL, last := spaces(s, i)
	switch {
	case afterNL > 0:
		return afterNL
	case end == len(s):
		return end
	case last > i:
		return last
	}
	return end
}

// lowerWord is [upper]*[lower]+ at i with backtracking: the upper run
// gives back characters until a lower one can start; 0 when nothing
// matches.
func lowerWord(s string, i int) int {
	lastLower := -1 // start of the last character of the upper run that is also lower
	j := i
	for j < len(s) {
		c, n := at(s, j)
		if c&cUpper == 0 {
			break
		}
		if c&cLower != 0 {
			lastLower = j
		}
		j += n
	}
	if j < len(s) {
		if c, n := at(s, j); c&cLower != 0 {
			return run(s, j+n, cLower)
		}
	}
	if lastLower >= 0 {
		// The character after lastLower is upper-only or ends the run.
		_, n := at(s, lastLower)
		return lastLower + n
	}
	return 0
}

// upperWord is [upper]+[lower]* at i; 0 when nothing matches.
func upperWord(s string, i int) int {
	j := run(s, i, cUpper)
	if j == i {
		return 0
	}
	return run(s, j, cLower)
}
