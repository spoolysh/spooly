package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Address is a name and address from a From/To/Cc header.
type Address struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Header is a header field as received: its name as written, and its value
// unfolded but not decoded.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Part is a MIME body part (a leaf: not a multipart) of a message, and
// where it is in the raw message, so it can be read without the rest.
type Part struct {
	Type string `json:"type"`
	// Disposition is the Content-Disposition type, such as "inline" or
	// "attachment"; empty when missing.
	Disposition string `json:"disposition,omitempty"`
	Name        string `json:"name,omitempty"` // the file name
	// CID is the Content-ID, without angle brackets.
	CID  string `json:"cid,omitempty"`
	Size int64  `json:"size"` // decoded
	// Offset and Length are its bytes in the raw message, header included.
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Body media types.
const (
	TextBody = "text/plain"
	HTMLBody = "text/html"
)

// Parts are a message's parts, in the order of the raw message.
type Parts []Part

// BodyIndex returns the index of the body of mediaType, TextBody or
// HTMLBody, or -1 if there's none: the first part of that type that isn't
// an attachment and has no file name.
func (ps Parts) BodyIndex(mediaType string) int {
	for i, p := range ps {
		if p.Type == mediaType && p.Disposition != "attachment" && p.Name == "" {
			return i
		}
	}
	return -1
}

// Body returns the body of mediaType, TextBody or HTMLBody, if any.
func (ps Parts) Body(mediaType string) (Part, bool) {
	if i := ps.BodyIndex(mediaType); i >= 0 {
		return ps[i], true
	}
	return Part{}, false
}

// AttachmentIndexes returns the indexes of the parts that aren't a body,
// in order: what a mail client lists as attachments.
func (ps Parts) AttachmentIndexes() []int {
	text, html := ps.BodyIndex(TextBody), ps.BodyIndex(HTMLBody)
	list := []int{}
	for i := range ps {
		if i != text && i != html {
			list = append(list, i)
		}
	}
	return list
}

// Attachments returns the parts that aren't a body, in order.
func (ps Parts) Attachments() []Part {
	list := []Part{}
	for _, i := range ps.AttachmentIndexes() {
		list = append(list, ps[i])
	}
	return list
}

// AttachmentCount counts the attachments a list shows a paperclip for: the
// parts that aren't a body, nor what looks like an image the HTML body
// embeds (one with a Content-ID that isn't an attachment). The message page
// checks the HTML itself.
func (ps Parts) AttachmentCount() int {
	n := 0
	for _, p := range ps.Attachments() {
		if !(p.CID != "" && strings.HasPrefix(p.Type, "image/") && p.Disposition != "attachment") {
			n++
		}
	}
	return n
}

// Flags are a message's state, wherever it is, as IMAP's \Seen, \Answered,
// \Flagged, \Draft, and $Forwarded, in that order.
type Flags struct {
	Seen      bool // read
	Answered  bool // replied to
	Flagged   bool // starred
	Draft     bool // not finished
	Forwarded bool
}

// Message is a stored message. Its raw source is in object storage under its
// ID; everything here is parsed from it, except the envelope, folder,
// labels, and flags.
type Message struct {
	ID        string
	MailboxID string
	MailFrom  string // SMTP envelope sender (MAIL FROM)
	RcptTo    string // SMTP envelope recipient (RCPT TO)
	From      Address
	To        []Address
	Cc        []Address
	Subject   string
	Date      *time.Time
	Snippet   string
	Size      int64
	Parts     Parts
	// Text, HTML, and Headers aren't stored: they're read from the raw
	// message when shown.
	Text           string
	HTML           string
	Headers        []Header
	MessageID      string // Message-ID, without angle brackets
	InReplyTo      string // the first In-Reply-To ID, without angle brackets
	Folder         string // where it is: one of Folders
	Labels         []string
	IdempotencyKey string // empty when not set
	ReceivedAt     time.Time
	Flags
}

var (
	// ErrMessageNotFound reports that no message has the requested ID.
	ErrMessageNotFound = errors.New("message not found")
	// ErrUnknownLabel reports a label name that doesn't exist.
	ErrUnknownLabel = errors.New("unknown label")
	// ErrUnknownFolder reports a folder name that isn't one of Folders.
	ErrUnknownFolder = errors.New("unknown folder")
)

// messageColumns are the columns of a message.
const messageColumns = `id, mailbox_id, mail_from, rcpt_to, from_name, from_address, to_addresses, cc_addresses,
	subject, date, snippet, size, parts, folder, seen, answered, flagged, draft, forwarded,
	idempotency_key, received_at`

func scanMessage(row interface{ Scan(...any) error }, extra ...any) (*Message, error) {
	var m Message
	var to, cc, parts string
	var date sql.NullTime
	var key sql.NullString
	dest := []any{&m.ID, &m.MailboxID, &m.MailFrom, &m.RcptTo, &m.From.Name, &m.From.Address, &to, &cc,
		&m.Subject, &date, &m.Snippet, &m.Size, &parts, &m.Folder, &m.Seen, &m.Answered, &m.Flagged, &m.Draft, &m.Forwarded,
		&key, &m.ReceivedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	if date.Valid {
		m.Date = &date.Time
	}
	m.IdempotencyKey = key.String
	for _, f := range []struct {
		src string
		dst any
	}{{to, &m.To}, {cc, &m.Cc}, {parts, &m.Parts}} {
		if err := json.Unmarshal([]byte(f.src), f.dst); err != nil {
			return nil, fmt.Errorf("decoding message %s: %w", m.ID, err)
		}
	}
	return &m, nil
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs, slices, and maps are marshaled
	}
	return string(b)
}

// CreateMessage stores m in its folder, with its flags and the given labels.
// If m has an idempotency key that its mailbox already used, the stored
// message is returned instead, with created false.
func (d *DB) CreateMessage(ctx context.Context, m Message, labels []string) (stored *Message, created bool, err error) {
	if !IsFolder(m.Folder) {
		return nil, false, fmt.Errorf("%w: %q", ErrUnknownFolder, m.Folder)
	}
	if m.IdempotencyKey != "" {
		existing, err := d.messageByIdempotencyKey(ctx, m.MailboxID, m.IdempotencyKey)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrMessageNotFound) {
			return nil, false, err
		}
	}

	tx, err := d.begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}
	defer tx.Rollback()
	if err := d.insertMessage(ctx, tx, m, labels, EventMessageCreated); err != nil {
		tx.Rollback()
		// Lost a race with a retry using the same idempotency key.
		if m.IdempotencyKey != "" {
			if existing, gerr := d.messageByIdempotencyKey(ctx, m.MailboxID, m.IdempotencyKey); gerr == nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	if err := d.commit(ctx, tx); err != nil {
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}

	stored, err = d.GetMessage(ctx, m.ID)
	return stored, true, err
}

// insertMessage inserts m in its folder, with its flags and labels, and logs
// it as event, created or restored.
func (d *DB) insertMessage(ctx context.Context, tx *txn, m Message, labels []string, event string) error {
	if m.To == nil {
		m.To = []Address{}
	}
	if m.Cc == nil {
		m.Cc = []Address{}
	}
	if m.Parts == nil {
		m.Parts = Parts{}
	}
	var key sql.NullString
	if m.IdempotencyKey != "" {
		key = sql.NullString{String: m.IdempotencyKey, Valid: true}
	}
	var date sql.NullTime
	if m.Date != nil {
		date = sql.NullTime{Time: m.Date.UTC(), Valid: true}
	}

	c := &change{d: d, tx: tx, mailboxID: m.MailboxID}
	seq, err := c.next(ctx)
	if err != nil {
		return err
	}
	query := d.q(`INSERT INTO messages (` + messageColumns + `, message_id, in_reply_to, folder_seq, attachments)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err = tx.ExecContext(ctx, query,
		m.ID, m.MailboxID, m.MailFrom, m.RcptTo, m.From.Name, m.From.Address, marshalJSON(m.To), marshalJSON(m.Cc),
		m.Subject, date, m.Snippet, m.Size, marshalJSON(m.Parts), m.Folder,
		m.Seen, m.Answered, m.Flagged, m.Draft, m.Forwarded, key, m.ReceivedAt.UTC(),
		m.MessageID, m.InReplyTo, seq, m.Parts.AttachmentCount())
	if err != nil {
		return fmt.Errorf("inserting message: %w", err)
	}
	d.logEvent(tx, event, m.ID, nil)
	for _, label := range labels {
		if _, err := d.addLabel(ctx, c, m.ID, label); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) messageByIdempotencyKey(ctx context.Context, mailboxID, key string) (*Message, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT id FROM messages WHERE mailbox_id = ? AND idempotency_key = ?`), mailboxID, key).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: idempotency key %s", ErrMessageNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("getting message: %w", err)
	}
	return d.GetMessage(ctx, id)
}

