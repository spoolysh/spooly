package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// A message coming into a folder (messages.folder_seq) or getting a label
// (message_labels.seq) takes its mailbox's next seq, the order of arrivals
// there: later arrivals always have higher seqs, though not consecutive
// ones. An IMAP gateway uses them as UIDs.

// nextSeq takes a mailbox's next seq.
func (d *DB) nextSeq(ctx context.Context, tx *sql.Tx, mailboxID string) (uint32, error) {
	var seq uint32
	err := tx.QueryRowContext(ctx, d.q(`UPDATE mailboxes SET next_seq = next_seq + 1 WHERE id = ?
		RETURNING next_seq - 1`), mailboxID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("taking a seq: %w", err)
	}
	return seq, nil
}

// change is one change to a message of a mailbox. It takes a seq the
// first time one's needed, so a change that brings the message nowhere
// takes none, and every place one change brings it to shares it.
type change struct {
	d         *DB
	tx        *sql.Tx
	mailboxID string
	seq       uint32
}

func (c *change) next(ctx context.Context) (uint32, error) {
	if c.seq == 0 {
		seq, err := c.d.nextSeq(ctx, c.tx, c.mailboxID)
		if err != nil {
			return 0, err
		}
		c.seq = seq
	}
	return c.seq, nil
}

