// Package service implements the Spooly API: AccountService, LabelService, and
// MessageService.
package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/gen/spooly/v1/spoolyv1connect"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/storage"
)

const defaultListLimit = 50

// Service implements all three Spooly services. The database holds everything
// but raw messages, which are in store.
type Service struct {
	db     *db.DB
	store  storage.Storage
	admins []string // lowercased emails
}

var (
	_ spoolyv1connect.AccountServiceHandler = (*Service)(nil)
	_ spoolyv1connect.LabelServiceHandler   = (*Service)(nil)
	_ spoolyv1connect.MessageServiceHandler = (*Service)(nil)
)

// Options configure a Service.
type Options struct {
	// Admins are the emails of users who can see and manage every account,
	// and create accounts.
	Admins []string
}

func New(database *db.DB, store storage.Storage, opts Options) *Service {
	s := &Service{db: database, store: store}
	for _, email := range opts.Admins {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			s.admins = append(s.admins, email)
		}
	}
	return s
}

// dbError maps a db-layer error to the matching connect code.
func dbError(err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, db.ErrAccountNotFound), errors.Is(err, db.ErrMessageNotFound), errors.Is(err, db.ErrLabelNotFound),
		errors.Is(err, db.ErrUserNotFound), errors.Is(err, storage.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, db.ErrAddressTaken), errors.Is(err, db.ErrLabelExists):
		code = connect.CodeAlreadyExists
	case errors.Is(err, db.ErrNoAddresses):
		code = connect.CodeInvalidArgument
	case errors.Is(err, db.ErrUnknownLabel), errors.Is(err, db.ErrUnknownFolder), errors.Is(err, db.ErrReservedLabel):
		code = connect.CodeInvalidArgument
	}
	return connect.NewError(code, err.Error()).WithCause(err)
}

func listLimit(limit *int32) int {
	if limit == nil {
		return defaultListLimit
	}
	return int(*limit)
}

// Accounts

func accountProto(a *db.Account) *spoolyv1.Account {
	return &spoolyv1.Account{Id: a.ID, Addresses: a.Addresses, Name: a.Name, CreatedAt: timestamppb.New(a.CreatedAt)}
}

// checkAddresses lowercases an account's addresses and rejects
// duplicates, and a "+", which separates a tag from the address it reaches,
// so it can't be part of an account's own address.
func checkAddresses(addresses []string) ([]string, error) {
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		address = strings.ToLower(address)
		if local, _, _ := strings.Cut(address, "@"); strings.Contains(local, "+") {
			return nil, connect.Errorf(connect.CodeInvalidArgument, `addresses: %s must not contain "+", which starts a tag`, address)
		}
		if slices.Contains(out, address) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "addresses: %s is listed twice", address)
		}
		out = append(out, address)
	}
	return out, nil
}

func (s *Service) CreateAccount(ctx context.Context, req *spoolyv1.CreateAccountRequest) (*spoolyv1.CreateAccountResponse, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	addresses, err := checkAddresses(req.Addresses)
	if err != nil {
		return nil, err
	}
	a, err := s.db.CreateAccount(ctx, db.Account{Addresses: addresses, Name: req.GetName()})
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.CreateAccountResponse{Account: accountProto(a)}, nil
}

func (s *Service) ListAccounts(ctx context.Context, req *spoolyv1.ListAccountsRequest) (*spoolyv1.ListAccountsResponse, error) {
	userID, admin, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	f := db.AccountFilter{After: req.GetAfter(), Limit: listLimit(req.Limit)}
	if !admin {
		f.UserID = userID
	}
	accounts, err := s.db.ListAccounts(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	resp := &spoolyv1.ListAccountsResponse{Accounts: make([]*spoolyv1.Account, 0, len(accounts))}
	for i := range accounts {
		resp.Accounts = append(resp.Accounts, accountProto(&accounts[i]))
	}
	return resp, nil
}

func (s *Service) GetAccount(ctx context.Context, req *spoolyv1.GetAccountRequest) (*spoolyv1.GetAccountResponse, error) {
	a, _, err := s.resolveAccount(ctx, req.IdOrAddress)
	if err != nil {
		return nil, err
	}
	if err := s.Require(ctx, a.ID); err != nil {
		return nil, err
	}
	return &spoolyv1.GetAccountResponse{Account: accountProto(a)}, nil
}

func (s *Service) UpdateAccount(ctx context.Context, req *spoolyv1.UpdateAccountRequest) (*spoolyv1.UpdateAccountResponse, error) {
	a, _, err := s.resolveAccount(ctx, req.IdOrAddress)
	if err != nil {
		return nil, err
	}
	if err := s.requireManage(ctx, a.ID); err != nil {
		return nil, err
	}
	u := db.AccountUpdate{Name: req.Name}
	if len(req.Addresses) > 0 {
		if u.Addresses, err = checkAddresses(req.Addresses); err != nil {
			return nil, err
		}
	}
	a, err = s.db.UpdateAccount(ctx, a.ID, u)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.UpdateAccountResponse{Account: accountProto(a)}, nil
}

// resolveAccount finds an account by ID, or by address if idOrAddress contains
// "@", and returns it with the address mail to it was sent to: the given
// address, lowercased, or the account's first address for an ID.
func (s *Service) resolveAccount(ctx context.Context, idOrAddress string) (*db.Account, string, error) {
	var a *db.Account
	var err error
	recipient := strings.ToLower(idOrAddress)
	if strings.Contains(idOrAddress, "@") {
		a, err = s.db.ResolveAccount(ctx, recipient)
	} else {
		a, err = s.db.GetAccount(ctx, idOrAddress)
		if err == nil {
			recipient = a.Address()
		}
	}
	if err != nil {
		return nil, "", dbError(err)
	}
	return a, recipient, nil
}

// Labels

func labelProto(l *db.Label) *spoolyv1.Label {
	return &spoolyv1.Label{Name: l.Name, CreatedAt: timestamppb.New(l.CreatedAt)}
}

// CreateLabel is for admins only: a label is shared by every account.
func (s *Service) CreateLabel(ctx context.Context, req *spoolyv1.CreateLabelRequest) (*spoolyv1.CreateLabelResponse, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	l, err := s.db.CreateLabel(ctx, req.Name)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.CreateLabelResponse{Label: labelProto(l)}, nil
}

func (s *Service) ListLabels(ctx context.Context, _ *spoolyv1.ListLabelsRequest) (*spoolyv1.ListLabelsResponse, error) {
	if _, _, err := s.caller(ctx); err != nil {
		return nil, err
	}
	labels, err := s.db.ListLabels(ctx)
	if err != nil {
		return nil, dbError(err)
	}
	resp := &spoolyv1.ListLabelsResponse{Labels: make([]*spoolyv1.Label, 0, len(labels))}
	for i := range labels {
		resp.Labels = append(resp.Labels, labelProto(&labels[i]))
	}
	return resp, nil
}

// DeleteLabel is for admins only: a label is shared by all accounts, so
// deleting it takes it off messages in accounts others can't see.
func (s *Service) DeleteLabel(ctx context.Context, req *spoolyv1.DeleteLabelRequest) (*spoolyv1.DeleteLabelResponse, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.db.DeleteLabel(ctx, req.Name); err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.DeleteLabelResponse{}, nil
}
