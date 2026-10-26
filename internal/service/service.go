// Package service implements the Boxy API: AccountService, LabelService, and
// MessageService.
package service

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/gen/boxy/v1/boxyv1connect"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/storage"
)

const defaultListLimit = 50

// Service implements all three Boxy services. The database holds everything
// but raw messages, which are in store.
type Service struct {
	db    *db.DB
	store storage.Storage
}

var (
	_ boxyv1connect.AccountServiceHandler = (*Service)(nil)
	_ boxyv1connect.LabelServiceHandler   = (*Service)(nil)
	_ boxyv1connect.MessageServiceHandler = (*Service)(nil)
)

func New(database *db.DB, store storage.Storage) *Service {
	return &Service{db: database, store: store}
}

// dbError maps a db-layer error to the matching connect code.
func dbError(err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, db.ErrAccountNotFound), errors.Is(err, db.ErrMessageNotFound), errors.Is(err, db.ErrLabelNotFound),
		errors.Is(err, storage.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, db.ErrAccountExists), errors.Is(err, db.ErrLabelExists):
		code = connect.CodeAlreadyExists
	case errors.Is(err, db.ErrSystemLabel):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, db.ErrUnknownLabel), errors.Is(err, db.ErrMetadataLimit):
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

func accountProto(a *db.Account) *boxyv1.Account {
	return &boxyv1.Account{Id: a.ID, Addr: a.Addr, Name: a.Name, CreatedAt: timestamppb.New(a.CreatedAt)}
}

func (s *Service) CreateAccount(ctx context.Context, req *boxyv1.CreateAccountRequest) (*boxyv1.CreateAccountResponse, error) {
	addr := strings.ToLower(req.Addr)
	// "+" separates a tag from the address it reaches, so it can't be part
	// of an account's own address.
	if local, _, _ := strings.Cut(addr, "@"); strings.Contains(local, "+") {
		return nil, connect.Errorf(connect.CodeInvalidArgument, `addr: must not contain "+", which starts a tag`)
	}
	a, err := s.db.CreateAccount(ctx, db.Account{ID: db.NewID(), Addr: addr, Name: req.GetName()})
	if err != nil {
		return nil, dbError(err)
	}
	return &boxyv1.CreateAccountResponse{Account: accountProto(a)}, nil
}

func (s *Service) ListAccounts(ctx context.Context, req *boxyv1.ListAccountsRequest) (*boxyv1.ListAccountsResponse, error) {
	accounts, err := s.db.ListAccounts(ctx, listLimit(req.Limit), req.GetAfter())
	if err != nil {
		return nil, dbError(err)
	}
	resp := &boxyv1.ListAccountsResponse{Accounts: make([]*boxyv1.Account, 0, len(accounts))}
	for i := range accounts {
		resp.Accounts = append(resp.Accounts, accountProto(&accounts[i]))
	}
	return resp, nil
}

func (s *Service) GetAccount(ctx context.Context, req *boxyv1.GetAccountRequest) (*boxyv1.GetAccountResponse, error) {
	a, _, err := s.resolveAccount(ctx, req.IdOrAddr)
	if err != nil {
		return nil, err
	}
	return &boxyv1.GetAccountResponse{Account: accountProto(a)}, nil
}

// resolveAccount finds an account by ID, or by address if idOrAddr contains
// "@", and returns it with the address mail to it was sent to: the given
// address, lowercased, or the account's own address for an ID.
func (s *Service) resolveAccount(ctx context.Context, idOrAddr string) (*db.Account, string, error) {
	var a *db.Account
	var err error
	recipient := strings.ToLower(idOrAddr)
	if strings.Contains(idOrAddr, "@") {
		a, err = s.db.ResolveAccount(ctx, recipient)
	} else {
		a, err = s.db.GetAccount(ctx, idOrAddr)
		if err == nil {
			recipient = a.Addr
		}
	}
	if err != nil {
		return nil, "", dbError(err)
	}
	return a, recipient, nil
}

// Labels

func labelProto(l *db.Label) *boxyv1.Label {
	p := &boxyv1.Label{Name: l.Name, System: l.System}
	if l.CreatedAt != nil {
		p.CreatedAt = timestamppb.New(*l.CreatedAt)
	}
	return p
}

func (s *Service) CreateLabel(ctx context.Context, req *boxyv1.CreateLabelRequest) (*boxyv1.CreateLabelResponse, error) {
	l, err := s.db.CreateLabel(ctx, req.Name)
	if err != nil {
		return nil, dbError(err)
	}
	return &boxyv1.CreateLabelResponse{Label: labelProto(l)}, nil
}

func (s *Service) ListLabels(ctx context.Context, _ *boxyv1.ListLabelsRequest) (*boxyv1.ListLabelsResponse, error) {
	labels, err := s.db.ListLabels(ctx)
	if err != nil {
		return nil, dbError(err)
	}
	resp := &boxyv1.ListLabelsResponse{Labels: make([]*boxyv1.Label, 0, len(labels))}
	for i := range labels {
		resp.Labels = append(resp.Labels, labelProto(&labels[i]))
	}
	return resp, nil
}

func (s *Service) DeleteLabel(ctx context.Context, req *boxyv1.DeleteLabelRequest) (*boxyv1.DeleteLabelResponse, error) {
	if err := s.db.DeleteLabel(ctx, req.Name); err != nil {
		return nil, dbError(err)
	}
	return &boxyv1.DeleteLabelResponse{}, nil
}