// GetMessage returns a message with its labels.
func (d *DB) GetMessage(ctx context.Context, id string) (*Message, error) {
	m, err := getMessage(ctx, d, d.sql, id)
	if err != nil {
		return nil, err
	}
	if err := d.loadLabels(ctx, d.sql, []*Message{m}); err != nil {
		return nil, err
	}
	return m, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getMessage(ctx context.Context, d *DB, q querier, id string) (*Message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, d.q(`SELECT `+messageColumns+` FROM messages WHERE id = ?`), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting message: %w", err)
	}
	return m, nil
}

// MessageFilter selects messages for ListMessages and CountFiltered. Empty
// fields match all.
type MessageFilter struct {
	MailboxID string
	// MailboxIDs, if not nil, limits messages to these mailboxes, such as
	// the ones shared with a user. Empty matches nothing.
	MailboxIDs []string
	Folder     string
	NotFolders []string // folders messages must not be in
	Label      string
	// Seen and Flagged, if not nil, match messages with that flag set so.
	Seen    *bool
	Flagged *bool
	// After and Before are message IDs, page cursors in list order: newest
	// received first, ties broken by ID. After selects the messages that
	// come after that one (older), Before the ones before it (newer).
	After  string
	Before string
	// Arrival lists summaries in the order messages came into Label, or
	// without one, Folder, after seq AfterSeq, instead of by received.
	Arrival  bool
	AfterSeq uint32
	Limit    int
}

