package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account receives all mail sent to any of its addresses.
type Account struct {
	ID        string
	Addresses []string // lowercased, in order; the first is shown for the account
	Name      string   // empty when not set
	CreatedAt time.Time
}

// Address is the address shown for the account: its first.
func (a Account) Address() string {
	if len(a.Addresses) == 0 {
		return ""
	}
	return a.Addresses[0]
}

var (
	// ErrAccountNotFound reports that no account has the requested ID or address.
	ErrAccountNotFound = errors.New("account not found")
	// ErrAddressTaken reports that an address already belongs to another
	// account.
	ErrAddressTaken = errors.New("address already belongs to an account")
	// ErrNoAddresses reports an account left without addresses.
	ErrNoAddresses = errors.New("an account needs at least one address")
)

const accountColumns = `id, name, created_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAccount inserts a with its addresses, and returns it with its ID,
// which the database assigns, and creation time set.
func (d *DB) CreateAccount(ctx context.Context, a Account) (*Account, error) {
	if len(a.Addresses) == 0 {
		return nil, ErrNoAddresses
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("inserting account: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, d.q(`INSERT INTO accounts (name, created_at) VALUES (?, ?) RETURNING id, created_at`),
		a.Name, time.Now().UTC()).Scan(&a.ID, &a.CreatedAt); err != nil {
		return nil, fmt.Errorf("inserting account: %w", err)
	}
	if err := d.setAddresses(ctx, tx, a.ID, a.Addresses); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("inserting account: %w", err)
	}
	return &a, nil
}

// setAddresses replaces an account's addresses with addresses, in order.
// Each must not belong to another account.
func (d *DB) setAddresses(ctx context.Context, tx *sql.Tx, accountID string, addresses []string) error {
	if len(addresses) == 0 {
		return ErrNoAddresses
	}
	for _, address := range addresses {
		var owner string
		err := tx.QueryRowContext(ctx, d.q(`SELECT account_id FROM account_addresses WHERE address = ?`), address).Scan(&owner)
		if err == nil && owner != accountID {
			return fmt.Errorf("%w: %s", ErrAddressTaken, address)
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("checking address: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM account_addresses WHERE account_id = ?`), accountID); err != nil {
		return fmt.Errorf("replacing addresses: %w", err)
	}
	for i, address := range addresses {
		if _, err := tx.ExecContext(ctx, d.q(`INSERT INTO account_addresses (address, account_id, position) VALUES (?, ?, ?)`),
			address, accountID, i); err != nil {
			// Lost a race with another account taking the address.
			return fmt.Errorf("%w: %s", ErrAddressTaken, address)
		}
	}
	return nil
}

func (d *DB) GetAccount(ctx context.Context, id string) (*Account, error) {
	a, err := scanAccount(d.sql.QueryRowContext(ctx, d.q(`SELECT `+accountColumns+` FROM accounts WHERE id = ?`), id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting account: %w", err)
	}
	if err := d.loadAddresses(ctx, []*Account{a}); err != nil {
		return nil, err
	}
	return a, nil
}

// GetAccountByAddress finds the account with exactly address among its
// addresses. address must already be lowercased.
func (d *DB) GetAccountByAddress(ctx context.Context, address string) (*Account, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT account_id FROM account_addresses WHERE address = ?`), address).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, address)
	}
	if err != nil {
		return nil, fmt.Errorf("getting address: %w", err)
	}
	return d.GetAccount(ctx, id)
}

// loadAddresses sets the addresses of accounts, in order.
func (d *DB) loadAddresses(ctx context.Context, accounts []*Account) error {
	if len(accounts) == 0 {
		return nil
	}
	byID := make(map[string]*Account, len(accounts))
	args := make([]any, len(accounts))
	for i, a := range accounts {
		a.Addresses = []string{}
		byID[a.ID] = a
		args[i] = a.ID
	}
	rows, err := d.sql.QueryContext(ctx, d.q(`SELECT account_id, address FROM account_addresses
		WHERE account_id IN (`+placeholders(len(args))+`) ORDER BY position`), args...)
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

// AccountFilter selects accounts for ListAccounts.
type AccountFilter struct {
	// UserID limits the list to accounts shared with that user; empty means
	// every account.
	UserID string
	After  string // only accounts with smaller IDs
	Limit  int
}

// ListAccounts returns accounts matching f, newest first by ID.
func (d *DB) ListAccounts(ctx context.Context, f AccountFilter) ([]Account, error) {
	query := `SELECT ` + accountColumns + ` FROM accounts WHERE 1 = 1`
	var args []any
	if f.UserID != "" {
		query += ` AND id IN (SELECT account_id FROM account_users WHERE user_id = ?)`
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
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	var list []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		list = append(list, a)
	}
	rows.Close() // before loading addresses: SQLite runs on one connection
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	if err := d.loadAddresses(ctx, list); err != nil {
		return nil, err
	}
	accounts := make([]Account, len(list))
	for i, a := range list {
		accounts[i] = *a
	}
	return accounts, nil
}

// ResolveAccount finds the account for an address, as mail delivered to it
// would: an exact match first, then "local+tag@domain" as "local@domain".
// address must already be lowercased.
func (d *DB) ResolveAccount(ctx context.Context, address string) (*Account, error) {
	a, err := d.GetAccountByAddress(ctx, address)
	if !errors.Is(err, ErrAccountNotFound) {
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
	return d.GetAccountByAddress(ctx, base+"@"+domain)
}

// AccountUpdate changes an account. Nil fields are left unchanged.
type AccountUpdate struct {
	Name *string
	// Addresses, if not nil, replaces the account's addresses, in order.
	Addresses []string
}

// UpdateAccount applies u to account id and returns the updated account.
func (d *DB) UpdateAccount(ctx context.Context, id string, u AccountUpdate) (*Account, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("updating account: %w", err)
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, d.q(`SELECT 1 FROM accounts WHERE id = ?`), id).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting account: %w", err)
	}
	if u.Name != nil {
		if _, err := tx.ExecContext(ctx, d.q(`UPDATE accounts SET name = ? WHERE id = ?`), *u.Name, id); err != nil {
			return nil, fmt.Errorf("renaming account: %w", err)
		}
	}
	if u.Addresses != nil {
		if err := d.setAddresses(ctx, tx, id, u.Addresses); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("updating account: %w", err)
	}
	return d.GetAccount(ctx, id)
}
