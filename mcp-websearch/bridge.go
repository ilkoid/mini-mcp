// Package main — bridge.go: level 4, the real-browser bridge.
//
// A tiny MV3 extension (extension/, vanilla JS) polls this process over HTTP
// on 127.0.0.1:8765: GET /job returns a pending job or 204, POST /result/{id}
// delivers the answer. The FIRST mcp-websearch process that needs the bridge
// binds the port and becomes the OWNER; sibling processes (ZCode + AutoClaw —
// one process per session) discover it via /health and act as CLIENTS through
// /api/*. When the owner dies its port frees; a client's next call fails with
// "connection refused", it re-runs the election and takes over (~5s, bounded
// by the extension's reconnect backoff).
//
// Concurrency discipline (plan: "no concurrent code"): one mutex-guarded
// mailbox with ONE job slot and ONE result slot; waiting for a result is a
// 250ms short-poll; a second dispatch while the slot is busy gets an
// immediate busy answer, never a queue. The single goroutine this file ever
// starts is the owner's HTTP listener.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bridge actions. fetch = plain network request from the browser profile
// (cookies + real fingerprint, no rendering); tab_read = navigate a visible
// pinned tab, let JS run like a user's, then scrape title/url/innerText;
// page_fetch = run the fetch INSIDE a page loaded from Origin (content-script
// context) — for APIs behind Origin/Referer checks (the WB search API 403s a
// bare service-worker fetch but passes the page's own XHR).
const (
	actionFetch     = "fetch"
	actionTabRead   = "tab_read"
	actionPageFetch = "page_fetch"
)

// Job is a unit of work for the extension. Carried as-is in GET /job.
type Job struct {
	ID     string `json:"id"`
	Action string `json:"action"` // "fetch" | "tab_read" | "page_fetch"
	URL    string `json:"url"`
	Origin string `json:"origin,omitempty"`  // page_fetch: page whose context runs the fetch
	Raw    bool   `json:"raw,omitempty"`     // fetch: skip text extraction
	WaitMs int    `json:"wait_ms,omitempty"` // tab_read: settle time after load
}

// Result is the extension's answer to a specific job (matched by ID — a
// late answer to an older job must never satisfy a newer waiter). For
// fetch, OK=true means "got an HTTP response" (a 403/498 body is still a
// valid result the caller wants to see); OK=false is a network/execution
// error in Err.
type Result struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status,omitempty"`    // fetch: HTTP status
	FinalURL string `json:"final_url,omitempty"` // fetch: after redirects; tab_read: location.href
	Title    string `json:"title,omitempty"`     // tab_read: document.title
	Body     string `json:"body,omitempty"`      // fetch: HTML; tab_read: innerText
	Links    []Link `json:"links,omitempty"`     // tab_read: anchors (href+text) — SERP cards are often <a> themselves
	Err      string `json:"err,omitempty"`
}

// Link is one anchor from a tab_read page: the href carries ids (WB nmID,
// Ozon SKU) and the text is the card content (brand+name+price) when the
// card itself is the anchor.
type Link struct {
	Href string `json:"href"`
	Text string `json:"text,omitempty"`
}

// mailbox is the ONLY shared structure of the bridge: one job slot, one
// result slot, one mutex. No queues, no channels, no waiting inside —
// contention resolves as an immediate busy refusal at the caller.
type mailbox struct {
	mu     sync.Mutex
	job    *Job
	result *Result
}

// offerJob claims the job slot; false = busy (the caller reports retry-ms,
// it does NOT wait — invariant from the plan self-audit).
func (m *mailbox) offerJob(j *Job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil {
		return false
	}
	m.job = j
	m.result = nil // a stale result must not outlive the job it belonged to
	return true
}

// takeJob atomically hands the job to the extension poller.
func (m *mailbox) takeJob() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.job
	m.job = nil
	return j
}

// dropJob frees a stuck job slot after a timeout — only if it is still OUR
// job (an extension may have taken it in the meantime; then the slot is
// already free and this is a no-op).
func (m *mailbox) dropJob(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.ID == id {
		m.job = nil
	}
}

// putResult stores the latest result (last write wins; a duplicate POST is
// harmless). ID matching in takeResult is what protects against stale ones.
func (m *mailbox) putResult(r *Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.result = r
}

// takeResult returns the result only if its ID matches — a late answer to
// job A must not satisfy the waiter of job B.
func (m *mailbox) takeResult(id string) *Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.result != nil && m.result.ID == id {
		r := m.result
		m.result = nil
		return r
	}
	return nil
}

