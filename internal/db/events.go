package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event types, named as webhooks would send them: the kind of object, then
// what happened to it. Every change to a message is logged as one.
const (
	EventMessageCreated  = "message.created"
	EventMessageUpdated  = "message.updated"
	EventMessageRestored = "message.restored"
	EventMessageDeleted  = "message.deleted"
)

// MessageChanges are what an update changed, as a message.updated event
// records them: the folder it moved from and to, the labels added and
// removed, and the flags set or cleared.
type MessageChanges struct {
	Folder       []string `json:"folder,omitempty"` // from, to
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
	Seen         *bool    `json:"seen,omitempty"`
	Answered     *bool    `json:"answered,omitempty"`
	Flagged      *bool    `json:"flagged,omitempty"`
	Draft        *bool    `json:"draft,omitempty"`
	Forwarded    *bool    `json:"forwarded,omitempty"`
}

// IsZero reports whether c changes nothing.
func (c *MessageChanges) IsZero() bool {
	return len(c.Folder) == 0 && len(c.AddLabels) == 0 && len(c.RemoveLabels) == 0 &&
		c.Seen == nil && c.Answered == nil && c.Flagged == nil && c.Draft == nil && c.Forwarded == nil
}

// Event is a logged change in the organization: to the object of
// ObjectID, of the kind its Type starts with.
type Event struct {
	ID        int64 // its place in commit order
	Type      string
	ObjectID  string
	Data      json.RawMessage // what the type has to say: MessageChanges for message.updated
	CreatedAt time.Time
}

// txn is a transaction that may log events, which its commit stores.
type txn struct {
	*sql.Tx
	events []Event
}

// begin starts a transaction that may log events; end it with commit.
func (d *DB) begin(ctx context.Context) (*txn, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &txn{Tx: tx}, nil
}

// logEvent logs a change to an object, with data, if any, as its type has
// it: an update's changes, or a deleted message, say. It's stored when tx
// commits.
func (d *DB) logEvent(tx *txn, typ, objectID string, data any) {
	e := Event{Type: typ, ObjectID: objectID, CreatedAt: time.Now().UTC()}
	if data != nil {
		e.Data = json.RawMessage(marshalJSON(data))
	}
	tx.events = append(tx.events, e)
}

