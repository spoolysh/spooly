package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"slices"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect/v2"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/types/known/timestamppb"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/gen/boxy/v1/boxyv1connect"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/mailparse"
)

const (
	// MaxMessageSize is the largest raw message accepted. Cloudflare Email
	// Routing and Amazon SES both stop at 25-40 MiB.
	MaxMessageSize = 40 << 20
	chunkSize      = 64 * 1024
)

func addressProto(a db.Address) *boxyv1.Address {
	return &boxyv1.Address{Name: a.Name, Address: a.Address}
}

func addressesProto(list []db.Address) []*boxyv1.Address {
	out := make([]*boxyv1.Address, 0, len(list))
	for _, a := range list {
		out = append(out, addressProto(a))
	}
	return out
}

// messageProto converts m; the body is set only if withBody.
func messageProto(m *db.Message, withBody bool) *boxyv1.Message {
	p := &boxyv1.Message{
		Id:          m.ID,
		AccountId:   m.AccountID,
		Sender:      m.Sender,
		Recipient:   m.Recipient,
		From:        addressProto(m.From),
		To:          addressesProto(m.To),
		Cc:          addressesProto(m.Cc),
		Subject:     m.Subject,
		Snippet:     m.Snippet,
		Size:        m.Size,
		Attachments: make([]*boxyv1.Attachment, 0, len(m.Attachments)),
		Labels:      m.Labels,
		Metadata:    m.Metadata,
		ReceivedAt:  timestamppb.New(m.ReceivedAt),
	}
	if m.Date != nil {
		p.Date = timestamppb.New(*m.Date)
	}
	for _, a := range m.Attachments {
		p.Attachments = append(p.Attachments, &boxyv1.Attachment{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	if withBody {
		p.Body = &boxyv1.MessageBody{Text: m.Text, Html: m.HTML}
	}
	return p
}

// Incoming is a message to store, as CreateMessage receives it.
type Incoming struct {
	AccountIDOrAddress string
	Sender             string
	ReceivedAt         time.Time // zero means now
	IdempotencyKey     string
	Raw                []byte
	// Labels replaces the inbox and unread labels new mail gets, as when a
	// mail client saves a message to a folder. Nil means the defaults.
	Labels []string
}

// Store parses and stores an incoming message in the account it was sent
// to, with the inbox and unread labels unless in.Labels says otherwise. A
// repeated idempotency key returns the stored message. Callers check access
// first. Errors are connect errors.
func (s *Service) Store(ctx context.Context, in Incoming) (*db.Message, error) {
	if in.AccountIDOrAddress == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "account_id_or_address is required")
	}
	if len(in.Raw) == 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "raw: the message is empty")
	}
	account, recipient, err := s.resolveAccount(ctx, in.AccountIDOrAddress)
	if err != nil {
		return nil, err
	}

	parsed, err := mailparse.Parse(in.Raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	labels := in.Labels
	if labels == nil {
		labels = []string{db.LabelInbox, db.LabelUnread}
	}
	for _, label := range labels {
		if _, err := s.db.GetLabel(ctx, label); errors.Is(err, db.ErrLabelNotFound) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "labels: no label %q; create it first", label)
		} else if err != nil {
			return nil, dbError(err)
		}
	}
	receivedAt := in.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}

	// Bytes first, then the row, so a stored row always has its source; if
	// the insert fails or finds a retry's message, the new bytes are removed.
	id := db.NewID()
	if _, err := s.store.Put(ctx, id, "message/rfc822", bytes.NewReader(in.Raw)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, "storing raw message").WithCause(err)
	}
	m, created, err := s.db.CreateMessage(ctx, db.Message{
		ID:             id,
		AccountID:      account.ID,
		Sender:         strings.TrimSpace(in.Sender),
		Recipient:      recipient,
		From:           parsed.From,
		To:             parsed.To,
		Cc:             parsed.Cc,
		Subject:        parsed.Subject,
		Date:           parsed.Date,
		Snippet:        parsed.Snippet,
		Size:           int64(len(in.Raw)),
		Attachments:    parsed.Attachments,
		Text:           parsed.Text,
		HTML:           parsed.HTML,
		IdempotencyKey: in.IdempotencyKey,
		ReceivedAt:     receivedAt,
	}, labels)
	if err != nil || !created {
		if derr := s.store.Delete(ctx, id); derr != nil {
			log.Printf("removing raw message %s: %v", id, derr)
		}
	}
	if err != nil {
		return nil, dbError(err)
	}
	return m, nil
}

