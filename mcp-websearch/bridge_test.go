// bridge_test.go — level-4 bridge tests. All sequential; the only goroutines
// are fake extensions (test-side stand-ins for the real MV3 worker) and the
// owner's own listener. No real browser is involved: handlers answer canned
// Results, exactly like extension/background.js would over the wire.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestBridge builds an unelected bridge with test-friendly timings.
func newTestBridge(t *testing.T, addr, tokenFile string, minInterval, jobTimeout time.Duration) *bridge {
	t.Helper()
	return &bridge{
		addr:        addr,
		tokenFile:   tokenFile,
		minInterval: minInterval,
		jobTimeout:  jobTimeout,
		httpCli:     &http.Client{Timeout: minInterval + jobTimeout + 5*time.Second},
		probeCli:    &http.Client{Timeout: 2 * time.Second},
	}
}

// TestDetectBrowserPath: explicit --browser-cmd passes through verbatim;
// autodetect returns either "" (nothing installed — exotic box) or an
// existing non-directory path. No launch happens here.
func TestDetectBrowserPath(t *testing.T) {
	if got := detectBrowserPath(t.TempDir() + "/explicit-browser"); !strings.HasSuffix(got, "explicit-browser") {
		t.Fatalf("explicit passthrough: %q", got)
	}
	if got := detectBrowserPath(""); got != "" {
		if fi, err := os.Stat(got); err != nil || fi.IsDir() {
			t.Fatalf("autodetected %q does not exist", got)
		}
	}
}

// TestMaybeStartBrowserDisabledNoop: with auto-start OFF the call must not
// even arm the debounce (no side effects, no logs, no browser).
func TestMaybeStartBrowserDisabledNoop(t *testing.T) {
	b := newTestBridge(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "token"), time.Millisecond, 50*time.Millisecond)
	b.role = bridgeOwner
	b.maybeStartBrowser() // autoStart=false — must be a silent no-op
	if !b.lastBrowserStart.IsZero() {
		t.Fatal("disabled auto-start must not arm the debounce")
	}
}

// TestMaybeStartBrowserDebounce: with auto-start ON but a start attempt
// already made <60s ago, the second call is a no-op (the debounce holds even
// while the extension stays offline).
func TestMaybeStartBrowserDebounce(t *testing.T) {
	b := newTestBridge(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "token"), time.Millisecond, 50*time.Millisecond)
	b.role = bridgeOwner
	b.autoStart = true
	b.browserCmd = "definitely-not-a-real-browser-binary"
	b.lastBrowserStart = time.Now().Add(-10 * time.Second) // 10s ago — inside the 60s window
	b.maybeStartBrowser()
	if since := time.Since(b.lastBrowserStart); since > 15*time.Second {
		t.Fatalf("debounce was reset despite a recent start attempt (since=%v)", since)
	}
}

// fakeExt mimics extension/background.js: poll /job, answer via /result.
// The handler runs in the fake worker's goroutine — guard swaps with mu.
type fakeExt struct {
	t     *testing.T
	addr  string
	token string

	mu      sync.Mutex
	handler func(*Job) *Result
}

func (f *fakeExt) setHandler(h func(*Job) *Result) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

func (f *fakeExt) run(stop <-chan struct{}) {
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, err := http.NewRequest(http.MethodGet, "http://"+f.addr+"/job", nil)
			if err != nil {
				return
			}
			req.Header.Set("X-Bridge-Token", f.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				time.Sleep(20 * time.Millisecond) // bridge down — reconnect loop
				continue
			}
			if resp.StatusCode == http.StatusNoContent {
				resp.Body.Close()
				time.Sleep(10 * time.Millisecond)
				continue
			}
			var j Job
			_ = json.NewDecoder(resp.Body).Decode(&j)
			resp.Body.Close()
			f.mu.Lock()
			h := f.handler
			f.mu.Unlock()
			var res *Result
			if h == nil {
				res = &Result{OK: true, Status: 200, Body: "ok"}
			} else {
				res = h(&j)
			}
			res.ID = j.ID
			payload, _ := json.Marshal(res)
			req2, _ := http.NewRequest(http.MethodPost, "http://"+f.addr+"/result/"+j.ID, bytes.NewReader(payload))
			req2.Header.Set("X-Bridge-Token", f.token)
			if r2, err := http.DefaultClient.Do(req2); err == nil {
				r2.Body.Close()
			}
		}
	}()
}

