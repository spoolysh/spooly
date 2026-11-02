package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Mailbox receives all mail sent to any of its addresses.
type Mailbox struct {
	ID        string
	Addresses []string // lowercased, in order; the first is shown for the mailbox
	Name      string   // empty when not set
	CreatedAt time.Time
}

// Address is the address shown for the mailbox: its first.
func (a Mailbox) Address() string {
	if len(a.Addresses) == 0 {
		return ""
	}
	return a.Addresses[0]
}

var (
	// ErrMailboxNotFound reports that no mailbox has the requested ID or address.
	ErrMailboxNotFound = errors.New("mailbox not found")
	// ErrAddressTaken reports that an address already belongs to another
	// mailbox.
	ErrAddressTaken = errors.New("address already belongs to a mailbox")
	// ErrNoAddresses reports a mailbox left without addresses.
	ErrNoAddresses = errors.New("a mailbox needs at least one address")
)

const mailboxColumns = `id, name, created_at`

func scanMailbox(row interface{ Scan(...any) error }) (*Mailbox, error) {
	var a Mailbox
	if err := row.Scan(&a.ID, &a.Name, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateMailbox inserts a with its addresses, and returns it with its ID,
// which the database assigns, and creation time set.
func (d *DB) CreateMailbox(ctx context.Context, a Mailbox) (*Mailbox, error) {
	if len(a.Addresses) == 0 {
		return nil, ErrNoAddresses
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("inserting mailbox: %w", err)
	}
	defer tx.Rollback()
	a.ID = NewID()
	if err := tx.QueryRowContext(ctx, d.q(`INSERT INTO mailboxes (id, name, created_at) VALUES (?, ?, ?) RETURNING created_at`),
		a.ID, a.Name, time.Now().UTC()).Scan(&a.CreatedAt); err != nil {
		return nil, fmt.Errorf("inserting mailbox: %w", err)
	}
	if err := d.setAddresses(ctx, tx, a.ID, a.Addresses); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("inserting mailbox: %w", err)
	}
	return &a, nil
}

// setAddresses replaces a mailbox's addresses with addresses, in order.
// Each must not belong to another mailbox.
func (d *DB) setAddresses(ctx context.Context, tx *sql.Tx, mailboxID string, addresses []string) error {
	if len(addresses) == 0 {
		return ErrNoAddresses
	}
	for _, address := range addresses {
		var owner string
		err := tx.QueryRowContext(ctx, d.q(`SELECT mailbox_id FROM mailbox_addresses WHERE address = ?`), address).Scan(&owner)
		if err == nil && owner != mailboxID {
			return fmt.Errorf("%w: %s", ErrAddressTaken, address)
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("checking address: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM mailbox_addresses WHERE mailbox_id = ?`), mailboxID); err != nil {
		return fmt.Errorf("replacing addresses: %w", err)
	}
	for i, address := range addresses {
		if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO mailbox_addresses (address, mailbox_id, position) VALUES (?, ?, ?)`),
			address, mailboxID, i); err != nil {
			// Lost a race with another mailbox taking the address.
			return fmt.Errorf("%w: %s", ErrAddressTaken, address)
		}
	}
	return nil
}

func (d *DB) GetMailbox(ctx context.Context, id string) (*Mailbox, error) {
	a, err := scanMailbox(d.sql.QueryRowContext(ctx, d.q(`SELECT `+mailboxColumns+` FROM mailboxes WHERE id = ?`), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrMailboxNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting mailbox: %w", err)
	}
	if err := d.loadAddresses(ctx, []*Mailbox{a}); err != nil {
		return nil, err
	}
	return a, nil
}

// GetMailboxByAddress finds the mailbox with exactly address among its
// addresses. address must already be lowercased.
func (d *DB) GetMailboxByAddress(ctx context.Context, address string) (*Mailbox, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT mailbox_id FROM mailbox_addresses WHERE address = ?`), address).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrMailboxNotFound, address)
	}
	if err != nil {
		return nil, fmt.Errorf("getting address: %w", err)
	}
	return d.GetMailbox(ctx, id)
}

