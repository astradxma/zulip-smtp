package main

import (
	"html"
	"io"
	"regexp"
	"strings"

	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	"github.com/emersion/go-message/mail"
)

// zulipMaxContent is Zulip's per-message limit. Longer messages are rejected
// outright by the API, so truncate rather than lose the whole thing.
const zulipMaxContent = 10000

// countingReader lets us log a byte count without holding the body.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// extractMessage pulls a subject and a plain-text body out of an RFC 5322
// message, and reports how many bytes it read.
//
// Preference order is text/plain, then text/html converted to text. Attachments
// are ignored -- deliberately and visibly, rather than silently: Zulip DMs are
// not a file transfer channel, and nothing that sends password-reset mail
// attaches anything.
func extractMessage(r io.Reader) (subject, body string, n int64, err error) {
	cr := &countingReader{r: r}

	mr, err := mail.CreateReader(cr)
	if err != nil {
		_, _ = io.Copy(io.Discard, cr) // drain, so the count is honest
		return "", "", cr.n, err
	}

	subject, _ = mr.Header.Subject()

	var plain, htmlBody string
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return subject, "", cr.n, perr
		}

		inline, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue // attachment
		}

		contentType, _, _ := inline.ContentType()
		raw, rerr := io.ReadAll(part.Body)
		if rerr != nil {
			continue
		}

		switch contentType {
		case "text/plain":
			if plain == "" {
				plain = string(raw)
			}
		case "text/html":
			if htmlBody == "" {
				htmlBody = string(raw)
			}
		}
	}

	if strings.TrimSpace(plain) != "" {
		return subject, strings.TrimSpace(plain), cr.n, nil
	}
	return subject, htmlToText(htmlBody), cr.n, nil
}

// formatForZulip renders subject and body as one Zulip message.
func formatForZulip(subject, body string) string {
	var b strings.Builder
	if s := strings.TrimSpace(subject); s != "" {
		b.WriteString("**")
		b.WriteString(s)
		b.WriteString("**\n\n")
	}
	b.WriteString(body)

	out := b.String()
	if len(out) > zulipMaxContent {
		const marker = "\n\n[truncated by zulip-smtp]"
		out = out[:zulipMaxContent-len(marker)] + marker
	}
	return out
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	reBreak       = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</tr>|</li>|</h[1-6]>`)
	reAnchor      = regexp.MustCompile(`(?is)<a\b[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlankRun    = regexp.MustCompile(`\n{3,}`)
	reTrailWS     = regexp.MustCompile(`[ \t]+\n`)
)

// htmlToText renders an HTML body as something readable in a chat message.
//
// ★ Anchors are handled specially rather than stripped. A password-reset mail
// whose only HTML is `<a href="https://…token">Reset</a>` would otherwise arrive
// as the word "Reset" and nothing else -- the message would look fine and be
// completely useless. Senders that provide text/plain never reach this path.
func htmlToText(in string) string {
	if strings.TrimSpace(in) == "" {
		return ""
	}

	out := reScriptStyle.ReplaceAllString(in, "")
	out = reBreak.ReplaceAllString(out, "\n")

	out = reAnchor.ReplaceAllStringFunc(out, func(m string) string {
		groups := reAnchor.FindStringSubmatch(m)
		if len(groups) != 3 {
			return m
		}
		href := strings.TrimSpace(groups[1])
		text := strings.TrimSpace(reTag.ReplaceAllString(groups[2], ""))
		switch {
		case text == "" || text == href:
			return href
		default:
			return text + " ( " + href + " )"
		}
	})

	out = reTag.ReplaceAllString(out, "")
	out = html.UnescapeString(out)
	out = reTrailWS.ReplaceAllString(out, "\n")
	out = reBlankRun.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}