func TestMailboxInvariants(t *testing.T) {
	m := &mailbox{}

	// offer → take is FIFO-of-one; second offer while held = busy
	if !m.offerJob(&Job{ID: "a"}) {
		t.Fatal("first offerJob must succeed on empty mailbox")
	}
	if m.offerJob(&Job{ID: "b"}) {
		t.Fatal("second offerJob must be refused (single slot, no queue)")
	}
	if j := m.takeJob(); j == nil || j.ID != "a" {
		t.Fatalf("takeJob = %v, want job a", j)
	}
	if j := m.takeJob(); j != nil {
		t.Fatalf("takeJob after take = %v, want nil", j)
	}

	// stale result must not satisfy a different waiter
	m.putResult(&Result{ID: "old", OK: true, Body: "stale"})
	if r := m.takeResult("new"); r != nil {
		t.Fatalf("takeResult(new) = %v, want nil (ID mismatch)", r)
	}
	if r := m.takeResult("old"); r == nil || r.Body != "stale" {
		t.Fatalf("takeResult(old) = %v, want the stored result", r)
	}
	if r := m.takeResult("old"); r != nil {
		t.Fatalf("result must be consumed once, got %v", r)
	}

	// offerJob wipes the previous result: no cross-job leakage even if the
	// extension is one job behind
	m.putResult(&Result{ID: "x", OK: true})
	m.offerJob(&Job{ID: "y"})
	if r := m.takeResult("x"); r != nil {
		t.Fatal("offerJob must clear a stale result")
	}

	// dropJob only drops ITS OWN job
	m.offerJob(&Job{ID: "keep"})
	m.dropJob("other")
	if m.takeJob() == nil {
		t.Fatal("dropJob(other) must not free the slot")
	}
	m.offerJob(&Job{ID: "drop"})
	m.dropJob("drop")
	if m.takeJob() != nil {
		t.Fatal("dropJob(drop) must free the slot")
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token")
	tok, err := loadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Fatalf("token = %q, want 64 hex chars", tok)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// POSIX permission bits exist only on Unix — Windows maps everything to
	// the profile's default DACL, so the 0600 assert is Unix-only (privacy on
	// Windows comes from the file living under the user profile).
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", fi.Mode().Perm())
	}
	again, err := loadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if again != tok {
		t.Fatal("second load must return the SAME token, not regenerate")
	}
}

// TestGateIssuePace exercises the politeness clock in isolation: back-to-back
// gates each wait ≈minInterval (a burst cannot compress the gap), and after a
// quiet spell the next gate is free. The claim-per-sleep design keeps the
// debt bounded (see gateIssue comment) — asserted via total elapsed time.
func TestGateIssuePace(t *testing.T) {
	const interval = 150 * time.Millisecond
	b := newTestBridge(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "token"), interval, time.Second)

	t0 := time.Now()
	b.gateIssue() // first issue: free
	if e := time.Since(t0); e > 30*time.Millisecond {
		t.Fatalf("first gate took %v, want ~0", e)
	}

	b.gateIssue()
	b.gateIssue()
	// two chained gates must span >= 2 intervals (minus scheduler slack)
	if e := time.Since(t0); e < 2*interval-30*time.Millisecond {
		t.Fatalf("3 gates took %v, want >= ~2 intervals", e)
	}

	time.Sleep(interval) // quiet spell beyond the debt
	fast := time.Now()
	b.gateIssue()
	if e := time.Since(fast); e > 30*time.Millisecond {
		t.Fatalf("gate after idle took %v, want ~0 (debt must not linger)", e)
	}
}

