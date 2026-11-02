package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Folder names. A message is in exactly one folder: where it is.
const (
	FolderInbox   = "inbox"
	FolderArchive = "archive"
	FolderSent    = "sent" // mail sent from a mailbox
	FolderDrafts  = "drafts"
	FolderJunk    = "junk"
	FolderTrash   = "trash"
)

// Folders lists every folder, in display order, as in mail clients.
var Folders = []string{FolderInbox, FolderArchive, FolderSent, FolderDrafts, FolderJunk, FolderTrash}

// Unlisted are the folders whose mail shows only in them, as in Gmail: not
// in All Mail, nor under its labels.
var Unlisted = []string{FolderJunk, FolderTrash}

// IsFolder reports whether name is a folder.
func IsFolder(name string) bool {
	return slices.Contains(Folders, name)
}

// Label says what a message is about, such as billing; a message can have
// several, and keeps them in any folder.
type Label struct {
	Name      string
	CreatedAt time.Time
}

// reservedLabels are names no label can take: the folders and the flags,
// and their everyday names, spam, unread, and starred, so a label never
// reads like a place or a state.
var reservedLabels = append(slices.Clone(Folders), "spam", "seen", "unread", "answered", "flagged", "starred", "draft", "forwarded")

var (
	// ErrLabelNotFound reports that no label has the requested name.
	ErrLabelNotFound = errors.New("label not found")
	// ErrLabelExists reports that a label with the name already exists.
	ErrLabelExists = errors.New("label already exists")
	// ErrReservedLabel reports a label name that reads like a folder or a
	// flag.
	ErrReservedLabel = errors.New("reserved for folders and flags")
)

func (d *DB) CreateLabel(ctx context.Context, name string) (*Label, error) {
	if slices.Contains(reservedLabels, name) {
		return nil, fmt.Errorf("%w: %s", ErrReservedLabel, name)
	}
	if _, err := d.GetLabel(ctx, name); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrLabelExists, name)
	} else if !errors.Is(err, ErrLabelNotFound) {
		return nil, err
	}

	var createdAt time.Time
	query := d.q(`INSERT INTO labels (name, created_at) VALUES (?, ?) RETURNING created_at`)
	if err := d.sql.QueryRowContext(ctx, query, name, time.Now().UTC()).Scan(&createdAt); err != nil {
		if _, gerr := d.GetLabel(ctx, name); gerr == nil {
			return nil, fmt.Errorf("%w: %s", ErrLabelExists, name)
		}
		return nil, fmt.Errorf("inserting label: %w", err)
	}
	return &Label{Name: name, CreatedAt: createdAt}, nil
}

func (d *DB) GetLabel(ctx context.Context, name string) (*Label, error) {
	var l Label
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT name, created_at FROM labels WHERE name = ?`), name).
		Scan(&l.Name, &l.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrLabelNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("getting label: %w", err)
	}
	return &l, nil
}

// ListLabels returns every label, by name.
func (d *DB) ListLabels(ctx context.Context) ([]Label, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, created_at FROM labels ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing labels: %w", err)
	}
	defer rows.Close()

	var labels []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.Name, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		labels = append(labels, l)
	}
	return labels, rows.Err()
}

// DeleteLabel deletes a label and removes it from its messages, which
// changes them.
func (d *DB) DeleteLabel(ctx context.Context, name string) error {
	if _, err := d.GetLabel(ctx, name); err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, d.q(`SELECT m.id, m.mailbox_id FROM message_labels l JOIN messages m ON m.id = l.message_id WHERE l.label = ?`), name)
	if err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	type labeled struct{ id, mailboxID string }
	var messages []labeled
	for rows.Next() {
		var m labeled
		if err := rows.Scan(&m.id, &m.mailboxID); err != nil {
			rows.Close()
			return fmt.Errorf("deleting label: %w", err)
		}
		messages = append(messages, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	for _, m := range messages {
		c := &change{d: d, tx: tx, mailboxID: m.mailboxID}
		if err := d.touch(ctx, c, m.id, &MessageChanges{RemoveLabels: []string{name}}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM message_labels WHERE label = ?`), name); err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM labels WHERE name = ?`), name); err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	return tx.Commit()
}
