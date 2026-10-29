package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// Message is a stored message. Its raw source is in object storage under its
// ID; everything here is parsed from it, except the envelope and labels.
type Message struct {
	ID        string
	AccountID string
	Sender    string // SMTP envelope sender (MAIL FROM)
	Recipient string // SMTP envelope recipient (RCPT TO)
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
	MsgID          string // Message-ID, without angle brackets
	ParentMsgID    string // the first In-Reply-To ID, without angle brackets
	Labels         []string
	Metadata       map[string]string
	IdempotencyKey string // empty when not set
	ReceivedAt     time.Time
}

// MaxMetadataKeys is how many metadata keys a message can have.
const MaxMetadataKeys = 50

var (
	// ErrMessageNotFound reports that no message has the requested ID.
	ErrMessageNotFound = errors.New("message not found")
	// ErrUnknownLabel reports a label name that doesn't exist.
	ErrUnknownLabel = errors.New("unknown label")
	// ErrMetadataLimit reports an update that would leave too many metadata keys.
	ErrMetadataLimit = fmt.Errorf("a message can have at most %d metadata keys", MaxMetadataKeys)
)

// messageColumns are the columns of a message.
const messageColumns = `id, account_id, sender, recipient, from_name, from_address, to_addresses, cc_addresses,
	subject, date, snippet, size, parts, metadata, idempotency_key, received_at`

func scanMessage(row interface{ Scan(...any) error }, extra ...any) (*Message, error) {
	var m Message
	var to, cc, parts, metadata string
	var date sql.NullTime
	var key sql.NullString
	dest := []any{&m.ID, &m.AccountID, &m.Sender, &m.Recipient, &m.From.Name, &m.From.Address, &to, &cc,
		&m.Subject, &date, &m.Snippet, &m.Size, &parts, &metadata, &key, &m.ReceivedAt}
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
	}{{to, &m.To}, {cc, &m.Cc}, {parts, &m.Parts}, {metadata, &m.Metadata}} {
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

// CreateMessage stores m with the given labels. If m has an idempotency key
// that its account already used, the stored message is returned instead,
// with created false.
func (d *DB) CreateMessage(ctx context.Context, m Message, labels []string) (stored *Message, created bool, err error) {
	if m.IdempotencyKey != "" {
		existing, err := d.messageByIdempotencyKey(ctx, m.AccountID, m.IdempotencyKey)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrMessageNotFound) {
			return nil, false, err
		}
	}

	if m.To == nil {
		m.To = []Address{}
	}
	if m.Cc == nil {
		m.Cc = []Address{}
	}
	if m.Parts == nil {
		m.Parts = Parts{}
	}
	if m.Metadata == nil {
		m.Metadata = map[string]string{}
	}
	var key sql.NullString
	if m.IdempotencyKey != "" {
		key = sql.NullString{String: m.IdempotencyKey, Valid: true}
	}
	var date sql.NullTime
	if m.Date != nil {
		date = sql.NullTime{Time: m.Date.UTC(), Valid: true}
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}
	defer tx.Rollback()

	seq, err := d.nextSeq(ctx, tx, m.AccountID)
	if err != nil {
		return nil, false, err
	}
	query := d.q(`INSERT INTO messages (` + messageColumns + `, msg_id, parent_msg_id, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err = tx.ExecContext(ctx, query,
		m.ID, m.AccountID, m.Sender, m.Recipient, m.From.Name, m.From.Address, marshalJSON(m.To), marshalJSON(m.Cc),
		m.Subject, date, m.Snippet, m.Size, marshalJSON(m.Parts), marshalJSON(m.Metadata), key, m.ReceivedAt.UTC(),
		m.MsgID, m.ParentMsgID, seq)
	if err != nil {
		tx.Rollback()
		// Lost a race with a retry using the same idempotency key.
		if m.IdempotencyKey != "" {
			if existing, gerr := d.messageByIdempotencyKey(ctx, m.AccountID, m.IdempotencyKey); gerr == nil {
				return existing, false, nil
			}
		}
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}
	for _, label := range labels {
		if err := d.addLabel(ctx, tx, m.AccountID, m.ID, label); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}

	stored, err = d.GetMessage(ctx, m.ID)
	return stored, true, err
}

func (d *DB) messageByIdempotencyKey(ctx context.Context, accountID, key string) (*Message, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT id FROM messages WHERE account_id = ? AND idempotency_key = ?`), accountID, key).Scan(&id)
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
	AccountID string
	// AccountIDs, if not nil, limits messages to these accounts, such as
	// the ones shared with a user. Empty matches nothing.
	AccountIDs []string
	Label      string
	Labels     []string // more labels messages must all have
	NotLabels  []string // labels messages must not have
	// After and Before are message IDs, page cursors in list order: newest
	// received first, ties broken by ID. After selects the messages that
	// come after that one (older), Before the ones before it (newer).
	After  string
	Before string
	Limit  int
}

