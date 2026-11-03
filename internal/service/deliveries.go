package service

import (
	"bytes"
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	spoolyv1alpha1 "github.com/spoolysh/spooly/gen/spooly/v1alpha1"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
	"github.com/spoolysh/spooly/internal/mailparse"
)

// MaxRecipients is the most recipients a sent message can have.
const MaxRecipients = 100

func deliveryProto(d *db.Delivery) *spoolyv1alpha1.Delivery {
	p := &spoolyv1alpha1.Delivery{
		Id:                 d.ID,
		MessageId:          d.MessageID,
		MailboxId:          d.MailboxID,
		MailFrom:           d.MailFrom,
		Subject:            d.Subject,
		Recipient:          d.Recipient,
		Status:             d.Status,
		Transport:          d.Transport,
		TransportMessageId: d.TransportMessageID,
		Attempts:           int32(d.Attempts),
		Detail:             d.Detail,
		CreatedAt:          timestamppb.New(d.CreatedAt),
		UpdatedAt:          timestamppb.New(d.UpdatedAt),
	}
	if d.NextAttemptAt != nil && d.Pending() {
		p.NextAttemptAt = timestamppb.New(*d.NextAttemptAt)
	}
	return p
}

func deliveryProtos(list []*db.Delivery) []*spoolyv1alpha1.Delivery {
	out := make([]*spoolyv1alpha1.Delivery, len(list))
	for i, d := range list {
		out[i] = deliveryProto(d)
	}
	return out
}

func (s *Service) SendMessage(ctx context.Context, req *spoolyv1alpha1.SendMessageRequest) (*spoolyv1alpha1.SendMessageResponse, error) {
	key, err := idempotencyKey(ctx)
	if err != nil {
		return nil, err
	}
	m, deliveries, err := s.Send(ctx, req.Raw, req.RcptTo, key)
	if err != nil {
		return nil, err
	}
	resp := &spoolyv1alpha1.SendMessageResponse{Deliveries: deliveryProtos(deliveries)}
	if m != nil {
		resp.Message = messageProto(m)
	}
	return resp, nil
}

// Send sends raw from the mailbox its From is an address of, if ctx's user
// can see it: it stores it in the sent folder, read, unless NoSentCopy,
// queues a delivery to each of rcptTo, or if none, of its To, Cc, and Bcc,
// and tries them. Without a copy kept, the message is nil. A repeated
// idempotency key returns the stored message and its deliveries, sending
// nothing again.
func (s *Service) Send(ctx context.Context, raw []byte, rcptTo []string, key string) (*db.Message, []*db.Delivery, error) {
	if s.spool == nil {
		return nil, nil, connect.Errorf(connect.CodeFailedPrecondition, "sending is off: set MAIL_TRANSPORT")
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	from := strings.ToLower(parsed.From.Address)
	if from == "" {
		return nil, nil, connect.Errorf(connect.CodeInvalidArgument, "From: the message has no sender")
	}
	// A plus address is its mailbox's, as for mail coming in.
	a, err := s.requireMailbox(ctx, from)
	if err != nil {
		return nil, nil, err
	}
	_, domain, _ := strings.Cut(from, "@")
	id := db.NewID()
	// A copy only comes back to be known for one that was kept.
	stamp := id
	if s.opts.NoSentCopy {
		stamp = ""
	}
	out, err := delivery.Prepare(raw, stamp, domain, time.Now())
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	recipients := rcptTo
	if len(recipients) == 0 {
		for _, list := range [][]db.Address{parsed.To, parsed.Cc} {
			for _, addr := range list {
				recipients = append(recipients, addr.Address)
			}
		}
		recipients = append(recipients, out.Bcc...)
	}
	recipients = uniqueAddresses(recipients)
	if len(recipients) == 0 {
		return nil, nil, connect.Errorf(connect.CodeInvalidArgument, "no recipients: set To, Cc, or Bcc, or rcpt_to")
	}
	if len(recipients) > MaxRecipients {
		return nil, nil, connect.Errorf(connect.CodeInvalidArgument, "%d recipients: at most %d", len(recipients), MaxRecipients)
	}

	if s.opts.NoSentCopy {
		deliveries, err := s.sendUnkept(ctx, db.Outgoing{
			MessageID: id, MailboxID: a.ID, MailFrom: from, Subject: parsed.Subject,
			IdempotencyKey: key, Transport: s.spool.Transport(), Recipients: recipients,
		}, out.Raw)
		return nil, deliveries, err
	}
	m, err := s.Store(ctx, Incoming{
		ID: id, MailboxIDOrAddress: a.ID, MailFrom: from, IdempotencyKey: key, Raw: out.Raw,
		Folder: db.FolderSent, Flags: db.Flags{Seen: true}, Recipients: recipients,
	})
	if err != nil {
		return nil, nil, err
	}
	// Not sent now, deliveries stay due, for the spool's next round.
	if err := s.spool.Send(ctx, m.ID); err != nil {
		log.Printf("sending message %s: %v", m.ID, err)
	}
	deliveries, err := s.db.ListDeliveries(ctx, db.DeliveryFilter{MessageIDs: []string{m.ID}})
	if err != nil {
		return nil, nil, dbError(err)
	}
	return m, deliveries, nil
}

// sendUnkept sends raw, kept only in storage until its deliveries are done,
// and returns them.
func (s *Service) sendUnkept(ctx context.Context, o db.Outgoing, raw []byte) ([]*db.Delivery, error) {
	if _, err := s.store.Put(ctx, o.MessageID, "message/rfc822", bytes.NewReader(raw)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, "storing raw message").WithCause(err)
	}
	deliveries, created, err := s.db.QueueDeliveries(ctx, o)
	if err != nil || !created {
		s.deleteRaw(ctx, o.MessageID)
	}
	if err != nil {
		return nil, dbError(err)
	}
	if !created {
		return deliveries, nil // a repeat: sent already, or on its way
	}
	if err := s.spool.Send(ctx, o.MessageID); err != nil {
		log.Printf("sending message %s: %v", o.MessageID, err)
	}
	if deliveries, err = s.db.ListDeliveries(ctx, db.DeliveryFilter{MessageIDs: []string{o.MessageID}}); err != nil {
		return nil, dbError(err)
	}
	return deliveries, nil
}