func TestBridgeFlowElectionsAPI(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	const minInterval = 60 * time.Millisecond
	const jobTimeout = 2 * time.Second

	// --- owner elects on an ephemeral port ---
	a := newTestBridge(t, "127.0.0.1:0", tokenFile, minInterval, jobTimeout)
	if err := a.ensure(); err != nil {
		t.Fatal(err)
	}
	if a.role != bridgeOwner {
		t.Fatalf("first bridge role = %v, want owner", a.role)
	}
	defer a.close()

	// --- auth surface: no token → 401, wrong token → 401, right → 200 ---
	probe := func(headers map[string]string) int {
		req, _ := http.NewRequest(http.MethodGet, "http://"+a.addr+"/health", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := probe(nil); code != http.StatusUnauthorized {
		t.Fatalf("/health without token = %d, want 401", code)
	}
	if code := probe(map[string]string{"X-Bridge-Token": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("/health wrong token = %d, want 401", code)
	}
	if code := probe(map[string]string{"X-Bridge-Token": a.token}); code != http.StatusOK {
		t.Fatalf("/health right token = %d, want 200", code)
	}

	// --- wire shape: empty /job = 204; a job round-trips through the slot ---
	getJob := func() (*Job, int) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+a.addr+"/job", nil)
		req.Header.Set("X-Bridge-Token", a.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil, resp.StatusCode
		}
		var j Job
		_ = json.NewDecoder(resp.Body).Decode(&j)
		return &j, resp.StatusCode
	}
	if _, code := getJob(); code != http.StatusNoContent {
		t.Fatalf("empty /job = %d, want 204", code)
	}
	a.mb.offerJob(&Job{ID: "wire", Action: actionFetch, URL: "https://x/"})
	j, code := getJob()
	if code != http.StatusOK || j == nil || j.ID != "wire" || j.Action != actionFetch || j.URL != "https://x/" {
		t.Fatalf("/job = (%v, %d), want the offered job", j, code)
	}
	if _, code := getJob(); code != http.StatusNoContent {
		t.Fatal("job must be handed out exactly once")
	}

	// --- fake extension answers a real dispatch ---
	stop := make(chan struct{})
	ext := &fakeExt{t: t, addr: a.addr, token: a.token}
	ext.setHandler(func(j *Job) *Result {
		return &Result{OK: true, Status: 201, FinalURL: j.URL + "final", Body: "<html><body><p>hello profile</p></body></html>"}
	})
	ext.run(stop)
	defer close(stop)

	res, err := a.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://x/"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 201 || res.FinalURL != "https://x/final" {
		t.Fatalf("dispatch result = %+v", res)
	}

	// --- busy: slot held → immediate refusal, never a wait ---
	a.mb.offerJob(&Job{ID: "held"})
	busyStart := time.Now()
	_, err = a.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://x/"})
	if elapsed := time.Since(busyStart); elapsed > 100*time.Millisecond {
		t.Fatalf("busy answer took %v, want immediate", elapsed)
	}
	var busy *busyError
	if !errors.As(err, &busy) || busy.retryMs <= 0 {
		t.Fatalf("dispatch while busy = %v, want busyError", err)
	}
	a.mb.takeJob() // free the slot

	// --- client election: second process on the same port becomes a client ---
	b := newTestBridge(t, a.addr, tokenFile, minInterval, jobTimeout)
	if err := b.ensure(); err != nil {
		t.Fatal(err)
	}
	if b.role != bridgeClient {
		t.Fatalf("second bridge role = %v, want client", b.role)
	}
	// client dispatch rides /api through the owner
	res, err = b.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://via-api/"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 201 || !strings.Contains(res.FinalURL, "via-api") {
		t.Fatalf("client dispatch result = %+v", res)
	}
	// busy propagates through /api as a 503 → busyError
	a.mb.offerJob(&Job{ID: "held2"})
	_, err = b.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://x/"})
	if !errors.As(err, &busy) {
		t.Fatalf("client busy = %v, want busyError", err)
	}
	a.mb.takeJob()

	// --- takeover: owner dies → client re-elects → becomes owner ---
	a.close()
	res, err = b.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://after-takeover/"})
	if err != nil {
		t.Fatalf("dispatch after owner death should take over, got %v", err)
	}
	if b.role != bridgeOwner {
		t.Fatalf("post-takeover role = %v, want owner", b.role)
	}
	if res.Status != 201 || !strings.Contains(res.FinalURL, "after-takeover") {
		t.Fatalf("post-takeover result = %+v", res)
	}
	b.close()

	// --- a third process with a DIFFERENT token must be refused ---
	foreignFile := filepath.Join(t.TempDir(), "other-token")
	// pre-create a different token so both sides exist
	if _, err := loadOrCreateToken(foreignFile); err != nil {
		t.Fatal(err)
	}
	owner := newTestBridge(t, "127.0.0.1:0", tokenFile, minInterval, jobTimeout)
	if err := owner.ensure(); err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	stranger := newTestBridge(t, owner.addr, foreignFile, minInterval, jobTimeout)
	if err := stranger.ensure(); err == nil {
		t.Fatal("different-token process must NOT become a client of this bridge")
	}
}