// bridgeRole is the process's stance in the lazy election.
type bridgeRole int

const (
	bridgeIdle   bridgeRole = iota // election not run yet
	bridgeOwner                    // we bound the port; the extension talks to us
	bridgeClient                   // another live owner holds the port; we use /api
)

func (r bridgeRole) String() string {
	switch r {
	case bridgeOwner:
		return "owner"
	case bridgeClient:
		return "client"
	default:
		return "idle"
	}
}

// busyError is the immediate "slot taken" answer. Callers surface retry_in_ms
// instead of blocking (one slot by design — see mailbox).
type busyError struct{ retryMs int }

func (e *busyError) Error() string {
	return fmt.Sprintf("bridge busy with another job — retry in ~%dms", e.retryMs)
}

// errOwnerGone marks a client call that failed at the transport level: the
// owner may have died, so the caller should re-run the election.
var errOwnerGone = errors.New("bridge owner unreachable")

// bridge is the per-process level-4 hub (singleton `bridged`, built from
// flags in main, elected lazily on the first bridge tool call).
type bridge struct {
	addr        string        // "127.0.0.1:8765"
	tokenFile   string        // token location (diagnostics + creation)
	token       string        // shared secret, X-Bridge-Token
	minInterval time.Duration // server-enforced pause between jobs (politeness)
	jobTimeout  time.Duration // how long to wait for the extension

	autoStart  bool   // --auto-start-browser: launch the browser when the extension is offline
	browserCmd string // --browser-cmd: explicit browser binary (else autodetect Chrome/Edge)

	role     bridgeRole
	mb       *mailbox // owner only
	srv      *http.Server
	httpCli  *http.Client // /api calls (client role); timeout > owner's worst case
	probeCli *http.Client // /health probes, short timeout

	mu               sync.Mutex // guards lastIssue/lastPoll (roles are stdio-serial)
	lastIssue        time.Time  // earliest permitted next-issue moment (throttle debt)
	lastPoll         time.Time  // last authenticated /job or /result hit — liveness
	lastAuthFail     time.Time  // last token rejection — "polls but wrong token" state
	lastBrowserStart time.Time  // debounce for the browser auto-start (≥60s apart)
}

// bridged is this process's bridge. Nil until initBridge runs (main).
var bridged *bridge

// initBridge builds the singleton from flags. Port 0 (tests) means
// "ephemeral": the real address is resolved after the owner binds.
func initBridge(port int, tokenFile string, minInterval, jobTimeout time.Duration, autoStart bool, browserCmd string) error {
	if bridged != nil {
		return nil
	}
	if tokenFile == "" {
		var err error
		tokenFile, err = defaultTokenFile()
		if err != nil {
			return fmt.Errorf("bridge token path: %w", err)
		}
	}
	bridged = &bridge{
		addr:        net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		tokenFile:   tokenFile,
		minInterval: minInterval,
		jobTimeout:  jobTimeout,
		autoStart:   autoStart,
		browserCmd:  browserCmd,
		httpCli:     &http.Client{Timeout: minInterval + jobTimeout + 5*time.Second},
		probeCli:    &http.Client{Timeout: 2 * time.Second},
	}
	return nil
}

// defaultTokenFile is ~/.config/mcp-browserbridge/token — deliberately the
// same relative layout on Windows (%USERPROFILE%\.config\...), NOT
// os.UserConfigDir (that would scatter it into ~/Library on macOS).
func defaultTokenFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mcp-browserbridge", "token"), nil
}

// loadOrCreateToken reads the shared token, generating one (32 random bytes
// → hex, file 0600, dir 0700) on first run. Lives outside the repo by
// design; the extension gets its copy pasted via its options page.
func loadOrCreateToken(path string) (string, error) {
	if tok, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(tok)); s != "" {
			return s, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token entropy: %w", err)
	}
	tok := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	log.Printf("bridge: created token %s", path)
	return tok, nil
}

