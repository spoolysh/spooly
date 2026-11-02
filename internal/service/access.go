package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/spoolysh/spooly/internal/auth"
	"github.com/spoolysh/spooly/internal/db"
)

// IsAdmin reports whether email belongs to an admin: they see every
// mailbox, and create and manage mailboxes, labels, and users.
func (s *Service) IsAdmin(email string) bool {
	return slices.Contains(s.admins, strings.ToLower(email))
}

// caller returns the user ctx acts for, and whether they're an admin.
func (s *Service) caller(ctx context.Context) (userID string, admin bool, err error) {
	userID, ok := auth.UserIDFromContext(ctx)
	if !ok {
		return "", false, connect.Errorf(connect.CodeUnauthenticated, "authentication required")
	}
	u, err := s.db.GetUserByID(ctx, userID)
	if err != nil {
		return "", false, connect.Errorf(connect.CodeUnauthenticated, "authentication required")
	}
	return u.ID, s.IsAdmin(u.Email), nil
}

// Require checks that ctx's user has access to a mailbox: they're an
// admin, or it's shared with them. Without access, the mailbox is reported
// as not found, so its existence isn't revealed.
func (s *Service) Require(ctx context.Context, mailboxID string) error {
	userID, admin, err := s.caller(ctx)
	if err != nil || admin {
		return err
	}
	ok, err := s.db.HasAccess(ctx, mailboxID, userID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return connect.Errorf(connect.CodeNotFound, "%s: %s", db.ErrMailboxNotFound, mailboxID)
	}
	return nil
}

// VisibleMailboxes returns the IDs of the mailboxes ctx's user has access to,
// or nil for an admin, who has all.
func (s *Service) VisibleMailboxes(ctx context.Context) ([]string, error) {
	userID, admin, err := s.caller(ctx)
	if err != nil || admin {
		return nil, err
	}
	ids, err := s.db.MailboxIDsFor(ctx, userID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return ids, nil
}

func (s *Service) requireAdmin(ctx context.Context) error {
	_, admin, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if !admin {
		return connect.Errorf(connect.CodePermissionDenied, "only admins can do this")
	}
	return nil
}

// requireManage checks that the caller may manage a mailbox: its
// addresses, name, and who it's shared with. That's admins only; others get
// the same not found as for a mailbox they can't see, or permission
// denied for one they can.
func (s *Service) requireManage(ctx context.Context, mailboxID string) error {
	if err := s.Require(ctx, mailboxID); err != nil {
		return err
	}
	return s.requireAdmin(ctx)
}

// Share gives the user who signs in with email access to a mailbox.
// Admins only; the user must have signed in once.
func (s *Service) Share(ctx context.Context, mailboxID, email string) error {
	if err := s.requireManage(ctx, mailboxID); err != nil {
		return err
	}
	u, err := s.db.GetUserByEmail(ctx, strings.TrimSpace(email))
	if errors.Is(err, db.ErrUserNotFound) {
		return connect.Errorf(connect.CodeNotFound, "no user signs in as %s: they need to sign in to Spooly once first", email)
	}
	if err != nil {
		return dbError(err)
	}
	if err := s.db.AddUser(ctx, mailboxID, u.ID); err != nil {
		return dbError(err)
	}
	return nil
}

// Unshare takes a user's access to a mailbox away. Admins only.
func (s *Service) Unshare(ctx context.Context, mailboxID, userID string) error {
	if err := s.requireManage(ctx, mailboxID); err != nil {
		return err
	}
	if err := s.db.RemoveUser(ctx, mailboxID, userID); err != nil {
		return dbError(err)
	}
	return nil
}

// MailboxUsers returns who a mailbox is shared with. Admins aren't listed:
// they reach every mailbox. Admins only.
func (s *Service) MailboxUsers(ctx context.Context, mailboxID string) ([]db.User, error) {
	if err := s.requireManage(ctx, mailboxID); err != nil {
		return nil, err
	}
	users, err := s.db.MailboxUsers(ctx, mailboxID)
	if err != nil {
		return nil, dbError(err)
	}
	return users, nil
}
