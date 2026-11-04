package limits_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	fv "github.com/patrickbrandao/white-wizard/internal/limits/storetest/fakevalkey"
)

// TestFakeGetQuota5h: Get reads the q5h minute buckets as the check script
// does (task 296): unparsable fields and buckets that left the 5 h window
// are skipped, the sum is in dollars and the oldest live bucket is the
// window's start; a failed q5h read surfaces.
func TestFakeGetQuota5h(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	minute := func(d time.Duration) string {
		return strconv.FormatInt(now.Add(-d).Truncate(time.Minute).UnixMilli(), 10)
	}
	q5h := fv.Map("x", "1", minute(6*time.Hour), "9000000", minute(3*time.Hour), "250000", minute(time.Hour), "1000000")
	v, _ := fakeStore(t, func(r fv.Request) string {
		switch {
		case r.Name == "SCRIPT LOAD":
			return ""
		case r.Name == "HGETALL" && strings.Contains(r.Args[0], "q5h:"):
			return q5h
		case r.Name == "HGETALL":
			return fv.Map("since", "1000")
		}
		return fv.Arr(fv.Null(), fv.Null())
	})
	st, err := v.Get(ctx, fkeyA)
	if err != nil || st.Quota5hUSD != 1.25 || st.Quota5hSince.UnixMilli() != now.Add(-3*time.Hour).Truncate(time.Minute).UnixMilli() {
		t.Fatalf("5h window: %v since %v (%v), want 1.25 since 3h ago", st.Quota5hUSD, st.Quota5hSince, err)
	}
	q5h = fv.Map()
	if st, err := v.Get(ctx, fkeyA); err != nil || st.Quota5hUSD != 0 || !st.Quota5hSince.IsZero() {
		t.Fatalf("no spend: %v since %v (%v)", st.Quota5hUSD, st.Quota5hSince, err)
	}
	q5h = fv.Err("ERR q5h")
	if _, err := v.Get(ctx, fkeyA); err == nil {
		t.Fatal("q5h read error must surface")
	}
}
