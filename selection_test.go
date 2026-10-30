package main

import (
	"testing"
	"time"
)

func TestSelectEarliestExpiringCreditByID(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name         string
		credits      []Credit
		want, status string
	}{
		{"unordered multiple", []Credit{
			{ID: "later", Status: "available", Type: "codex_rate_limits", Expires: now.Add(24 * time.Hour)},
			{ID: "soon", Status: "available", Type: "codex_rate_limits", Expires: now.Add(5 * time.Minute)},
			{ID: "middle", Status: "available", Type: "codex_rate_limits", Expires: now.Add(time.Hour)},
		}, "soon", "redeemed"},
		{"expired and nonexpiring", []Credit{
			{ID: "no-expiry", Status: "available", Type: "codex_rate_limits"},
			{ID: "expired", Status: "available", Type: "codex_rate_limits", Expires: now.Add(-time.Minute)},
			{ID: "valid", Status: "available", Type: "codex_rate_limits", Expires: now.Add(time.Minute)},
		}, "valid", "redeemed"},
		{"redeemed and wrong type", []Credit{
			{ID: "used", Status: "redeemed", Type: "codex_rate_limits", Expires: now.Add(time.Minute)},
			{ID: "wrong-type", Status: "available", Type: "other", Expires: now.Add(time.Minute)},
			{ID: "valid", Status: "available", Type: "codex_rate_limits", Expires: now.Add(9 * time.Minute)},
		}, "valid", "redeemed"},
		{"tie broken by ID", []Credit{
			{ID: "b", Status: "available", Type: "codex_rate_limits", Expires: now.Add(time.Minute)},
			{ID: "a", Status: "available", Type: "codex_rate_limits", Expires: now.Add(time.Minute)},
		}, "a", "redeemed"},
		{"all future", []Credit{
			{ID: "later", Status: "available", Type: "codex_rate_limits", Expires: now.Add(time.Hour)},
			{ID: "earlier", Status: "available", Type: "codex_rate_limits", Expires: now.Add(20 * time.Minute)},
		}, "", "waiting"},
		{"nonexpiring only", []Credit{{ID: "forever", Status: "available", Type: "codex_rate_limits"}}, "", "no_expiring_credit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, f := harness(t, false, now.Add(time.Minute))
			e.now = func() time.Time { return now }
			f.credits.Credits = tc.credits
			f.credits.Available = intp(len(tc.credits))
			got := e.process(Account{Index: "idx"}, credential{"tok", "acct"})
			if got != tc.status {
				t.Fatalf("got %s want %s", got, tc.status)
			}
			if tc.want == "" {
				if f.postCount != 0 {
					t.Fatal("unexpected redemption")
				}
				return
			}
			if len(f.postCreditIDs) != 1 || f.postCreditIDs[0] != tc.want {
				t.Fatalf("sent credit IDs %v, want %s", f.postCreditIDs, tc.want)
			}
		})
	}
}

func TestPendingRetainsSelectedCreditAcrossInventoryChangeAndRestart(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	f.postErr = true
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
		t.Fatal(got)
	}
	requestID := f.postIDs[0]
	e.close()
	e2, err := newEngine(e.cfg, f.call)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	f.postErr = false
	f.postCode = "already_redeemed"
	f.credits.Credits = []Credit{{ID: "different", Status: "available", Type: "codex_rate_limits", Expires: time.Now().Add(time.Minute)}}
	if got := e2.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed" {
		t.Fatal(got)
	}
	if len(f.postIDs) != 2 || f.postIDs[1] != requestID || f.postCreditIDs[1] != "credit" {
		t.Fatalf("changed retry payload: %v %v", f.postIDs, f.postCreditIDs)
	}
}

func TestUnsupportedSelectedCreditHasNoUntargetedFallback(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	f.postStatus = 400
	for i := 0; i < 2; i++ {
		if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
			t.Fatal(got)
		}
	}
	if len(f.postCreditIDs) != 2 || f.postCreditIDs[0] != "credit" || f.postCreditIDs[1] != "credit" || f.postIDs[0] != f.postIDs[1] {
		t.Fatal("fallback or changed idempotency key", f.postCreditIDs, f.postIDs)
	}
}

func TestLegacyPendingDoesNotChangePayloadContract(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	f.postErr = true
	e.process(Account{Index: "idx"}, credential{"tok", "acct"})
	for key, a := range e.ledger {
		a.ExplicitCredit = false
		e.ledger[key] = a
	}
	e.save()
	e.close()
	e2, err := newEngine(e.cfg, f.call)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	if got := e2.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "legacy_pending_requires_review" {
		t.Fatal(got)
	}
	if f.postCount != 1 {
		t.Fatal("replayed legacy payload with changed contract")
	}
}
