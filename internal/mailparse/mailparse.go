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

// Parsed holds what Parse extracts. Text, HTML, and Headers are for the
// snippet and tests; Boxy reads them from the raw message when shown.
type Parsed struct {
	From      db.Address // empty when there's no From header
	To        []db.Address
	Cc        []db.Address
	Subject   string
	Date      *time.Time // nil when missing or invalid
	Headers   []db.Header
	MessageID string // without angle brackets; empty when missing
	InReplyTo string // the first ID, without angle brackets; empty when missing
	Text      string
	HTML      string
	Snippet   string
	Parts     db.Parts
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

	p := &Parsed{To: []db.Address{}, Cc: []db.Address{}, Parts: db.Parts{}}
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

	for _, l := range leaves(raw, 0, len(raw), 0, nil) {
		e, err := readEntity(bytes.NewReader(raw[l.start:l.end]))
		if err != nil {
			continue
		}
		pt := describe(e.Header)
		pt.Offset, pt.Length = int64(l.start), int64(l.end-l.start)
		i := len(p.Parts)
		p.Parts = append(p.Parts, pt)
		// Size counts the decoded bytes, as GetMessagePart serves them.
		switch i {
		case p.Parts.BodyIndex(db.TextBody):
			b, _ := io.ReadAll(e.Body)
			p.Text, p.Parts[i].Size = strings.ToValidUTF8(string(b), "�"), int64(len(b))
		case p.Parts.BodyIndex(db.HTMLBody):
			b, _ := io.ReadAll(e.Body)
			p.HTML, p.Parts[i].Size = strings.ToValidUTF8(string(b), "�"), int64(len(b))
		default:
			p.Parts[i].Size, _ = io.Copy(io.Discard, e.Body)
		}
	}

	p.Snippet = snippet(p.Text, p.HTML)
	return p, nil
}

// ReadHeaders reads the header fields at the start of a raw message, and
// nothing after them.
func ReadHeaders(r io.Reader) ([]db.Header, error) {
	e, err := readEntity(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return headers(mail.Header{Header: e.Header}), nil
}

// ReadPart returns the decoded content of a part, given its bytes in the
// raw message: Part.Length bytes from Part.Offset. Text is decoded to UTF-8.
func ReadPart(r io.Reader) (io.Reader, error) {
	e, err := readEntity(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return e.Body, nil
}

// readEntity reads an entity's header, and returns it with its body
// decoded. An unknown charset or transfer encoding leaves the body as is.
func readEntity(r io.Reader) (*message.Entity, error) {
	e, err := message.Read(r)
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, err
	}
	return e, nil
}

// maxDepth is how deep multiparts may nest; deeper ones are one part.
const maxDepth = 20

// leaf is the byte range of a leaf entity, header included.
type leaf struct{ start, end int }

// leaves appends the leaf entities of the entity at raw[start:end], in order:
// the parts of multiparts, depth first, and everything else whole. Attached
// messages (message/rfc822) are leaves.
func leaves(raw []byte, start, end, depth int, out []leaf) []leaf {
	body := start + bodyStart(raw[start:end])
	e, err := readEntity(bytes.NewReader(raw[start:body]))
	if err == nil && depth < maxDepth {
		if mt, params, err := e.Header.ContentType(); err == nil && strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
			for _, r := range multipartRanges(raw[body:end], params["boundary"]) {
				out = leaves(raw, body+r.start, body+r.end, depth+1, out)
			}
			return out
		}
	}
	return append(out, leaf{start, end})
}

// bodyStart returns where an entity's body starts: after the blank line
// that ends its header, or at the end if there's none.
func bodyStart(b []byte) int {
	for i := 0; i < len(b); {
		if b[i] == '\n' {
			return i + 1
		}
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			return i + 2
		}
		eol := bytes.IndexByte(b[i:], '\n')
		if eol < 0 {
			break
		}
		i += eol + 1
	}
	return len(b)
}

// multipartRanges returns the ranges of a multipart body's parts, between
// its boundary lines. The line break before a boundary line is part of it,
// as RFC 2046 says. Without a closing boundary, the last part runs to the
// end.
func multipartRanges(body []byte, boundary string) []leaf {
	delim := []byte("--" + boundary)
	var parts []leaf
	start := -1
	for i := 0; i < len(body); {
		next := len(body)
		if eol := bytes.IndexByte(body[i:], '\n'); eol >= 0 {
			next = i + eol + 1
		}
		if line := body[i:next]; bytes.HasPrefix(line, delim) {
			rest := bytes.TrimRight(line[len(delim):], " \t\r\n")
			closing := string(rest) == "--"
			if closing || len(rest) == 0 {
				if start >= 0 {
					end := i
					if end > start && body[end-1] == '\n' {
						end--
					}
					if end > start && body[end-1] == '\r' {
						end--
					}
					parts = append(parts, leaf{start, end})
				}
				if closing {
					return parts
				}
				start = next
			}
		}
		i = next
	}
	if start >= 0 && start < len(body) {
		parts = append(parts, leaf{start, len(body)})
	}
	return parts
}

// describe returns a part's media type, file name, Content-ID, and
// disposition.
func describe(h message.Header) db.Part {
	var pt db.Part
	ct, params, _ := h.ContentType()
	disp, dparams, _ := h.ContentDisposition()
	pt.Type, pt.Name, pt.Disposition = ct, dparams["filename"], strings.ToLower(disp)
	if pt.Name == "" {
		pt.Name = params["name"]
	}
	if name := filename(h); name != "" {
		pt.Name = name
	}
	if id := strings.TrimSpace(h.Get("Content-Id")); id != "" {
		pt.CID = strings.ToValidUTF8(strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">"), "�")
	}
	if pt.Type == "" {
		pt.Type = "application/octet-stream"
	}
	return pt
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