func (s *Service) CreateMessage(ctx context.Context, stream boxyv1connect.MessageServiceCreateMessageServerStream) (*boxyv1.CreateMessageResponse, error) {
	var in Incoming
	var raw bytes.Buffer
	first := true
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if first {
			in.AccountIDOrAddress = msg.AccountIdOrAddress
			in.Sender = msg.Sender
			in.IdempotencyKey = msg.GetIdempotencyKey()
			if len(msg.Labels) > 0 {
				in.Labels = msg.Labels
			}
			if msg.ReceivedAt != nil {
				in.ReceivedAt = msg.ReceivedAt.AsTime()
			}
			first = false
			// Reject unknown addresses before reading the message: a relay
			// may forward every address of a domain, spam included.
			if err := s.requireDelivery(ctx, in.AccountIDOrAddress); err != nil {
				return nil, err
			}
		}
		if raw.Len()+len(msg.GetRaw().GetData()) > MaxMessageSize {
			return nil, connect.Errorf(connect.CodeResourceExhausted, "raw: the message is larger than %d bytes", MaxMessageSize)
		}
		raw.Write(msg.GetRaw().GetData())
	}
	in.Raw = raw.Bytes()

	m, err := s.Store(ctx, in)
	if err != nil {
		return nil, err
	}
	return &boxyv1.CreateMessageResponse{Message: messageProto(m, false)}, nil
}

// requireDelivery checks that ctx's user may deliver mail to an account:
// they have access to it.
func (s *Service) requireDelivery(ctx context.Context, idOrAddress string) error {
	if idOrAddress == "" {
		return connect.Errorf(connect.CodeInvalidArgument, "account_id_or_address is required")
	}
	a, _, err := s.resolveAccount(ctx, idOrAddress)
	if err != nil {
		return err
	}
	return s.Require(ctx, a.ID)
}

// requireMessage checks that ctx's user has access to a message's account,
// and returns the message. A message they can't see is reported as not
// found.
func (s *Service) requireMessage(ctx context.Context, id string) (*db.Message, error) {
	if _, _, err := s.caller(ctx); err != nil {
		return nil, err
	}
	m, err := s.db.GetMessage(ctx, id, false)
	if err != nil {
		return nil, dbError(err)
	}
	if err := s.Require(ctx, m.AccountID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, connect.Errorf(connect.CodeNotFound, "%s: %s", db.ErrMessageNotFound, id)
		}
		return nil, err
	}
	return m, nil
}

