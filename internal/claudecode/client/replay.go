package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
)

// Replay is the outcome of rebuilding a captured request.
type Replay struct {
	// Request holds the inputs extracted from the capture.
	Request claudecode.Request
	// Want is the capture; Got is what [claudecode.Build] made of its inputs.
	Want, Got []byte
	// Diff is nil when Got is byte for byte Want.
	Diff *Diff
}

// SHA256 returns the hex sha256 of the capture.
func (r *Replay) SHA256() string {
	sum := sha256.Sum256(r.Want)
	return hex.EncodeToString(sum[:])
}

// Diff is the first divergence between a capture and its rebuild.
type Diff struct {
	Offset int    // byte offset of the first difference
	Where  string // head line (and header name) or JSON path of the body
	Want   string // bytes of the capture around the offset
	Got    string // bytes of the rebuild around the offset
}

func (d *Diff) String() string {
	return fmt.Sprintf("differs at byte %d (%s)\n  want: %q\n  got:  %q", d.Offset, d.Where, d.Want, d.Got)
}

// ReplayCapture extracts the inputs of a raw Claude Code request (head and
// body, CRLF), rebuilds it with [claudecode.Build] and compares the bytes.
func ReplayCapture(raw []byte) (*Replay, error) {
	h, body, err := claudecode.ParseHead(raw)
	if err != nil {
		return nil, err
	}
	r, err := claudecode.Extract(h, body)
	if err != nil {
		return nil, err
	}
	head, built, err := claudecode.Build(r)
	if err != nil {
		return nil, err
	}
	rp := &Replay{Request: r, Want: raw, Got: append(head, built...)}
	if !bytes.Equal(rp.Want, rp.Got) {
		rp.Diff = diff(rp.Want, rp.Got)
	}
	return rp, nil
}

// diff locates the first difference of want and got (which differ).
func diff(want, got []byte) *Diff {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	from := max(0, i-20)
	d := &Diff{
		Offset: i,
		Want:   string(want[from:min(len(want), i+40)]),
		Got:    string(got[from:min(len(got), i+40)]),
	}
	wantEnd := bytes.Index(want, []byte("\r\n\r\n"))
	gotEnd := bytes.Index(got, []byte("\r\n\r\n"))
	if i <= wantEnd+3 || i <= gotEnd+3 {
		line := bytes.Count(want[:min(i, len(want))], []byte("\r\n")) + 1
		d.Where = "head line " + strconv.Itoa(line)
		if h, _, err := claudecode.ParseHead(want); err == nil && line >= 2 && line-2 < len(h.Headers) {
			d.Where += " (" + h.Headers[line-2].Name + ")"
		}
		return d
	}
	d.Where = "body"
	wv, werr := claudecode.Parse(want[wantEnd+4:])
	gv, gerr := claudecode.Parse(got[gotEnd+4:])
	if werr == nil && gerr == nil {
		if p, ok := pathDiff(wv, gv, "$"); ok {
			d.Where = "body " + p
		} else {
			d.Where = "body (same JSON values, different encoding)"
		}
	}
	return d
}

// pathDiff returns the JSON path of the first value that differs.
func pathDiff(a, b claudecode.Value, path string) (string, bool) {
	if a.Kind() != b.Kind() {
		return path, true
	}
	switch a.Kind() {
	case claudecode.KindObject:
		ak, bk := a.Keys(), b.Keys()
		if !slices.Equal(ak, bk) {
			return path + " (keys)", true
		}
		for i, m := range a.Members() {
			if p, ok := pathDiff(m.Value, b.Members()[i].Value, path+"."+m.Key); ok {
				return p, true
			}
		}
		return "", false
	case claudecode.KindArray:
		ae, be := a.Elems(), b.Elems()
		for i := range min(len(ae), len(be)) {
			if p, ok := pathDiff(ae[i], be[i], path+"["+strconv.Itoa(i)+"]"); ok {
				return p, true
			}
		}
		if len(ae) != len(be) {
			return path + " (length)", true
		}
		return "", false
	}
	if !bytes.Equal(claudecode.Marshal(a), claudecode.Marshal(b)) {
		return path, true
	}
	return "", false
}
