package oai

import "bytes"

// Content deltas are most of a stream's frames and differ only in the text
// and the obfuscation value. The first one renders the whole chunk once
// with sentinel values and keeps the bytes around them (FrameTemplate); the
// following ones append the escaped text and the drawn obfuscation into
// that layout, without building an Obj (spec 014: chunks pre-serialized).

// Sentinels: plain letters and digits, so every Format writes them
// verbatim; a short and a long pair, so a hook that looks at the length of
// a value shows up as a mismatch. Hooks are expected to be structural
// (reorder, add or drop keys), which every driver's are.
const (
	tplTextA = "wwzT9f"
	tplTextB = "wwzTplTextB7e2dWithAMuchLongerSentinelThanTheFirst4c1a"
	tplObfA  = "wwzO3c"
	tplObfB  = "wwzTplObfB5d0eWithAMuchLongerSentinelThanTheFirst8b2f"
)

// FrameTemplate is a frame split around its holes: segs[i] precedes
// holes[i] and segs[len(holes)] closes it.
type FrameTemplate struct {
	segs  [][]byte
	holes []byte // 't' text, 'o' obfuscation (both JSON strings)
	html  bool   // the text is escaped like encoding/json with HTML escaping
}

// NewFrameTemplate renders a frame twice through render, with two sets of
// sentinel values, and returns its template only when filling it
// reproduces the second render byte for byte; nil means the frame depends
// on the values in some other way (or render failed) and must be rendered
// every time. withObf says the frame carries the obfuscation value once;
// html says the format escapes <, > and & in strings.
func NewFrameTemplate(render func(text, obf string) ([]byte, error), withObf, html bool) *FrameTemplate {
	a, err := render(tplTextA, tplObfA)
	if err != nil {
		return nil
	}
	t := splitTemplate(append([]byte(nil), a...), tplTextA, tplObfA, withObf, html)
	if t == nil {
		return nil
	}
	want, err := render(tplTextB, tplObfB)
	if err != nil {
		return nil
	}
	var tmp [1024]byte
	got := t.Append(tmp[:0], tplTextB, fixedObf(tplObfB))
	if !bytes.Equal(got, want) {
		return nil
	}
	return t
}

// splitTemplate splits b at the quoted sentinels text (exactly once) and
// obf (exactly once when withObf, else never).
func splitTemplate(b []byte, text, obf string, withObf, html bool) *FrameTemplate {
	qt, qo := []byte(`"`+text+`"`), []byte(`"`+obf+`"`)
	wantObf := 0
	if withObf {
		wantObf = 1
	}
	if bytes.Count(b, qt) != 1 || bytes.Count(b, qo) != wantObf {
		return nil
	}
	t := &FrameTemplate{html: html, segs: make([][]byte, 0, 4), holes: make([]byte, 0, 3)}
	for {
		it, io := bytes.Index(b, qt), bytes.Index(b, qo)
		switch {
		case it < 0 && io < 0:
			t.segs = append(t.segs, b)
			return t
		case io < 0 || (it >= 0 && it < io):
			t.segs = append(t.segs, b[:it])
			t.holes = append(t.holes, 't')
			b = b[it+len(qt):]
		default:
			t.segs = append(t.segs, b[:io])
			t.holes = append(t.holes, 'o')
			b = b[io+len(qo):]
		}
	}
}

// obfSource draws obfuscation values (*obfuscator).
type obfSource interface{ appendNext(dst []byte) []byte }

// fixedObf is an obfSource that always appends itself.
type fixedObf string

func (f fixedObf) appendNext(dst []byte) []byte { return append(dst, f...) }

// Append appends the frame of text to dst; obf appends the unquoted
// obfuscation value (used only when the template has that hole; nil
// otherwise).
func (t *FrameTemplate) Append(dst []byte, text string, obf obfSource) []byte {
	enc := encoder{html: t.html}
	for i, h := range t.holes {
		dst = append(dst, t.segs[i]...)
		if h == 't' {
			dst = enc.str(dst, text)
			continue
		}
		dst = append(dst, '"')
		dst = obf.appendNext(dst)
		dst = append(dst, '"')
	}
	return append(dst, t.segs[len(t.holes)]...)
}

// contentChunk writes the content delta chunk of text, from the template
// when the hooks allow it.
func (s *Stream) contentChunk(text string) error {
	if s.text == nil && !s.textOff && text != "" {
		s.text = NewFrameTemplate(s.renderContent, s.obf != nil, !s.h.ChunkFormat.RawHTML)
		s.textOff = s.text == nil
	}
	if s.text == nil || text == "" {
		return s.chunk(Obj{{"content", text}}, nil)
	}
	var obf obfSource
	if s.obf != nil {
		obf = s.obf
	}
	s.buf = s.text.Append(s.buf[:0], text, obf)
	return s.w.DataRaw(s.buf)
}

// renderContent renders the content chunk of text with a fixed
// obfuscation value: it consumes no obfuscation draw.
func (s *Stream) renderContent(text, obf string) ([]byte, error) {
	s.obfFixed = obf
	defer func() { s.obfFixed = "" }()
	return s.render(s.chunkObj(Obj{{"content", text}}, nil), nil)
}
