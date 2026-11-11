package main

// Unit tests: SERP parsers and read_url run on fixtures via httptest — no
// network, deterministic. Live-engine behavior (bot-walls, layout drift) is
// NOT covered here; `--selftest` is the live probe for that.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/text/encoding/charmap"
)

const ddgFixture = `
<html><body><div class="results">
  <div class="result result--ad">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fads.example.com%2Fbuy&amp;rut=abc">Buy pgx stickers</a>
    <a class="result__snippet">best price ad snippet</a>
  </div>
  <div class="result">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fgithub.com%2Fjackc%2Fpgx%2Fv5&amp;rut=xyz">pgx v5 - Go Packages</a>
    <a class="result__snippet">pgx is a pure Go driver and toolkit for PostgreSQL</a>
  </div>
  <div class="result">
    <a class="result__a" href="https://www.postgresql.org/docs/current/runtime-config-client.html">PostgreSQL: Runtime Config</a>
    <a class="result__snippet">statement_timeout aborts any statement</a>
  </div>
  <div class="result">
    <a class="result__a" href="">broken hit without href</a>
  </div>
</div></body></html>`

func mustDoc(t *testing.T, html string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return doc
}

func TestParseDDG(t *testing.T) {
	got := parseDDG(mustDoc(t, ddgFixture), 10)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (ad + hrefless skipped): %+v", len(got), got)
	}
	if got[0].Title != "pgx v5 - Go Packages" {
		t.Errorf("result[0].Title = %q", got[0].Title)
	}
	if got[0].URL != "https://pkg.go.dev/github.com/jackc/pgx/v5" {
		t.Errorf("uddg unwrap failed: result[0].URL = %q", got[0].URL)
	}
	if got[0].Snippet == "" || strings.Contains(got[0].Snippet, "best price") {
		t.Errorf("result[0].Snippet = %q (ad snippet leaked or empty)", got[0].Snippet)
	}
	if got[1].URL != "https://www.postgresql.org/docs/current/runtime-config-client.html" {
		t.Errorf("direct href mangled: result[1].URL = %q", got[1].URL)
	}
}

func TestParseDDG_MaxResults(t *testing.T) {
	got := parseDDG(mustDoc(t, ddgFixture), 1)
	if len(got) != 1 {
		t.Fatalf("maxResults=1 not honored: %d results", len(got))
	}
}

func TestUnwrapDDG(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&rut=1": "https://example.com/a",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fb":  "https://example.com/b",
		"/l/?uddg=https%3A%2F%2Fexample.com%2Fc":                       "https://example.com/c",
		"https://example.com/direct":                                    "https://example.com/direct",
	}
	for in, want := range cases {
		if got := unwrapDDG(in); got != want {
			t.Errorf("unwrapDDG(%q) = %q, want %q", in, got, want)
		}
	}
}

func bingFixture(wrappedHref string) string {
	return fmt.Sprintf(`
<html><body><ol id="b_results">
  <li class="b_algo">
    <h2><a href="https://www.postgresql.org/docs/">PostgreSQL Documentation</a></h2>
    <div class="b_caption"><p>The official manual</p></div>
  </li>
  <li class="b_algo">
    <h2><a href="%s">jackc/pgx: PostgreSQL Driver and Toolkit</a></h2>
    <div class="b_caption"><p>pure Go driver for PostgreSQL</p></div>
  </li>
  <li class="b_ad"><h2><a href="https://ads.example.com">Sponsored</a></h2></li>
</ol></body></html>`, wrappedHref)
}

func TestParseBing(t *testing.T) {
	wrapped := "https://www.bing.com/ck/a?!&&p=deadbeef&u=a1" +
		base64.RawURLEncoding.EncodeToString([]byte("https://github.com/jackc/pgx")) + "&ntb=1"
	got := parseBing(mustDoc(t, bingFixture(wrapped)), 10)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (b_ad skipped): %+v", len(got), got)
	}
	if got[1].URL != "https://github.com/jackc/pgx" {
		t.Errorf("ck/a unwrap failed: result[1].URL = %q", got[1].URL)
	}
	if got[0].Snippet != "The official manual" {
		t.Errorf("result[0].Snippet = %q", got[0].Snippet)
	}
}

func TestUnwrapBing_NonBingPassthrough(t *testing.T) {
	href := "https://example.com/x?u=a1zz"
	if got := unwrapBing(href); got != href {
		t.Errorf("unwrapBing mangled non-bing href: %q", got)
	}
}

// withEngines swaps the engine endpoints for the test and restores them.
func withEngines(t *testing.T, ddg, bing string) {
	t.Helper()
	oldDdg, oldBing := ddgSearchURL, bingSearchURL
	ddgSearchURL, bingSearchURL = ddg, bing
	t.Cleanup(func() { ddgSearchURL, bingSearchURL = oldDdg, oldBing })
}

