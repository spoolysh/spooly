// Package service implements the Spooly API: MailboxService, LabelService,
// MessageService, and the alpha ChangeService and DeliveryService.
package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/gen/spooly/v1/spoolyv1connect"
	"github.com/spoolysh/spooly/gen/spooly/v1alpha1/spoolyv1alpha1connect"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
	"github.com/spoolysh/spooly/internal/storage"
)

const defaultListLimit = 50

// Service implements all three Spooly services. The database holds everything
// but raw messages, which are in store.
type Service struct {
	db     *db.DB
	store  storage.Storage
	admins []string // lowercased emails
	opts   Options
	spool  *delivery.Spool // nil without a transport: sending is off
}

var (
	_ spoolyv1connect.MailboxServiceHandler = (*Service)(nil)
	_ spoolyv1connect.LabelServiceHandler   = (*Service)(nil)
	_ spoolyv1connect.MessageServiceHandler = (*Service)(nil)

	_ spoolyv1alpha1connect.MessageServiceHandler  = (*Service)(nil)
	_ spoolyv1alpha1connect.ChangeServiceHandler   = (*Service)(nil)
	_ spoolyv1alpha1connect.DeliveryServiceHandler = (*Service)(nil)
)

// Options configure a Service.
type Options struct {
	// Admins are the emails of users who can see and manage every mailbox,
	// and create mailboxes.
	Admins []string
	// ExpungeAfter is how long a message deleted for good can still be
	// restored, its raw source kept; 0 removes it at once.
	ExpungeAfter time.Duration
	// EventsRetention is how long changes are kept for lists of changes; it
	// must be at least ExpungeAfter.
	EventsRetention time.Duration
	// Transport sends mail; nil turns sending off.
	Transport delivery.Transport
}

func New(database *db.DB, store storage.Storage, opts Options) *Service {
	s := &Service{db: database, store: store, opts: opts}
	if opts.Transport != nil {
		s.spool = delivery.NewSpool(database, store, opts.Transport)
	}
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
	case errors.Is(err, db.ErrMailboxNotFound), errors.Is(err, db.ErrMessageNotFound), errors.Is(err, db.ErrLabelNotFound),
		errors.Is(err, db.ErrNotRestorable), errors.Is(err, db.ErrDeliveryNotFound),
		errors.Is(err, db.ErrUserNotFound), errors.Is(err, storage.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, db.ErrAddressTaken), errors.Is(err, db.ErrLabelExists):
		code = connect.CodeAlreadyExists
	case errors.Is(err, db.ErrNoAddresses):
		code = connect.CodeInvalidArgument
	case errors.Is(err, db.ErrUnknownLabel), errors.Is(err, db.ErrUnknownFolder), errors.Is(err, db.ErrReservedLabel):
		code = connect.CodeInvalidArgument
	case errors.Is(err, db.ErrChangesPruned), errors.Is(err, db.ErrNotRetryable):
		code = connect.CodeFailedPrecondition
	}
	return connect.NewError(code, err.Error()).WithCause(err)
}

func listLimit(limit *int32) int {
	if limit == nil {
		return defaultListLimit
	}
	return int(*limit)
}

// Mailboxes

func mailboxProto(a *db.Mailbox) *spoolyv1.Mailbox {
	return &spoolyv1.Mailbox{Id: a.ID, Addresses: a.Addresses, Name: a.Name, CreatedAt: timestamppb.New(a.CreatedAt)}
}

// checkAddresses lowercases a mailbox's addresses and rejects
// duplicates, and a "+", which separates a tag from the address it reaches,
// so it can't be part of a mailbox's own address.
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

func (s *Service) CreateMailbox(ctx context.Context, req *spoolyv1.CreateMailboxRequest) (*spoolyv1.CreateMailboxResponse, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	addresses, err := checkAddresses(req.Addresses)
	if err != nil {
		return nil, err
	}
	a, err := s.db.CreateMailbox(ctx, db.Mailbox{Addresses: addresses, Name: req.GetName()})
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.CreateMailboxResponse{Mailbox: mailboxProto(a)}, nil
}

func (s *Service) ListMailboxes(ctx context.Context, req *spoolyv1.ListMailboxesRequest) (*spoolyv1.ListMailboxesResponse, error) {
	userID, admin, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	f := db.MailboxFilter{After: req.GetAfter(), Limit: listLimit(req.Limit)}
	if !admin {
		f.UserID = userID
	}
	mailboxes, err := s.db.ListMailboxes(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	resp := &spoolyv1.ListMailboxesResponse{Mailboxes: make([]*spoolyv1.Mailbox, 0, len(mailboxes))}
	for i := range mailboxes {
		resp.Mailboxes = append(resp.Mailboxes, mailboxProto(&mailboxes[i]))
	}
	return resp, nil
}

func (s *Service) GetMailbox(ctx context.Context, req *spoolyv1.GetMailboxRequest) (*spoolyv1.GetMailboxResponse, error) {
	a, _, err := s.resolveMailbox(ctx, req.IdOrAddress)
	if err != nil {
		return nil, err
	}
	if err := s.Require(ctx, a.ID); err != nil {
		return nil, err
	}
	return &spoolyv1.GetMailboxResponse{Mailbox: mailboxProto(a)}, nil
}

func (s *Service) UpdateMailbox(ctx context.Context, req *spoolyv1.UpdateMailboxRequest) (*spoolyv1.UpdateMailboxResponse, error) {
	a, _, err := s.resolveMailbox(ctx, req.IdOrAddress)
	if err != nil {
		return nil, err
	}
	if err := s.requireManage(ctx, a.ID); err != nil {
		return nil, err
	}
	u := db.MailboxUpdate{Name: req.Name}
	if len(req.Addresses) > 0 {
		if u.Addresses, err = checkAddresses(req.Addresses); err != nil {
			return nil, err
		}
	}
	a, err = s.db.UpdateMailbox(ctx, a.ID, u)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.UpdateMailboxResponse{Mailbox: mailboxProto(a)}, nil
}

// resolveMailbox finds a mailbox by ID, or by address if idOrAddress contains
// "@", and returns it with the address mail to it was sent to: the given
// address, lowercased, or the mailbox's first address for an ID.
func (s *Service) resolveMailbox(ctx context.Context, idOrAddress string) (*db.Mailbox, string, error) {
	var a *db.Mailbox
	var err error
	recipient := strings.ToLower(idOrAddress)
	if strings.Contains(idOrAddress, "@") {
		a, err = s.db.ResolveMailbox(ctx, recipient)
	} else {
		a, err = s.db.GetMailbox(ctx, idOrAddress)
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

// CreateLabel is for admins only: a label is shared by every mailbox.
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

// DeleteLabel is for admins only: a label is shared by all mailboxes, so
// deleting it takes it off messages in mailboxes others can't see.
func (s *Service) DeleteLabel(ctx context.Context, req *spoolyv1.DeleteLabelRequest) (*spoolyv1.DeleteLabelResponse, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.db.DeleteLabel(ctx, req.Name); err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.DeleteLabelResponse{}, nil
}