// addLabel labels a message at the change's seq, unless it has the
// label already, and reports whether it added it.
func (d *DB) addLabel(ctx context.Context, c *change, messageID, label string) (bool, error) {
	var exists int
	err := c.tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM message_labels WHERE message_id = ? AND label = ?`), messageID, label).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("checking label: %w", err)
	}
	seq, err := c.next(ctx)
	if err != nil {
		return false, err
	}
	if _, err := c.tx.ExecContext(ctx, d.q(`INSERT INTO message_labels (message_id, label, seq) VALUES (?, ?, ?)`), messageID, label, seq); err != nil {
		return false, fmt.Errorf("labeling message: %w", err)
	}
	return true, nil
}

// relist moves a message that has just come out of junk or trash to the
// change's seq in each of its labels, which it rejoins.
func (d *DB) relist(ctx context.Context, c *change, messageID string) error {
	seq, err := c.next(ctx)
	if err != nil {
		return err
	}
	if _, err := c.tx.ExecContext(ctx, d.q(`UPDATE message_labels SET seq = ? WHERE message_id = ?`), seq, messageID); err != nil {
		return fmt.Errorf("renumbering message: %w", err)
	}
	return nil
}

// touch logs a message as updated with changes.
func (d *DB) touch(ctx context.Context, c *change, messageID string, changes *MessageChanges) error {
	return d.logEvent(ctx, c.tx, EventMessageUpdated, messageID, changes)
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

// Summary is what a list shows of a message: its place, state, and what a
// list line needs, but not its recipients, envelope, or parts.
type Summary struct {
	ID        string
	MailboxID string
	// Seq is its place in the folder or label listed in arrival order, the
	// order messages came there; 0 in other lists.
	Seq         uint32
	Folder      string
	Labels      []string // by name
	From        Address
	Subject     string
	Snippet     string
	Date        *time.Time
	Size        int64
	Attachments int
	ReceivedAt  time.Time
	Flags
}

// summaryColumns are the columns of a Summary but its seq and labels.
const summaryColumns = `messages.id, mailbox_id, folder, from_name, from_address, subject, date, snippet,
	size, attachments, received_at, seen, answered, flagged, draft, forwarded`

// scanSummaries reads summaries from rows of summaryColumns and a seq, then
// their labels.
func (d *DB) scanSummaries(ctx context.Context, rows *sql.Rows) ([]Summary, error) {
	var list []Summary
	for rows.Next() {
		var m Summary
		var date sql.NullTime
		if err := rows.Scan(&m.ID, &m.MailboxID, &m.Folder, &m.From.Name, &m.From.Address, &m.Subject, &date, &m.Snippet,
			&m.Size, &m.Attachments, &m.ReceivedAt, &m.Seen, &m.Answered, &m.Flagged, &m.Draft, &m.Forwarded, &m.Seq); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		if date.Valid {
			m.Date = &date.Time
		}
		m.Labels = []string{}
		list = append(list, m)
	}
	// Close before loading labels: SQLite runs on a single connection.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	if len(list) == 0 {
		return list, nil
	}

	byID := make(map[string]*Summary, len(list))
	args := make([]any, len(list))
	for i := range list {
		byID[list[i].ID] = &list[i]
		args[i] = list[i].ID
	}
	labels, err := d.sql.QueryContext(ctx, d.q(`SELECT message_id, label FROM message_labels
		WHERE message_id IN (`+placeholders(len(args))+`) ORDER BY label`), args...)
	if err != nil {
		return nil, fmt.Errorf("loading labels: %w", err)
	}
	defer labels.Close()
	for labels.Next() {
		var id, label string
		if err := labels.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		byID[id].Labels = append(byID[id].Labels, label)
	}
	return list, labels.Err()
}

// ListSummaries returns summaries of the messages f selects: newest
// received first, or with f.Arrival, in the order they came into f.Label,
// or without one, f.Folder, after seq f.AfterSeq. With no f.Limit, it
// returns all of them.
func (d *DB) ListSummaries(ctx context.Context, f MessageFilter) ([]Summary, error) {
	where, args := f.where()
	var query string
	switch {
	case f.Arrival && f.Label != "":
		query = `SELECT ` + summaryColumns + `, l.seq FROM messages
			JOIN message_labels l ON l.message_id = messages.id AND l.label = ?` + where + ` AND l.seq > ? ORDER BY l.seq`
		args = append(append([]any{f.Label}, args...), f.AfterSeq)
	case f.Arrival:
		query = `SELECT ` + summaryColumns + `, folder_seq FROM messages` + where + ` AND folder_seq > ? ORDER BY folder_seq`
		args = append(args, f.AfterSeq)
	default:
		order := ` ORDER BY received_at DESC, messages.id DESC`
		if f.Before != "" {
			order = ` ORDER BY received_at ASC, messages.id ASC`
		}
		query = `SELECT ` + summaryColumns + `, 0 FROM messages` + where + order
	}
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	list, err := d.scanSummaries(ctx, rows)
	if err != nil {
		return nil, err
	}
	if f.Before != "" && !f.Arrival {
		slices.Reverse(list)
	}
	return list, nil
}

// SeqMessage is a message in a view, with its seq there as IMAP
// numbers it.
type SeqMessage struct {
	Seq        uint32
	ID         string
	Size       int64
	ReceivedAt time.Time
	Flags
}

// SeqList is a mailbox's messages in a view, by sequence number.
type SeqList struct {
	// Since is when the view began: a label's creation, or for folders, the
	// mailbox's. Only a label deleted and created again starts over.
	Since    time.Time
	NextSeq  uint32       // higher than any number in the list, now or later
	Messages []SeqMessage // by Seq
}

// SeqBounds returns when a mailbox's seqs in view, a folder or a label,
// began, and a seq higher than any there, now or later: IMAP's UIDVALIDITY
// and UIDNEXT.
func (d *DB) SeqBounds(ctx context.Context, mailboxID, view string) (since time.Time, next uint32, err error) {
	if err := d.sql.QueryRowContext(ctx, d.q(`SELECT next_seq, created_at FROM mailboxes WHERE id = ?`), mailboxID).
		Scan(&next, &since); err != nil {
		return time.Time{}, 0, fmt.Errorf("reading mailbox: %w", err)
	}
	if !IsFolder(view) {
		l, err := d.GetLabel(ctx, view)
		if err != nil {
			return time.Time{}, 0, err
		}
		since = l.CreatedAt
	}
	return since, next, nil
}

// ListBySeq returns all of a mailbox's messages in view, by sequence
// number: in a folder, or with a label. Junk and trash show only in their
// own folders.
func (d *DB) ListBySeq(ctx context.Context, mailboxID, view string) (*SeqList, error) {
	f := View(view, "")
	if !IsFolder(view) {
		f = View("", view)
	}
	f.MailboxID, f.Arrival = mailboxID, true
	summaries, err := d.ListSummaries(ctx, f)
	if err != nil {
		return nil, err
	}
	// Bounds after, so NextSeq is above every seq listed.
	list := &SeqList{}
	if list.Since, list.NextSeq, err = d.SeqBounds(ctx, mailboxID, view); err != nil {
		return nil, err
	}
	for _, m := range summaries {
		list.Messages = append(list.Messages, SeqMessage{Seq: m.Seq, ID: m.ID, Size: m.Size, ReceivedAt: m.ReceivedAt, Flags: m.Flags})
	}
	return list, nil
}
