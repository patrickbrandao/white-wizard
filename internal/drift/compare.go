package drift

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Divergence kinds (spec 017).
const (
	KindStatusMismatch = "status_mismatch"
	KindMissingField   = "missing_field"
	KindExtraField     = "extra_field"
	KindTypeMismatch   = "type_mismatch"
	KindOrderMismatch  = "order_mismatch"
	KindEventSequence  = "event_sequence"
	KindMissingHeader  = "missing_header"
	KindExtraHeader    = "extra_header"
)

// Kinds lists every divergence kind.
var Kinds = []string{
	KindStatusMismatch, KindMissingField, KindExtraField, KindTypeMismatch,
	KindOrderMismatch, KindEventSequence, KindMissingHeader, KindExtraHeader,
}

// Divergence is one shape difference between the real provider and wwz.
type Divergence struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Real    string `json:"real"`
	WWZ     string `json:"wwz"`
	Ignored bool   `json:"ignored"`
}

// present is the value shown for a header found on one side.
const present = "present"

// Compare returns the divergences of wwz (sim) against the real provider (ref). A status mismatch
// is reported alone: the bodies of different statuses are not comparable.
func Compare(ref, sim Signature) []Divergence {
	if ref.Status != sim.Status {
		return []Divergence{{Kind: KindStatusMismatch, Path: "status", Real: strconv.Itoa(ref.Status), WWZ: strconv.Itoa(sim.Status)}}
	}
	var out []Divergence
	if ref.ContentType != sim.ContentType {
		out = append(out, Divergence{Kind: KindTypeMismatch, Path: "content-type", Real: ref.ContentType, WWZ: sim.ContentType})
	}
	for _, h := range ref.Headers {
		if !slices.Contains(sim.Headers, h) {
			out = append(out, Divergence{Kind: KindMissingHeader, Path: h, Real: present})
		}
	}
	for _, h := range sim.Headers {
		if !slices.Contains(ref.Headers, h) {
			out = append(out, Divergence{Kind: KindExtraHeader, Path: h, WWZ: present})
		}
	}
	switch {
	case ref.Stream && sim.Stream:
		out = append(out, compareEvents(ref.Events, sim.Events)...)
	case ref.Stream != sim.Stream:
		out = append(out, Divergence{Kind: KindEventSequence, Path: "events", Real: streamKind(ref.Stream), WWZ: streamKind(sim.Stream)})
	default:
		out = append(out, compareItem(ref, sim)...)
		out = append(out, CompareShapes(ref.Body, sim.Body)...)
	}
	return out
}

// compareItem reports the /models item of the probe model present on only
// one side (SignModels).
func compareItem(ref, sim Signature) []Divergence {
	if ref.Item == "" || sim.Item == "" || ref.ItemFound == sim.ItemFound {
		return nil
	}
	path := "data[id=" + ref.Item + "]"
	if ref.ItemFound {
		return []Divergence{{Kind: KindMissingField, Path: path, Real: TypeObject}}
	}
	return []Divergence{{Kind: KindExtraField, Path: path, WWZ: TypeObject}}
}

func streamKind(stream bool) string {
	if stream {
		return "sse"
	}
	return "json"
}

// compareEvents aligns the collapsed sequences by the longest common
// subsequence of the event keys and lists every difference: each event
// without a pair is one event_sequence at events[<i>] (index on its own
// side), and each aligned pair compares its data with the JSON rules, with
// paths under events[<i>:<key>] (index on the real side).
func compareEvents(ref, sim []Event) []Divergence {
	var out []Divergence
	i, j := 0, 0
	for _, p := range alignEvents(ref, sim) {
		for ; i < p[0]; i++ {
			out = append(out, Divergence{Kind: KindEventSequence, Path: fmt.Sprintf("events[%d]", i), Real: ref[i].String()})
		}
		for ; j < p[1]; j++ {
			out = append(out, Divergence{Kind: KindEventSequence, Path: fmt.Sprintf("events[%d]", j), WWZ: sim[j].String()})
		}
		if i < len(ref) && j < len(sim) {
			out = append(out, compareEvent(i, ref[i], sim[j])...)
		}
		i, j = i+1, j+1
	}
	return out
}

