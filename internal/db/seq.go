package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Each account counts up a sequence. A message is stamped with the next
// number whenever it joins a label (message_labels.seq) or All Mail
// (messages.seq). Within any label, later arrivals always have higher
// numbers, though not consecutive ones. IMAP uses them as UIDs; sync APIs
// such as JMAP can build on them too.

// AllMail stands for an account's every message but spam and trash, where a
// label name is expected.
const AllMail = ""

// nextSeq takes the next number from an account's sequence.
func (d *DB) nextSeq(ctx context.Context, tx *sql.Tx, accountID string) (uint32, error) {
	var seq uint32
	err := tx.QueryRowContext(ctx, d.q(`UPDATE accounts SET next_seq = next_seq + 1 WHERE id = ? RETURNING next_seq - 1`), accountID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("taking a sequence number: %w", err)
	}
	return seq, nil
}

// addLabel labels a message, stamping it with the next sequence number,
// unless it has the label already.
func (d *DB) addLabel(ctx context.Context, tx *sql.Tx, accountID, messageID, label string) error {
	var exists int
	err := tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM message_labels WHERE message_id = ? AND label = ?`), messageID, label).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("checking label: %w", err)
	}
	seq, err := d.nextSeq(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO message_labels (message_id, label, seq) VALUES (?, ?, ?)`), messageID, label, seq); err != nil {
		return fmt.Errorf("labeling message: %w", err)
	}
	return nil
}

// HiddenFrom returns the labels that keep a message out of the view of
// label, or of AllMail, as in Gmail: spam and trash show only in their own
// folders, and trash wins over spam.
func HiddenFrom(label string) []string {
	switch label {
	case LabelTrash:
		return nil
	case LabelSpam:
		return []string{LabelTrash}
	default:
		return []string{LabelSpam, LabelTrash}
	}
}

// views returns the views a message with labels shows in: AllMail and each
// of its labels, less those HiddenFrom keeps it out of.
func views(labels []string) []string {
	var in []string
	for _, v := range append([]string{AllMail}, labels...) {
		if !slices.ContainsFunc(HiddenFrom(v), func(h string) bool { return slices.Contains(labels, h) }) {
			in = append(in, v)
		}
	}
	return in
}

// labelsOf returns a message's labels.
func (d *DB) labelsOf(ctx context.Context, tx *sql.Tx, messageID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, d.q(`SELECT label FROM message_labels WHERE message_id = ?`), messageID)
	if err != nil {
		return nil, fmt.Errorf("reading labels: %w", err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// renumber stamps a message with the next sequence number in view, which it
// has just come back to.
func (d *DB) renumber(ctx context.Context, tx *sql.Tx, accountID, messageID, view string) error {
	seq, err := d.nextSeq(ctx, tx, accountID)
	if err != nil {
		return err
	}
	query, args := `UPDATE messages SET seq = ? WHERE id = ?`, []any{seq, messageID}
	if view != AllMail {
		query, args = `UPDATE message_labels SET seq = ? WHERE message_id = ? AND label = ?`, append(args, view)
	}
	if _, err := tx.ExecContext(ctx, d.q(query), args...); err != nil {
		return fmt.Errorf("renumbering message: %w", err)
	}
	return nil
}

// SeqMessage is a message in a label, with the sequence number it got when
// it joined.
type SeqMessage struct {
	Seq        uint32
	ID         string
	Size       int64
	ReceivedAt time.Time
	Unread     bool
	Starred    bool
	Answered   bool
	Draft      bool
}

// SeqList is an account's messages with a label, or in AllMail, by
// sequence number.
type SeqList struct {
	// Since is when the label's numbering began: its creation, or for
	// system labels and All Mail, the account's. Numbers are only ever
	// reused when a label is deleted and created again, which changes it.
	Since    time.Time
	NextSeq  uint32       // higher than any number in the list, now or later
	Messages []SeqMessage // by Seq
}

// ListBySeq returns an account's messages with label, or in AllMail, by
// sequence number.
func (d *DB) ListBySeq(ctx context.Context, accountID, label string) (*SeqList, error) {
	list := &SeqList{}
	if err := d.sql.QueryRowContext(ctx, d.q(`SELECT next_seq, created_at FROM accounts WHERE id = ?`), accountID).
		Scan(&list.NextSeq, &list.Since); err != nil {
		return nil, fmt.Errorf("reading account: %w", err)
	}
	if label != AllMail {
		l, err := d.GetLabel(ctx, label)
		if err != nil {
			return nil, err
		}
		if l.CreatedAt != nil {
			list.Since = *l.CreatedAt
		}
	}

	hidden := HiddenFrom(label)
	flags := `,
		EXISTS (SELECT 1 FROM message_labels f WHERE f.message_id = m.id AND f.label = 'unread'),
		EXISTS (SELECT 1 FROM message_labels f WHERE f.message_id = m.id AND f.label = 'starred'),
		EXISTS (SELECT 1 FROM message_labels f WHERE f.message_id = m.id AND f.label = 'answered'),
		EXISTS (SELECT 1 FROM message_labels f WHERE f.message_id = m.id AND f.label = 'draft')`
	query := `SELECT l.seq, m.id, m.size, m.received_at` + flags + `
		FROM message_labels l JOIN messages m ON m.id = l.message_id
		WHERE m.account_id = ? AND l.label = ?`
	args := []any{accountID, label}
	order := ` ORDER BY l.seq`
	if label == AllMail {
		query = `SELECT m.seq, m.id, m.size, m.received_at` + flags + `
			FROM messages m WHERE m.account_id = ?`
		args = []any{accountID}
		order = ` ORDER BY m.seq`
	}
	if len(hidden) > 0 {
		query += ` AND m.id NOT IN (SELECT message_id FROM message_labels WHERE label IN (` + placeholders(len(hidden)) + `))`
		for _, h := range hidden {
			args = append(args, h)
		}
	}
	query += order
	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m SeqMessage
		if err := rows.Scan(&m.Seq, &m.ID, &m.Size, &m.ReceivedAt, &m.Unread, &m.Starred, &m.Answered, &m.Draft); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		list.Messages = append(list.Messages, m)
	}
	return list, rows.Err()
}
