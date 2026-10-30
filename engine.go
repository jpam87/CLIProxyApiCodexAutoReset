package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const creditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

type HostCall func(string, any, any) error
type Account struct {
	ID       string `json:"id"`
	Index    string `json:"auth_index"`
	Provider string `json:"provider"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
}
type Credit struct {
	ID      string    `json:"id"`
	Status  string    `json:"status"`
	Type    string    `json:"reset_type"`
	Expires time.Time `json:"expires_at"`
}
type Credits struct {
	Credits    []Credit `json:"credits"`
	Available  *int     `json:"available_count"`
	Applicable *int     `json:"applicable_available_count"`
}
type Attempt struct {
	ExplicitCredit  bool            `json:"explicit_credit"`
	CreditID        string          `json:"credit_id"`
	Expires         time.Time       `json:"expires_at"`
	Cleared         map[string]bool `json:"cleared"`
	Account         string          `json:"account"`
	AuthIndex       string          `json:"auth_index"`
	RequestID       string          `json:"request_id"`
	Confirmed       bool            `json:"confirmed"`
	CooldownCleared bool            `json:"cooldown_cleared"`
}
type Engine struct {
	stopped atomic.Bool
	cfg     Config
	host    HostCall
	now     func() time.Time
	mu      sync.Mutex
	status  map[string]string
	ledger  map[string]Attempt
	lock    *os.File
}

func newEngine(c Config, h HostCall) (*Engine, error) {
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(c.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("state file already owned by another instance")
	}
	e := &Engine{cfg: c, host: h, now: time.Now, status: map[string]string{}, ledger: map[string]Attempt{}, lock: lock}
	b, err := os.ReadFile(c.StateFile)
	if err == nil {
		err = json.Unmarshal(b, &e.ledger)
		if e.ledger == nil {
			err = errors.New("invalid state")
		}
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err == nil {
		uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
		for key, a := range e.ledger {
			want := fmt.Sprintf("%x", sha256.Sum256([]byte(a.Account+"\x00"+a.CreditID)))
			if key != want || a.Account == "" || a.AuthIndex == "" || a.CreditID == "" || a.Expires.IsZero() || !uuid.MatchString(a.RequestID) {
				err = errors.New("invalid journal entry")
				break
			}
		}
	}
	if err != nil {
		e.close()
		return nil, errors.New("cannot load persistent state")
	}
	return e, nil
}
func (e *Engine) close() {
	if e.lock != nil {
		syscall.Flock(int(e.lock.Fd()), syscall.LOCK_UN)
		e.lock.Close()
		e.lock = nil
	}
}
func (e *Engine) save() error {
	b, err := json.Marshal(e.ledger)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.cfg.StateFile), ".autoreset-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, e.cfg.StateFile); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(e.cfg.StateFile))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (e *Engine) snapshot() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := map[string]string{}
	for k, v := range e.status {
		r[k] = v
	}
	return r
}
func (e *Engine) tick() {
	var list struct {
		Files []Account `json:"files"`
	}
	if e.host("host.auth.list", map[string]any{}, &list) != nil {
		e.setStatus("worker", "account_list_failed")
		return
	}
	e.mu.Lock()
	delete(e.status, "worker")
	e.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range list.Files {
		if e.stopped.Load() {
			return
		}
		if a.Index == "" || a.Disabled || (a.Provider != "codex" && a.Type != "codex") {
			continue
		}
		if len(e.cfg.Accounts) > 0 {
			selected := false
			for _, v := range e.cfg.Accounts {
				if v == a.Index {
					selected = true
				}
			}
			if !selected {
				continue
			}
		}
		// Auth aliases for the same upstream account are deduplicated after credential lookup.
		cred, err := e.credential(a.Index)
		if err != nil {
			e.setStatus(a.Index, "credential_unavailable")
			continue
		}
		identity := cred.account
		for key, attempt := range e.ledger {
			if attempt.AuthIndex == a.Index && attempt.Account == identity && attempt.Confirmed && !attempt.Cleared[a.Index] {
				e.setStatus(a.Index, e.clearCooldown(a.Index, key, attempt))
			}
		}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		e.setStatus(a.Index, e.process(a, cred))
	}
}

func (e *Engine) setStatus(key, value string) { e.mu.Lock(); e.status[key] = value; e.mu.Unlock() }

type credential struct{ token, account string }

func (e *Engine) credential(index string) (credential, error) {
	var r struct {
		JSON struct {
			Token   string `json:"access_token"`
			Account string `json:"account_id"`
		} `json:"json"`
	}
	if err := e.host("host.auth.get", map[string]string{"auth_index": index}, &r); err != nil {
		return credential{}, err
	}
	c := credential{r.JSON.Token, r.JSON.Account}
	if c.account == "" {
		parts := strings.Split(c.token, ".")
		if len(parts) == 3 {
			b, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Auth struct {
					ID string `json:"chatgpt_account_id"`
				} `json:"https://api.openai.com/auth"`
			}
			json.Unmarshal(b, &claims)
			c.account = claims.Auth.ID
		}
	}
	if c.token == "" || c.account == "" {
		return c, errors.New("missing credential fields")
	}
	return c, nil
}
func (e *Engine) http(c credential, method, url string, body any, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	// Fixed host and endpoints only; no externally configured URL or credential output.
	req := map[string]any{"method": method, "url": url, "headers": map[string][]string{"Authorization": {"Bearer " + c.token}, "Chatgpt-Account-Id": {c.account}, "Content-Type": {"application/json"}, "Accept": {"application/json"}, "OpenAI-Beta": {"codex-1"}, "Originator": {"Codex Desktop"}, "User-Agent": {"codex-tui/0.149.1"}}, "body": b}
	var op struct {
		ID string `json:"operation_id"`
	}
	if err = e.host("host.http.operation_open", map[string]any{}, &op); err != nil {
		return err
	}
	if op.ID == "" {
		return errors.New("missing HTTP operation")
	}
	req["operation_id"] = op.ID
	// The timer is joined before return, so it cannot call a host after plugin shutdown.
	done := make(chan struct{})
	timer := time.AfterFunc(time.Duration(e.cfg.Timeout)*time.Second, func() {
		defer close(done)
		e.host("host.http.cancel", map[string]string{"operation_id": op.ID}, &struct{}{})
	})
	var resp struct {
		Status int    `json:"StatusCode"`
		Body   []byte `json:"Body"`
	}
	err = e.host("host.http.do", req, &resp)
	if !timer.Stop() {
		<-done
	}
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return fmt.Errorf("upstream status %d", resp.Status)
	}
	return json.Unmarshal(resp.Body, out)
}
func (e *Engine) process(a Account, c credential) string {
	var list Credits
	if e.http(c, "GET", creditsURL, nil, &list) != nil {
		return "credit_lookup_failed"
	}
	available := []Credit{}
	for _, cr := range list.Credits {
		if cr.Status == "available" && cr.Type == "codex_rate_limits" {
			available = append(available, cr)
		}
	}
	sort.Slice(available, func(i, j int) bool {
		if available[i].Expires.Equal(available[j].Expires) {
			return available[i].ID < available[j].ID
		}
		return available[i].Expires.Before(available[j].Expires)
	})
	for key, attempt := range e.ledger {
		if attempt.Account != c.account || attempt.Confirmed {
			continue
		}
		if e.cfg.DryRun {
			return "dry_run_pending"
		}
		// A pre-0.2 attempt omitted credit_id. Changing its payload during a
		// retry would invalidate the original idempotency contract.
		if !attempt.ExplicitCredit {
			return "legacy_pending_requires_review"
		}
		if !attempt.Expires.After(e.now()) {
			return "unconfirmed_expired_requires_review"
		}
		// Both IDs stay fixed even if inventory now contains different credits.
		return e.retryPending(a.Index, key, attempt, c)
	}
	if len(available) == 0 {
		return "no_available_credit"
	}
	now := e.now()
	var cr Credit
	skipped := "no_expiring_credit"
	for _, candidate := range available {
		if candidate.ID == "" {
			skipped = "invalid_credit"
			continue
		}
		if candidate.Expires.IsZero() {
			continue
		}
		if !candidate.Expires.After(now) {
			skipped = "expired"
			continue
		}
		cr = candidate
		break
	}
	if cr.ID == "" {
		return skipped
	}
	if cr.Expires.Sub(now) > time.Duration(e.cfg.Minutes)*time.Minute {
		return "waiting"
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(c.account+"\x00"+cr.ID)))
	attempt, exists := e.ledger[key]
	if attempt.Confirmed {
		return e.clearCooldown(a.Index, key, attempt)
	}
	if list.Applicable != nil && *list.Applicable < 1 {
		return "not_applicable"
	}
	if e.cfg.DryRun {
		return "dry_run_due"
	}
	if e.stopped.Load() {
		return "stopped"
	}
	if !exists {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "random_id_failed"
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		attempt.RequestID = fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
		attempt.AuthIndex = a.Index
		attempt.Account = c.account
		attempt.CreditID = cr.ID
		attempt.Expires = cr.Expires
		attempt.ExplicitCredit = true
		e.ledger[key] = attempt
	}
	return e.retryPending(a.Index, key, attempt, c)
}
func (e *Engine) retryPending(index, key string, attempt Attempt, c credential) string {
	if !attempt.ExplicitCredit || attempt.CreditID == "" {
		return "legacy_pending_requires_review"
	}
	if e.cfg.DryRun {
		return "dry_run_pending"
	}
	if e.stopped.Load() {
		return "stopped"
	}
	if e.save() != nil {
		return "state_write_failed"
	}
	if !attempt.Expires.After(e.now()) {
		return "unconfirmed_expired_requires_review"
	}
	if e.stopped.Load() {
		return "stopped"
	}
	var resp struct {
		Code string `json:"code"`
	}
	if e.http(c, "POST", creditsURL+"/consume", map[string]string{"redeem_request_id": attempt.RequestID, "credit_id": attempt.CreditID}, &resp) != nil ||
		(resp.Code != "reset" && resp.Code != "already_redeemed") {
		return "consume_unconfirmed_retry_same_id"
	}
	attempt.Confirmed = true
	e.ledger[key] = attempt
	if e.save() != nil {
		return "confirmation_write_failed"
	}
	return e.clearCooldown(index, key, attempt)
}
func (e *Engine) clearCooldown(index, key string, a Attempt) string {
	// The initiating credential is the only recovery target recorded for this
	// redemption. Historical resets must never clear a newly created alias.
	if index != a.AuthIndex {
		return "redeemed_alias_not_targeted"
	}
	if a.Cleared[index] {
		return "redeemed"
	}
	var r struct {
		Index string `json:"auth_index"`
	}
	if e.host("host.routing.reset_cooldown", map[string]string{"auth_index": index}, &r) != nil || r.Index != index {
		return "redeemed_cooldown_pending"
	}
	a.CooldownCleared = true
	if a.Cleared == nil {
		a.Cleared = map[string]bool{}
	}
	a.Cleared[index] = true
	e.ledger[key] = a
	if e.save() != nil {
		return "confirmation_write_failed"
	}
	return "redeemed"
}