// alignEvents returns the aligned index pairs of the longest common
// subsequence of the keys, ending with the sentinel {len(ref), len(sim)}.
func alignEvents(ref, sim []Event) [][2]int {
	n, m := len(ref), len(sim)
	// lcs[i][j] is the LCS length of ref[i:] and sim[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ref[i].Key() == sim[j].Key() {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var pairs [][2]int
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case ref[i].Key() == sim[j].Key():
			pairs = append(pairs, [2]int{i, j})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return append(pairs, [2]int{n, m})
}

// compareEvent compares the data of two aligned events (same key).
func compareEvent(i int, r, w Event) []Divergence {
	prefix := fmt.Sprintf("events[%d:%s]", i, r.Key())
	var ds []Divergence
	if r.Event != w.Event {
		// Same type in the data, but the "event:" line differs.
		ds = append(ds, Divergence{Kind: KindEventSequence, Path: prefix, Real: "event=" + r.Event, WWZ: "event=" + w.Event})
	}
	if r.shape == nil || w.shape == nil {
		if r.Data != w.Data {
			ds = append(ds, Divergence{Kind: KindTypeMismatch, Path: prefix, Real: r.Data, WWZ: w.Data})
		}
		return ds
	}
	for _, d := range CompareShapes(r.shape, w.shape) {
		if d.Path == RootPath {
			d.Path = prefix
		} else {
			d.Path = prefix + "." + d.Path
		}
		ds = append(ds, d)
	}
	return ds
}

// Compatible reports whether two JSON types match: equal, or one is null.
func Compatible(a, b string) bool {
	return a == b || a == TypeNull || b == TypeNull
}

// CompareShapes returns the field divergences of wwz (sim) against real (ref): missing,
// extra and incompatible fields and sibling order. A field whose ancestor is
// missing, null, an empty array or of another type on the other side is not
// reported again.
func CompareShapes(ref, sim Shape) []Divergence {
	rm, wm := index(ref), index(sim)
	var out []Divergence
	for _, f := range ref {
		w, ok := wm[f.Path]
		switch {
		case covered(f, rm, wm):
		case !ok:
			out = append(out, Divergence{Kind: KindMissingField, Path: f.Path, Real: f.Type})
		case !Compatible(f.Type, w.Type):
			out = append(out, Divergence{Kind: KindTypeMismatch, Path: f.Path, Real: f.Type, WWZ: w.Type})
		}
	}
	for _, f := range sim {
		if _, ok := rm[f.Path]; !ok && !covered(f, wm, rm) {
			out = append(out, Divergence{Kind: KindExtraField, Path: f.Path, WWZ: f.Type})
		}
	}
	return append(out, compareOrder(ref, sim, rm, wm)...)
}

func index(s Shape) map[string]Field {
	m := make(map[string]Field, len(s))
	for _, f := range s {
		m[f.Path] = f
	}
	return m
}

// covered reports whether an ancestor of f (from own) makes f's absence or
// type on the other side already explained.
func covered(f Field, own, other map[string]Field) bool {
	for p := f.parent; p != ""; {
		a := own[p]
		o, ok := other[p]
		if !ok || o.Type == TypeNull || (o.Type == TypeArray && o.Empty) || !Compatible(a.Type, o.Type) {
			return true
		}
		p = a.parent
	}
	return false
}

// compareOrder reports parents whose common children appear in a
// different order.
func compareOrder(ref, sim Shape, rm, wm map[string]Field) []Divergence {
	var out []Divergence
	rc, wc := children(ref, wm), children(sim, rm)
	for _, f := range ref {
		r, w := rc[f.Path], wc[f.Path]
		if len(r) > 1 && !slices.Equal(r, w) {
			out = append(out, Divergence{Kind: KindOrderMismatch, Path: f.Path, Real: strings.Join(r, ","), WWZ: strings.Join(w, ",")})
		}
	}
	return out
}

// children maps each parent of s to the names of its children that also
// exist in other, in order.
func children(s Shape, other map[string]Field) map[string][]string {
	m := map[string][]string{}
	for _, f := range s {
		if _, ok := other[f.Path]; ok && f.parent != "" {
			m[f.parent] = append(m[f.parent], f.name)
		}
	}
	return m
}
