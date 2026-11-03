package db

import (
	"context"
	"database/sql"
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
	MessageID          string
	MailboxID          string
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

const deliveryColumns = `id, message_id, mailbox_id, recipient, status, transport, transport_message_id,
	attempts, next_attempt_at, detail, created_at, updated_at`

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var next sql.NullTime
	if err := row.Scan(&d.ID, &d.MessageID, &d.MailboxID, &d.Recipient, &d.Status, &d.Transport, &d.TransportMessageID,
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

// SendMessage stores m, as CreateMessage does, with a queued delivery to
// each recipient through transport, due now. A repeated idempotency key
// returns the stored message, whose deliveries are already there.
func (d *DB) SendMessage(ctx context.Context, m Message, labels []string, transport string, recipients []string) (stored *Message, created bool, err error) {
	return d.createMessage(ctx, m, labels, func(tx *txn) error {
		now := time.Now().UTC()
		for _, rcpt := range recipients {
			id := NewID()
			_, err := tx.ExecContext(ctx, d.q(`INSERT INTO deliveries (id, message_id, mailbox_id, recipient, status, transport, next_attempt_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`), id, m.ID, m.MailboxID, rcpt, DeliveryQueued, transport, now, now, now)
			if err != nil {
				return fmt.Errorf("queueing delivery: %w", err)
			}
			d.logEvent(tx, EventDeliveryCreated, id, deliveryData{MessageID: m.ID, MailboxID: m.MailboxID, Status: DeliveryQueued})
		}
		return nil
	})
}

// deliveryData is a delivery event's data: whose delivery it is, and its
// status after.
type deliveryData struct {
	MessageID string `json:"message_id"`
	MailboxID string `json:"mailbox_id"`
	Status    string `json:"status"`
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
	After      string // the ID of the last delivery of the previous page
	Limit      int
}

// ListDeliveries returns the deliveries f selects, oldest first.
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
	if f.After != "" {
		where, args = append(where, `id > ?`), append(args, f.After)
	}
	query := `SELECT ` + deliveryColumns + ` FROM deliveries`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing deliveries: %w", err)
	}
	return scanDeliveries(rows)
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
	d.logEvent(tx, EventDeliveryUpdated, id, deliveryData{MessageID: dl.MessageID, MailboxID: dl.MailboxID, Status: dl.Status})
	if err := d.commit(ctx, tx); err != nil {
		return nil, fmt.Errorf("recording delivery: %w", err)
	}
	return dl, nil
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
	return d.RecordDelivery(ctx, id, DeliveryAttempt{Status: DeliveryQueued, Detail: dl.Detail, NextAttemptAt: &now})
}
