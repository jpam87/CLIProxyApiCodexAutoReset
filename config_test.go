package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DryRun != true || c.Minutes != 10 || c.Poll != 15 || c.Timeout != 30 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if !strings.HasPrefix(c.StateFile, "/") {
		t.Fatalf("default state path is not absolute: %q", c.StateFile)
	}
	if c.interval() != 15*time.Second {
		t.Fatalf("interval = %s", c.interval())
	}
}

func TestParseConfigValuesAndValidation(t *testing.T) {
	c, err := parseConfig([]byte("enabled: true\ndry_run: false\nminutes_before_expiry: 1\npoll_seconds: 60\nrequest_timeout_seconds: 120\nstate_file: /tmp/state.json\naccounts: [one, two]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.DryRun || c.Minutes != 1 || c.Poll != 60 || c.Timeout != 120 || c.StateFile != "/tmp/state.json" || len(c.Accounts) != 2 {
		t.Fatalf("parsed values differ: %+v", c)
	}
	for _, raw := range []string{
		"minutes_before_expiry: 0", "minutes_before_expiry: 61", "poll_seconds: 0",
		"poll_seconds: 61", "request_timeout_seconds: 0", "request_timeout_seconds: 121",
		"state_file: relative.json", "[invalid yaml",
	} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("parseConfig(%q) unexpectedly succeeded", raw)
		}
	}
}
