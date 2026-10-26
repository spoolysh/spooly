// Package mailparse extracts the fields Boxy stores from a raw RFC 5322
// message: headers, text and HTML bodies, a snippet, and attachments.
package mailparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 charsets
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"

	"github.com/boxysh/boxy/internal/db"
)

// SnippetLength is the most characters a snippet has.
const SnippetLength = 200

// Parsed holds what Parse extracts.
type Parsed struct {
	From        db.Address // empty when there's no From header
	To          []db.Address
	Cc          []db.Address
	Subject     string
	Date        *time.Time // nil when missing or invalid
	Text        string
	HTML        string
	Snippet     string
	Attachments []db.Attachment
}

// ErrInvalid reports a message whose header can't be parsed at all.
var ErrInvalid = errors.New("not an RFC 5322 message")

// Parse parses raw. It is lenient: bad addresses, dates, charsets, or parts
// leave those fields empty rather than failing.
func Parse(raw []byte) (*Parsed, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer mr.Close()

	p := &Parsed{To: []db.Address{}, Cc: []db.Address{}, Attachments: []db.Attachment{}}
	h := mr.Header
	if from := addressList(h, "From"); len(from) > 0 {
		p.From = from[0]
	}
	p.To = addressList(h, "To")
	p.Cc = addressList(h, "Cc")
	if s, err := h.Subject(); err == nil {
		p.Subject = s
	} else {
		p.Subject = h.Get("Subject")
	}
	if d, err := h.Date(); err == nil && !d.IsZero() {
		d = d.UTC()
		p.Date = &d
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				continue
			}
			break // keep what was parsed so far
		}
		p.addPart(part)
	}

	p.Snippet = snippet(p.Text, p.HTML)
	return p, nil
}

func (p *Parsed) addPart(part *mail.Part) {
	switch h := part.Header.(type) {
	case *mail.InlineHeader:
		ct, params, _ := h.ContentType()
		filename := params["name"]
		if _, dp, err := h.ContentDisposition(); err == nil && dp["filename"] != "" {
			filename = dp["filename"]
		}
		if filename == "" {
			switch {
			case ct == "text/plain" && p.Text == "":
				p.Text = readText(part.Body)
				return
			case ct == "text/html" && p.HTML == "":
				p.HTML = readText(part.Body)
				return
			case strings.HasPrefix(ct, "text/"):
				return // alternative bodies beyond the first
			}
		}
		p.addAttachment(filename, ct, part.Body)
	case *mail.AttachmentHeader:
		filename, _ := h.Filename()
		ct, _, _ := h.ContentType()
		p.addAttachment(filename, ct, part.Body)
	}
}

func (p *Parsed) addAttachment(filename, contentType string, body io.Reader) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	n, _ := io.Copy(io.Discard, body)
	p.Attachments = append(p.Attachments, db.Attachment{Filename: filename, ContentType: contentType, Size: n})
}

func readText(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return strings.ToValidUTF8(string(b), "�")
}

func addressList(h mail.Header, key string) []db.Address {
	list, err := h.AddressList(key)
	if err != nil {
		// Fall back to the decoded header as a single address, so a
		// malformed header still shows something.
		v, derr := new(mime.WordDecoder).DecodeHeader(h.Get(key))
		if derr != nil || strings.TrimSpace(v) == "" {
			return []db.Address{}
		}
		return []db.Address{{Addr: strings.TrimSpace(v)}}
	}
	addrs := make([]db.Address, 0, len(list))
	for _, a := range list {
		addrs = append(addrs, db.Address{Name: a.Name, Addr: strings.ToLower(a.Address)})
	}
	return addrs
}

// snippet is the start of the text body, or of the HTML body's text, with
// whitespace collapsed.
func snippet(text, htmlBody string) string {
	if strings.TrimSpace(text) == "" {
		text = htmlText(htmlBody)
	}
	s := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(s) <= SnippetLength {
		return s
	}
	return string([]rune(s)[:SnippetLength])
}

// htmlText returns the visible text of an HTML document.
func htmlText(s string) string {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			if name, _ := z.TagName(); isHidden(name) {
				skip++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); isHidden(name) && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}

func isHidden(tag []byte) bool {
	switch string(tag) {
	case "script", "style", "head", "title":
		return true
	}
	return false
}
