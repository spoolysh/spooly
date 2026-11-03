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

// Delivery statuses: queued and deferred are still to go, the rest say how
// it went.
const (
	DeliveryQueued     = "queued"     // waiting to be handed to the transport
	DeliveryDeferred   = "deferred"   // the transport failed for now; tried again later
	DeliverySent       = "sent"       // the transport took it
	DeliveryDelayed    = "delayed"    // the transport is still trying
	DeliveryDelivered  = "delivered"  // the recipient's server took it
	DeliveryBounced    = "bounced"    // it failed for good after the transport took it
	DeliveryComplained = "complained" // the recipient reported it as spam
	DeliveryFailed     = "failed"     // Spooly gave up: rejected for good, or too many tries
)

// Delivery event types.
const (
	EventDeliveryCreated = "delivery.created"
	EventDeliveryUpdated = "delivery.updated"
)

// ErrDeliveryNotFound reports a delivery that doesn't exist.
var ErrDeliveryNotFound = errors.New("delivery not found")

// ErrNotRetryable reports a delivery that's neither deferred nor failed.
var ErrNotRetryable = errors.New("only a deferred or failed delivery can be retried")

// Delivery is a sent message's way to one recipient.
type Delivery struct {
	ID                 string
	MessageID          string // which may be gone, or never kept
	MailboxID          string
	MailFrom           string // the envelope sender
	Subject            string
	Recipient          string
	Status             string
	Transport          string
	TransportMessageID string
	Attempts           int
	NextAttemptAt      *time.Time // while queued or deferred
	Detail             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Pending reports whether d is still to go.
func (d *Delivery) Pending() bool {
	return d.Status == DeliveryQueued || d.Status == DeliveryDeferred
}

const deliveryColumns = `id, message_id, mailbox_id, mail_from, subject, recipient, status, transport, transport_message_id,
	attempts, next_attempt_at, detail, created_at, updated_at`

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var next sql.NullTime
	if err := row.Scan(&d.ID, &d.MessageID, &d.MailboxID, &d.MailFrom, &d.Subject, &d.Recipient, &d.Status, &d.Transport, &d.TransportMessageID,
		&d.Attempts, &next, &d.Detail, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if next.Valid {
		d.NextAttemptAt = &next.Time
	}
	return &d, nil
}

func scanDeliveries(rows *sql.Rows) ([]*Delivery, error) {
	defer rows.Close()
	list := []*Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("reading deliveries: %w", err)
		}
		list = append(list, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading deliveries: %w", err)
	}
	return list, nil
}

// Outgoing is a message to deliver: its ID, whose raw source storage has,
// its mailbox, sender, and subject, the send's idempotency key, if any,
// and the transport and recipients.
type Outgoing struct {
	MessageID, MailboxID, MailFrom, Subject string
	IdempotencyKey                          string
	Transport                               string
	Recipients                              []string
}

// SendMessage stores m, as CreateMessage does, with a queued delivery to
// each recipient through transport, due now. A repeated idempotency key
// returns the stored message, whose deliveries are already there.
func (d *DB) SendMessage(ctx context.Context, m Message, labels []string, transport string, recipients []string) (stored *Message, created bool, err error) {
	return d.createMessage(ctx, m, labels, func(tx *txn) error {
		return d.queue(ctx, tx, Outgoing{
			MessageID: m.ID, MailboxID: m.MailboxID, MailFrom: m.MailFrom, Subject: m.Subject,
			IdempotencyKey: m.IdempotencyKey, Transport: transport, Recipients: recipients,
		})
	})
}

// QueueDeliveries queues o's deliveries, due now, for a message kept
// nowhere but in storage until they're done. A repeated idempotency key
// returns the deliveries it queued, with created false.
func (d *DB) QueueDeliveries(ctx context.Context, o Outgoing) (deliveries []*Delivery, created bool, err error) {
	repeat := func() ([]*Delivery, error) {
		if o.IdempotencyKey == "" {
			return nil, nil
		}
		return d.ListDeliveries(ctx, DeliveryFilter{MailboxID: o.MailboxID, IdempotencyKey: o.IdempotencyKey})
	}
	if list, err := repeat(); err != nil || len(list) > 0 {
		return list, false, err
	}
	tx, err := d.begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("queueing deliveries: %w", err)
	}
	defer tx.Rollback()
	if err := d.queue(ctx, tx, o); err != nil {
		tx.Rollback()
		// Lost a race with a retry using the same idempotency key.
		if list, rerr := repeat(); rerr == nil && len(list) > 0 {
			return list, false, nil
		}
		return nil, false, err
	}
	if err := d.commit(ctx, tx); err != nil {
		return nil, false, fmt.Errorf("queueing deliveries: %w", err)
	}
	deliveries, err = d.ListDeliveries(ctx, DeliveryFilter{MessageIDs: []string{o.MessageID}})
	return deliveries, true, err
}

