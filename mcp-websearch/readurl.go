// Package main — read_url: fetch a page over plain HTTP and extract readable
// text. This is the quota-free stand-in for hosted web-reader tools.
//
// It is deliberately NOT a browser: no JS execution, no login cookies. Pages
// that render client-side return little or nothing — the tool description
// steers the agent to the interactive browser for those, not to metered
// hosted tools.
package main

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// maxTextBytes caps the extracted text (48KB, mirroring mcp-pg-readonly's
// output budget: stay under the client's ~50KB resultBudget with envelope
// overhead, so `truncated` stays readable even when the tail gets chopped).
const maxTextBytes = 48 << 10

// ReadOutput is the read_url envelope. Metadata BEFORE text, same ordering
// discipline as everywhere in this family of servers.
type ReadOutput struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Truncated   bool   `json:"truncated"`
	Notice      string `json:"notice,omitempty"`
	Text        string `json:"text"`
}

// collapseRuns folds 3+ consecutive newlines down to one blank line.
var collapseRuns = regexp.MustCompile(`\n{3,}`)

// readURL fetches rawURL and returns its readable text. HTML is stripped to
// main-content block text (headings/paragraphs/list items), plain text types
// pass through as-is. Binary content types are rejected with a hint toward
// the browser instead of dumping mojibake into the tool result.
func readURL(ctx context.Context, rawURL string) (*ReadOutput, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("url is required")
	}
	fr, err := fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	out := &ReadOutput{
		URL:         fr.finalURL,
		ContentType: fr.contentType,
	}
	var notices []string
	if fr.truncatedBody {
		notices = append(notices, fmt.Sprintf("page exceeded the %dMB read cap; parsed a prefix only", maxBodyBytes>>20))
	}

	ct := strings.ToLower(fr.contentType)
	switch {
	case ct == "", strings.Contains(ct, "html"), strings.Contains(ct, "xml"):
		text, title := extractHTML(fr.body)
		out.Text = text
		out.Title = title
	case strings.Contains(ct, "plain"), strings.Contains(ct, "markdown"), strings.Contains(ct, "json"), strings.Contains(ct, "csv"):
		out.Text = string(fr.body)
	default:
		return nil, fmt.Errorf("unsupported content type %q — likely binary/media; use the interactive browser for this URL", fr.contentType)
	}

	out.Text = strings.TrimSpace(out.Text)
	if len(out.Text) > maxTextBytes {
		// Cut on a rune boundary — slicing mid-rune would emit invalid UTF-8.
		cut := maxTextBytes
		for cut > 0 && !utf8.RuneStart(out.Text[cut]) {
			cut--
		}
		out.Text = out.Text[:cut]
		out.Truncated = true
	}
	if len(notices) > 0 {
		out.Notice = strings.Join(notices, "; ")
	}
	return out, nil
}

// extractHTML pulls (text, title) out of an HTML page: chrome elements are
// removed, the main content region is located (article/main/#content, else
// body), and block-level elements become paragraphs. The block scan is what
// keeps paragraphs and list items separated — goquery's plain .Text() would
// glue the whole page into one wall of text.
func extractHTML(body []byte) (string, string) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return string(body), ""
	}
	title := strings.TrimSpace(doc.Find("title").First().Text())

	doc.Find("script,style,noscript,template,svg,canvas,iframe,form,nav,header,footer,aside,button,select").Remove()

	main := doc.Find("article").First()
	if main.Length() == 0 {
		main = doc.Find("main, [role='main']").First()
	}
	if main.Length() == 0 {
		main = doc.Find("#content, .content, #mw-content-text").First()
	}
	if main.Length() == 0 {
		main = doc.Find("body")
	}

	var sb strings.Builder
	var lastBlock string
	main.Find("h1,h2,h3,h4,h5,h6,p,li,pre,blockquote,dt,dd,figcaption,td").EachWithBreak(func(_ int, el *goquery.Selection) bool {
		t := strings.TrimSpace(el.Text())
		if t == "" {
			return true
		}
		// Nested blocks (a <p> inside an <li>) duplicate text: the parent is
		// visited first and its text contains the child's, so comparing with
		// the immediately preceding block is enough to skip the echo.
		if lastBlock != "" && strings.Contains(lastBlock, t) {
			return true
		}
		sb.WriteString(t)
		sb.WriteString("\n\n")
		lastBlock = t
		return true
	})
	text := sb.String()
	if strings.TrimSpace(text) == "" {
		// Pages with no block markup at all: fall back to the whole subtree.
		text = main.Text()
	}
	return collapseRuns.ReplaceAllString(strings.TrimSpace(text), "\n\n"), title
}
