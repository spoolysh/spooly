// Package mailparse extracts the fields Boxy stores from a raw RFC 5322
// message: header fields, text and HTML bodies, a snippet, and attachments.
package mailparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset" // also registers non-UTF-8 charsets
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
	Headers     []db.Header
	MessageID   string // without angle brackets; empty when missing
	InReplyTo   string // the first ID, without angle brackets; empty when missing
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
	mr, err := newReader(raw)
	if err != nil {
		return nil, err
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
	p.Headers = headers(h)
	p.MessageID = msgID(h, "Message-Id")
	p.InReplyTo = msgID(h, "In-Reply-To")

	walkParts(mr, func(pt part) bool {
		switch pt.kind {
		case partText:
			p.Text = readText(pt.body)
		case partHTML:
			p.HTML = readText(pt.body)
		case partAttachment:
			n, _ := io.Copy(io.Discard, pt.body)
			p.Attachments = append(p.Attachments, db.Attachment{Filename: pt.filename, ContentType: pt.contentType, Size: n})
		}
		return true
	})

	p.Snippet = snippet(p.Text, p.HTML)
	return p, nil
}

// ErrNoAttachment reports an attachment index past the message's last one.
var ErrNoAttachment = errors.New("no such attachment")

// Attachment returns attachment index of raw, counted from 0 in the order
// Parse lists them, with its decoded content.
func Attachment(raw []byte, index int) (db.Attachment, []byte, error) {
	mr, err := newReader(raw)
	if err != nil {
		return db.Attachment{}, nil, err
	}
	defer mr.Close()

	var found *db.Attachment
	var content []byte
	i := 0
	walkParts(mr, func(pt part) bool {
		if pt.kind != partAttachment {
			return true
		}
		if i < index {
			i++
			return true
		}
		content, _ = io.ReadAll(pt.body)
		found = &db.Attachment{Filename: pt.filename, ContentType: pt.contentType, Size: int64(len(content))}
		return false
	})
	if found == nil {
		return db.Attachment{}, nil, fmt.Errorf("%w: %d", ErrNoAttachment, index)
	}
	return *found, content, nil
}

func newReader(raw []byte) (*mail.Reader, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return mr, nil
}

type partKind int

const (
	partSkip       partKind = iota
	partText                // the text body
	partHTML                // the HTML body
	partAttachment          // anything else with content
)

type part struct {
	kind        partKind
	filename    string
	contentType string
	body        io.Reader
}

// walkParts calls fn with each part of mr in order, classified, until fn
// returns false. The first text/plain and text/html parts without a filename
// are the bodies; other text parts without one are alternative bodies and
// skipped; everything else is an attachment. Parse and Attachment both walk
// this way, so attachment indexes agree.
func walkParts(mr *mail.Reader, fn func(part) bool) {
	var haveText, haveHTML bool
	for {
		mp, err := mr.NextPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				continue
			}
			return // keep what was walked so far
		}

		pt := part{kind: partAttachment, body: mp.Body}
		switch h := mp.Header.(type) {
		case *mail.InlineHeader:
			ct, params, _ := h.ContentType()
			pt.contentType, pt.filename = ct, params["name"]
			if _, dp, err := h.ContentDisposition(); err == nil && dp["filename"] != "" {
				pt.filename = dp["filename"]
			}
			if name := filename(h.Header); name != "" {
				pt.filename = name
			}
			if pt.filename == "" {
				switch {
				case ct == "text/plain" && !haveText:
					pt.kind, haveText = partText, true
				case ct == "text/html" && !haveHTML:
					pt.kind, haveHTML = partHTML, true
				case strings.HasPrefix(ct, "text/"):
					pt.kind = partSkip
				}
			}
		case *mail.AttachmentHeader:
			pt.filename, _ = h.Filename()
			if name := filename(h.Header); name != "" {
				pt.filename = name
			}
			pt.contentType, _, _ = h.ContentType()
		default:
			continue
		}
		if pt.kind == partAttachment && pt.contentType == "" {
			pt.contentType = "application/octet-stream"
		}
		if !fn(pt) {
			return
		}
	}
}

// headers returns h's fields in order, with names as written and values
// unfolded but not decoded.
func headers(h mail.Header) []db.Header {
	list := []db.Header{}
	fields := h.Fields()
	for fields.Next() {
		name := fields.Key()
		if raw, err := fields.Raw(); err == nil {
			if n, _, ok := bytes.Cut(raw, []byte(":")); ok {
				name = string(bytes.TrimSpace(n))
			}
		}
		list = append(list, db.Header{
			Name:  strings.ToValidUTF8(name, "�"),
			Value: strings.ToValidUTF8(fields.Value(), "�"),
		})
	}
	return list
}

// msgID returns the first message ID in header key, without angle brackets.
// A malformed one, such as without an @, is kept as written.
func msgID(h mail.Header, key string) string {
	if ids, err := h.MsgIDList(key); len(ids) > 0 {
		return ids[0]
	} else if err == nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(h.Get(key)), " ")
	return strings.ToValidUTF8(strings.Trim(first, "<>"), "�")
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
		return []db.Address{{Address: strings.TrimSpace(v)}}
	}
	addrs := make([]db.Address, 0, len(list))
	for _, a := range list {
		addrs = append(addrs, db.Address{Name: a.Name, Address: strings.ToLower(a.Address)})
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

// filename decodes a part's file name from its RFC 2231 extended form, such
// as:
//
//	filename*=windows-1251''%D1%F7%E5%F2.pdf
//
// Go's mime.ParseMediaType decodes only UTF-8 there and drops other
// charsets, either entirely or just the first of several continuations, so a
// name found here wins. It tries Content-Disposition's filename, then
// Content-Type's name.
func filename(h message.Header) string {
	for _, f := range []struct{ header, param string }{
		{"Content-Disposition", "filename"},
		{"Content-Type", "name"},
	} {
		if name := rfc2231Param(h.Get(f.header), f.param); name != "" {
			return name
		}
	}
	return ""
}

// rfc2231Param decodes parameter name of a header value from its extended
// form (name*=charset'lang'value), which may be split into numbered
// continuations (name*0*=..., name*1*=...).
func rfc2231Param(value, name string) string {
	segments := map[int]string{}
	extended := map[int]bool{}
	for _, field := range strings.Split(value, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		rest, ok := strings.CutPrefix(key, strings.ToLower(name)+"*")
		if !ok {
			continue
		}
		n := 0
		if num := strings.TrimSuffix(rest, "*"); num != "" {
			var err error
			if n, err = strconv.Atoi(num); err != nil {
				continue
			}
		}
		segments[n] = strings.Trim(strings.TrimSpace(val), `"`)
		extended[n] = rest == "" || strings.HasSuffix(rest, "*")
	}
	if len(segments) == 0 || !extended[0] {
		return ""
	}

	// The first segment starts with charset'language'.
	cs, first, ok := strings.Cut(segments[0], "'")
	if !ok {
		return ""
	}
	if _, first, ok = strings.Cut(first, "'"); !ok {
		return ""
	}
	var raw []byte
	for i := 0; ; i++ {
		seg, ok := segments[i]
		if !ok {
			break
		}
		if i == 0 {
			seg = first
		}
		if !extended[i] {
			raw = append(raw, seg...)
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return ""
		}
		raw = append(raw, decoded...)
	}
	if cs == "" || strings.EqualFold(cs, "utf-8") || strings.EqualFold(cs, "us-ascii") {
		return strings.ToValidUTF8(string(raw), "_")
	}
	r, err := charset.Reader(cs, bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return string(b)
}