// newJobID mints a random job id (hex); the fallback is unreachable in
// practice but keeps IDs unique even if the entropy source fails.
func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("j%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ensure runs the lazy election exactly once (then caches the role):
// bind = owner; port busy + our /health answers = client; port busy +
// anything else = hard error (a foreign service squatting the port).
func (b *bridge) ensure() error {
	if b.role != bridgeIdle {
		return nil
	}
	if b.token == "" {
		tok, err := loadOrCreateToken(b.tokenFile)
		if err != nil {
			return err
		}
		b.token = tok
	}
	ln, err := net.Listen("tcp", b.addr)
	if err == nil {
		b.role = bridgeOwner
		b.mb = &mailbox{}
		b.srv = &http.Server{Handler: b.ownerMux()}
		b.addr = ln.Addr().String() // resolve ephemeral :0 (tests)
		go b.srv.Serve(ln)          // the ONE goroutine this codebase starts
		log.Printf("bridge: owner on %s (token: %s)", b.addr, b.tokenFile)
		return nil
	}
	resp, err := b.probeOwner()
	if err != nil {
		return fmt.Errorf("port %s is taken and no bridge answers there (%v) — check --bridge-port", b.addr, err)
	}
	defer resp.Body.Close()
	var h struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil || !h.OK {
		return fmt.Errorf("port %s is taken by a foreign service (health HTTP %d) — check --bridge-port", b.addr, resp.StatusCode)
	}
	b.role = bridgeClient
	log.Printf("bridge: client — another process owns %s", b.addr)
	return nil
}

// probeOwner asks GET /health with the token. 200+ok:true is the proof that
// the port holder is a sibling mcp-websearch with the SAME token file.
func (b *bridge) probeOwner() (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+b.addr+"/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Bridge-Token", b.token)
	return b.probeCli.Do(req)
}

// resetToIdle drops a CLIENT role so the next ensure re-runs the election
// (takeover after the owner died). Owners never reset: they own the port
// until the process exits.
func (b *bridge) resetToIdle() {
	if b.role == bridgeClient {
		b.role = bridgeIdle
	}
}

// close stops the owner listener (tests / clean shutdown). The OS frees the
// port on process death anyway; this makes takeover testable in-process.
func (b *bridge) close() {
	if b.srv != nil {
		_ = b.srv.Close()
		b.srv = nil
	}
	b.role = bridgeIdle
	b.mb = nil
}

// gateIssue blocks until >= minInterval has passed since the previously
// issued job, claiming the next issue-moment up front. Because every claim
// is paid for with an equal real sleep, lastIssue never runs ahead of the
// clock by more than one interval — a burst of busy-retries cannot pile up
// an artificial wait after the queue drains (self-audit fix). The OWNER
// gates here, so the pace holds across all client processes.
func (b *bridge) gateIssue() {
	b.mu.Lock()
	now := time.Now()
	if !b.lastIssue.IsZero() && b.lastIssue.Add(b.minInterval).After(now) {
		wait := b.lastIssue.Add(b.minInterval).Sub(now)
		b.lastIssue = b.lastIssue.Add(b.minInterval)
		b.mu.Unlock()
		time.Sleep(wait)
		return
	}
	b.lastIssue = now
	b.mu.Unlock()
}

func (b *bridge) retryInMs() int {
	ms := int(b.minInterval / time.Millisecond)
	if ms < 250 {
		ms = 250
	}
	return ms
}

