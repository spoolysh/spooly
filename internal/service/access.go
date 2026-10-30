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
// account, and create and manage accounts, labels, and users.
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

// Require checks that ctx's user has access to an account: they're an
// admin, or it's shared with them. Without access, the account is reported
// as not found, so its existence isn't revealed.
func (s *Service) Require(ctx context.Context, accountID string) error {
	userID, admin, err := s.caller(ctx)
	if err != nil || admin {
		return err
	}
	ok, err := s.db.HasAccess(ctx, accountID, userID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return connect.Errorf(connect.CodeNotFound, "%s: %s", db.ErrAccountNotFound, accountID)
	}
	return nil
}

// VisibleAccounts returns the IDs of the accounts ctx's user has access to,
// or nil for an admin, who has all.
func (s *Service) VisibleAccounts(ctx context.Context) ([]string, error) {
	userID, admin, err := s.caller(ctx)
	if err != nil || admin {
		return nil, err
	}
	ids, err := s.db.AccountIDsFor(ctx, userID)
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

// requireManage checks that the caller may manage an account: its
// addresses, name, and who it's shared with. That's admins only; others get
// the same not found as for an account they can't see, or permission
// denied for one they can.
func (s *Service) requireManage(ctx context.Context, accountID string) error {
	if err := s.Require(ctx, accountID); err != nil {
		return err
	}
	return s.requireAdmin(ctx)
}

// Share gives the user who signs in with email access to an account.
// Admins only; the user must have signed in once.
func (s *Service) Share(ctx context.Context, accountID, email string) error {
	if err := s.requireManage(ctx, accountID); err != nil {
		return err
	}
	u, err := s.db.GetUserByEmail(ctx, strings.TrimSpace(email))
	if errors.Is(err, db.ErrUserNotFound) {
		return connect.Errorf(connect.CodeNotFound, "no user signs in as %s: they need to sign in to Spooly once first", email)
	}
	if err != nil {
		return dbError(err)
	}
	if err := s.db.AddUser(ctx, accountID, u.ID); err != nil {
		return dbError(err)
	}
	return nil
}

// Unshare takes a user's access to an account away. Admins only.
func (s *Service) Unshare(ctx context.Context, accountID, userID string) error {
	if err := s.requireManage(ctx, accountID); err != nil {
		return err
	}
	if err := s.db.RemoveUser(ctx, accountID, userID); err != nil {
		return dbError(err)
	}
	return nil
}

// AccountUsers returns who an account is shared with. Admins aren't listed:
// they reach every account. Admins only.
func (s *Service) AccountUsers(ctx context.Context, accountID string) ([]db.User, error) {
	if err := s.requireManage(ctx, accountID); err != nil {
		return nil, err
	}
	users, err := s.db.AccountUsers(ctx, accountID)
	if err != nil {
		return nil, dbError(err)
	}
	return users, nil
}

// ErrLoginFailed reports a mail client login that didn't work, for any
// reason, so it doesn't reveal which accounts exist.
var ErrLoginFailed = errors.New("invalid address or API key")

// LoginMail authenticates a mail client (IMAP, POP3): an account's address
// and an API key of a user with access to it. It returns the account and a
// context acting as the user, for the session's service calls.
func (s *Service) LoginMail(ctx context.Context, address, key string) (*db.Account, context.Context, error) {
	k, err := s.db.LookupAPIKey(ctx, key)
	if err != nil {
		return nil, nil, ErrLoginFailed
	}
	a, err := s.db.ResolveAccount(ctx, strings.ToLower(strings.TrimSpace(address)))
	if err != nil {
		return nil, nil, ErrLoginFailed
	}
	userCtx := auth.ContextWithUserID(context.Background(), k.UserID)
	if err := s.Require(userCtx, a.ID); err != nil {
		return nil, nil, ErrLoginFailed
	}
	return a, userCtx, nil
}
