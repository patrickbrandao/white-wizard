package storetest

import (
	"context"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// Responses API registry (cache/valkey/README.md 2.5, ADR 0055), exercised
// only when the store implements limits.ResponseRegistry. Expiry is
// backend-specific (key TTL in Valkey, checked on read in memory) and is
// covered by each implementation's own tests.

func responseCases() []testCase {
	return []testCase{
		{"PutGet", testRespPutGet},
		{"NotFound", testRespNotFound},
		{"OtherAccount", testRespOtherAccount},
		{"Overwrite", testRespOverwrite},
		{"EscapedID", testRespEscapedID},
		{"ResetClears", testRespResetClears},
		{"PartialResetsKeep", testRespPartialResetsKeep},
	}
}

func responseRegistry(t T, s limits.Store) limits.ResponseRegistry {
	t.Helper()
	r, ok := s.(limits.ResponseRegistry)
	if !ok {
		t.Skip("store does not implement limits.ResponseRegistry")
	}
	return r
}

// respRec is a full record created at at.
func respRec(at time.Time) limits.ResponseRecord {
	return limits.ResponseRecord{
		Account: "acct-1", Driver: "openai", Model: "gpt-4o-mini",
		Messages: 3, Tokens: 27, Prefix: pcH1, At: at,
	}
}

func respPut(t T, r limits.ResponseRegistry, id string, rec limits.ResponseRecord) {
	t.Helper()
	if err := r.ResponsePut(context.Background(), id, rec); err != nil {
		t.Fatalf("ResponsePut(%q): %v", id, err)
	}
}

// respGet asserts the lookup of id by account: want nil means not found.
func respGet(t T, r limits.ResponseRegistry, id, account string, now time.Time, want *limits.ResponseRecord) {
	t.Helper()
	got, err := r.ResponseGet(context.Background(), id, account, now)
	if err != nil {
		t.Fatalf("ResponseGet(%q): %v", id, err)
	}
	switch {
	case want == nil && got != nil:
		t.Errorf("ResponseGet(%q, %q) = %+v, want not found", id, account, *got)
	case want != nil && got == nil:
		t.Errorf("ResponseGet(%q, %q) not found, want %+v", id, account, *want)
	case want != nil && !sameResp(*got, *want):
		t.Errorf("ResponseGet(%q, %q) = %+v, want %+v", id, account, *got, *want)
	}
}

func sameResp(a, b limits.ResponseRecord) bool {
	return a.Account == b.Account && a.Driver == b.Driver && a.Model == b.Model &&
		a.Messages == b.Messages && a.Tokens == b.Tokens && a.Prefix == b.Prefix &&
		a.At.UnixMilli() == b.At.UnixMilli()
}

// testRespPutGet: every field comes back, with At at ms precision, on
// repeated reads.
func testRespPutGet(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	rec := respRec(now.Add(-time.Minute).Add(123456 * time.Nanosecond))
	respPut(t, r, "resp_1", rec)
	respGet(t, r, "resp_1", "acct-1", now, &rec)
	respGet(t, r, "resp_1", "acct-1", now.Add(time.Hour), &rec)
	empty := limits.ResponseRecord{Account: "acct-1", At: now}
	respPut(t, r, "resp_empty", empty)
	respGet(t, r, "resp_empty", "acct-1", now, &empty)
}

// testRespNotFound: an id never written is not found.
func testRespNotFound(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	respGet(t, r, "resp_missing", "acct-1", now, nil)
	respPut(t, r, "resp_1", respRec(now))
	respGet(t, r, "resp_2", "acct-1", now, nil)
}

// testRespOtherAccount: a record of another account (or of no account
// asked by one) is not found and stays readable by its owner.
func testRespOtherAccount(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	rec := respRec(now)
	respPut(t, r, "resp_1", rec)
	respGet(t, r, "resp_1", "acct-2", now, nil)
	respGet(t, r, "resp_1", "", now, nil)
	respGet(t, r, "resp_1", "acct-1", now, &rec)
}

// testRespOverwrite: a repeated id overwrites the record.
func testRespOverwrite(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	respPut(t, r, "resp_1", respRec(now))
	rec := limits.ResponseRecord{Account: "acct-2", Driver: "azure", Model: "gpt-4o", Messages: 5, Tokens: 99, At: now.Add(time.Second)}
	respPut(t, r, "resp_1", rec)
	respGet(t, r, "resp_1", "acct-1", now, nil)
	respGet(t, r, "resp_1", "acct-2", now, &rec)
}

// testRespEscapedID: ids with reserved bytes are distinct records.
func testRespEscapedID(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	a, b := respRec(now), respRec(now)
	b.Tokens = 1
	respPut(t, r, "resp:a{b}", a)
	respPut(t, r, "resp%3Aa%7Bb%7D", b)
	respGet(t, r, "resp:a{b}", "acct-1", now, &a)
	respGet(t, r, "resp%3Aa%7Bb%7D", "acct-1", now, &b)
}

// testRespResetClears: Reset drops the registry.
func testRespResetClears(t T, s limits.Store) {
	r := responseRegistry(t, s)
	now := time.Now()
	respPut(t, r, "resp_1", respRec(now))
	if err := s.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	respGet(t, r, "resp_1", "acct-1", now, nil)
}

// testRespPartialResetsKeep: ResetCounters, ResetEntity and
// ResetConversations keep the registry (README 2.5).
func testRespPartialResetsKeep(t T, s limits.Store) {
	r := responseRegistry(t, s)
	ctx := context.Background()
	now := time.Now()
	rec := respRec(now)
	respPut(t, r, "resp_1", rec)
	if cr, ok := s.(limits.CounterResetter); ok {
		if err := cr.ResetCounters(ctx); err != nil {
			t.Fatalf("ResetCounters: %v", err)
		}
	}
	if er, ok := s.(limits.EntityResetter); ok {
		if err := er.ResetEntity(ctx, accountEnt, true, true); err != nil {
			t.Fatalf("ResetEntity: %v", err)
		}
	}
	if cr, ok := s.(limits.ConversationResetter); ok {
		if _, err := cr.ResetConversations(ctx, ""); err != nil {
			t.Fatalf("ResetConversations: %v", err)
		}
	}
	respGet(t, r, "resp_1", "acct-1", now, &rec)
}