func (s *Service) ListMessages(ctx context.Context, req *boxyv1.ListMessagesRequest) (*boxyv1.ListMessagesResponse, error) {
	// As in the dashboard and IMAP, spam and trash show only under their
	// own labels.
	f := db.MessageFilter{Label: req.GetLabel(), NotLabels: db.HiddenFrom(req.GetLabel()), After: req.GetAfter(), Limit: listLimit(req.Limit)}
	if req.AccountIdOrAddress != nil {
		a, _, err := s.resolveAccount(ctx, *req.AccountIdOrAddress)
		if err != nil {
			return nil, err
		}
		if err := s.Require(ctx, a.ID); err != nil {
			return nil, err
		}
		f.AccountID = a.ID
	} else {
		ids, err := s.VisibleAccounts(ctx)
		if err != nil {
			return nil, err
		}
		f.AccountIDs = ids
	}
	messages, err := s.db.ListMessages(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	resp := &boxyv1.ListMessagesResponse{Messages: make([]*boxyv1.Message, 0, len(messages))}
	for _, m := range messages {
		resp.Messages = append(resp.Messages, messageProto(m, false))
	}
	return resp, nil
}

func (s *Service) UpdateMessage(ctx context.Context, req *boxyv1.UpdateMessageRequest) (*boxyv1.UpdateMessageResponse, error) {
	for _, l := range req.AddLabels {
		if slices.Contains(req.RemoveLabels, l) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "label %q is both added and removed", l)
		}
	}
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	m, err := s.db.UpdateMessage(ctx, req.Id, db.MessageUpdate{
		AddLabels:    req.AddLabels,
		RemoveLabels: req.RemoveLabels,
		Metadata:     req.Metadata,
	})
	if err != nil {
		return nil, dbError(err)
	}
	return &boxyv1.UpdateMessageResponse{Message: messageProto(m, false)}, nil
}

func (s *Service) GetMessage(ctx context.Context, req *boxyv1.GetMessageRequest) (*boxyv1.GetMessageResponse, error) {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	m, err := s.db.GetMessage(ctx, req.Id, true)
	if err != nil {
		return nil, dbError(err)
	}
	return &boxyv1.GetMessageResponse{Message: messageProto(m, true)}, nil
}

// OpenRaw returns a message, without its bodies, and its raw source. The
// caller must close the source and check access first. Errors are connect
// errors.
func (s *Service) OpenRaw(ctx context.Context, id string) (*db.Message, io.ReadCloser, error) {
	m, err := s.db.GetMessage(ctx, id, false)
	if err != nil {
		return nil, nil, dbError(err)
	}
	rc, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, nil, dbError(err)
	}
	return m, rc, nil
}

// unsafeInFilename are characters that Windows, macOS, or Linux reject in
// file names, or that would end up as a path.
var unsafeInFilename = strings.NewReplacer(
	"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
)

// maxFilenameRunes keeps names well under file system limits (255 bytes).
const maxFilenameRunes = 100

// EmlFilename names a message's .eml file after its subject, like mail
// clients do, falling back to the ID when there's no usable subject.
func EmlFilename(m *db.Message) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, unsafeInFilename.Replace(m.Subject))
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > maxFilenameRunes {
		name = string(r[:maxFilenameRunes])
	}
	// Leading dots hide files; trailing dots and spaces are dropped by Windows.
	name = strings.Trim(name, ". ")
	if name == "" {
		name = m.ID
	}
	return name + ".eml"
}

func (s *Service) GetRawMessage(ctx context.Context, req *boxyv1.GetRawMessageRequest, stream boxyv1connect.MessageServiceGetRawMessageServerStream) error {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return err
	}
	m, rc, err := s.OpenRaw(ctx, req.Id)
	if err != nil {
		return err
	}
	defer rc.Close()
	// Over REST, Vanguard copies response headers onto the HTTP response.
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("Content-Disposition", ContentDisposition("attachment", EmlFilename(m)))
	}

	buf := make([]byte, chunkSize)
	first := true
	for {
		n, readErr := io.ReadFull(rc, buf)
		if n > 0 || first {
			chunk := &httpbody.HttpBody{Data: buf[:n]}
			if first {
				chunk.ContentType = "message/rfc822"
				first = false
			}
			if err := stream.Send(&boxyv1.GetRawMessageResponse{Raw: chunk}); err != nil {
				return err
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return nil
		}
		if readErr != nil {
			return connect.NewError(connect.CodeInternal, "reading raw message").WithCause(readErr)
		}
	}
}