// View returns the filter for a folder's messages, or with no folder, for
// All Mail: every folder but junk and trash, which show only in their own,
// as in Gmail. A label, if set, narrows it to messages with that label.
func View(folder, label string) MessageFilter {
	f := MessageFilter{Folder: folder, Label: label}
	if folder == "" {
		f.NotFolders = Unlisted
	}
	return f
}

// where returns the WHERE clause and arguments that select f's messages.
func (f MessageFilter) where() (string, []any) {
	where := ` WHERE 1 = 1`
	var args []any
	if f.MailboxID != "" {
		where += ` AND mailbox_id = ?`
		args = append(args, f.MailboxID)
	}
	if f.MailboxIDs != nil {
		if len(f.MailboxIDs) == 0 {
			where += ` AND 1 = 0`
		} else {
			where += ` AND mailbox_id IN (` + placeholders(len(f.MailboxIDs)) + `)`
			for _, id := range f.MailboxIDs {
				args = append(args, id)
			}
		}
	}
	if f.Folder != "" {
		where += ` AND folder = ?`
		args = append(args, f.Folder)
	}
	if len(f.NotFolders) > 0 {
		where += ` AND folder NOT IN (` + placeholders(len(f.NotFolders)) + `)`
		for _, folder := range f.NotFolders {
			args = append(args, folder)
		}
	}
	if f.Label != "" {
		where += ` AND id IN (SELECT message_id FROM message_labels WHERE label = ?)`
		args = append(args, f.Label)
	}
	if f.Seen != nil {
		where += ` AND seen = ?`
		args = append(args, *f.Seen)
	}
	if f.Flagged != nil {
		where += ` AND flagged = ?`
		args = append(args, *f.Flagged)
	}
	// A cursor that names no message matches nothing.
	if f.After != "" {
		where += ` AND (received_at, id) < (SELECT received_at, id FROM messages WHERE id = ?)`
		args = append(args, f.After)
	}
	if f.Before != "" {
		where += ` AND (received_at, id) > (SELECT received_at, id FROM messages WHERE id = ?)`
		args = append(args, f.Before)
	}
	return where, args
}