// browserCandidates lists well-known real-browser binaries (Chrome first,
// Edge fallback) per OS. The bridge lives INSIDE the user's daily browser —
// these are the only executors its cookies make trustworthy.
func browserCandidates() []string {
	var out []string
	if p := os.Getenv("ProgramFiles"); p != "" {
		out = append(out,
			filepath.Join(p, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(p, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	if p := os.Getenv("ProgramFiles(x86)"); p != "" {
		out = append(out,
			filepath.Join(p, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(p, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	if p := os.Getenv("LocalAppData"); p != "" {
		out = append(out, filepath.Join(p, "Google", "Chrome", "Application", "chrome.exe"))
	}
	out = append(out,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium-browser",
	)
	return out
}

// detectBrowserPath returns the explicit --browser-cmd when set, else the
// first candidate that exists. Empty string = nothing found.
func detectBrowserPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, p := range browserCandidates() {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// maybeStartBrowser closes the "user closed the browser mid-session" gap
// (RDP reality: the extension is dead until Chrome lives again). OWNER only,
// best-effort, debounced to ≥60s between attempts: if no authenticated poll
// arrived in the last 5s (extension offline) and auto-start is armed, launch
// the browser binary in THIS user session — the dispatch that triggered it
// keeps waiting its normal jobTimeout, which comfortably covers a cold
// Chrome start + service-worker boot (~3-10s). The reaper goroutine touches
// no bridge state, so the "one listener goroutine" discipline still holds.
func (b *bridge) maybeStartBrowser() {
	if !b.autoStart || b.role != bridgeOwner {
		return
	}
	b.mu.Lock()
	last := b.lastPoll
	if !b.lastBrowserStart.IsZero() && time.Since(b.lastBrowserStart) < 60*time.Second {
		b.mu.Unlock()
		return
	}
	b.lastBrowserStart = time.Now()
	b.mu.Unlock()
	if !last.IsZero() && time.Since(last) < 5*time.Second {
		return // extension is polling — nothing to start
	}
	path := detectBrowserPath(b.browserCmd)
	if path == "" {
		log.Printf("bridge: extension offline and no browser binary found (set --browser-cmd)")
		return
	}
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		log.Printf("bridge: browser auto-start failed: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }() // process reaper — no shared state, runs once per launch
	log.Printf("bridge: extension offline — started %s", path)
}

// ownerDispatch is the owner's direct path into the mailbox (no HTTP hop to
// itself): gate (politeness) → claim slot (busy = immediate refusal) →
// short-poll for OUR result id until jobTimeout.
func (b *bridge) ownerDispatch(j *Job) (*Result, error) {
	b.gateIssue()
	if !b.mb.offerJob(j) {
		return nil, &busyError{retryMs: b.retryInMs()}
	}
	deadline := time.Now().Add(b.jobTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if r := b.mb.takeResult(j.ID); r != nil {
			if !r.OK {
				msg := r.Err
				if msg == "" && r.Body != "" { // extension put the reason in Body
					msg = r.Body
				}
				return nil, fmt.Errorf("extension: %s", msg)
			}
			return r, nil
		}
	}
	b.mb.dropJob(j.ID) // extension never showed up — free the slot
	return nil, fmt.Errorf("extension did not answer within %s (offline? load the extension — see extension/README.md; bridge_status shows liveness)", b.jobTimeout)
}

// dispatch is the single entry point for tools: owner → mailbox directly,
// client → owner's /api. A transport failure as a client triggers one
// re-election + retry (takeover path).
func (b *bridge) dispatch(j *Job) (*Result, error) {
	if err := b.ensure(); err != nil {
		return nil, err
	}
	if b.role == bridgeOwner {
		b.maybeStartBrowser() // best-effort; no-op when the extension is alive
		return b.ownerDispatch(j)
	}
	res, err := b.clientDispatch(j)
	if errors.Is(err, errOwnerGone) {
		log.Printf("bridge: owner unreachable (%v) — re-electing", err)
		b.resetToIdle()
		if err2 := b.ensure(); err2 != nil {
			return nil, err2
		}
		if b.role == bridgeOwner {
			return b.ownerDispatch(j) // we took over mid-flight
		}
		return b.clientDispatch(j)
	}
	return res, err
}

// clientDispatch ships the job to the owner over /api/{fetch,tab}. The owner
// mints its own job id and relays the finished Result (or a busy/timeout
// marker) in the HTTP response.
func (b *bridge) clientDispatch(j *Job) (*Result, error) {
	path := "/api/fetch"
	if j.Action == actionTabRead {
		path = "/api/tab"
	}
	payload, err := json.Marshal(struct {
		URL    string `json:"url"`
		Origin string `json:"origin"`
		Raw    bool   `json:"raw"`
		WaitMs int    `json:"wait_ms"`
	}{URL: j.URL, Origin: j.Origin, Raw: j.Raw, WaitMs: j.WaitMs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+b.addr+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Token", b.token)
	resp, err := b.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOwnerGone, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var res Result
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes+64<<10)).Decode(&res); err != nil {
			return nil, fmt.Errorf("malformed owner response: %w", err)
		}
		return &res, nil
	case http.StatusServiceUnavailable:
		var busy struct {
			RetryInMs int `json:"retry_in_ms"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&busy)
		if busy.RetryInMs == 0 {
			busy.RetryInMs = b.retryInMs()
		}
		return nil, &busyError{retryMs: busy.RetryInMs}
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("owner rejected the token — is %s shared between both mcp-websearch processes?", b.tokenFile)
	default:
		var e struct {
			Err string `json:"err"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&e)
		if e.Err != "" {
			return nil, errors.New(e.Err)
		}
		return nil, fmt.Errorf("owner answered HTTP %d", resp.StatusCode)
	}
}

// statusInfo feeds the bridge_status tool: role, address, token file and
// extension liveness. "online" = authenticated poll in the last 5s;
// "auth_failing" = something polls but the token is wrong (extension is
// loaded, its options need the right token); "offline" = no polls at all.
// A FRESH owner starts as offline for up to ~5s — the extension's reconnect
// backoff; callers that need a verdict should re-check or wait (see
// runSelftestBridge's grace loop).
func (b *bridge) statusInfo() map[string]any {
	info := map[string]any{
		"version":         version,
		"addr":            b.addr,
		"token_file":      b.tokenFile,
		"min_interval_ms": b.minInterval.Milliseconds(),
		"job_timeout_ms":  b.jobTimeout.Milliseconds(),
		"auto_start":      b.autoStart,
	}
	if err := b.ensure(); err != nil {
		info["error"] = err.Error()
		return info
	}
	info["role"] = b.role.String()
	b.mu.Lock()
	last, lastFail := b.lastPoll, b.lastAuthFail
	b.mu.Unlock()
	switch {
	case !last.IsZero() && time.Since(last) < 5*time.Second:
		info["extension"] = "online"
	case !lastFail.IsZero() && time.Since(lastFail) < 5*time.Second:
		info["extension"] = "auth_failing"
	default:
		info["extension"] = "offline"
	}
	return info
}

// writeJSON is the bridge endpoints' responder (NOT for MCP results).
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// auth guards every bridge endpoint: correct X-Bridge-Token AND a loopback
// Host header (token closes random callers, the Host check closes
// DNS-rebinding — a public name resolving to 127.0.0.1 arrives with a
// foreign Host and dies here).
func (b *bridge) auth(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "127.0.0.1" && host != "localhost" {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return false
	}
	if r.Header.Get("X-Bridge-Token") != b.token {
		b.mu.Lock()
		b.lastAuthFail = time.Now()
		b.mu.Unlock()
		http.Error(w, "bad token", http.StatusUnauthorized)
		return false
	}
	return true
}

func (b *bridge) touchPoll() {
	b.mu.Lock()
	b.lastPoll = time.Now()
	b.mu.Unlock()
}

// ownerMux builds the owner's HTTP surface:
//
//	GET  /job          — extension poll: a Job or 204
//	POST /result/{id}  — extension answer
//	GET  /health       — election probe (token required)
//	POST /api/fetch    — client processes: {url, raw}   → Result | 503 busy
//	POST /api/tab      — client processes: {url, wait_ms} → same
//	GET  /api/status   — client-facing diagnostics
func (b *bridge) ownerMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !b.auth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": "owner", "version": version})
	})

	mux.HandleFunc("/job", func(w http.ResponseWriter, r *http.Request) {
		if !b.auth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		b.touchPoll()
		if j := b.mb.takeJob(); j != nil {
			writeJSON(w, http.StatusOK, j)
			return
		}
		w.WriteHeader(http.StatusNoContent) // nothing pending — poll again in ~1s
	})

	mux.HandleFunc("/result/", func(w http.ResponseWriter, r *http.Request) {
		if !b.auth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b.touchPoll()
		id := strings.TrimPrefix(r.URL.Path, "/result/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "bad result id", http.StatusBadRequest)
			return
		}
		var res Result
		// extension caps bodies at 5MB; +slack for JSON escaping overhead
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes+512<<10)).Decode(&res); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		res.ID = id // the path is the truth, not the body
		b.mb.putResult(&res)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/fetch", b.apiDispatch(actionFetch))
	mux.HandleFunc("/api/tab", b.apiDispatch(actionTabRead))

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !b.auth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, b.statusInfo())
	})

	return mux
}

// apiDispatch is the shared /api/{fetch,tab} handler: parse {url, raw,
// wait_ms}, mint a job, run the owner dispatch path (throttle + mailbox +
// wait), answer with the Result — or map busy/timeout to 503/504 markers
// the client understands.
func (b *bridge) apiDispatch(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !b.auth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			URL    string `json:"url"`
			Origin string `json:"origin"`
			Raw    bool   `json:"raw"`
			WaitMs int    `json:"wait_ms"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		j := &Job{ID: newJobID(), Action: action, URL: req.URL, Origin: req.Origin, Raw: req.Raw, WaitMs: req.WaitMs}
		res, err := b.ownerDispatch(j)
		var busy *busyError
		if errors.As(err, &busy) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"busy": true, "retry_in_ms": busy.retryMs})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}
