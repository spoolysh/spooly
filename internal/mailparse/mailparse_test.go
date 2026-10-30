package mailparse

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spoolysh/spooly/internal/db"
)

func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(s, "\n", "\r\n"))
}

func TestParseMultipart(t *testing.T) {
	raw := crlf(`From: "Ann Lee" <Ann@Example.com>
To: hello@spooly.sh, Bob <bob@example.com>
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
	if len(p.Parts.Attachments()) != 1 || p.Parts.Attachments()[0].Name != "invoice.pdf" || p.Parts.Attachments()[0].Size != 8 {
		t.Errorf("Attachments = %+v", p.Parts.Attachments())
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
	attachments := p.Parts.Attachments()
	if len(attachments) != 2 {
		t.Fatalf("attachments = %+v", attachments)
	}
	for i, want := range []struct {
		name, contentType, content string
	}{{"a.txt", "text/plain", "first"}, {"b.png", "image/png", "\x89PNG"}} {
		a := attachments[i]
		r, err := ReadPart(bytes.NewReader(raw[a.Offset : a.Offset+a.Length]))
		if err != nil {
			t.Fatalf("ReadPart(%d): %v", i, err)
		}
		content, _ := io.ReadAll(r)
		if a.Name != want.name || a.Type != want.contentType || string(content) != want.content || a.Size != int64(len(content)) {
			t.Errorf("attachment %d = %+v %q", i, a, content)
		}
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
	for _, a := range p.Parts.Attachments() {
		names = append(names, a.Name)
	}
	if got := strings.Join(names, ","); got != "Счет.pdf,café_menu.txt" {
		t.Errorf("file names = %q", got)
	}
}

func TestParseHeaders(t *testing.T) {
	raw := crlf(`Received: from b.example.com; Mon, 26 Oct 2026 10:00:02 +0000
Received: from a.example.com; Mon, 26 Oct 2026 10:00:01 +0000
From: =?UTF-8?B?0JzQsNGI0LA=?= <masha@example.com>
Subject: A long subject
  folded onto two lines
Message-ID: <quote-2@example.com>
In-Reply-To: <quote-reply@example.com> <quote@example.com>
X-Ticket-ID: T-123

Hi
`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var got []string
	for _, h := range p.Headers {
		got = append(got, h.Name+": "+h.Value)
	}
	want := []string{
		"Received: from b.example.com; Mon, 26 Oct 2026 10:00:02 +0000",
		"Received: from a.example.com; Mon, 26 Oct 2026 10:00:01 +0000",
		"From: =?UTF-8?B?0JzQsNGI0LA=?= <masha@example.com>",
		"Subject: A long subject folded onto two lines",
		"Message-ID: <quote-2@example.com>",
		"In-Reply-To: <quote-reply@example.com> <quote@example.com>",
		"X-Ticket-ID: T-123",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Headers =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p.MessageID != "quote-2@example.com" || p.InReplyTo != "quote-reply@example.com" {
		t.Errorf("MessageID = %q, InReplyTo = %q", p.MessageID, p.InReplyTo)
	}
}

func TestParseMalformedMessageID(t *testing.T) {
	for raw, want := range map[string]string{
		"Message-ID: <no-at-sign>\r\n\r\nHi\r\n":     "no-at-sign",
		"Message-ID: bare@example.com\r\n\r\nHi\r\n": "bare@example.com",
		"Subject: none\r\n\r\nHi\r\n":                "",
	} {
		p, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if p.MessageID != want {
			t.Errorf("Parse(%q).MessageID = %q, want %q", raw, p.MessageID, want)
		}
	}
}

// Parse finds the bodies' byte ranges in the raw message, and each reads
// back as the body.
func TestPartRanges(t *testing.T) {
	for name, tc := range map[string]struct {
		raw        []byte
		text, html string
	}{
		"single part": {crlf("Subject: hi\n\nJust text.\n"), "Just text.\r\n", ""},
		"lf only":     {[]byte("Subject: hi\nContent-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/plain\n\nplain\n--b\nContent-Type: text/html\n\n<p>html</p>\n--b--\n"), "plain", "<p>html</p>"},
		"preamble and epilogue": {crlf(`Content-Type: multipart/alternative; boundary="b"

This is a MIME message.
--b
Content-Type: text/plain

plain
--bb
not a boundary
--b
Content-Type: text/html

<p>html</p>
--b--
epilogue
`), "plain\r\n--bb\r\nnot a boundary", "<p>html</p>"},
		"no closing boundary": {crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

caf=E9
--b
Content-Type: application/pdf

%PDF
`), "café", ""},
		"nested": {crlf(`Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: multipart/related; boundary=rel

--rel
Content-Type: multipart/alternative; boundary=alt

--alt
Content-Type: text/plain

plain
--alt
Content-Type: text/html

<p>html <img src="cid:logo"></p>
--alt--
--rel
Content-Type: image/png
Content-ID: <logo>
Content-Transfer-Encoding: base64

iVBORw==
--rel--
--outer--
`), "plain", `<p>html <img src="cid:logo"></p>`},
	} {
		t.Run(name, func(t *testing.T) {
			raw := tc.raw
			p, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if p.Text != tc.text || p.HTML != tc.html {
				t.Errorf("text, html = %q, %q; want %q, %q", p.Text, p.HTML, tc.text, tc.html)
			}
			for _, b := range []struct{ kind, want string }{{db.TextBody, tc.text}, {db.HTMLBody, tc.html}} {
				part, ok := p.Parts.Body(b.kind)
				if !ok {
					if b.want != "" {
						t.Errorf("no %s part", b.kind)
					}
					continue
				}
				r, err := ReadPart(bytes.NewReader(raw[part.Offset : part.Offset+part.Length]))
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := io.ReadAll(r); string(got) != b.want {
					t.Errorf("%s part = %q, want %q", b.kind, got, b.want)
				}
			}
		})
	}
}

// Every part is listed, the ones that aren't bodies included.
func TestParseListsEveryPart(t *testing.T) {
	raw := crlf(`Subject: Sync
Content-Type: multipart/mixed; boundary=m

--m
Content-Type: multipart/alternative; boundary=a

--a
Content-Type: text/plain

plain
--a
Content-Type: text/html

<p>html</p>
--a
Content-Type: text/calendar; method=REQUEST

BEGIN:VCALENDAR
END:VCALENDAR
--a--
--m
Content-Type: text/plain

-- list footer
--m--
`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, pt := range p.Parts {
		types = append(types, pt.Type)
	}
	if got := strings.Join(types, ","); got != "text/plain,text/html,text/calendar,text/plain" {
		t.Errorf("parts = %s", got)
	}
	if p.Text != "plain" || p.HTML != "<p>html</p>" {
		t.Errorf("text, html = %q, %q", p.Text, p.HTML)
	}
	if got := p.Parts.AttachmentIndexes(); fmt.Sprint(got) != "[2 3]" {
		t.Errorf("attachments = %v", got)
	}
}
