package limits

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEntityStateJSONMatchesContract(t *testing.T) {
	b, err := json.Marshal(EntityState{Entity: Entity{Kind: KindKey, ID: "k"}, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"entity":{"kind":"key","id":"k"}`) || strings.Contains(s, `"until"`) || strings.Contains(s, `"since"`) {
		t.Fatalf("json: %s", s)
	}
}

func TestMemoryResetCountersKeepsBlocks(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	k := Entity{Kind: KindKey, ID: "k"}
	a := Entity{Kind: KindAccount, ID: "a"}
	if err := m.Finish(ctx, FinishRequest{Subjects: []Subject{{Entity: k}}, Now: time.Now(), Status: 200}); err != nil {
		t.Fatal(err)
	}
	_ = m.Block(ctx, a, "x", 0, "admin")
	_, _ = m.Strike(ctx, a, "s", time.Hour)
	_ = m.SetCredits(ctx, a, 3)
	if err := m.ResetCounters(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := m.Get(ctx, a)
	if st.Status != StatusSuspended || st.Strikes != 1 || st.Credits == nil || *st.Credits != 3 {
		t.Fatalf("account: %+v", st)
	}
	ks, _ := m.Get(ctx, k)
	if ks.Requests != 0 {
		t.Fatalf("key: %+v", ks)
	}
}

func TestMemoryResetConversations(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	for _, c := range []string{"c1", "c1", "c2"} {
		_, _ = m.Attempts(ctx, c, "s"+c, time.Hour)
	}
	_, _ = m.Attempts(ctx, "c1", "other", time.Hour)
	if n, _ := m.ResetConversations(ctx, "c1"); n != 1 {
		t.Fatalf("one: %d", n)
	}
	if n, _ := m.ResetConversations(ctx, "missing"); n != 0 {
		t.Fatalf("missing: %d", n)
	}
	if n, _ := m.Attempts(ctx, "c1", "sc1", time.Hour); n != 1 {
		t.Fatalf("c1 kept: %d", n)
	}
	if n, _ := m.ResetConversations(ctx, ""); n != 2 {
		t.Fatalf("all: %d", n)
	}
	var _ CounterResetter = m
	var _ ConversationResetter = m
}
