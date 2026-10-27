package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrUserNotFound reports that no user has the requested ID or email.
var ErrUserNotFound = errors.New("user not found")

// AddUser gives a user access to an account.
func (d *DB) AddUser(ctx context.Context, accountID, userID string) error {
	_, err := d.sql.ExecContext(ctx, d.q(`INSERT INTO account_users (account_id, user_id)
		VALUES (?, ?) ON CONFLICT DO NOTHING`), accountID, userID)
	if err != nil {
		return fmt.Errorf("sharing account: %w", err)
	}
	return nil
}

// RemoveUser takes a user's access to an account away.
func (d *DB) RemoveUser(ctx context.Context, accountID, userID string) error {
	if _, err := d.sql.ExecContext(ctx, d.q(`DELETE FROM account_users WHERE account_id = ? AND user_id = ?`), accountID, userID); err != nil {
		return fmt.Errorf("unsharing account: %w", err)
	}
	return nil
}

// HasAccess reports whether an account is shared with a user.
func (d *DB) HasAccess(ctx context.Context, accountID, userID string) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT 1 FROM account_users WHERE account_id = ? AND user_id = ?`), accountID, userID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking access: %w", err)
	}
	return true, nil
}

// AccountIDsFor returns the IDs of the accounts shared with a user.
func (d *DB) AccountIDsFor(ctx context.Context, userID string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT account_id FROM account_users WHERE user_id = ?`), userID)
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

// AccountUsers returns the users an account is shared with, by email.
func (d *DB) AccountUsers(ctx context.Context, accountID string) ([]User, error) {
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT u.id, u.google_id, u.email, u.name, u.created_at
		FROM account_users a JOIN users u ON u.id = a.user_id
		WHERE a.account_id = ? ORDER BY u.email`), accountID)
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
