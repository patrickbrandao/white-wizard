package oai

import (
	"hash/fnv"
	"math/rand/v2"
	"testing"
)

// oldDerive and oldObfuscator are the forms before task 270 (hash/fnv,
// heap PCG): the values must not change.
func oldDerive(seed, salt, alphabet string, n int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(seed))
	r := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

func oldObfuscator(id string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return rand.New(rand.NewPCG(h.Sum64(), 0x5851f42d4c957f2d))
}

func TestDeriveSameValues(t *testing.T) {
	for _, seed := range []string{"", "wwz-key-01", "acme\x00proj-x", "ação"} {
		for _, salt := range []string{"project", "organization", "organization-id"} {
			if got, want := derive(seed, salt, base62, 24), oldDerive(seed, salt, base62, 24); got != want {
				t.Fatalf("derive(%q, %q) = %q, want %q", seed, salt, got, want)
			}
			if got, want := deriveID("proj_", seed, salt, lowerAlnum, 24), "proj_"+oldDerive(seed, salt, lowerAlnum, 24); got != want {
				t.Fatalf("deriveID(%q, %q) = %q, want %q", seed, salt, got, want)
			}
		}
	}
	// Longer than the stack buffer.
	if got, want := deriveID("p_", "k", "s", base62, 100), "p_"+oldDerive("k", "s", base62, 100); got != want {
		t.Fatalf("long deriveID = %q, want %q", got, want)
	}
	if n := testing.AllocsPerRun(100, func() { _ = deriveID("proj_", "k", "project", base62, 24) }); n != 2 {
		t.Fatalf("deriveID allocs = %v, want 2 (the PCG and the string)", n)
	}
}

func TestObfuscatorSameValues(t *testing.T) {
	for _, id := range []string{"chatcmpl-abc", "", "resp_123"} {
		o, old := newObfuscator(id), oldObfuscator(id)
		for i := range 20 {
			n := 4 + old.IntN(8)
			b := make([]byte, n)
			for j := range b {
				b[j] = base62[old.IntN(len(base62))]
			}
			if got := o.next(); got != string(b) {
				t.Fatalf("%q draw %d: %q, want %q", id, i, got, b)
			}
		}
	}
}
