package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeHost struct {
	mu             sync.Mutex
	accounts       []Account
	token, account string
	credits        Credits
	postCode       string
	postCount      int
	postIDs        []string
	postCreditIDs  []string
	postStatus     int
	postErr        bool
	accountByIndex map[string]string
	calls          []string
	failState      bool
}

func (f *fakeHost) call(method string, payload, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	var v any
	switch method {
	case "host.auth.list":
		v = struct {
			Files []Account `json:"files"`
		}{f.accounts}
	case "host.auth.get":
		v = struct {
			JSON struct {
				Token   string `json:"access_token"`
				Account string `json:"account_id"`
			} `json:"json"`
		}{}
	case "host.http.operation_open":
		v = struct {
			ID string `json:"operation_id"`
		}{"op-1"}
	case "host.http.do":
		var req struct {
			Method string `json:"method"`
			Body   []byte `json:"body"`
		}
		b, _ := json.Marshal(payload)
		_ = json.Unmarshal(b, &req)
		if req.Method == "POST" {
			f.postCount++
			var body struct {
				RequestID string `json:"redeem_request_id"`
				CreditID  string `json:"credit_id"`
			}
			_ = json.Unmarshal(req.Body, &body)
			f.postIDs = append(f.postIDs, body.RequestID)
			f.postCreditIDs = append(f.postCreditIDs, body.CreditID)
			if f.postErr {
				return testingError("response lost after upstream consume")
			}
			v = struct {
				Status int    `json:"StatusCode"`
				Body   []byte `json:"Body"`
			}{func() int {
				if f.postStatus != 0 {
					return f.postStatus
				}
				return 200
			}(), mustJSON(map[string]string{"code": f.postCode})}
		} else {
			v = struct {
				Status int    `json:"StatusCode"`
				Body   []byte `json:"Body"`
			}{200, mustJSON(f.credits)}
		}
	case "host.routing.reset_cooldown":
		if f.failState {
			return testingError("state reset failed")
		}
		v = struct {
			Index string `json:"auth_index"`
		}{"idx"}
	case "host.http.cancel":
		v = struct{}{}
	default:
		return testingError("unexpected host method: " + method)
	}
	if method == "host.auth.get" {
		v = struct {
			JSON struct {
				Token   string `json:"access_token"`
				Account string `json:"account_id"`
			} `json:"json"`
		}{JSON: struct {
			Token   string `json:"access_token"`
			Account string `json:"account_id"`
		}{f.token, func() string {
			if f.accountByIndex != nil {
				var req struct {
					Index string `json:"auth_index"`
				}
				b, _ := json.Marshal(payload)
				_ = json.Unmarshal(b, &req)
				if a := f.accountByIndex[req.Index]; a != "" {
					return a
				}
			}
			return f.account
		}()}}
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

type testingError string

func (e testingError) Error() string { return string(e) }
func mustJSON(v any) []byte          { b, _ := json.Marshal(v); return b }

func harness(t *testing.T, dry bool, expires time.Time) (*Engine, *fakeHost) {
	t.Helper()
	f := &fakeHost{accounts: []Account{{Index: "idx", Provider: "codex"}}, token: "tok", account: "acct", postCode: "reset", credits: Credits{Credits: []Credit{{ID: "credit", Status: "available", Type: "codex_rate_limits", Expires: expires}}, Available: intp(1), Applicable: intp(1)}}
	c := Config{DryRun: dry, Minutes: 10, Timeout: 1, StateFile: filepath.Join(t.TempDir(), "state.json")}
	e, err := newEngine(c, f.call)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.close)
	return e, f
}
func intp(v int) *int { return &v }

func TestProcessCreditWindowAndGuards(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		delta time.Duration
		dry   bool
		want  string
		posts int
	}{
		{"before window", 11 * time.Minute, false, "waiting", 0}, {"ten minute boundary", 10 * time.Minute, true, "dry_run_due", 0},
		{"expired", -time.Minute, false, "expired", 0}, {"dry run", 5 * time.Minute, true, "dry_run_due", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f := harness(t, tc.dry, now.Add(tc.delta))
			e.now = func() time.Time { return now }
			got := e.process(Account{Index: "idx"}, credential{"tok", "acct"})
			if got != tc.want || f.postCount != tc.posts {
				t.Fatalf("got %q posts=%d", got, f.postCount)
			}
		})
	}
}

func TestProcessRejectsMissingCreditID(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	f.credits.Credits[0].ID = ""
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "invalid_credit" || f.postCount != 0 {
		t.Fatalf("got %q posts=%d", got, f.postCount)
	}
}

