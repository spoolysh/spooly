package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account receives all mail sent to its address.
type Account struct {
	ID        string
	Addr      string // lowercased
	Name      string // empty when not set
	CreatedAt time.Time
}

var (
	// ErrAccountNotFound reports that no account has the requested ID or address.
	ErrAccountNotFound = errors.New("account not found")
	// ErrAccountExists reports that another account already has the address.
	ErrAccountExists = errors.New("account already exists")
)

const accountColumns = `id, addr, name, created_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.Addr, &a.Name, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAccount inserts a and returns it with its creation time set.
func (d *DB) CreateAccount(ctx context.Context, a Account) (*Account, error) {
	if _, err := d.GetAccountByAddr(ctx, a.Addr); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAccountExists, a.Addr)
	} else if !errors.Is(err, ErrAccountNotFound) {
		return nil, err
	}

	query := d.q(`INSERT INTO accounts (` + accountColumns + `) VALUES (?, ?, ?, ?) RETURNING created_at`)
	if err := d.sql.QueryRowContext(ctx, query, a.ID, a.Addr, a.Name, time.Now().UTC()).Scan(&a.CreatedAt); err != nil {
		// Lost a race with another insert of the same address.
		if _, gerr := d.GetAccountByAddr(ctx, a.Addr); gerr == nil {
			return nil, fmt.Errorf("%w: %s", ErrAccountExists, a.Addr)
		}
		return nil, fmt.Errorf("inserting account: %w", err)
	}
	return &a, nil
}

func (d *DB) GetAccount(ctx context.Context, id string) (*Account, error) {
	return d.getAccount(ctx, "id", id)
}

// GetAccountByAddr finds the account with exactly addr, which must already be
// lowercased.
func (d *DB) GetAccountByAddr(ctx context.Context, addr string) (*Account, error) {
	return d.getAccount(ctx, "addr", addr)
}

func (d *DB) getAccount(ctx context.Context, column, value string) (*Account, error) {
	query := d.q(`SELECT ` + accountColumns + ` FROM accounts WHERE ` + column + ` = ?`)
	a, err := scanAccount(d.sql.QueryRowContext(ctx, query, value))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, value)
	}
	if err != nil {
		return nil, fmt.Errorf("getting account: %w", err)
	}
	return a, nil
}

// ListAccounts returns up to limit accounts, newest first by ID. A non-empty
// after returns only accounts with smaller IDs.
func (d *DB) ListAccounts(ctx context.Context, limit int, after string) ([]Account, error) {
	query := `SELECT ` + accountColumns + ` FROM accounts`
	var args []any
	if after != "" {
		query += ` WHERE id < ?`
		args = append(args, after)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, d.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		accounts = append(accounts, *a)
	}
	return accounts, rows.Err()
}

// ResolveAccount finds the account for an address, as mail delivered to it
// would: an exact match first, then "local+tag@domain" as "local@domain".
// addr must already be lowercased.
func (d *DB) ResolveAccount(ctx context.Context, addr string) (*Account, error) {
	a, err := d.GetAccountByAddr(ctx, addr)
	if !errors.Is(err, ErrAccountNotFound) {
		return a, err
	}
	local, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return nil, err
	}
	base, _, tagged := strings.Cut(local, "+")
	if !tagged || base == "" {
		return nil, err
	}
	return d.GetAccountByAddr(ctx, base+"@"+domain)
}