// commit stores the events tx logged, in the order logged, then commits
// it. Their IDs come from the events counter, whose row stays locked until
// the commit, so they follow commit order: no event commits with an ID
// below one a reader has seen. The counter is locked last, after any
// mailbox, so transactions can't deadlock on it.
func (d *DB) commit(ctx context.Context, tx *txn) error {
	if n := len(tx.events); n > 0 {
		var last int64
		if err := tx.QueryRowContext(ctx, d.q(`UPDATE state SET value = value + ? WHERE name = 'events' RETURNING value`), n).
			Scan(&last); err != nil {
			return fmt.Errorf("logging events: %w", err)
		}
		for i, e := range tx.events {
			var data sql.NullString
			if e.Data != nil {
				data = sql.NullString{String: string(e.Data), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO events (id, type, object_id, data, created_at) VALUES (?, ?, ?, ?, ?)`),
				last-int64(n)+int64(i)+1, e.Type, e.ObjectID, data, e.CreatedAt); err != nil {
				return fmt.Errorf("logging events: %w", err)
			}
		}
	}
	return tx.Commit()
}

// Now returns the ID of the last event committed: changes from now on come
// after it. It needs no event to exist.
func (d *DB) Now(ctx context.Context) (int64, error) {
	var id int64
	if err := d.sql.QueryRowContext(ctx, `SELECT value FROM state WHERE name = 'events'`).Scan(&id); err != nil {
		return 0, fmt.Errorf("reading the events counter: %w", err)
	}
	return id, nil
}

// ListEvents returns up to limit of the organization's events after the
// event since, in commit order: what webhooks send.
func (d *DB) ListEvents(ctx context.Context, since int64, limit int) ([]Event, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT id, type, object_id, data, created_at FROM events
		WHERE id > ? ORDER BY id LIMIT ?`), since, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var data sql.NullString
		if err := rows.Scan(&e.ID, &e.Type, &e.ObjectID, &data, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		if data.Valid {
			e.Data = json.RawMessage(data.String)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Changes are what changed in a mailbox after an event.
type Changes struct {
	Created []Summary // came into the mailbox, delivered or restored, in their current state
	Updated []Summary // there before and changed, in their current state
	Deleted []string  // IDs of messages deleted, even those created since
	LastID  int64     // of the last event read: to list changes after next
	HasMore bool      // whether there are more events after LastID already
}

// ErrChangesPruned reports a list of changes since an event pruned since,
// which it can no longer tell.
var ErrChangesPruned = errors.New("changes since then were pruned; list the mailbox again")

// ListChanges returns what changed in a mailbox after the event since: the messages that came into it and those changed, as
// they are now, and those deleted, each listed once. It reads up to limit of the
// organization's events, those of other mailboxes included, and resumes
// after the last. Since must not be pruned.
func (d *DB) ListChanges(ctx context.Context, mailboxID string, since int64, limit int) (*Changes, error) {
	// Events are pruned oldest first: since is no longer told once an event
	// after it was.
	var pruned int64
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM state WHERE name = 'pruned'`).Scan(&pruned)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("listing changes: %w", err)
	}
	if since < pruned {
		return nil, fmt.Errorf("%w: since %d", ErrChangesPruned, since)
	}

	events, err := d.ListEvents(ctx, since, limit+1)
	if err != nil {
		return nil, err
	}
	c := &Changes{Created: []Summary{}, Updated: []Summary{}, Deleted: []string{}, LastID: since}
	if len(events) > limit {
		events, c.HasMore = events[:limit], true
	}
	// The messages changed, by their last event, those that came in, and
	// the mailbox of each deleted: its row is gone, but its deleted event
	// tells.
	last := map[string]int{}
	came := map[string]bool{}
	deletedFrom := map[string]string{}
	var order []string
	for n, e := range events {
		c.LastID = e.ID
		if !strings.HasPrefix(e.Type, "message.") {
			continue
		}
		last[e.ObjectID] = n
		order = append(order, e.ObjectID)
		if e.Type == EventMessageCreated || e.Type == EventMessageRestored {
			came[e.ObjectID] = true
		}
		if e.Type == EventMessageDeleted && e.Data != nil {
			var data deletedData
			if err := json.Unmarshal(e.Data, &data); err != nil {
				return nil, fmt.Errorf("decoding event %d: %w", e.ID, err)
			}
			deletedFrom[e.ObjectID] = data.MailboxID
		}
	}
	var ids []any
	for n, id := range order {
		if last[id] == n {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return c, nil
	}

	// As they are now, in this mailbox; gone, they were deleted, and are this
	// mailbox's if their deleted event says so. One in another mailbox, or
	// whose deleted event is still to come, isn't this mailbox's change.
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT `+summaryColumns+`, 0 FROM messages
		WHERE messages.id IN (`+placeholders(len(ids))+`)`), ids...)
	if err != nil {
		return nil, fmt.Errorf("listing changes: %w", err)
	}
	summaries, err := d.scanSummaries(ctx, rows)
	if err != nil {
		return nil, err
	}
	live := map[string]*Summary{}
	for i := range summaries {
		live[summaries[i].ID] = &summaries[i]
	}
	for _, id := range ids {
		id := id.(string)
		m, ok := live[id]
		switch {
		case ok && m.MailboxID == mailboxID && came[id]:
			c.Created = append(c.Created, *m)
		case ok && m.MailboxID == mailboxID:
			c.Updated = append(c.Updated, *m)
		case !ok && deletedFrom[id] == mailboxID:
			c.Deleted = append(c.Deleted, id)
		}
	}
	return c, nil
}

// DeletedMessage is a message deleted for good but not yet expunged: what
// restoring it needs besides its raw source, which stays in storage until
// then, and what lists of deleted messages show.
type DeletedMessage struct {
	ID             string    `json:"-"`
	MailboxID      string    `json:"-"`
	DeletedAt      time.Time `json:"-"`
	MailFrom       string    `json:"mail_from"`
	RcptTo         string    `json:"rcpt_to"`
	From           Address   `json:"from"`
	Subject        string    `json:"subject"`
	Size           int64     `json:"size"`
	Folder         string    `json:"folder"`
	Labels         []string  `json:"labels"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	ReceivedAt     time.Time `json:"received_at"`
	Flags
}

// deletedData is a message.deleted event's data: the mailbox the message
// was in, and until it's expunged, what restoring it needs.
type deletedData struct {
	MailboxID string          `json:"mailbox_id"`
	Restore   *DeletedMessage `json:"restore,omitempty"`
}

// ErrNotRestorable reports a message that isn't deleted, or was expunged.
var ErrNotRestorable = errors.New("no deleted message to restore")

// restorable returns the condition selecting the message.deleted events of
// messages not yet expunged: their data still has restore.
func (d *DB) restorable() string {
	if d.dialect == "pgx" {
		return `type = 'message.deleted' AND (data::jsonb -> 'restore') IS NOT NULL`
	}
	return `type = 'message.deleted' AND json_extract(data, '$.restore') IS NOT NULL`
}

// DeleteMessage deletes a message for good, logging it as deleted with its
// mailbox. If restorable, the event also keeps the DeletedMessage, which
// RestoreMessage needs until Expunge drops it, and the caller keeps the raw
// source until then; otherwise the caller removes it now. Its deliveries go
// too, so those still to go are cancelled. Deleting a missing message is
// not an error.
func (d *DB) DeleteMessage(ctx context.Context, id string, restorable bool) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("deleting message: %w", err)
	}
	defer tx.Rollback()
	m, err := getMessage(ctx, d, tx, id)
	if errors.Is(err, ErrMessageNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	labels, err := d.labelsOf(ctx, tx.Tx, id)
	if err != nil {
		return err
	}
	for _, query := range []string{`DELETE FROM message_labels WHERE message_id = ?`, `DELETE FROM deliveries WHERE message_id = ?`, `DELETE FROM messages WHERE id = ?`} {
		if _, err := tx.ExecContext(ctx, d.q(query), id); err != nil {
			return fmt.Errorf("deleting message: %w", err)
		}
	}
	data := deletedData{MailboxID: m.MailboxID}
	if restorable {
		if labels == nil {
			labels = []string{}
		}
		data.Restore = &DeletedMessage{
			MailFrom: m.MailFrom, RcptTo: m.RcptTo, From: m.From, Subject: m.Subject, Size: m.Size,
			Folder: m.Folder, Labels: labels, IdempotencyKey: m.IdempotencyKey, ReceivedAt: m.ReceivedAt, Flags: m.Flags,
		}
	}
	d.logEvent(tx, EventMessageDeleted, id, data)
	return d.commit(ctx, tx)
}

const deletedColumns = `object_id, created_at, data`

func scanDeleted(row interface{ Scan(...any) error }) (*DeletedMessage, error) {
	var id, data string
	var deletedAt time.Time
	if err := row.Scan(&id, &deletedAt, &data); err != nil {
		return nil, err
	}
	var d deletedData
	if err := json.Unmarshal([]byte(data), &d); err != nil || d.Restore == nil {
		return nil, fmt.Errorf("decoding deleted message %s: %v", id, err)
	}
	m := d.Restore
	m.ID, m.MailboxID, m.DeletedAt = id, d.MailboxID, deletedAt
	return m, nil
}

// ListDeleted returns a mailbox's messages deleted but not yet expunged,
// most recently deleted first.
func (d *DB) ListDeleted(ctx context.Context, mailboxID string) ([]*DeletedMessage, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT `+deletedColumns+` FROM events
		WHERE `+d.restorable()+` ORDER BY id DESC`))
	if err != nil {
		return nil, fmt.Errorf("listing deleted messages: %w", err)
	}
	defer rows.Close()
	var list []*DeletedMessage
	for rows.Next() {
		m, err := scanDeleted(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning deleted message: %w", err)
		}
		if m.MailboxID == mailboxID {
			list = append(list, m)
		}
	}
	return list, rows.Err()
}

// GetDeleted returns a message deleted but not yet expunged, or an error
// wrapping ErrNotRestorable.
func (d *DB) GetDeleted(ctx context.Context, id string) (*DeletedMessage, error) {
	m, err := scanDeleted(d.sql.QueryRowContext(ctx, d.q(`SELECT `+deletedColumns+` FROM events
		WHERE object_id = ? AND `+d.restorable()), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrNotRestorable, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting deleted message: %w", err)
	}
	return m, nil
}

// unrestorable drops what restoring a deleted message needs from its
// deleted event, keeping its mailbox, and reports whether there was any.
func (d *DB) unrestorable(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, m *DeletedMessage) (bool, error) {
	res, err := q.ExecContext(ctx, d.q(`UPDATE events SET data = ? WHERE object_id = ? AND `+d.restorable()),
		marshalJSON(deletedData{MailboxID: m.MailboxID}), m.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RestoreMessage stores m, parsed again from a deleted message's raw source,
// under its ID, with labels, logging it as restored; its deleted event no
// longer restores it, so Expunge leaves its raw source alone. If a message
// stored since has its idempotency key, m does without.
func (d *DB) RestoreMessage(ctx context.Context, m Message, labels []string) (*Message, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("restoring message: %w", err)
	}
	defer tx.Rollback()
	ok, err := d.unrestorable(ctx, tx, &DeletedMessage{ID: m.ID, MailboxID: m.MailboxID})
	if err != nil {
		return nil, fmt.Errorf("restoring message: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotRestorable, m.ID)
	}
	if m.IdempotencyKey != "" {
		var taken int
		err := tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM messages WHERE mailbox_id = ? AND idempotency_key = ?`), m.MailboxID, m.IdempotencyKey).Scan(&taken)
		if err == nil {
			m.IdempotencyKey = ""
		} else if err != sql.ErrNoRows {
			return nil, fmt.Errorf("restoring message: %w", err)
		}
	}
	if err := d.insertMessage(ctx, tx, m, labels, EventMessageRestored); err != nil {
		return nil, err
	}
	if err := d.commit(ctx, tx); err != nil {
		return nil, fmt.Errorf("restoring message: %w", err)
	}
	return d.GetMessage(ctx, m.ID)
}

// Expirable returns the messages deleted before, and not yet expunged:
// their raw sources are to be removed, then Expunge called.
func (d *DB) Expirable(ctx context.Context, before time.Time) ([]*DeletedMessage, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT `+deletedColumns+` FROM events
		WHERE `+d.restorable()+` AND created_at < ?`), before.UTC())
	if err != nil {
		return nil, fmt.Errorf("listing expirable messages: %w", err)
	}
	defer rows.Close()
	var list []*DeletedMessage
	for rows.Next() {
		m, err := scanDeleted(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning expirable message: %w", err)
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// Expunge marks a deleted message, whose raw source is gone, as no longer
// restorable; its deleted event keeps its mailbox.
func (d *DB) Expunge(ctx context.Context, m *DeletedMessage) error {
	if _, err := d.unrestorable(ctx, d.sql, m); err != nil {
		return fmt.Errorf("expunging message: %w", err)
	}
	return nil
}

// PruneEvents removes the events logged before, oldest first, and returns
// how many it removed. None is removed from the first that still restores
// a message not yet expunged on. The ID of the newest it removes is kept
// as pruned: lists of changes since before it fail.
func (d *DB) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	defer tx.Rollback()
	var newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, d.q(`SELECT MAX(id) FROM events WHERE created_at < ?
		AND id < COALESCE((SELECT MIN(id) FROM events WHERE `+d.restorable()+`), id + 1)`), before.UTC()).
		Scan(&newest); err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	if !newest.Valid {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, d.q(`DELETE FROM events WHERE id <= ?`), newest.Int64)
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO state (name, value) VALUES ('pruned', ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`), newest.Int64); err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	return n, tx.Commit()
}