// uniqueAddresses lowercases addresses and drops repeats and empties.
func uniqueAddresses(addresses []string) []string {
	out := []string{}
	for _, a := range addresses {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func (s *Service) ListDeliveries(ctx context.Context, req *spoolyv1alpha1.ListDeliveriesRequest) (*spoolyv1alpha1.ListDeliveriesResponse, error) {
	f := db.DeliveryFilter{Status: req.GetStatus(), After: req.GetAfter(), Limit: listLimit(req.Limit)}
	if len(req.MessageIds) > 0 {
		f.MessageIDs = req.MessageIds
	}
	if req.MailboxIdOrAddress != nil {
		a, err := s.requireMailbox(ctx, *req.MailboxIdOrAddress)
		if err != nil {
			return nil, err
		}
		f.MailboxID = a.ID
	} else {
		ids, err := s.VisibleMailboxes(ctx)
		if err != nil {
			return nil, err
		}
		f.MailboxIDs = ids
	}
	list, err := s.db.ListDeliveries(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1alpha1.ListDeliveriesResponse{Deliveries: deliveryProtos(list)}, nil
}

func (s *Service) RetryDelivery(ctx context.Context, req *spoolyv1alpha1.RetryDeliveryRequest) (*spoolyv1alpha1.RetryDeliveryResponse, error) {
	d, err := s.Retry(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return &spoolyv1alpha1.RetryDeliveryResponse{Delivery: deliveryProto(d)}, nil
}

// Retry tries a deferred or failed delivery again now, if ctx's user can
// see its mailbox, and returns it after.
func (s *Service) Retry(ctx context.Context, id string) (*db.Delivery, error) {
	if s.spool == nil {
		return nil, connect.Errorf(connect.CodeFailedPrecondition, "sending is off: set MAIL_TRANSPORT")
	}
	d, err := s.getDelivery(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.RetryDelivery(ctx, id); err != nil {
		return nil, dbError(err)
	}
	if err := s.spool.Send(ctx, d.MessageID); err != nil {
		log.Printf("retrying delivery %s: %v", id, err)
	}
	if d, err = s.db.GetDelivery(ctx, id); err != nil {
		return nil, dbError(err)
	}
	return d, nil
}

// Sends reports whether the service has a transport to send with.
func (s *Service) Sends() bool { return s.spool != nil }

func (s *Service) GetDelivery(ctx context.Context, req *spoolyv1alpha1.GetDeliveryRequest) (*spoolyv1alpha1.GetDeliveryResponse, error) {
	d, events, err := s.Delivery(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	resp := &spoolyv1alpha1.GetDeliveryResponse{Delivery: deliveryProto(d), Events: make([]*spoolyv1alpha1.DeliveryEvent, len(events))}
	for i, e := range events {
		resp.Events[i] = &spoolyv1alpha1.DeliveryEvent{
			At: timestamppb.New(e.At), Status: e.Status, Attempt: int32(e.Attempt), Detail: e.Detail, TransportMessageId: e.TransportMessageID,
		}
	}
	return resp, nil
}

// Delivery returns a delivery and its events, if ctx's user can see its
// mailbox.
func (s *Service) Delivery(ctx context.Context, id string) (*db.Delivery, []db.DeliveryEvent, error) {
	d, err := s.getDelivery(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	events, err := s.db.DeliveryEvents(ctx, id)
	if err != nil {
		return nil, nil, dbError(err)
	}
	return d, events, nil
}

// getDelivery returns a delivery if ctx's user can see its mailbox;
// otherwise it's not found.
func (s *Service) getDelivery(ctx context.Context, id string) (*db.Delivery, error) {
	d, err := s.db.GetDelivery(ctx, id)
	if err != nil {
		return nil, dbError(err)
	}
	if err := s.Require(ctx, d.MailboxID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, connect.Errorf(connect.CodeNotFound, "%s: %s", db.ErrDeliveryNotFound, id)
		}
		return nil, err
	}
	return d, nil
}