// loadAddresses sets the addresses of mailboxes, in order.
func (d *DB) loadAddresses(ctx context.Context, mailboxes []*Mailbox) error {
	if len(mailboxes) == 0 {
		return nil
	}
	byID := make(map[string]*Mailbox, len(mailboxes))
	args := make([]any, len(mailboxes))
	for i, a := range mailboxes {
		a.Addresses = []string{}
		byID[a.ID] = a
		args[i] = a.ID
	}
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT mailbox_id, address FROM mailbox_addresses
		WHERE mailbox_id IN (`+placeholders(len(args))+`) ORDER BY position`), args...)
	if err != nil {
		return fmt.Errorf("loading addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, address string
		if err := rows.Scan(&id, &address); err != nil {
			return fmt.Errorf("scanning address: %w", err)
		}
		byID[id].Addresses = append(byID[id].Addresses, address)
	}
	return rows.Err()
}

// MailboxFilter selects mailboxes for ListMailboxes.
type MailboxFilter struct {
	// UserID limits the list to mailboxes shared with that user; empty means
	// every mailbox.
	UserID string
	After  string // only mailboxes with smaller IDs
	Limit  int
}

// ListMailboxes returns mailboxes matching f, newest first by ID.
func (d *DB) ListMailboxes(ctx context.Context, f MailboxFilter) ([]Mailbox, error) {
	query := `SELECT ` + mailboxColumns + ` FROM mailboxes WHERE 1 = 1`
	var args []any
	if f.UserID != "" {
		query += ` AND id IN (SELECT mailbox_id FROM mailbox_users WHERE user_id = ?)`
		args = append(args, f.UserID)
	}
	if f.After != "" {
		query += ` AND id < ?`
		args = append(args, f.After)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing mailboxes: %w", err)
	}
	var list []*Mailbox
	for rows.Next() {
		a, err := scanMailbox(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning mailbox: %w", err)
		}
		list = append(list, a)
	}
	rows.Close() // before loading addresses: SQLite runs on one connection
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing mailboxes: %w", err)
	}
	if err := d.loadAddresses(ctx, list); err != nil {
		return nil, err
	}
	mailboxes := make([]Mailbox, len(list))
	for i, a := range list {
		mailboxes[i] = *a
	}
	return mailboxes, nil
}

// ResolveMailbox finds the mailbox for an address, as mail delivered to it
// would: an exact match first, then "local+tag@domain" as "local@domain".
// address must already be lowercased.
func (d *DB) ResolveMailbox(ctx context.Context, address string) (*Mailbox, error) {
	a, err := d.GetMailboxByAddress(ctx, address)
	if !errors.Is(err, ErrMailboxNotFound) {
		return a, err
	}
	local, domain, ok := strings.Cut(address, "@")
	if !ok {
		return nil, err
	}
	base, _, tagged := strings.Cut(local, "+")
	if !tagged || base == "" {
		return nil, err
	}
	return d.GetMailboxByAddress(ctx, base+"@"+domain)
}

// MailboxUpdate changes a mailbox. Nil fields are left unchanged.
type MailboxUpdate struct {
	Name *string
	// Addresses, if not nil, replaces the mailbox's addresses, in order.
	Addresses []string
}

// UpdateMailbox applies u to mailbox id and returns the updated mailbox.
func (d *DB) UpdateMailbox(ctx context.Context, id string, u MailboxUpdate) (*Mailbox, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("updating mailbox: %w", err)
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM mailboxes WHERE id = ?`), id).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrMailboxNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting mailbox: %w", err)
	}
	if u.Name != nil {
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE mailboxes SET name = ? WHERE id = ?`), *u.Name, id); err != nil {
			return nil, fmt.Errorf("renaming mailbox: %w", err)
		}
	}
	if u.Addresses != nil {
		if err := d.setAddresses(ctx, tx, id, u.Addresses); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("updating mailbox: %w", err)
	}
	return d.GetMailbox(ctx, id)
}
