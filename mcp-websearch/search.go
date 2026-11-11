// Package main — HTTP fetching + SERP parsing for mcp-websearch.
//
// Engines are the plain-HTML, no-JS SERPs: html.duckduckgo.com (primary) and
// bing.com (fallback). No API keys, no headless browser — a plain GET with a
// desktop UA is enough, and the day both engines wall us off (403/captcha),
// the documented escape hatch is the browser fallback, not more scraping
// cleverness (see README).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
)

// Search endpoints as vars so tests can point them at httptest servers. The
// query is appended URL-escaped by searchWeb; the URLs keep their "?q=" shape
// so a swapped test URL behaves like the real one.
var (
	ddgSearchURL  = "https://html.duckduckgo.com/html/?q="
	bingSearchURL = "https://www.bing.com/search?q="
)

// httpClient is shared by both tools. Timeout is (re)set from --timeout in
// main before serving; Go's default redirect cap (10 hops) applies.
var httpClient = &http.Client{Timeout: defaultHTTPTimeout}

// maxBodyBytes caps the raw bytes read from any single response (5MB): a
// read_url against a huge page must not drag megabytes through the tool
// result. The page is parsed up to the cap and flagged truncated.
const maxBodyBytes = 5 << 20

const (
	// Honest desktop-Chrome UA: plain-http clients with Go's default UA get
	// 403'd by DDG on sight. Nothing here logs in or fingerprints further.
	browserUA  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	acceptLang = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"
)

// SearchResult is one organic hit. Snippet may be empty — both engines
// sometimes serve a hit without one.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// SearchOutput is the web_search envelope: engine metadata BEFORE results, so
// the "fell back to bing" notice survives even if a client chops the JSON tail
// (same ordering discipline as mcp-pg-readonly's Result).
type SearchOutput struct {
	Query   string         `json:"query"`
	Engine  string         `json:"engine"`
	Notice  string         `json:"notice,omitempty"`
	Results []SearchResult `json:"results"`
}

// fetchResult is a fetched page: charset-decoded UTF-8 body plus response
// metadata. truncatedBody marks the 5MB read cap kicking in.
type fetchResult struct {
	body          []byte
	finalURL      string
	contentType   string
	truncatedBody bool
}

// fetch retrieves rawURL over http(s) and decodes the body to UTF-8. Charset
// sniffing covers the windows-1251 world (still common on RU pages): the
// x/net charset.NewReader reads the Content-Type header and falls back to
// <meta charset> sniffing.
func fetch(ctx context.Context, rawURL string) (*fetchResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http(s) URLs are supported, got %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", acceptLang)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, resp.Request.URL.Host)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	truncated := len(raw) > maxBodyBytes
	if truncated {
		raw = raw[:maxBodyBytes]
	}

	// Decode to UTF-8; on any charset weirdness fall back to raw bytes —
	// a mangled decoding of a search page is still parseable enough.
	// charset.NewReader returns (reader, error): it sniffs the Content-Type
	// header and falls back to <meta charset> detection.
	var decoded []byte
	if reader, cerr := charset.NewReader(bytes.NewReader(raw), resp.Header.Get("Content-Type")); cerr == nil {
		if b, rerr := io.ReadAll(reader); rerr == nil {
			decoded = b
		} else {
			decoded = raw
		}
	} else {
		decoded = raw
	}

	return &fetchResult{
		body:          decoded,
		finalURL:      resp.Request.URL.String(),
		contentType:   resp.Header.Get("Content-Type"),
		truncatedBody: truncated,
	}, nil
}

// searchWeb runs a query through the requested engine. engine is "ddg",
// "bing", or "" (auto: ddg first, bing fallback). On auto, a failed/empty
// primary engine degrades to the fallback with a notice instead of an error —
// a transient 403 from one SERP must not fail the tool call outright.
func searchWeb(ctx context.Context, query, engine string, maxResults int) (*SearchOutput, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > searchResultsCap {
		maxResults = searchResultsCap
	}

	var engines []string
	switch engine {
	case "", "auto":
		engines = []string{"ddg", "bing"}
	case "ddg", "bing":
		engines = []string{engine}
	default:
		return nil, fmt.Errorf("unknown engine %q (want ddg, bing, or auto)", engine)
	}

	var out *SearchOutput
	var failures []string
	for _, eng := range engines {
		res, err := searchOne(ctx, eng, query, maxResults)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", eng, err))
			continue
		}
		if len(res.Results) == 0 {
			failures = append(failures, fmt.Sprintf("%s: 0 results", eng))
			continue
		}
		out = res
		break
	}
	if out == nil {
		return nil, fmt.Errorf("all engines failed (%s) — likely a bot-wall (403/captcha) or no network; retry, try engine=ddg|bing explicitly, or use the interactive browser", strings.Join(failures, "; "))
	}
	// Report the degradation path to the agent (auto mode only).
	if len(failures) > 0 && engine == "" || engine == "auto" {
		out.Notice = fmt.Sprintf("fell back: %s", strings.Join(failures, "; "))
	}
	return out, nil
}

