// mcp-websearch is a tiny quota-free web search / page reader MCP server.
//
// It exposes two read-only tools (web_search, read_url) over stdio JSON-RPC.
// Both speak plain HTTP directly from the local machine: no hosted Z.ai
// tooling, no API keys, no browser. The point is quota economics — hosted
// web-search/web-reader calls burn the coding plan's tool limits, MCP tool
// calls do not.
//
// Usage (stdio):
//
//	mcp-websearch
//
// Selftest (live network check of both tools, prints to stderr):
//
//	mcp-websearch --selftest
//
// All logging goes to stderr — stdout carries ONLY MCP protocol frames. Any
// stray stdout byte breaks the stdio transport (ZCode shows toolCount=0).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultHTTPTimeout caps each outbound request (search fetch or page read).
// Generous enough for slow SERPs, short enough to keep tool calls snappy —
// there is no layered DB-style timeout here; the client timeout is the whole
// budget. Go's http.Client also stops after 10 redirects by default, which is
// the redirect cap for free.
const defaultHTTPTimeout = 20 * time.Second

// version is the server (and bridge /health) version. 0.2.0 = level 4:
// the real-browser bridge (bridge.go + extension/) on top of web_search and
// read_url.
const version = "0.2.0"

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-websearch: ")
	// All logger output goes to stderr (stdout discipline — same rule as
	// mcp-pg-readonly).
	log.SetOutput(os.Stderr)

	var (
		selftest       = flag.Bool("selftest", false, "run a live search + read_url check, then exit (prints to stderr)")
		selftestBridge = flag.Bool("selftest-bridge", false, "run a live level-4 bridge check (needs the extension loaded in a real browser), then exit")
		profileFetch   = flag.String("profile-fetch", "", "one-shot level-4 fetch: run ONE profile_fetch job for this URL, print the envelope to stderr, exit")
		profileTab     = flag.String("profile-tab", "", "one-shot level-4 tab read: run ONE profile_tab job for this URL, print the envelope to stderr, exit")
		pageFetch      = flag.String("page-fetch", "", "one-shot level-4 page-context fetch: load --origin in the real browser, fetch this URL as that page (APIs behind Origin checks)")
		originFlag     = flag.String("origin", "", "with --page-fetch: page origin the fetch runs from")
		oneShotRaw     = flag.Bool("raw", false, "with --profile-fetch: raw HTML instead of extracted text")
		oneShotWaitMs  = flag.Int("wait-ms", 3000, "with --profile-tab: settle time after load, ms (capped at 15000)")
		timeout        = flag.Duration("timeout", defaultHTTPTimeout, "per-request HTTP timeout (default: 20s)")
		bridgePort     = flag.Int("bridge-port", 8765, "level-4 bridge port on 127.0.0.1 (must match the extension's host_permissions)")
		tokenFile      = flag.String("bridge-token-file", "", "level-4 bridge token file (default: ~/.config/mcp-browserbridge/token; created 0600 if missing)")
		minInterval    = flag.Duration("min-interval", 2*time.Second, "server-enforced pause between bridge jobs (anti-WAF politeness)")
		bridgeTimeout  = flag.Duration("bridge-timeout", 30*time.Second, "how long to wait for the extension to answer a job")
		autoStart      = flag.Bool("auto-start-browser", false, "owner: launch the browser binary in this user session when the extension is offline (closes the 'user closed Chrome mid-session' gap; debounced 60s)")
		browserCmd     = flag.String("browser-cmd", "", "explicit browser binary for --auto-start-browser (default: autodetect Chrome, then Edge)")
	)
	flag.Parse()
	httpClient.Timeout = *timeout

	if err := initBridge(*bridgePort, *tokenFile, *minInterval, *bridgeTimeout, *autoStart, *browserCmd); err != nil {
		fmt.Fprintf(os.Stderr, "bridge init: %v\n", err)
		os.Exit(1)
	}

	if *selftest {
		os.Exit(runSelftest(context.Background()))
	}
	if *selftestBridge {
		os.Exit(runSelftestBridge())
	}
	if *profileFetch != "" {
		os.Exit(runOneShotBridge(&Job{ID: newJobID(), Action: actionFetch, URL: *profileFetch, Raw: *oneShotRaw}))
	}
	if *profileTab != "" {
		os.Exit(runOneShotBridge(&Job{ID: newJobID(), Action: actionTabRead, URL: *profileTab, WaitMs: *oneShotWaitMs}))
	}
	if *pageFetch != "" {
		os.Exit(runOneShotBridge(&Job{ID: newJobID(), Action: actionPageFetch, URL: *pageFetch, Origin: *originFlag}))
	}

	// Serve MCP over stdio. From here on, NOTHING may write to stdout except
	// the SDK's protocol frames.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-websearch",
		Title:   "Local web search",
		Version: version,
	}, nil)
	registerTools(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		os.Exit(1)
	}
}

