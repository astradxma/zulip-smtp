package main

import (
	"strings"
	"testing"
)

func TestExtractMessagePrefersPlainText(t *testing.T) {
	raw := "From: a@b.com\r\n" +
		"To: c@d.com\r\n" +
		"Subject: Update Your Account\r\n" +
		"Content-Type: multipart/alternative; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the plain one\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>the html one</p>\r\n" +
		"--BOUND--\r\n"

	subject, body, n, err := extractMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("extractMessage: %v", err)
	}
	if subject != "Update Your Account" {
		t.Errorf("subject = %q", subject)
	}
	if body != "the plain one" {
		t.Errorf("body = %q, want the plain part", body)
	}
	if n == 0 {
		t.Error("byte count not recorded")
	}
}

// The case that matters: an HTML-only sender whose entire payload is a link.
// Stripping tags naively would deliver the word "Reset" and lose the token.
func TestExtractMessageKeepsLinkFromHTMLOnly(t *testing.T) {
	raw := "Subject: Reset\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		`<p>Click <a href="https://kc.example/token?key=abc123">Reset</a> now.</p>` + "\r\n"

	_, body, _, err := extractMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("extractMessage: %v", err)
	}
	if !strings.Contains(body, "https://kc.example/token?key=abc123") {
		t.Errorf("link lost in html conversion: %q", body)
	}
}

func TestHTMLToText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare link text equals href", `<a href="https://x.test/a">https://x.test/a</a>`, "https://x.test/a"},
		{"entities decoded", `<p>a &amp; b</p>`, "a & b"},
		{"script dropped", `<script>evil()</script><p>safe</p>`, "safe"},
		{"empty stays empty", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := htmlToText(c.in); got != c.want {
				t.Errorf("htmlToText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFormatForZulip(t *testing.T) {
	got := formatForZulip("Hello", "world")
	if got != "**Hello**\n\nworld" {
		t.Errorf("got %q", got)
	}

	if got := formatForZulip("", "body only"); got != "body only" {
		t.Errorf("empty subject should add nothing, got %q", got)
	}
}

func TestFormatForZulipTruncates(t *testing.T) {
	got := formatForZulip("", strings.Repeat("x", zulipMaxContent*2))
	if len(got) > zulipMaxContent {
		t.Errorf("length %d exceeds Zulip's limit %d", len(got), zulipMaxContent)
	}
	if !strings.HasSuffix(got, "[truncated by zulip-smtp]") {
		t.Error("truncation should be visible in the message")
	}
}
