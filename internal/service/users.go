package service

import (
	"context"

	"github.com/spoolysh/spooly/internal/db"
)

// UserInfo is a user as admins see them.
type UserInfo struct {
	db.User
	Admin      bool
	AccountIDs []string // the accounts shared with them
	Keys       int      // how many API keys they have
}

// ListUsers returns everyone who has signed in, newest first. Admins only.
func (s *Service) ListUsers(ctx context.Context) ([]UserInfo, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, dbError(err)
	}
	access, err := s.db.AllAccess(ctx)
	if err != nil {
		return nil, dbError(err)
	}
	keys, err := s.db.CountAPIKeys(ctx)
	if err != nil {
		return nil, dbError(err)
	}
	infos := make([]UserInfo, len(users))
	for i, u := range users {
		infos[i] = UserInfo{User: u, Admin: s.IsAdmin(u.Email), AccountIDs: access[u.ID], Keys: keys[u.ID]}
	}
	return infos, nil
}

// RevokeKeys deletes all of a user's API keys, which also signs their mail
// clients out, and returns how many there were. Admins only.
func (s *Service) RevokeKeys(ctx context.Context, userID string) (int, error) {
	if err := s.requireUser(ctx, userID); err != nil {
		return 0, err
	}
	n, err := s.db.DeleteUserAPIKeys(ctx, userID)
	if err != nil {
		return 0, dbError(err)
	}
	return n, nil
}

// RemoveAccess takes a user off every account shared with them and returns
// how many there were. Admins only.
func (s *Service) RemoveAccess(ctx context.Context, userID string) (int, error) {
	if err := s.requireUser(ctx, userID); err != nil {
		return 0, err
	}
	n, err := s.db.RemoveAllAccess(ctx, userID)
	if err != nil {
		return 0, dbError(err)
	}
	return n, nil
}

// requireUser checks that the caller is an admin and that user userID
// exists.
func (s *Service) requireUser(ctx context.Context, userID string) error {
	if err := s.requireAdmin(ctx); err != nil {
		return err
	}
	if _, err := s.db.GetUserByID(ctx, userID); err != nil {
		return dbError(err)
	}
	return nil
}