// where returns the WHERE clause and arguments that select f's messages.
func (f MessageFilter) where() (string, []any) {
	where := ` WHERE 1 = 1`
	var args []any
	if f.AccountID != "" {
		where += ` AND account_id = ?`
		args = append(args, f.AccountID)
	}
	if f.AccountIDs != nil {
		if len(f.AccountIDs) == 0 {
			where += ` AND 1 = 0`
		} else {
			where += ` AND account_id IN (` + placeholders(len(f.AccountIDs)) + `)`
			for _, id := range f.AccountIDs {
				args = append(args, id)
			}
		}
	}
	for _, label := range append([]string{f.Label}, f.Labels...) {
		if label == "" {
			continue
		}
		where += ` AND id IN (SELECT message_id FROM message_labels WHERE label = ?)`
		args = append(args, label)
	}
	if len(f.NotLabels) > 0 {
		where += ` AND id NOT IN (SELECT message_id FROM message_labels WHERE label IN (` + placeholders(len(f.NotLabels)) + `))`
		for _, label := range f.NotLabels {
			args = append(args, label)
		}
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

// loadLabels sets the labels of messages, in display order.
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
		SortLabels(m.Labels)
	}
	return rows.Err()
}

// MessageUpdate changes a message's labels and metadata. Metadata is merged;
// an empty value deletes the key.
type MessageUpdate struct {
	AddLabels    []string
	RemoveLabels []string
	Metadata     map[string]string
}

// UpdateMessage applies u to message id and returns the updated message.
func (d *DB) UpdateMessage(ctx context.Context, id string, u MessageUpdate) (*Message, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("updating message: %w", err)
	}
	defer tx.Rollback()

	m, err := getMessage(ctx, d, tx, id)
	if err != nil {
		return nil, err
	}
	before, err := d.labelsOf(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	for _, label := range u.AddLabels {
		var exists int
		err := tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM labels WHERE name = ?`), label).Scan(&exists)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", ErrUnknownLabel, label)
		}
		if err != nil {
			return nil, fmt.Errorf("checking label: %w", err)
		}
		if err := d.addLabel(ctx, tx, m.AccountID, id, label); err != nil {
			return nil, err
		}
	}
	for _, label := range u.RemoveLabels {
		if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM message_labels WHERE message_id = ? AND label = ?`), id, label); err != nil {
			return nil, fmt.Errorf("removing label: %w", err)
		}
	}
	// Back from spam or trash, the message rejoins All Mail and its other
	// labels' views, so it needs new sequence numbers there. Labels just
	// added have new ones already.
	after, err := d.labelsOf(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	wasIn := views(before)
	for _, v := range views(after) {
		if slices.Contains(wasIn, v) || (v != AllMail && !slices.Contains(before, v)) {
			continue
		}
		if err := d.renumber(ctx, tx, m.AccountID, id, v); err != nil {
			return nil, err
		}
	}

	if len(u.Metadata) > 0 {
		for k, v := range u.Metadata {
			if v == "" {
				delete(m.Metadata, k)
			} else {
				m.Metadata[k] = v
			}
		}
		if len(m.Metadata) > MaxMetadataKeys {
			return nil, ErrMetadataLimit
		}
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE messages SET metadata = ? WHERE id = ?`), marshalJSON(m.Metadata), id); err != nil {
			return nil, fmt.Errorf("updating metadata: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
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

// MaildropEntry is a message as POP3 lists it.
type MaildropEntry struct {
	ID   string
	Size int64
}

// ListMaildrop returns the IDs and sizes of the newest received limit
// messages of an account with label, oldest first, as POP3 numbers them.
func (d *DB) ListMaildrop(ctx context.Context, accountID, label string, limit int) ([]MaildropEntry, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT id, size FROM messages
		WHERE account_id = ? AND id IN (SELECT message_id FROM message_labels WHERE label = ?)
		ORDER BY received_at DESC, id DESC LIMIT ?`), accountID, label, limit)
	if err != nil {
		return nil, fmt.Errorf("listing maildrop: %w", err)
	}
	defer rows.Close()

	var entries []MaildropEntry
	for rows.Next() {
		var e MaildropEntry
		if err := rows.Scan(&e.ID, &e.Size); err != nil {
			return nil, fmt.Errorf("scanning maildrop: %w", err)
		}
		entries = append(entries, e)
	}
	slices.Reverse(entries)
	return entries, rows.Err()
}

// DeleteMessage deletes a message's row and labels for good. Its raw source
// is the caller's to remove. Deleting a missing message is not an error.
func (d *DB) DeleteMessage(ctx context.Context, id string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting message: %w", err)
	}
	defer tx.Rollback()
	for _, query := range []string{`DELETE FROM message_labels WHERE message_id = ?`, `DELETE FROM messages WHERE id = ?`} {
		if _, err := tx.ExecContext(ctx, d.q(query), id); err != nil {
			return fmt.Errorf("deleting message: %w", err)
		}
	}
	return tx.Commit()
}
