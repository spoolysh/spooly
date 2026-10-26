package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Address is a name and address from a From/To/Cc header.
type Address struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Attachment describes one attached file; its bytes stay in the raw message.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Message is a stored message. Its raw source is in object storage under its
// ID; everything here is parsed from it, except the envelope and labels.
type Message struct {
	ID             string
	AccountID      string
	Sender         string // SMTP envelope sender (MAIL FROM)
	Recipient      string // SMTP envelope recipient (RCPT TO)
	From           Address
	To             []Address
	Cc             []Address
	Subject        string
	Date           *time.Time
	Snippet        string
	Size           int64
	Attachments    []Attachment
	Text           string // only loaded by GetMessage with body
	HTML           string // only loaded by GetMessage with body
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

// messageColumns are the columns of a message without its bodies and raw source.
const messageColumns = `id, account_id, sender, recipient, from_name, from_addr, to_addrs, cc_addrs,
	subject, date, snippet, size, attachments, metadata, idempotency_key, received_at`

func scanMessage(row interface{ Scan(...any) error }, extra ...any) (*Message, error) {
	var m Message
	var to, cc, attachments, metadata string
	var date sql.NullTime
	var key sql.NullString
	dest := []any{&m.ID, &m.AccountID, &m.Sender, &m.Recipient, &m.From.Name, &m.From.Addr, &to, &cc,
		&m.Subject, &date, &m.Snippet, &m.Size, &attachments, &metadata, &key, &m.ReceivedAt}
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
	}{{to, &m.To}, {cc, &m.Cc}, {attachments, &m.Attachments}, {metadata, &m.Metadata}} {
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
	if m.Attachments == nil {
		m.Attachments = []Attachment{}
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

	query := d.q(`INSERT INTO messages (` + messageColumns + `, text_body, html_body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err = tx.ExecContext(ctx, query,
		m.ID, m.AccountID, m.Sender, m.Recipient, m.From.Name, m.From.Addr, marshalJSON(m.To), marshalJSON(m.Cc),
		m.Subject, date, m.Snippet, m.Size, marshalJSON(m.Attachments), marshalJSON(m.Metadata), key, m.ReceivedAt.UTC(),
		m.Text, m.HTML)
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
		if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO messages_labels (message_id, label) VALUES (?, ?)`), m.ID, label); err != nil {
			return nil, false, fmt.Errorf("labeling message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("inserting message: %w", err)
	}

	stored, err = d.GetMessage(ctx, m.ID, false)
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
	return d.GetMessage(ctx, id, false)
}

// GetMessage returns a message with its labels, and its text and HTML bodies
// if withBody is set.
func (d *DB) GetMessage(ctx context.Context, id string, withBody bool) (*Message, error) {
	m, err := getMessage(ctx, d, d.sql, id, withBody)
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

func getMessage(ctx context.Context, d *DB, q querier, id string, withBody bool) (*Message, error) {
	var text, html string
	var extra []any
	columns := messageColumns
	if withBody {
		columns += `, text_body, html_body`
		extra = []any{&text, &html}
	}
	m, err := scanMessage(q.QueryRowContext(ctx, d.q(`SELECT `+columns+` FROM messages WHERE id = ?`), id), extra...)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting message: %w", err)
	}
	m.Text, m.HTML = text, html
	return m, nil
}

// MessageExists reports whether a message with id exists, as an error
// wrapping ErrMessageNotFound if not.
func (d *DB) MessageExists(ctx context.Context, id string) error {
	var one int
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT 1 FROM messages WHERE id = ?`), id).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("getting message: %w", err)
	}
	return nil
}

// MessageFilter selects messages for ListMessages. Empty fields match all.
type MessageFilter struct {
	AccountID string
	Label     string
	After     string // only messages with smaller IDs
	Limit     int
}

// ListMessages returns messages matching f, newest first by ID, without
// their bodies.
func (d *DB) ListMessages(ctx context.Context, f MessageFilter) ([]*Message, error) {
	query := `SELECT ` + messageColumns + ` FROM messages WHERE 1 = 1`
	var args []any
	if f.AccountID != "" {
		query += ` AND account_id = ?`
		args = append(args, f.AccountID)
	}
	if f.Label != "" {
		query += ` AND id IN (SELECT message_id FROM messages_labels WHERE label = ?)`
		args = append(args, f.Label)
	}
	if f.After != "" {
		query += ` AND id < ?`
		args = append(args, f.After)
	}
	query += ` ORDER BY id DESC LIMIT ?`
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

	rows, err := q.QueryContext(ctx, d.q(`SELECT message_id, label FROM messages_labels WHERE message_id IN (`+placeholders(len(args))+`)`), args...)
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

	m, err := getMessage(ctx, d, tx, id, false)
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
		if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO messages_labels (message_id, label) VALUES (?, ?)
			ON CONFLICT DO NOTHING`), id, label); err != nil {
			return nil, fmt.Errorf("adding label: %w", err)
		}
	}
	for _, label := range u.RemoveLabels {
		if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM messages_labels WHERE message_id = ? AND label = ?`), id, label); err != nil {
			return nil, fmt.Errorf("removing label: %w", err)
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
	return d.GetMessage(ctx, id, false)
}

// CountMessages returns how many messages have all the labels, in one
// account if accountID is set.
func (d *DB) CountMessages(ctx context.Context, accountID string, labels ...string) (int, error) {
	query := `SELECT COUNT(*) FROM messages WHERE 1 = 1`
	var args []any
	if accountID != "" {
		query += ` AND account_id = ?`
		args = append(args, accountID)
	}
	for _, label := range labels {
		query += ` AND id IN (SELECT message_id FROM messages_labels WHERE label = ?)`
		args = append(args, label)
	}
	var n int
	if err := d.sql.QueryRowContext(ctx, d.q(query), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting messages: %w", err)
	}
	return n, nil
}