// runSelftest exercises both tools against the live web: a real search (auto
// engine, ddg→bing fallback included) and a read of example.com. Prints a
// human-readable report to stderr and returns the exit code.
func runSelftest(ctx context.Context) int {
	fmt.Fprintln(os.Stderr, "selftest: live web_search (auto engine)…")
	out, err := searchWeb(ctx, "postgresql pgx pool", "", 5)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: web_search -> %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: web_search engine=%s, %d result(s)\n", out.Engine, len(out.Results))
	for i, r := range out.Results {
		if i >= 3 {
			break
		}
		fmt.Fprintf(os.Stderr, "  %d. %s\n     %s\n", i+1, r.Title, r.URL)
	}
	if len(out.Results) == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: web_search returned 0 results on both engines")
		return 1
	}
	if out.Notice != "" {
		fmt.Fprintf(os.Stderr, "notice: %s\n", out.Notice)
	}

	fmt.Fprintln(os.Stderr, "selftest: live read_url(example.com)…")
	rd, err := readURL(ctx, "https://example.com/")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: read_url -> %v\n", err)
		return 1
	}
	// example.com's markup: "Example Domain" lives in <title> only (the old
	// <h1> is gone since the 2025 redesign); body is one <p> about
	// documentation examples, plus a script that must NOT leak into text.
	if rd.Title != "Example Domain" || !strings.Contains(rd.Text, "documentation examples") {
		fmt.Fprintf(os.Stderr, "FAIL: read_url got title=%q text=%.120q\n", rd.Title, rd.Text)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: read_url title=%q, %d bytes\n", rd.Title, len(rd.Text))

	fmt.Fprintln(os.Stderr, "selftest: PASS")
	return 0
}

// runSelftestBridge exercises level 4 end-to-end: elects the bridge (owner or
// client), checks the extension is polling /job, then fetches example.com
// THROUGH the real browser profile. Requires the extension to be loaded in
// the user's browser — see extension/README.md. Prints to stderr.
func runSelftestBridge() int {
	info := bridged.statusInfo()
	fmt.Fprintf(os.Stderr, "bridge: addr=%s role=%s token=%s\n", info["addr"], info["role"], info["token_file"])
	if errS, ok := info["error"]; ok {
		fmt.Fprintf(os.Stderr, "FAIL: bridge election: %v\n", errS)
		return 1
	}
	// grace: a FRESH owner is invisible to the extension until its reconnect
	// backoff fires (≤5s) — wait before declaring it offline
	ext, _ := info["extension"].(string)
	for i := 0; i < 8 && ext == "offline"; i++ {
		time.Sleep(time.Second)
		info = bridged.statusInfo()
		ext, _ = info["extension"].(string)
		fmt.Fprintf(os.Stderr, "bridge: waiting for the extension… %ds (state: %v)\n", i+1, ext)
	}
	switch ext {
	case "online":
	case "auth_failing":
		fmt.Fprintln(os.Stderr, "FAIL: extension polls with a WRONG token — reopen its options page and paste the token from the file above")
		return 1
	default:
		fmt.Fprintln(os.Stderr, "FAIL: extension offline — load it (edge://extensions → Load unpacked → mcp-websearch/extension), paste the token from the file above into its options page, then retry")
		return 1
	}
	fmt.Fprintln(os.Stderr, "bridge: extension online, fetching https://example.com/ through the real profile…")
	res, err := bridged.dispatch(&Job{ID: newJobID(), Action: actionFetch, URL: "https://example.com/"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: profile fetch of example.com -> %v\n", err)
		return 1
	}
	text, _ := extractHTML([]byte(res.Body))
	if res.Status != 200 || !strings.Contains(text+res.Body, "Example Domain") {
		fmt.Fprintf(os.Stderr, "FAIL: unexpected example.com response: status=%d body=%.160q\n", res.Status, res.Body)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ok: status=%d final_url=%s (%d bytes of HTML)\n", res.Status, res.FinalURL, len(res.Body))
	fmt.Fprintln(os.Stderr, "selftest-bridge: PASS")
	return 0
}

// runOneShotBridge executes ONE level-4 job and prints the shaped envelope
// (the same one the MCP tools return) to stderr. This is the acceptance-run
// surface: each invocation is its own process, elects the bridge lazily,
// does its single job and exits — the extension reconnects to the next
// owner within its 5s backoff, well inside the 30s job timeout. Pacing
// between separate invocations is the OPERATOR's responsibility (there is
// no cross-process clock but a human pressing Enter).
func runOneShotBridge(j *Job) int {
	info := bridged.statusInfo()
	fmt.Fprintf(os.Stderr, "bridge: role=%s addr=%s extension=%v\n", info["role"], info["addr"], info["extension"])
	res, err := bridged.dispatch(j)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		return 1
	}
	var out any
	switch j.Action {
	case actionFetch:
		out = shapeProfileFetch(res, j.Raw)
	case actionPageFetch:
		out = shapeProfileFetch(res, true) // API bodies are JSON — no HTML extraction
	default:
		out = shapeProfileTab(res)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: shape: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, string(b))
	return 0
}
