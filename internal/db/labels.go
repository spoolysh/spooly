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

// Label tags messages. System labels always exist and have no CreatedAt.
type Label struct {
	Name      string
	System    bool
	CreatedAt *time.Time
}

// System label names, in display order.
const (
	LabelInbox   = "inbox"
	LabelUnread  = "unread"
	LabelStarred = "starred"
	// LabelAnswered marks mail someone replied to, set by mail clients as
	// \Answered. Like unread, it's a state rather than a folder.
	LabelAnswered = "answered"
	LabelSent     = "sent" // mail sent from an account
	LabelDraft    = "draft"
	LabelSpam     = "spam"
	LabelTrash    = "trash"
)

// systemLabels follows Gmail's folder order: Inbox, Starred, Sent, Drafts,
// Spam, Trash, with unread after inbox and answered after starred.
var systemLabels = []string{LabelInbox, LabelUnread, LabelStarred, LabelAnswered, LabelSent, LabelDraft, LabelSpam, LabelTrash}

// IsSystemLabel reports whether name is one of the built-in labels.
func IsSystemLabel(name string) bool {
	return slices.Contains(systemLabels, name)
}

// SortLabels orders label names as they are listed: system labels first in
// their fixed order, then the rest by name.
func SortLabels(names []string) {
	slices.SortFunc(names, func(a, b string) int {
		ia, ib := slices.Index(systemLabels, a), slices.Index(systemLabels, b)
		switch {
		case ia >= 0 && ib >= 0:
			return ia - ib
		case ia >= 0:
			return -1
		case ib >= 0:
			return 1
		default:
			return strings.Compare(a, b)
		}
	})
}

var (
	// ErrLabelNotFound reports that no label has the requested name.
	ErrLabelNotFound = errors.New("label not found")
	// ErrLabelExists reports that a label with the name already exists.
	ErrLabelExists = errors.New("label already exists")
	// ErrSystemLabel reports an attempt to delete a system label.
	ErrSystemLabel = errors.New("system labels can't be deleted")
)

func (d *DB) CreateLabel(ctx context.Context, name string) (*Label, error) {
	if _, err := d.GetLabel(ctx, name); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrLabelExists, name)
	} else if !errors.Is(err, ErrLabelNotFound) {
		return nil, err
	}

	var createdAt time.Time
	query := d.q(`INSERT INTO labels (name, system, created_at) VALUES (?, FALSE, ?) RETURNING created_at`)
	if err := d.sql.QueryRowContext(ctx, query, name, time.Now().UTC()).Scan(&createdAt); err != nil {
		if _, gerr := d.GetLabel(ctx, name); gerr == nil {
			return nil, fmt.Errorf("%w: %s", ErrLabelExists, name)
		}
		return nil, fmt.Errorf("inserting label: %w", err)
	}
	return &Label{Name: name, CreatedAt: &createdAt}, nil
}

func (d *DB) GetLabel(ctx context.Context, name string) (*Label, error) {
	var l Label
	var createdAt sql.NullTime
	err := d.sql.QueryRowContext(ctx, d.q(`SELECT name, system, created_at FROM labels WHERE name = ?`), name).
		Scan(&l.Name, &l.System, &createdAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrLabelNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("getting label: %w", err)
	}
	if createdAt.Valid {
		l.CreatedAt = &createdAt.Time
	}
	return &l, nil
}

// ListLabels returns every label: system labels first, then user labels by
// name.
func (d *DB) ListLabels(ctx context.Context) ([]Label, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, system, created_at FROM labels`)
	if err != nil {
		return nil, fmt.Errorf("listing labels: %w", err)
	}
	defer rows.Close()

	byName := map[string]Label{}
	var names []string
	for rows.Next() {
		var l Label
		var createdAt sql.NullTime
		if err := rows.Scan(&l.Name, &l.System, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		if createdAt.Valid {
			l.CreatedAt = &createdAt.Time
		}
		byName[l.Name] = l
		names = append(names, l.Name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	SortLabels(names)
	labels := make([]Label, len(names))
	for i, name := range names {
		labels[i] = byName[name]
	}
	return labels, nil
}

// DeleteLabel deletes a user label and removes it from its messages.
func (d *DB) DeleteLabel(ctx context.Context, name string) error {
	l, err := d.GetLabel(ctx, name)
	if err != nil {
		return err
	}
	if l.System {
		return fmt.Errorf("%w: %s", ErrSystemLabel, name)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM message_labels WHERE label = ?`), name); err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	if _, err := tx.ExecContext(ctx, d.q(`DELETE FROM labels WHERE name = ?`), name); err != nil {
		return fmt.Errorf("deleting label: %w", err)
	}
	return tx.Commit()
}