// ListMessages returns messages matching f, newest received first, ties
// broken by ID, without their bodies. With Before, it returns the f.Limit
// oldest of the newer messages, so a page before the current one ends right
// above it.
func (d *DB) ListMessages(ctx context.Context, f MessageFilter) ([]*Message, error) {
	where, args := f.where()
	order := ` ORDER BY received_at DESC, id DESC`
	if f.Before != "" {
		order = ` ORDER BY received_at ASC, id ASC`
	}
	query := `SELECT ` + messageColumns + ` FROM messages` + where + order + ` LIMIT ?`
	args = append(args, f.Limit)

	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	var messages []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		messages = append(messages, m)
	}
	// Close before loading labels: SQLite runs on a single connection.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	if f.Before != "" {
		slices.Reverse(messages)
	}

	if err := d.loadLabels(ctx, d.sql, messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// loadLabels sets the labels of messages, by name.
func (d *DB) loadLabels(ctx context.Context, q querier, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	byID := make(map[string]*Message, len(messages))
	args := make([]any, len(messages))
	for i, m := range messages {
		m.Labels = []string{}
		byID[m.ID] = m
		args[i] = m.ID
	}

	rows, err := q.QueryContext(ctx, d.q(`SELECT message_id, label FROM message_labels WHERE message_id IN (`+placeholders(len(args))+`)`), args...)
	if err != nil {
		return fmt.Errorf("loading labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("scanning label: %w", err)
		}
		byID[id].Labels = append(byID[id].Labels, label)
	}
	for _, m := range messages {
		slices.Sort(m.Labels)
	}
	return rows.Err()
}

// MessageUpdate changes a message's folder, labels, and flags. An empty
// folder or a nil flag is left alone.
type MessageUpdate struct {
	Folder       string
	AddLabels    []string
	RemoveLabels []string
	Seen         *bool
	Answered     *bool
	Flagged      *bool
	Draft        *bool
	Forwarded    *bool
}

// IsZero reports whether u changes nothing.
func (u MessageUpdate) IsZero() bool {
	return u.Folder == "" && len(u.AddLabels) == 0 && len(u.RemoveLabels) == 0 &&
		u.Seen == nil && u.Answered == nil && u.Flagged == nil && u.Draft == nil && u.Forwarded == nil
}

// UpdateMessage applies u to message id and returns the updated message.
func (d *DB) UpdateMessage(ctx context.Context, id string, u MessageUpdate) (*Message, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("updating message: %w", err)
	}
	defer tx.Rollback()

	m, err := getMessage(ctx, d, tx, id)
	if err != nil {
		return nil, err
	}
	// What actually changes, for the event: adding a label the message has
	// already, or setting a flag that's set, changes nothing, and logs no
	// event.
	var c MessageChanges
	ch := &change{d: d, tx: tx, mailboxID: m.MailboxID}

	for _, label := range u.AddLabels {
		var exists int
		err := tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM labels WHERE name = ?`), label).Scan(&exists)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", ErrUnknownLabel, label)
		}
		if err != nil {
			return nil, fmt.Errorf("checking label: %w", err)
		}
		added, err := d.addLabel(ctx, ch, id, label)
		if err != nil {
			return nil, err
		}
		if added {
			c.AddLabels = append(c.AddLabels, label)
		}
	}
	for _, label := range u.RemoveLabels {
		res, err := tx.ExecContext(ctx, d.q(`DELETE FROM message_labels WHERE message_id = ? AND label = ?`), id, label)
		if err != nil {
			return nil, fmt.Errorf("removing label: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, fmt.Errorf("removing label: %w", err)
		} else if n > 0 {
			c.RemoveLabels = append(c.RemoveLabels, label)
		}
	}

	// A message coming into a folder takes the change's seq there. Back from
	// junk or trash, it also rejoins its labels' views, and takes it in those
	// too.
	if u.Folder != "" && u.Folder != m.Folder {
		if !IsFolder(u.Folder) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownFolder, u.Folder)
		}
		seq, err := ch.next(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE messages SET folder = ?, folder_seq = ? WHERE id = ?`), u.Folder, seq, id); err != nil {
			return nil, fmt.Errorf("moving message: %w", err)
		}
		c.Folder = []string{m.Folder, u.Folder}
		if slices.Contains(Unlisted, m.Folder) && !slices.Contains(Unlisted, u.Folder) {
			if err := d.relist(ctx, ch, id); err != nil {
				return nil, err
			}
		}
	}

	for _, f := range []struct {
		column  string
		value   *bool
		current bool
		changed **bool
	}{
		{"seen", u.Seen, m.Seen, &c.Seen},
		{"answered", u.Answered, m.Answered, &c.Answered},
		{"flagged", u.Flagged, m.Flagged, &c.Flagged},
		{"draft", u.Draft, m.Draft, &c.Draft},
		{"forwarded", u.Forwarded, m.Forwarded, &c.Forwarded},
	} {
		if f.value == nil || *f.value == f.current {
			continue
		}
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE messages SET `+f.column+` = ? WHERE id = ?`), *f.value, id); err != nil {
			return nil, fmt.Errorf("updating flags: %w", err)
		}
		*f.changed = f.value
	}
	if !c.IsZero() {
		d.touch(ch, id, &c)
	}

	if err := d.commit(ctx, tx); err != nil {
		return nil, fmt.Errorf("updating message: %w", err)
	}
	return d.GetMessage(ctx, id)
}

// CountFiltered returns how many messages match f, ignoring its limit.
func (d *DB) CountFiltered(ctx context.Context, f MessageFilter) (int, error) {
	where, args := f.where()
	var n int
	if err := d.sql.QueryRowContext(ctx, d.q(`SELECT COUNT(*) FROM messages`+where), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting messages: %w", err)
	}
	return n, nil
}
