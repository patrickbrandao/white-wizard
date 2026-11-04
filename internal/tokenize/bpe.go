package tokenize

import "math"

// part is a token boundary during the merge: where the token starts and the
// rank of merging it with the next token (noRank when that pair is not in
// the vocabulary).
type part struct {
	start int32
	rank  uint32
}

const noRank = math.MaxUint32

// maxLinear is the longest piece merged by the quadratic scan of tiktoken's
// _byte_pair_merge; longer pieces (rare: long runs of letters or symbols)
// use the heap, which picks the same pair in O(log n).
const maxLinear = 64

// merge splits a pre-token absent from the vocabulary into its BPE tokens,
// merging first the pair with the lowest rank (leftmost on a tie), exactly
// as tiktoken does. It returns the token starts followed by len(piece), in
// buf when it fits.
func (e *Encoding) merge(piece string, buf []part) []part {
	if len(piece) > maxLinear {
		return e.mergeHeap(piece)
	}
	return e.mergeLinear(piece, buf)
}

func (e *Encoding) rank(s string) uint32 {
	if r, ok := e.ranks[s]; ok {
		return r
	}
	return noRank
}

func (e *Encoding) mergeLinear(piece string, parts []part) []part {
	n := len(piece)
	minRank, minAt := uint32(noRank), -1
	for i := 0; i+1 < n; i++ {
		r := e.rank(piece[i : i+2])
		if r < minRank {
			minRank, minAt = r, i
		}
		parts = append(parts, part{int32(i), r}) //nolint:gosec // G115: i < maxLinear
	}
	parts = append(parts, part{int32(n - 1), noRank}, part{int32(n), noRank}) //nolint:gosec // G115: n <= maxLinear
	pairRank := func(i int) uint32 {
		if i+3 < len(parts) {
			return e.rank(piece[parts[i].start:parts[i+3].start])
		}
		return noRank
	}
	for minRank != noRank {
		i := minAt
		if i > 0 {
			parts[i-1].rank = pairRank(i - 1)
		}
		parts[i].rank = pairRank(i)
		parts = append(parts[:i+1], parts[i+2:]...)
		minRank, minAt = noRank, -1
		for k, p := range parts[:len(parts)-1] {
			if p.rank < minRank {
				minRank, minAt = p.rank, k
			}
		}
	}
	return parts
}

// mergeHeap is mergeLinear over a linked list and a min-heap of
// (rank, start), with stale entries skipped on pop.
func (e *Encoding) mergeHeap(piece string) []part {
	n := len(piece)
	next := make([]int32, n) // start of the following token (n at the end)
	prev := make([]int32, n)
	rank := make([]uint32, n) // rank of merging the token with the next one
	alive := make([]bool, n)
	h := make(pairHeap, 0, n)
	for i := range n {
		next[i], prev[i], alive[i] = int32(i+1), int32(i-1), true
		rank[i] = noRank
		if i+1 < n {
			rank[i] = e.rank(piece[i : i+2])
		}
		if rank[i] != noRank {
			h = append(h, key(rank[i], i))
		}
	}
	h.init()
	pairRank := func(i int32) uint32 {
		j := next[i]
		if int(j) >= n {
			return noRank
		}
		return e.rank(piece[i:next[j]])
	}
	update := func(i int32) {
		rank[i] = pairRank(i)
		if rank[i] != noRank {
			h.push(key(rank[i], int(i)))
		}
	}
	for len(h) > 0 {
		k := h.pop()
		r, i := uint32(k>>32), int32(uint32(k)) //nolint:gosec // G115: unpacks key(): rank, then start < n
		if !alive[i] || rank[i] != r {
			continue // stale entry
		}
		j := next[i]
		alive[j] = false
		next[i] = next[j]
		if int(next[j]) < n {
			prev[next[j]] = i
		}
		update(i)
		if prev[i] >= 0 {
			update(prev[i])
		}
	}
	parts := make([]part, 0, n+1)
	for i := int32(0); int(i) < n; i = next[i] {
		parts = append(parts, part{i, rank[i]})
	}
	return append(parts, part{int32(n), noRank}) //nolint:gosec // G115: a piece is a slice of a request body, far below 2 GiB
}

func key(rank uint32, start int) uint64 { return uint64(rank)<<32 | uint64(uint32(start)) } //nolint:gosec // G115: start < len(piece)

// pairHeap is a binary min-heap of (rank<<32 | start): the lowest rank
// first, the leftmost start on a tie.
type pairHeap []uint64

func (h pairHeap) init() {
	for i := len(h)/2 - 1; i >= 0; i-- {
		h.down(i)
	}
}

func (h *pairHeap) push(k uint64) {
	*h = append(*h, k)
	s := *h
	for i := len(s) - 1; i > 0; {
		p := (i - 1) / 2
		if s[p] <= s[i] {
			break
		}
		s[p], s[i] = s[i], s[p]
		i = p
	}
}

func (h *pairHeap) pop() uint64 {
	s := *h
	top := s[0]
	last := len(s) - 1
	s[0] = s[last]
	*h = s[:last]
	h.down(0)
	return top
}

func (h pairHeap) down(i int) {
	for {
		l := 2*i + 1
		if l >= len(h) {
			return
		}
		m := l
		if r := l + 1; r < len(h) && h[r] < h[l] {
			m = r
		}
		if h[i] <= h[m] {
			return
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}
