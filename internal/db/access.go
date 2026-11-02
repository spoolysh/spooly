package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrUserNotFound reports that no user has the requested ID or email.
var ErrUserNotFound = errors.New("user not found")

// AddUser gives a user access to a mailbox.
func (d *DB) AddUser(ctx context.Context, mailboxID, userID string) error {
	_, err := d.sql.ExecContext(ctx, d.q(`INSERT INTO mailbox_users (mailbox_id, user_id)
		VALUES (?, ?) ON CONFLICT DO NOTHING`), mailboxID, userID)
	if err != nil {
		return fmt.Errorf("sharing mailbox: %w", err)
	}
	return nil
}

// RemoveUser takes a user's access to a mailbox away.
func (d *DB) RemoveUser(ctx context.Context, mailboxID, userID string) error {
	if _, err := d.sql.ExecContext(ctx, d.q(`DELETE FROM mailbox_users WHERE mailbox_id = ? AND user_id = ?`), mailboxID, userID); err != nil {
		return fmt.Errorf("unsharing mailbox: %w", err)
	}
	return nil
}

// HasAccess reports whether a mailbox is shared with a user.
func (d *DB) HasAccess(ctx context.Context, mailboxID, userID string) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT 1 FROM mailbox_users WHERE mailbox_id = ? AND user_id = ?`), mailboxID, userID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking access: %w", err)
	}
	return true, nil
}

// MailboxIDsFor returns the IDs of the mailboxes shared with a user.
func (d *DB) MailboxIDsFor(ctx context.Context, userID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT mailbox_id FROM mailbox_users WHERE user_id = ?`), userID)
	if err != nil {
		return nil, fmt.Errorf("listing access: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning access: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MailboxUsers returns the users a mailbox is shared with, by email.
func (d *DB) MailboxUsers(ctx context.Context, mailboxID string) ([]User, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT u.id, u.google_id, u.email, u.name, u.created_at
		FROM mailbox_users a JOIN users u ON u.id = a.user_id
		WHERE a.mailbox_id = ? ORDER BY u.email`), mailboxID)
	if err != nil {
		return nil, fmt.Errorf("listing access: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.GoogleID, &u.Email, &u.Name, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning access: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetUserByEmail finds a user by the email they sign in with.
func (d *DB) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT id, google_id, email, name, created_at FROM users WHERE LOWER(email) = LOWER(?)`), email).
		Scan(&u.ID, &u.GoogleID, &u.Email, &u.Name, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrUserNotFound, email)
	}
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return &u, nil
}

// ListUsers returns every user who has signed in, newest first.
func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, google_id, email, name, created_at FROM users ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.GoogleID, &u.Email, &u.Name, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// AllAccess returns the IDs of the mailboxes shared with each user, by user
// ID.
func (d *DB) AllAccess(ctx context.Context) (map[string][]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT user_id, mailbox_id FROM mailbox_users ORDER BY mailbox_id`)
	if err != nil {
		return nil, fmt.Errorf("listing access: %w", err)
	}
	defer rows.Close()
	access := map[string][]string{}
	for rows.Next() {
		var userID, mailboxID string
		if err := rows.Scan(&userID, &mailboxID); err != nil {
			return nil, fmt.Errorf("scanning access: %w", err)
		}
		access[userID] = append(access[userID], mailboxID)
	}
	return access, rows.Err()
}

// RemoveAllAccess takes a user off every mailbox and returns how many they
// had.
func (d *DB) RemoveAllAccess(ctx context.Context, userID string) (int, error) {
	result, err := d.sql.ExecContext(ctx, d.q(`DELETE FROM mailbox_users WHERE user_id = ?`), userID)
	if err != nil {
		return 0, fmt.Errorf("unsharing mailboxes: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
