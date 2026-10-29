package imap

import (
	"bytes"
	"io"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// Search matching is adapted from go-imap's imapmemserver (MIT license,
// copyright the go-imap authors), with flags from labels and the raw
// message read only for criteria that need it.

func (c *session) Search(kind imapserver.NumKind, criteria *goimap.SearchCriteria, options *goimap.SearchOptions) (*goimap.SearchData, error) {
	c.staticCriteria(criteria)
	raws := needsRaw(criteria)
	// Header criteria need only the header block; text ones the whole
	// message.
	headerOnly := !needsBody(criteria)

	var data goimap.SearchData
	var seqSet goimap.SeqSet
	var uidSet goimap.UIDSet
	for i, m := range c.msgs {
		seq := uint32(i) + 1
		var entity func() *gomessage.Entity
		if raws {
			var raw []byte
			loaded := false
			entity = func() *gomessage.Entity {
				if !loaded {
					raw, _ = c.raw(m, headerOnly)
					loaded = true
				}
				e, _ := gomessage.Read(bytes.NewReader(raw))
				if e == nil {
					e, _ = gomessage.New(gomessage.Header{}, bytes.NewReader(nil))
				}
				return e
			}
		}
		if !match(seq, m, criteria, entity) {
			continue
		}
		num := seq
		if kind == imapserver.NumKindUID {
			num = m.Seq
			uidSet.AddNum(goimap.UID(m.Seq))
		} else {
			seqSet.AddNum(seq)
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if num > data.Max {
			data.Max = num
		}
		data.Count++
	}
	if kind == imapserver.NumKindUID {
		data.All = uidSet
	} else {
		data.All = seqSet
	}
	return &data, nil
}

// staticCriteria resolves "*" in the criteria's number sets.
func (c *session) staticCriteria(criteria *goimap.SearchCriteria) {
	maxSeq := uint32(len(c.msgs))
	maxUID := uint32(0)
	if len(c.msgs) > 0 {
		maxUID = c.msgs[len(c.msgs)-1].Seq
	}
	for _, set := range criteria.SeqNum {
		for i := range set {
			set[i].Start, set[i].Stop = staticRange(set[i].Start, set[i].Stop, maxSeq)
		}
	}
	for _, set := range criteria.UID {
		for i := range set {
			start, stop := staticRange(uint32(set[i].Start), uint32(set[i].Stop), maxUID)
			set[i].Start, set[i].Stop = goimap.UID(start), goimap.UID(stop)
		}
	}
	for i := range criteria.Not {
		c.staticCriteria(&criteria.Not[i])
	}
	for i := range criteria.Or {
		c.staticCriteria(&criteria.Or[i][0])
		c.staticCriteria(&criteria.Or[i][1])
	}
}

// needsRaw reports whether criteria look at headers or text.
func needsRaw(criteria *goimap.SearchCriteria) bool {
	if len(criteria.Header) > 0 || len(criteria.Text) > 0 || len(criteria.Body) > 0 ||
		!criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
		return true
	}
	for i := range criteria.Not {
		if needsRaw(&criteria.Not[i]) {
			return true
		}
	}
	for i := range criteria.Or {
		if needsRaw(&criteria.Or[i][0]) || needsRaw(&criteria.Or[i][1]) {
			return true
		}
	}
	return false
}

// needsBody reports whether criteria look at the text, not just headers.
func needsBody(criteria *goimap.SearchCriteria) bool {
	if len(criteria.Text) > 0 || len(criteria.Body) > 0 {
		return true
	}
	for i := range criteria.Not {
		if needsBody(&criteria.Not[i]) {
			return true
		}
	}
	for i := range criteria.Or {
		if needsBody(&criteria.Or[i][0]) || needsBody(&criteria.Or[i][1]) {
			return true
		}
	}
	return false
}

func hasFlag(m *msg, flag goimap.Flag) bool {
	for _, f := range m.flags() {
		if strings.EqualFold(string(f), string(flag)) {
			return true
		}
	}
	return false
}

// match reports whether a message meets criteria. entity reads the raw
// message; it is nil when no criterion needs it.
func match(seq uint32, m *msg, criteria *goimap.SearchCriteria, entity func() *gomessage.Entity) bool {
	for _, set := range criteria.SeqNum {
		if !set.Contains(seq) {
			return false
		}
	}
	for _, set := range criteria.UID {
		if !set.Contains(goimap.UID(m.Seq)) {
			return false
		}
	}
	if !matchDate(m.ReceivedAt, criteria.Since, criteria.Before) {
		return false
	}
	for _, flag := range criteria.Flag {
		if !hasFlag(m, flag) {
			return false
		}
	}
	for _, flag := range criteria.NotFlag {
		if hasFlag(m, flag) {
			return false
		}
	}
	if criteria.Larger != 0 && m.Size <= criteria.Larger {
		return false
	}
	if criteria.Smaller != 0 && m.Size >= criteria.Smaller {
		return false
	}

	if entity != nil {
		header := mail.Header{Header: entity().Header}
		for _, f := range criteria.Header {
			if !matchHeaderFields(header.FieldsByKey(f.Key), f.Value) {
				return false
			}
		}
		if !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
			t, err := header.Date()
			if err != nil || !matchDate(t, criteria.SentSince, criteria.SentBefore) {
				return false
			}
		}
		for _, text := range criteria.Text {
			if !matchEntity(entity(), text, true) {
				return false
			}
		}
		for _, body := range criteria.Body {
			if !matchEntity(entity(), body, false) {
				return false
			}
		}
	}

	for i := range criteria.Not {
		if match(seq, m, &criteria.Not[i], entity) {
			return false
		}
	}
	for _, or := range criteria.Or {
		if !match(seq, m, &or[0], entity) && !match(seq, m, &or[1], entity) {
			return false
		}
	}
	return true
}

func matchDate(t, since, before time.Time) bool {
	// RFC 3501 compares dates without times or zones.
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

func matchHeaderFields(fields gomessage.HeaderFields, pattern string) bool {
	if pattern == "" {
		return fields.Len() > 0
	}
	pattern = strings.ToLower(pattern)
	for fields.Next() {
		v, _ := fields.Text()
		if strings.Contains(strings.ToLower(v), pattern) {
			return true
		}
	}
	return false
}

func matchEntity(e *gomessage.Entity, pattern string, includeHeader bool) bool {
	if pattern == "" {
		return true
	}
	if includeHeader && matchHeaderFields(e.Header.Fields(), pattern) {
		return true
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return false
			}
			if err != nil {
				return false
			}
			if matchEntity(part, pattern, includeHeader) {
				return true
			}
		}
	}
	t, _, err := e.Header.ContentType()
	if err != nil || (!strings.HasPrefix(t, "text/") && !strings.HasPrefix(t, "message/")) {
		return false
	}
	buf, err := io.ReadAll(e.Body)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(buf), bytes.ToLower([]byte(pattern)))
}
