package limits_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Tests that only make sense on a Valkey Cluster (WWZ_TEST_VALKEY=cluster,
// ADR 0021). The rest of valkey_test.go runs on both targets.

func clusterOnly(t *testing.T) {
	t.Helper()
	if !valkeyTargetEnv(t).cluster {
		t.Skip("precisa de WWZ_TEST_VALKEY=cluster (Valkey Cluster com 3 mestres)")
	}
}

// clusterSubjects spread over the slots: ip, key, user, project and account.
var clusterSubjects = []limits.Entity{
	{Kind: limits.KindIP, ID: "10.1.2.3"},
	keyA,
	{Kind: limits.KindUser, ID: "user-a"},
	{Kind: limits.KindProject, ID: "proj-a"},
	acctA,
}

// TestValkeyClusterKeySlots: every key of an entity is on one node (the hash
// tag), and the entities of one request are on more than one node, so the
// suite really crosses slots.
func TestValkeyClusterKeySlots(t *testing.T) {
	clusterOnly(t)
	s := newValkeyStore(t)
	subs := make([]limits.Subject, len(clusterSubjects))
	for i, e := range clusterSubjects {
		subs[i] = limits.Subject{Entity: e, Limits: config.Limits{RPS: 100, RPM: 100, TPM: 1_000_000, RPD: 100, Concurrency: 10}}
	}
	if d := mustCheck(t, s, limits.CheckRequest{Subjects: subs, EstPromptTokens: 10, Now: time.Now()}); !d.Allowed {
		t.Fatalf("Check across slots: %+v", d)
	}
	ctx := context.Background()
	nodesWithEnt := 0
	tags := map[string]int{} // hash tag -> nodes holding it
	for _, n := range masters(t, s) {
		ks, err := n.Do(ctx, n.B().Arbitrary("KEYS").Args("wwz:*{*}").Build()).AsStrSlice()
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		hasEnt := false
		for _, k := range ks {
			tag := k[strings.Index(k, "{"):]
			if !seen[tag] {
				seen[tag] = true
				tags[tag]++
			}
			hasEnt = hasEnt || strings.HasPrefix(k, "wwz:ent:")
		}
		if hasEnt {
			nodesWithEnt++
		}
	}
	if len(tags) != len(clusterSubjects) {
		t.Fatalf("hash tags = %v, want one per entity", tags)
	}
	for tag, n := range tags {
		if n != 1 {
			t.Errorf("keys of %s spread over %d nodes", tag, n)
		}
	}
	if nodesWithEnt < 2 {
		t.Fatalf("entities on %d node(s): the subjects do not cross slots", nodesWithEnt)
	}
}

// TestValkeyClusterConcurrentRelease: many callers on two instances; the
// account (last subject, other slot) admits exactly its rpm, the denials
// release what the earlier entities took, and nothing is admitted beyond the
// limit (ADR 0021: denied in excess at most, never allowed in excess).
func TestValkeyClusterConcurrentRelease(t *testing.T) {
	clusterOnly(t)
	a := newValkeyStore(t)
	b := openValkey(t, func(_ *config.StoreConfig, o *limits.ValkeyOptions) { o.Instance = "test-2" })
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	ip := limits.Entity{Kind: limits.KindIP, ID: "10.9.9.9"}
	subs := []limits.Subject{
		{Entity: ip, Limits: config.Limits{RPM: 1000}},
		{Entity: keyA, Limits: config.Limits{RPM: 1000, TPM: 1_000_000, Concurrency: 1000}},
		{Entity: acctA, Limits: config.Limits{RPM: 7}},
	}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 40 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			for range 5 {
				d, err := s.Check(context.Background(), limits.CheckRequest{Subjects: subs, EstPromptTokens: 10, Now: now})
				if err != nil {
					t.Error(err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
				} else if d.Entity != acctA {
					t.Errorf("denied by %v, want the account: %+v", d.Entity, d)
				}
			}
		})
	}
	wg.Wait()
	if n := allowed.Load(); n != 7 {
		t.Fatalf("allowed %d across the cluster, want the account's 7", n)
	}
	ctx := context.Background()
	for _, e := range []limits.Entity{ip, keyA, acctA} {
		st, err := a.Get(ctx, e)
		if err != nil || st.Requests != 7 {
			t.Fatalf("%v requests = %d (%v), want 7 (released ones do not count)", e, st.Requests, err)
		}
	}
	// Released capacity: the key alone still has 1000-7 requests this minute.
	one := limits.CheckRequest{Subjects: subs[1:2], EstPromptTokens: 10, Now: now}
	for i := range 1000 - 7 {
		if d := mustCheck(t, a, one); !d.Allowed {
			t.Fatalf("key denied at %d of the released capacity: %+v", i, d)
		}
	}
	if d := mustCheck(t, a, one); d.Allowed {
		t.Fatal("key allowed beyond its rpm after the releases")
	}
}