// TestBridgeTimeoutFreesSlot: extension offline → dispatch fails after
// jobTimeout AND the slot is freed (the next offer is not stuck busy).
func TestBridgeTimeoutFreesSlot(t *testing.T) {
	b := newTestBridge(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "token"), 10*time.Millisecond, 400*time.Millisecond)
	if err := b.ensure(); err != nil {
		t.Fatal(err)
	}
	defer b.close()
	// no fake extension: nobody polls /job
	j := &Job{ID: newJobID(), Action: actionFetch, URL: "https://x/"}
	_, err := b.dispatch(j)
	if err == nil || !strings.Contains(err.Error(), "extension") {
		t.Fatalf("offline dispatch err = %v, want extension-timeout error", err)
	}
	if !b.mb.offerJob(&Job{ID: "next"}) {
		t.Fatal("slot must be free after a timed-out dispatch")
	}
}

// TestElectionForeignPort: a non-bridge HTTP service on the port is a hard
// error, not a silent client role.
func TestElectionForeignPort(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	b := newTestBridge(t, addr, filepath.Join(t.TempDir(), "token"), 10*time.Millisecond, time.Second)
	err := b.ensure()
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("foreign-port ensure = %v, want port-taken error", err)
	}
}

func TestShapeProfileFetch(t *testing.T) {
	res := &Result{
		Status:   200,
		FinalURL: "https://example.com/final",
		Body:     "<html><head><title>T</title></head><body><article><h1>Head</h1><p>Para one.</p></article></body></html>",
	}
	out := shapeProfileFetch(res, false)
	if out.Title != "T" || !strings.Contains(out.Text, "Head") || !strings.Contains(out.Text, "Para one.") {
		t.Fatalf("shaped fetch = %+v", out)
	}
	if out.Status != 200 || out.FinalURL != "https://example.com/final" || out.Truncated {
		t.Fatalf("metadata = %+v", out)
	}

	raw := shapeProfileFetch(res, true)
	if raw.Text != res.Body || raw.Title != "" {
		t.Fatalf("raw mode must return the HTML verbatim, got title=%q", raw.Title)
	}

	big := &Result{Status: 200, Body: "<p>" + strings.Repeat("x", maxTextBytes+4096) + "</p>"}
	capped := shapeProfileFetch(big, false)
	if !capped.Truncated || len(capped.Text) > maxTextBytes {
		t.Fatalf("cap: truncated=%v len=%d", capped.Truncated, len(capped.Text))
	}

	tab := shapeProfileTab(&Result{FinalURL: "https://u/", Title: "T", Body: strings.Repeat("y", maxTextBytes+10)})
	if !tab.Truncated || tab.Title != "T" || len(tab.Text) > maxTextBytes {
		t.Fatalf("shapeProfileTab cap failed: %+v", tab)
	}
}
