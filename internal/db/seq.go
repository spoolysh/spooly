package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Each account counts up a sequence. A message is stamped with the next
// number whenever it comes into a folder (messages.folder_seq) or joins a
// label (message_labels.seq). Within any folder or label, later arrivals
// always have higher numbers, though not consecutive ones. IMAP uses them as
// UIDs; sync APIs such as JMAP can build on them too.

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

// relist stamps a message that has just come out of junk or trash with new
// sequence numbers in each of its labels, which it rejoins.
func (d *DB) relist(ctx context.Context, tx *sql.Tx, accountID, messageID string) error {
	labels, err := d.labelsOf(ctx, tx, messageID)
	if err != nil {
		return err
	}
	for _, label := range labels {
		seq, err := d.nextSeq(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE message_labels SET seq = ? WHERE message_id = ? AND label = ?`), seq, messageID, label); err != nil {
			return fmt.Errorf("renumbering message: %w", err)
		}
	}
	return nil
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

// SeqMessage is a message in a view, with the sequence number it got when
// it came into it.
type SeqMessage struct {
	Seq        uint32
	ID         string
	Size       int64
	ReceivedAt time.Time
	Flags
}

// SeqList is an account's messages in a view, by sequence number.
type SeqList struct {
	// Since is when the view's numbering began: a label's creation, or for
	// folders, the account's. Numbers are only ever reused when a label is
	// deleted and created again, which changes it.
	Since    time.Time
	NextSeq  uint32       // higher than any number in the list, now or later
	Messages []SeqMessage // by Seq
}

// ListBySeq returns an account's messages in view, by sequence number: in
// a folder, or with a label. Junk and trash show only in their own folders.
func (d *DB) ListBySeq(ctx context.Context, accountID, view string) (*SeqList, error) {
	list := &SeqList{}
	if err := d.sql.QueryRowContext(ctx, d.q(`SELECT next_seq, created_at FROM accounts WHERE id = ?`), accountID).
		Scan(&list.NextSeq, &list.Since); err != nil {
		return nil, fmt.Errorf("reading account: %w", err)
	}

	columns := `m.id, m.size, m.received_at, m.seen, m.answered, m.flagged, m.draft, m.forwarded`
	query := `SELECT m.folder_seq, ` + columns + ` FROM messages m WHERE m.account_id = ? AND m.folder = ? ORDER BY m.folder_seq`
	args := []any{accountID, view}
	if !IsFolder(view) {
		l, err := d.GetLabel(ctx, view)
		if err != nil {
			return nil, err
		}
		list.Since = l.CreatedAt
		// A label's view leaves out junk and trash.
		query = `SELECT l.seq, ` + columns + ` FROM message_labels l JOIN messages m ON m.id = l.message_id
			WHERE m.account_id = ? AND l.label = ? AND m.folder NOT IN (` + placeholders(len(Unlisted)) + `) ORDER BY l.seq`
		for _, f := range Unlisted {
			args = append(args, f)
		}
	}

	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m SeqMessage
		if err := rows.Scan(&m.Seq, &m.ID, &m.Size, &m.ReceivedAt, &m.Seen, &m.Answered, &m.Flagged, &m.Draft, &m.Forwarded); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		list.Messages = append(list.Messages, m)
	}
	return list, rows.Err()
}
