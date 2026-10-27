package mailparse

import (
	"errors"
	"strings"
	"testing"
)

func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(s, "\n", "\r\n"))
}

func TestParseMultipart(t *testing.T) {
	raw := crlf(`From: "Ann Lee" <Ann@Example.com>
To: hello@boxy.sh, Bob <bob@example.com>
Cc: =?UTF-8?B?0JzQsNGI0LA=?= <masha@example.com>
Subject: =?UTF-8?Q?Caf=C3=A9?= order
Date: Mon, 26 Oct 2026 10:00:00 +0300
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: multipart/alternative; boundary=inner

--inner
Content-Type: text/plain; charset=utf-8

Hello   there,
your order is ready.
--inner
Content-Type: text/html; charset=utf-8

<p>Hello <b>there</b></p>
--inner--
--outer
Content-Type: application/pdf
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQ=
--outer--
`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.From.Name != "Ann Lee" || p.From.Address != "ann@example.com" {
		t.Errorf("From = %+v", p.From)
	}
	if len(p.To) != 2 || p.To[1].Address != "bob@example.com" {
		t.Errorf("To = %+v", p.To)
	}
	if len(p.Cc) != 1 || p.Cc[0].Name != "Маша" {
		t.Errorf("Cc = %+v", p.Cc)
	}
	if p.Subject != "Café order" {
		t.Errorf("Subject = %q", p.Subject)
	}
	if p.Date == nil || p.Date.Hour() != 7 {
		t.Errorf("Date = %v, want 07:00 UTC", p.Date)
	}
	if !strings.Contains(p.Text, "your order is ready") {
		t.Errorf("Text = %q", p.Text)
	}
	if !strings.Contains(p.HTML, "<b>there</b>") {
		t.Errorf("HTML = %q", p.HTML)
	}
	if p.Snippet != "Hello there, your order is ready." {
		t.Errorf("Snippet = %q", p.Snippet)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "invoice.pdf" || p.Attachments[0].Size != 8 {
		t.Errorf("Attachments = %+v", p.Attachments)
	}
}

func TestParseHTMLOnlySnippet(t *testing.T) {
	p, err := Parse(crlf(`Subject: hi
Content-Type: text/html

<html><head><style>p{}</style></head><body><p>Only   <i>HTML</i></p></body></html>
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Snippet != "Only HTML" {
		t.Errorf("Snippet = %q", p.Snippet)
	}
	if p.From.Address != "" || len(p.To) != 0 || p.Date != nil {
		t.Errorf("missing headers parsed as %+v %+v %v", p.From, p.To, p.Date)
	}
}

func TestParseLongSnippet(t *testing.T) {
	p, err := Parse(crlf("Subject: x\n\n" + strings.Repeat("я", 500) + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := len([]rune(p.Snippet)); n != SnippetLength {
		t.Errorf("snippet length = %d", n)
	}
}

func TestAttachment(t *testing.T) {
	raw := crlf(`Subject: files
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

body
--b
Content-Type: text/plain; name="a.txt"
Content-Disposition: attachment; filename="a.txt"

first
--b
Content-Type: image/png
Content-Disposition: inline; filename="b.png"
Content-Transfer-Encoding: base64

iVBORw==
--b--
`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		name, contentType, content string
	}{{"a.txt", "text/plain", "first"}, {"b.png", "image/png", "\x89PNG"}} {
		a, content, err := Attachment(raw, i)
		if err != nil {
			t.Fatalf("Attachment(%d): %v", i, err)
		}
		if a.Filename != want.name || a.ContentType != want.contentType || string(content) != want.content {
			t.Errorf("Attachment(%d) = %+v %q", i, a, content)
		}
		if a != p.Attachments[i] {
			t.Errorf("Attachment(%d) = %+v, Parse listed %+v", i, a, p.Attachments[i])
		}
	}
	if _, _, err := Attachment(raw, 2); !errors.Is(err, ErrNoAttachment) {
		t.Errorf("Attachment(2) = %v, want ErrNoAttachment", err)
	}
}

func TestNonUTF8FileNames(t *testing.T) {
	raw := crlf(`Subject: files
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: application/pdf
Content-Disposition: attachment; filename*=windows-1251''%D1%F7%E5%F2.pdf

x
--b
Content-Type: text/plain; name*0*=iso-8859-1''caf%E9; name*1*=_menu; name*2=.txt
Content-Disposition: attachment

y
--b--
`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range p.Attachments {
		names = append(names, a.Filename)
	}
	if got := strings.Join(names, ","); got != "Счет.pdf,café_menu.txt" {
		t.Errorf("file names = %q", got)
	}
}