// OpenAttachment returns attachment index of message id, decoded, with its
// filename and a content type to serve it as: text attachments are decoded
// to UTF-8, so they say so. Callers check access first. Errors are connect
// errors.
func (s *Service) OpenAttachment(ctx context.Context, id string, index int) (db.Attachment, []byte, error) {
	_, rc, err := s.OpenRaw(ctx, id)
	if err != nil {
		return db.Attachment{}, nil, err
	}
	raw, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return db.Attachment{}, nil, connect.NewError(connect.CodeInternal, "reading raw message").WithCause(err)
	}
	a, content, err := mailparse.Attachment(raw, index)
	if errors.Is(err, mailparse.ErrNoAttachment) {
		return db.Attachment{}, nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	if err != nil {
		return db.Attachment{}, nil, connect.NewError(connect.CodeInternal, "parsing raw message").WithCause(err)
	}
	if strings.HasPrefix(a.ContentType, "text/") {
		a.ContentType += "; charset=utf-8"
	}
	return a, content, nil
}

// ContentDisposition is the Content-Disposition for serving a file named
// filename: "attachment" to download it, or "inline" to show it.
func ContentDisposition(disposition, filename string) string {
	filename = strings.ToValidUTF8(filename, "_")
	if filename == "" {
		return disposition
	}
	// FormatMediaType encodes non-ASCII names per RFC 2231; it returns ""
	// for names it can't encode.
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); v != "" {
		return v
	}
	return disposition
}

func (s *Service) GetAttachment(ctx context.Context, req *boxyv1.GetAttachmentRequest, stream boxyv1connect.MessageServiceGetAttachmentServerStream) error {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return err
	}
	a, content, err := s.OpenAttachment(ctx, req.Id, int(req.Index))
	if err != nil {
		return err
	}
	// Over REST, Vanguard copies response headers onto the HTTP response.
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("Content-Disposition", ContentDisposition("attachment", a.Filename))
		info.ResponseHeader().Set("X-Content-Type-Options", "nosniff")
	}
	for i := 0; i == 0 || i < len(content); i += chunkSize {
		chunk := &httpbody.HttpBody{Data: content[i:min(i+chunkSize, len(content))]}
		if i == 0 {
			chunk.ContentType = a.ContentType
		}
		if err := stream.Send(&boxyv1.GetAttachmentResponse{Content: chunk}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) DeleteMessage(ctx context.Context, req *boxyv1.DeleteMessageRequest) (*boxyv1.DeleteMessageResponse, error) {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	if err := s.PurgeMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	return &boxyv1.DeleteMessageResponse{}, nil
}

// EmptyLabel deletes for good every message of an account in the view of
// label, which must be trash or spam, and returns how many it deleted.
func (s *Service) EmptyLabel(ctx context.Context, accountID, label string) (int, error) {
	if label != db.LabelTrash && label != db.LabelSpam {
		return 0, connect.Errorf(connect.CodeInvalidArgument, "only trash and spam can be emptied, not %q", label)
	}
	if err := s.Require(ctx, accountID); err != nil {
		return 0, err
	}
	f := db.MessageFilter{AccountID: accountID, Label: label, NotLabels: db.HiddenFrom(label), Limit: 500}
	deleted := 0
	for {
		messages, err := s.db.ListMessages(ctx, f)
		if err != nil {
			return deleted, dbError(err)
		}
		if len(messages) == 0 {
			return deleted, nil
		}
		for _, m := range messages {
			if err := s.PurgeMessage(ctx, m.ID); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
}

// PurgeMessage deletes a message for good: its row, labels, and raw
// source. Callers check access first.
func (s *Service) PurgeMessage(ctx context.Context, id string) error {
	if err := s.db.DeleteMessage(ctx, id); err != nil {
		return dbError(err)
	}
	// The row is gone, so the message is already unreachable; leftover
	// bytes from a failed delete are orphans, not corruption.
	if err := s.store.Delete(ctx, id); err != nil {
		log.Printf("deleting raw message %s: %v", id, err)
	}
	return nil
}
