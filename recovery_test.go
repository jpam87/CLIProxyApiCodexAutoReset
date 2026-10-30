package main

import (
	"testing"
	"time"
)

func TestHistoricalRedemptionDoesNotClearNewAlias(t *testing.T) {
	e, f := harness(t, false, time.Now().Add(time.Minute))
	if got := e.process(Account{Index: "idx"}, credential{"tok", "acct"}); got != "redeemed" {
		t.Fatal(got)
	}
	f.credits.Credits = nil
	f.credits.Available = intp(0)
	f.accounts = []Account{{Index: "new-alias", Provider: "codex"}}
	f.calls = nil
	e.tick()
	for _, call := range f.calls {
		if call == "host.routing.reset_cooldown" {
			t.Fatal("historical reset cleared a new alias")
		}
	}
}

func TestStatusSnapshotDoesNotWaitForNetwork(t *testing.T) {
	e, f := harness(t, true, time.Now().Add(time.Minute))
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	e.host = func(method string, payload, out any) error {
		if method == "host.http.do" {
			close(entered)
			<-release
		}
		return f.call(method, payload, out)
	}
	go func() { defer close(finished); e.tick() }()
	<-entered
	snapshot := make(chan struct{})
	go func() { e.snapshot(); close(snapshot) }()
	select {
	case <-snapshot:
	case <-time.After(time.Second):
		close(release)
		<-finished
		t.Fatal("status blocked behind network")
	}
	close(release)
	<-finished
}