func TestProcessConsumeCooldownAndRetryReuse(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	e.now = time.Now
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed" || f.postCount != 1 {
		t.Fatalf("first: %q posts=%d", got, f.postCount)
	}
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed" || f.postCount != 1 {
		t.Fatalf("cooldown: %q posts=%d", got, f.postCount)
	}
	// A transport-level failure leaves the same request ID in the journal for retry.
	f.postCode = "unknown"
	f.credits.Credits[0].Expires = time.Now().Add(time.Minute)
	key := ""
	for k := range e.ledger {
		key = k
	}
	id := e.ledger[key].RequestID
	attempt := e.ledger[key]
	attempt.Confirmed = false
	attempt.CooldownCleared = false
	attempt.Cleared = nil
	e.ledger[key] = attempt
	_ = e.save()
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
		t.Fatalf("retry: %q", got)
	}
	if e.ledger[key].RequestID != id {
		t.Fatal("request id changed on retry")
	}
	statePath := e.cfg.StateFile
	e.close()
	e2, err := newEngine(e.cfg, f.call)
	if err != nil {
		t.Fatal(err)
	}
	e2.now = time.Now
	defer e2.close()
	if got := e2.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
		t.Fatalf("restart retry: %q", got)
	}
	var persistedKey string
	for k := range e2.ledger {
		persistedKey = k
	}
	if e2.ledger[persistedKey].RequestID != id || e2.cfg.StateFile != statePath {
		t.Fatal("request id was not persisted across restart")
	}
}

func TestLostPostResponseReplayAfterRestartWithEmptyInventory(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	e.now = time.Now
	f.postErr = true
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
		t.Fatalf("lost response: %q", got)
	}
	if len(f.postIDs) != 1 || f.postIDs[0] == "" {
		t.Fatalf("POST IDs after first attempt: %v", f.postIDs)
	}
	id := f.postIDs[0]
	e.close()
	e2, err := newEngine(e.cfg, f.call)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	e2.now = time.Now
	f.postErr = false
	f.postCode = "already_redeemed"
	f.credits = Credits{Available: intp(0), Applicable: intp(0)}
	if got := e2.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed" {
		t.Fatalf("replay with empty inventory: %q", got)
	}
	if len(f.postIDs) != 2 || f.postIDs[1] != id || len(f.postCreditIDs) != 2 || f.postCreditIDs[0] != "credit" || f.postCreditIDs[1] != "credit" {
		t.Fatalf("replay IDs = %v, want same ID %q", f.postIDs, id)
	}
}

func TestPendingAttemptRefusesUnsafeReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dry     bool
		expired bool
		want    string
	}{
		{"dry run", true, false, "dry_run_pending"},
		{"expired", false, true, "unconfirmed_expired_requires_review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expires := time.Now().Add(time.Minute)
			e, f := harness(t, false, expires)
			e.now = time.Now
			f.postErr = true
			if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "consume_unconfirmed_retry_same_id" {
				t.Fatalf("seed pending: %q", got)
			}
			before := f.postCount
			if tc.expired {
				for k, a := range e.ledger {
					a.Expires = time.Now().Add(-time.Minute)
					e.ledger[k] = a
				}
				f.credits.Credits = nil
				f.credits.Available = intp(0)
			}
			e.cfg.DryRun = tc.dry
			got := e.process(Account{Index: "idx"}, credential{"tok", "acct"})
			if got != tc.want || f.postCount != before {
				t.Fatalf("got %q posts=%d want %q and no new POST", got, f.postCount, tc.want)
			}
		})
	}
}

func TestConfirmedCooldownRecoveryRequiresSameUpstreamAccount(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	e.now = time.Now
	f.failState = true
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed_cooldown_pending" {
		t.Fatalf("confirm with failed cooldown: %q", got)
	}
	f.failState = false
	f.credits = Credits{Available: intp(0)}
	f.accountByIndex = map[string]string{"idx": "other-account"}
	f.calls = nil
	e.tick()
	if len(f.postIDs) != 1 {
		t.Fatalf("unexpected consume retries: %v", f.postIDs)
	}
	for _, call := range f.calls {
		if call == "host.routing.reset_cooldown" {
			t.Fatal("cooldown cleared for changed upstream identity")
		}
	}
}

func TestJournalSaveFailurePreventsConsume(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	e.now = time.Now
	e.cfg.StateFile = filepath.Join(t.TempDir(), "missing", "state.json")
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "state_write_failed" || f.postCount != 0 {
		t.Fatalf("got %q, posts=%d", got, f.postCount)
	}
}

func TestCorruptJournalFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"bad":{"account":"acct","auth_index":"idx","credit_id":"credit","request_id":"not-a-uuid"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newEngine(Config{StateFile: path}, func(string, any, any) error { return nil })
	if err == nil {
		t.Fatal("malformed journal was accepted")
	}
}

func TestTickFiltersAccountsAndDisabled(t *testing.T) {
	e, f := harness(t, true, time.Now().Add(time.Minute))
	e.now = time.Now
	f.accounts = []Account{{Index: "idx", Provider: "codex"}, {Index: "disabled", Provider: "codex", Disabled: true}, {Index: "other", Provider: "other"}, {Index: "idx2", Provider: "codex"}}
	e.cfg.Accounts = []string{"idx"}
	e.tick()
	if _, ok := e.status["idx"]; !ok {
		t.Fatal("selected account not processed")
	}
	if _, ok := e.status["disabled"]; ok {
		t.Fatal("disabled account processed")
	}
	if _, ok := e.status["idx2"]; ok {
		t.Fatal("unselected account processed")
	}
}