func TestSearchWeb_DDGFallbackToBing(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer blocked.Close()
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, bingFixture("https://www.postgresql.org/docs/"))
	}))
	defer serving.Close()
	withEngines(t, blocked.URL+"/search?q=", serving.URL+"/search?q=")

	out, err := searchWeb(context.Background(), "pgx pool", "", 10)
	if err != nil {
		t.Fatalf("auto fallback failed: %v", err)
	}
	if out.Engine != "bing" {
		t.Errorf("Engine = %q, want bing", out.Engine)
	}
	if len(out.Results) == 0 {
		t.Fatal("fallback returned 0 results")
	}
	if !strings.Contains(out.Notice, "ddg") {
		t.Errorf("Notice should mention the ddg failure, got %q", out.Notice)
	}
}

func TestSearchWeb_ExplicitEngineDoesNotFallback(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer blocked.Close()
	withEngines(t, blocked.URL+"/search?q=", blocked.URL+"/search?q=")

	_, err := searchWeb(context.Background(), "pgx pool", "ddg", 10)
	if err == nil {
		t.Fatal("explicit ddg engine should fail hard when ddg is blocked, not silently succeed")
	}
	if !strings.Contains(err.Error(), "ddg") {
		t.Errorf("error should name the engine: %v", err)
	}
}

const readFixture = `
<html><head><title>Test Page</title></head><body>
<nav>Home Docs Blog Login</nav>
<script>var tracking = "should not appear";</script>
<aside>Related links</aside>
<article>
  <h1>Big Header</h1>
  <p>First paragraph about pgx.</p>
  <ul><li>list item one</li><li>list item two</li></ul>
  <p>Second paragraph with <a href="/x">a link</a> inside.</p>
</article>
<footer>© nobody</footer>
</body></html>`

func TestReadURL_HTMLExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, readFixture)
	}))
	defer srv.Close()

	out, err := readURL(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatalf("readURL: %v", err)
	}
	if out.Title != "Test Page" {
		t.Errorf("Title = %q", out.Title)
	}
	for _, want := range []string{"Big Header", "First paragraph about pgx.", "list item one", "Second paragraph with a link inside."} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text missing %q", want)
		}
	}
	for _, banned := range []string{"tracking", "Related links", "© nobody", "Home Docs Blog"} {
		if strings.Contains(out.Text, banned) {
			t.Errorf("chrome leaked into text: %q", banned)
		}
	}
	if out.Truncated {
		t.Error("small page must not be truncated")
	}
}

func TestReadURL_CharsetWindows1251(t *testing.T) {
	const ru = "Привет, мир виндовой кодировки."
	body, err := charmap.Windows1251.NewEncoder().String("<html><head><title>РУ</title></head><body><article><p>" + ru + "</p></article></body></html>")
	if err != nil {
		t.Skipf("encoder unavailable: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	out, err := readURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("readURL: %v", err)
	}
	if !strings.Contains(out.Text, ru) {
		t.Errorf("windows-1251 not decoded, got: %q", out.Text)
	}
}

func TestReadURL_TruncationCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<html><body><article>")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "<p>Параграф номер %d с некоторым количеством текста для превышения лимита.</p>", i)
	}
	sb.WriteString("</article></body></html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sb.String())
	}))
	defer srv.Close()

	out, err := readURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("readURL: %v", err)
	}
	if !out.Truncated {
		t.Error("expected truncated=true")
	}
	if len(out.Text) > maxTextBytes {
		t.Errorf("text %d bytes exceeds cap %d", len(out.Text), maxTextBytes)
	}
}

func TestReadURL_BinaryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()

	_, err := readURL(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("binary content should be rejected with a hint, got: %v", err)
	}
}

func TestReadURL_SchemeGate(t *testing.T) {
	if _, err := readURL(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("non-http scheme must be rejected")
	}
}

// TestAnnotations_WireSerialization is load-bearing (same as in
// mcp-pg-readonly): destructiveHint:false must survive JSON marshalling with
// the SDK's omitempty tags, or the ZCode plan-mode predicate (`isMcp &&
// !destructive`) silently loses auto-approval.
func TestAnnotations_WireSerialization(t *testing.T) {
	b, err := json.Marshal(&mcp.Tool{Name: "web_search", Annotations: readOnlyAnnotations})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"readOnlyHint":true`, `"destructiveHint":false`, `"idempotentHint":true`, `"openWorldHint":true`} {
		if !strings.Contains(s, want) {
			t.Errorf("wire JSON missing %s: %s", want, s)
		}
	}
}