// queue inserts a queued delivery to each of o's recipients, due now.
func (d *DB) queue(ctx context.Context, tx *txn, o Outgoing) error {
	var key sql.NullString
	if o.IdempotencyKey != "" {
		key = sql.NullString{String: o.IdempotencyKey, Valid: true}
	}
	now := time.Now().UTC()
	for _, rcpt := range o.Recipients {
		id := NewID()
		_, err := tx.ExecContext(ctx, d.q(`INSERT INTO deliveries (id, message_id, mailbox_id, mail_from, subject, recipient,
				status, transport, next_attempt_at, idempotency_key, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, o.MessageID, o.MailboxID, o.MailFrom, o.Subject, rcpt, DeliveryQueued, o.Transport, now, key, now, now)
		if err != nil {
			return fmt.Errorf("queueing delivery: %w", err)
		}
		d.logEvent(tx, EventDeliveryCreated, id, deliveryData{MessageID: o.MessageID, MailboxID: o.MailboxID, Status: DeliveryQueued})
	}
	return nil
}

// Pending reports whether any delivery of a message is still to go.
func (d *DB) Pending(ctx context.Context, messageID string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT COUNT(*) FROM deliveries
		WHERE message_id = ? AND status IN ('queued', 'deferred')`), messageID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking deliveries: %w", err)
	}
	return n > 0, nil
}

// deliveryData is a delivery event's data: whose delivery it is, what it
// became, and why.
type deliveryData struct {
	MessageID          string `json:"message_id"`
	MailboxID          string `json:"mailbox_id"`
	Status             string `json:"status"`
	Attempt            int    `json:"attempt,omitempty"` // which, from 1, if it was one
	Detail             string `json:"detail,omitempty"`
	TransportMessageID string `json:"transport_message_id,omitempty"`
}

// GetDelivery returns a delivery.
func (d *DB) GetDelivery(ctx context.Context, id string) (*Delivery, error) {
	dl, err := scanDelivery(d.sql.QueryRowContext(ctx, d.q(`SELECT `+deliveryColumns+` FROM deliveries WHERE id = ?`), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting delivery: %w", err)
	}
	return dl, nil
}

// DeliveryFilter selects deliveries. Empty fields select all.
type DeliveryFilter struct {
	MessageIDs []string
	MailboxID  string
	MailboxIDs []string // nil means all; empty, none
	Status     string
	// IdempotencyKey selects those a send with it queued.
	IdempotencyKey string
	After          string // the ID of the last delivery of the previous page
	Before         string // the ID of the first delivery of the next page, for Newest
	Newest         bool   // newest first
	Limit          int
}

// ListDeliveries returns the deliveries f selects, oldest first unless
// f.Newest.
func (d *DB) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]*Delivery, error) {
	var where []string
	var args []any
	if f.MessageIDs != nil {
		if len(f.MessageIDs) == 0 {
			return []*Delivery{}, nil
		}
		where = append(where, `message_id IN (`+placeholders(len(f.MessageIDs))+`)`)
		for _, id := range f.MessageIDs {
			args = append(args, id)
		}
	}
	if f.MailboxID != "" {
		where, args = append(where, `mailbox_id = ?`), append(args, f.MailboxID)
	}
	if f.MailboxIDs != nil {
		if len(f.MailboxIDs) == 0 {
			return []*Delivery{}, nil
		}
		where = append(where, `mailbox_id IN (`+placeholders(len(f.MailboxIDs))+`)`)
		for _, id := range f.MailboxIDs {
			args = append(args, id)
		}
	}
	if f.Status != "" {
		where, args = append(where, `status = ?`), append(args, f.Status)
	}
	if f.IdempotencyKey != "" {
		where, args = append(where, `idempotency_key = ?`), append(args, f.IdempotencyKey)
	}
	if f.After != "" {
		where, args = append(where, `id > ?`), append(args, f.After)
	}
	if f.Before != "" {
		where, args = append(where, `id < ?`), append(args, f.Before)
	}
	query := `SELECT ` + deliveryColumns + ` FROM deliveries`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id`
	if f.Newest {
		query += ` DESC`
	}
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing deliveries: %w", err)
	}
	return scanDeliveries(rows)
}

// CountDeliveries counts deliveries by status, a mailbox's if mailboxID
// isn't empty.
func (d *DB) CountDeliveries(ctx context.Context, mailboxID string) (map[string]int, error) {
	query, args := `SELECT status, COUNT(*) FROM deliveries`, []any{}
	if mailboxID != "" {
		query, args = query+` WHERE mailbox_id = ?`, append(args, mailboxID)
	}
	rows, err := d.sql.QueryContext(ctx, d.q(query+` GROUP BY status`), args...)
	if err != nil {
		return nil, fmt.Errorf("counting deliveries: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("counting deliveries: %w", err)
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

// ClaimDeliveries claims up to limit deliveries due by now, of the message
// messageID or of any if it's empty, and returns them: it moves their next
// attempt lease ahead, so no other spool sends them meanwhile. If sending
// stops halfway, they're due again then.
func (d *DB) ClaimDeliveries(ctx context.Context, messageID string, now time.Time, lease time.Duration, limit int) ([]*Delivery, error) {
	due := `status IN ('queued', 'deferred') AND next_attempt_at <= ?`
	args := []any{now.Add(lease).UTC(), now.UTC()}
	if messageID != "" {
		due += ` AND message_id = ?`
		args = append(args, messageID)
	}
	args = append(args, now.UTC())
	// Claimed by another spool meanwhile, a row no longer matches next_attempt_at <= now
	// when this update gets to it, and is left out.
	rows, err := d.sql.QueryContext(ctx, d.q(fmt.Sprintf(`UPDATE deliveries SET next_attempt_at = ?
		WHERE id IN (SELECT id FROM deliveries WHERE %s ORDER BY next_attempt_at LIMIT %d) AND next_attempt_at <= ?
		RETURNING `+deliveryColumns, due, limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("claiming deliveries: %w", err)
	}
	claimed, err := scanDeliveries(rows)
	if err != nil {
		return nil, err
	}
	// RETURNING has no order; queued first, sent first.
	slices.SortFunc(claimed, func(a, b *Delivery) int { return strings.Compare(a.ID, b.ID) })
	return claimed, nil
}

// DeliveryAttempt is how an attempt to hand a delivery to the transport, or
// news of it from the transport later, went.
type DeliveryAttempt struct {
	Status             string
	Detail             string
	TransportMessageID string     // kept if empty
	NextAttemptAt      *time.Time // for deferred
	Attempted          bool       // counts as an attempt
	// Note is what its event says, if not Detail: "retried by
	// hand", say.
	Note string
}

// RecordDelivery records an attempt on the delivery id, and logs it as updated.
func (d *DB) RecordDelivery(ctx context.Context, id string, a DeliveryAttempt) (*Delivery, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("recording delivery: %w", err)
	}
	defer tx.Rollback()
	var next sql.NullTime
	if a.NextAttemptAt != nil {
		next = sql.NullTime{Time: a.NextAttemptAt.UTC(), Valid: true}
	}
	attempts := 0
	if a.Attempted {
		attempts = 1
	}
	dl, err := scanDelivery(tx.QueryRowContext(ctx, d.q(`UPDATE deliveries
		SET status = ?, detail = ?, transport_message_id = CASE WHEN ? = '' THEN transport_message_id ELSE ? END,
			next_attempt_at = ?, attempts = attempts + ?, updated_at = ?
		WHERE id = ? RETURNING `+deliveryColumns),
		a.Status, a.Detail, a.TransportMessageID, a.TransportMessageID, next, attempts, time.Now().UTC(), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("recording delivery: %w", err)
	}
	data := deliveryData{MessageID: dl.MessageID, MailboxID: dl.MailboxID, Status: dl.Status, Detail: a.Detail, TransportMessageID: a.TransportMessageID}
	if a.Note != "" {
		data.Detail = a.Note
	}
	if a.Attempted {
		data.Attempt = dl.Attempts
	}
	d.logEvent(tx, EventDeliveryUpdated, id, data)
	if err := d.commit(ctx, tx); err != nil {
		return nil, fmt.Errorf("recording delivery: %w", err)
	}
	return dl, nil
}

// DeliveryEvent is an event of a delivery: what it became, and why.
type DeliveryEvent struct {
	At                 time.Time
	Status             string
	Attempt            int // which attempt, from 1; 0 if it wasn't one
	Detail             string
	TransportMessageID string
}

// DeliveryEvents returns a delivery's events, oldest first: what happened to
// it, as far back as events are kept.
func (d *DB) DeliveryEvents(ctx context.Context, deliveryID string) ([]DeliveryEvent, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT id, created_at, data FROM events
		WHERE object_id = ? AND type IN (?, ?) ORDER BY id`), deliveryID, EventDeliveryCreated, EventDeliveryUpdated)
	if err != nil {
		return nil, fmt.Errorf("reading delivery events: %w", err)
	}
	defer rows.Close()
	events := []DeliveryEvent{}
	for rows.Next() {
		var id int64
		var data string
		var e DeliveryEvent
		if err := rows.Scan(&id, &e.At, &data); err != nil {
			return nil, fmt.Errorf("reading delivery events: %w", err)
		}
		var dd deliveryData
		if err := json.Unmarshal([]byte(data), &dd); err != nil {
			return nil, fmt.Errorf("decoding event %d: %w", id, err)
		}
		e.Status, e.Attempt, e.Detail, e.TransportMessageID = dd.Status, dd.Attempt, dd.Detail, dd.TransportMessageID
		events = append(events, e)
	}
	return events, rows.Err()
}

// RetryDelivery queues a deferred or failed delivery again, due now.
func (d *DB) RetryDelivery(ctx context.Context, id string) (*Delivery, error) {
	dl, err := d.GetDelivery(ctx, id)
	if err != nil {
		return nil, err
	}
	if dl.Status != DeliveryDeferred && dl.Status != DeliveryFailed {
		return nil, fmt.Errorf("%w: it's %s", ErrNotRetryable, dl.Status)
	}
	now := time.Now()
	return d.RecordDelivery(ctx, id, DeliveryAttempt{Status: DeliveryQueued, Detail: dl.Detail, NextAttemptAt: &now, Note: "retried by hand"})
}
