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

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-websearch: ")
	// All logger output goes to stderr (stdout discipline — same rule as
	// mcp-pg-readonly).
	log.SetOutput(os.Stderr)

	var (
		selftest = flag.Bool("selftest", false, "run a live search + read_url check, then exit (prints to stderr)")
		timeout  = flag.Duration("timeout", defaultHTTPTimeout, "per-request HTTP timeout (default: 20s)")
	)
	flag.Parse()
	httpClient.Timeout = *timeout

	if *selftest {
		os.Exit(runSelftest(context.Background()))
	}

	// Serve MCP over stdio. From here on, NOTHING may write to stdout except
	// the SDK's protocol frames.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-websearch",
		Title:   "Local web search",
		Version: "0.1.0",
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