// searchOne fetches and parses one engine's SERP.
func searchOne(ctx context.Context, eng, query string, maxResults int) (*SearchOutput, error) {
	var u string
	switch eng {
	case "ddg":
		u = ddgSearchURL + url.QueryEscape(query)
	case "bing":
		// count=20 asks for a fuller first page (bing's default serves ~10).
		u = bingSearchURL + url.QueryEscape(query) + "&count=20"
	default:
		return nil, fmt.Errorf("unknown engine %q", eng)
	}

	fr, err := fetch(ctx, u)
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(fr.body))
	if err != nil {
		return nil, fmt.Errorf("parse SERP: %w", err)
	}

	var results []SearchResult
	switch eng {
	case "ddg":
		results = parseDDG(doc, maxResults)
	case "bing":
		results = parseBing(doc, maxResults)
	}
	return &SearchOutput{Query: query, Engine: eng, Results: results}, nil
}

// parseDDG extracts organic hits from html.duckduckgo.com. Result links come
// wrapped as //duckduckgo.com/l/?uddg=<escaped real URL>&rut=… — unwrapDDG
// recovers the destination. Ads carry the result--ad class and are skipped.
func parseDDG(doc *goquery.Document, maxResults int) []SearchResult {
	var out []SearchResult
	doc.Find("div.result, div.web-result").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if len(out) >= maxResults {
			return false
		}
		if s.HasClass("result--ad") || s.Find(".result--ad").Length() > 0 {
			return true
		}
		link := s.Find("a.result__a").First()
		if link.Length() == 0 {
			return true
		}
		href, _ := link.Attr("href")
		href = unwrapDDG(href)
		title := strings.TrimSpace(link.Text())
		if title == "" || href == "" || !strings.HasPrefix(href, "http") {
			return true
		}
		snippet := strings.TrimSpace(s.Find(".result__snippet").First().Text())
		out = append(out, SearchResult{Title: title, URL: href, Snippet: snippet})
		return true
	})
	return out
}

// unwrapDDG resolves a SERP redirect link to its destination URL. Handles the
// //duckduckgo.com/l/, https://duckduckgo.com/l/ and relative /l/ variants;
// anything that is not an /l/ redirect is returned as-is.
func unwrapDDG(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if u.Path == "/l/" && (u.Host == "duckduckgo.com" || u.Host == "www.duckduckgo.com" || u.Host == "") {
		if dest := u.Query().Get("uddg"); dest != "" {
			return dest
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

// parseBing extracts organic hits from bing.com. Organic rows are li.b_algo
// (ads are li.b_ad and never match); title is h2 > a, snippet lives in
// .b_caption p. Hrefs are usually direct, but modern bing sometimes wraps
// them as https://www.bing.com/ck/a?...&u=a1<base64url> — unwrapBing decodes.
func parseBing(doc *goquery.Document, maxResults int) []SearchResult {
	var out []SearchResult
	doc.Find("li.b_algo").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if len(out) >= maxResults {
			return false
		}
		if s.HasClass("b_ad") {
			return true
		}
		link := s.Find("h2 a").First()
		if link.Length() == 0 {
			return true
		}
		href, _ := link.Attr("href")
		href = unwrapBing(href)
		title := strings.TrimSpace(link.Text())
		if title == "" || href == "" || !strings.HasPrefix(href, "http") {
			return true
		}
		snippet := strings.TrimSpace(s.Find(".b_caption p").First().Text())
		if snippet == "" {
			snippet = strings.TrimSpace(s.Find("p").First().Text())
		}
		out = append(out, SearchResult{Title: title, URL: href, Snippet: snippet})
		return true
	})
	return out
}

// unwrapBing decodes bing's /ck/ redirect wrapper: the real URL is the u=
// parameter, base64url-encoded behind a literal "a1" prefix. Non-wrapped
// hrefs pass through untouched.
func unwrapBing(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if u.Host != "www.bing.com" && u.Host != "bing.com" {
		return href
	}
	if !strings.HasPrefix(u.Path, "/ck/") {
		return href
	}
	v := u.Query().Get("u")
	if !strings.HasPrefix(v, "a1") {
		return href
	}
	if dec, err := base64.RawURLEncoding.DecodeString(v[2:]); err == nil {
		return string(dec)
	}
	return href
}

// searchResultsCap is the hard ceiling on returned hits per call (the tool
// schema default is 10; agents may ask for up to this many).
const searchResultsCap = 20
