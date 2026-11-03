package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect/v2"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/types/known/timestamppb"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/gen/spooly/v1/spoolyv1connect"
	spoolyv1alpha1 "github.com/spoolysh/spooly/gen/spooly/v1alpha1"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
	"github.com/spoolysh/spooly/internal/mailparse"
)

const (
	// MaxMessageSize is the largest raw message accepted. Cloudflare Email
	// Routing and Amazon SES both stop at 25-40 MiB.
	MaxMessageSize = 40 << 20
	chunkSize      = 64 * 1024
)

func addressProto(a db.Address) *spoolyv1.Address {
	return &spoolyv1.Address{Name: a.Name, Address: a.Address}
}

func addressesProto(list []db.Address) []*spoolyv1.Address {
	out := make([]*spoolyv1.Address, 0, len(list))
	for _, a := range list {
		out = append(out, addressProto(a))
	}
	return out
}

func messageProto(m *db.Message) *spoolyv1.Message {
	p := &spoolyv1.Message{
		Id:              m.ID,
		MailboxId:       m.MailboxID,
		MailFrom:        m.MailFrom,
		RcptTo:          m.RcptTo,
		From:            addressProto(m.From),
		To:              addressesProto(m.To),
		Cc:              addressesProto(m.Cc),
		Subject:         m.Subject,
		Snippet:         m.Snippet,
		Size:            m.Size,
		Parts:           make([]*spoolyv1.MessagePart, 0, len(m.Parts)),
		AttachmentCount: int32(m.Parts.AttachmentCount()),
		Folder:          m.Folder,
		Labels:          m.Labels,
		ReceivedAt:      timestamppb.New(m.ReceivedAt),
		Seen:            m.Seen,
		Answered:        m.Answered,
		Flagged:         m.Flagged,
		Draft:           m.Draft,
		Forwarded:       m.Forwarded,
	}
	if m.Date != nil {
		p.Date = timestamppb.New(*m.Date)
	}
	for _, pt := range m.Parts {
		p.Parts = append(p.Parts, &spoolyv1.MessagePart{
			Type: pt.Type, Name: pt.Name, Size: pt.Size, Cid: pt.CID,
			Disposition: pt.Disposition,
		})
	}
	return p
}

// Incoming is a message to store, as CreateMessage receives it.
type Incoming struct {
	MailboxIDOrAddress string
	MailFrom           string
	ReceivedAt         time.Time // zero means now
	IdempotencyKey     string
	Raw                []byte
	// Folder is where the message goes, as when a mail client saves a
	// message to a folder. Empty means the inbox.
	Folder string
	// Labels must exist. A plus address's tag adds its label too, if there's
	// one by that name.
	Labels []string
	// Flags are none for new mail; a mirrored or saved message may have
	// some, such as Seen.
	Flags db.Flags
	// Recipients, if any, get the message through the transport: Send's.
	Recipients []string
	// ID is the message's, if chosen already; empty means a new one.
	ID string
}

// Store parses and stores an incoming message in the mailbox it was sent
// to, in the inbox unless in.Folder says otherwise. A repeated idempotency
// key returns the stored message. Callers check access first. Errors are
// connect errors.
func (s *Service) Store(ctx context.Context, in Incoming) (*db.Message, error) {
	if in.MailboxIDOrAddress == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "mailbox_id_or_address is required")
	}
	if len(in.Raw) == 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "raw: the message is empty")
	}
	mailbox, rcptTo, err := s.resolveMailbox(ctx, in.MailboxIDOrAddress)
	if err != nil {
		return nil, err
	}

	parsed, err := mailparse.Parse(in.Raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	folder := in.Folder
	if folder == "" {
		folder = db.FolderInbox
	}
	labels := slices.Clone(in.Labels)
	for _, label := range labels {
		if _, err := s.db.GetLabel(ctx, label); errors.Is(err, db.ErrLabelNotFound) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "labels: no label %q; create it first", label)
		} else if err != nil {
			return nil, dbError(err)
		}
	}
	// hello+billing@ asks for the billing label. Only an existing one: any
	// sender can pick the tag.
	if tag := plusTag(rcptTo); tag != "" && !slices.Contains(labels, tag) {
		if _, err := s.db.GetLabel(ctx, tag); err == nil {
			labels = append(labels, tag)
		} else if !errors.Is(err, db.ErrLabelNotFound) {
			return nil, dbError(err)
		}
	}
	receivedAt := in.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}

	// A copy of a message Spooly sent from this mailbox, such as one a mail
	// client saves, or Gmail's in its Sent folder, is that message.
	if in.Recipients == nil {
		if sent := sentID(parsed); sent != "" {
			if m, err := s.db.GetMessage(ctx, sent); err == nil && m.MailboxID == mailbox.ID {
				return m, nil
			}
		}
	}

	// Bytes first, then the row, so a stored row always has its source; if
	// the insert fails or finds a retry's message, the new bytes are removed.
	id := in.ID
	if id == "" {
		id = db.NewID()
	}
	if _, err := s.store.Put(ctx, id, "message/rfc822", bytes.NewReader(in.Raw)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, "storing raw message").WithCause(err)
	}
	m := parsedMessage(id, mailbox.ID, parsed, len(in.Raw))
	m.MailFrom, m.RcptTo = strings.TrimSpace(in.MailFrom), rcptTo
	m.Folder, m.Flags, m.IdempotencyKey, m.ReceivedAt = folder, in.Flags, in.IdempotencyKey, receivedAt
	var stored *db.Message
	var created bool
	if in.Recipients != nil {
		stored, created, err = s.db.SendMessage(ctx, m, labels, s.spool.Transport(), in.Recipients)
	} else {
		stored, created, err = s.db.CreateMessage(ctx, m, labels)
	}
	if err != nil || !created {
		if derr := s.store.Delete(ctx, id); derr != nil {
			log.Printf("removing raw message %s: %v", id, derr)
		}
	}
	if err != nil {
		return nil, dbError(err)
	}
	return stored, nil
}

func (s *Service) CreateMessage(ctx context.Context, stream spoolyv1connect.MessageServiceCreateMessageServerStream) (*spoolyv1.CreateMessageResponse, error) {
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
			in.MailboxIDOrAddress = msg.MailboxIdOrAddress
			in.MailFrom = msg.MailFrom
			in.Folder, in.Labels = msg.Folder, msg.Labels
			in.Flags = db.Flags{Seen: msg.Seen, Answered: msg.Answered, Flagged: msg.Flagged, Draft: msg.Draft, Forwarded: msg.Forwarded}
			if msg.ReceivedAt != nil {
				in.ReceivedAt = msg.ReceivedAt.AsTime()
			}
			first = false
			if in.IdempotencyKey, err = idempotencyKey(ctx); err != nil {
				return nil, err
			}
			// Reject unknown addresses before reading the message: a relay
			// may forward every address of a domain, spam included.
			if err := s.requireDelivery(ctx, in.MailboxIDOrAddress); err != nil {
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
	return &spoolyv1.CreateMessageResponse{Message: messageProto(m)}, nil
}

// sentID returns the ID of the message Spooly sent that parsed is a copy
// of, from its MessageIDHeader, or empty.
func sentID(parsed *mailparse.Parsed) string {
	for _, h := range parsed.Headers {
		if strings.EqualFold(h.Name, delivery.MessageIDHeader) {
			if id := strings.TrimSpace(h.Value); isMessageID(id) {
				return id
			}
		}
	}
	return ""
}

// isMessageID reports whether s looks like a message ID: a lowercase ULID.
func isMessageID(s string) bool {
	return len(s) == 26 && strings.Trim(s, "0123456789abcdefghjkmnpqrstvwxyz") == ""
}

// plusTag returns the tag of a plus address, "billing" for
// hello+billing@spooly.sh, or empty.
func plusTag(address string) string {
	local, _, _ := strings.Cut(address, "@")
	_, tag, _ := strings.Cut(local, "+")
	return tag
}

// MaxIdempotencyKeyLength is the longest idempotency key, in bytes.
const MaxIdempotencyKeyLength = 128

// idempotencyKey returns the Idempotency-Key request header, or empty.
func idempotencyKey(ctx context.Context) (string, error) {
	info, ok := connect.CallInfoForServerContext(ctx)
	if !ok {
		return "", nil
	}
	key := strings.TrimSpace(info.RequestHeader().Get("Idempotency-Key"))
	if len(key) > MaxIdempotencyKeyLength {
		return "", connect.Errorf(connect.CodeInvalidArgument, "Idempotency-Key: longer than %d bytes", MaxIdempotencyKeyLength)
	}
	return key, nil
}

// requireDelivery checks that ctx's user may deliver mail to a mailbox:
// they have access to it.
func (s *Service) requireDelivery(ctx context.Context, idOrAddress string) error {
	if idOrAddress == "" {
		return connect.Errorf(connect.CodeInvalidArgument, "mailbox_id_or_address is required")
	}
	a, _, err := s.resolveMailbox(ctx, idOrAddress)
	if err != nil {
		return err
	}
	return s.Require(ctx, a.ID)
}

// requireMessage checks that ctx's user has access to a message's mailbox,
// and returns the message. A message they can't see is reported as not
// found.
func (s *Service) requireMessage(ctx context.Context, id string) (*db.Message, error) {
	if _, _, err := s.caller(ctx); err != nil {
		return nil, err
	}
	m, err := s.db.GetMessage(ctx, id)
	if err != nil {
		return nil, dbError(err)
	}
	if err := s.Require(ctx, m.MailboxID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, connect.Errorf(connect.CodeNotFound, "%s: %s", db.ErrMessageNotFound, id)
		}
		return nil, err
	}
	return m, nil
}

func (s *Service) ListMessages(ctx context.Context, req *spoolyv1.ListMessagesRequest) (*spoolyv1.ListMessagesResponse, error) {
	// As in the dashboard and IMAP, junk and trash show only under their
	// own folders.
	f := db.View(req.GetFolder(), req.GetLabel())
	f.Seen, f.Flagged, f.After, f.Limit = req.Seen, req.Flagged, req.GetAfter(), listLimit(req.Limit)
	if req.MailboxIdOrAddress != nil {
		a, _, err := s.resolveMailbox(ctx, *req.MailboxIdOrAddress)
		if err != nil {
			return nil, err
		}
		if err := s.Require(ctx, a.ID); err != nil {
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
	messages, err := s.db.ListSummaries(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.ListMessagesResponse{Messages: summaryProtos(messages)}, nil
}

func (s *Service) ListMessagesBySeq(ctx context.Context, req *spoolyv1alpha1.ListMessagesBySeqRequest) (*spoolyv1alpha1.ListMessagesBySeqResponse, error) {
	a, err := s.requireMailbox(ctx, req.MailboxIdOrAddress)
	if err != nil {
		return nil, err
	}
	f := db.View(req.GetFolder(), req.GetLabel())
	f.MailboxID, f.Arrival, f.AfterSeq, f.Limit = a.ID, true, req.GetAfter(), listLimit(req.Limit)
	messages, err := s.db.ListSummaries(ctx, f)
	if err != nil {
		return nil, dbError(err)
	}
	// Bounds after: next_seq is above every seq listed. A message coming in
	// meanwhile is below it but unlisted, and lists after the last seq.
	since, next, err := s.db.SeqBounds(ctx, a.ID, req.GetFolder()+req.GetLabel())
	if err != nil {
		return nil, dbError(err)
	}
	resp := &spoolyv1alpha1.ListMessagesBySeqResponse{
		Messages: make([]*spoolyv1alpha1.ListMessagesBySeqResponse_Entry, 0, len(messages)),
		Since:    timestamppb.New(since),
		NextSeq:  next,
	}
	for i := range messages {
		resp.Messages = append(resp.Messages, &spoolyv1alpha1.ListMessagesBySeqResponse_Entry{Seq: messages[i].Seq, Message: summaryProto(&messages[i])})
	}
	return resp, nil
}

func summaryProtos(messages []db.Summary) []*spoolyv1.MessageSummary {
	list := make([]*spoolyv1.MessageSummary, 0, len(messages))
	for i := range messages {
		list = append(list, summaryProto(&messages[i]))
	}
	return list
}

func summaryProto(m *db.Summary) *spoolyv1.MessageSummary {
	p := &spoolyv1.MessageSummary{
		Id:              m.ID,
		MailboxId:       m.MailboxID,
		Folder:          m.Folder,
		Labels:          m.Labels,
		Seen:            m.Seen,
		Answered:        m.Answered,
		Flagged:         m.Flagged,
		Draft:           m.Draft,
		Forwarded:       m.Forwarded,
		From:            addressProto(m.From),
		Subject:         m.Subject,
		Snippet:         m.Snippet,
		Size:            m.Size,
		AttachmentCount: int32(m.AttachmentCount),
		ReceivedAt:      timestamppb.New(m.ReceivedAt),
	}
	if m.Date != nil {
		p.Date = timestamppb.New(*m.Date)
	}
	return p
}

// checkLabels refuses a label that's both added and removed.
func checkLabels(add, remove []string) error {
	for _, l := range add {
		if slices.Contains(remove, l) {
			return connect.Errorf(connect.CodeInvalidArgument, "label %q is both added and removed", l)
		}
	}
	return nil
}

func (s *Service) UpdateMessage(ctx context.Context, req *spoolyv1.UpdateMessageRequest) (*spoolyv1.UpdateMessageResponse, error) {
	if err := checkLabels(req.AddLabels, req.RemoveLabels); err != nil {
		return nil, err
	}
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	m, err := s.db.UpdateMessage(ctx, req.Id, db.MessageUpdate{
		Folder:       req.GetFolder(),
		AddLabels:    req.AddLabels,
		RemoveLabels: req.RemoveLabels,
		Seen:         req.Seen,
		Answered:     req.Answered,
		Flagged:      req.Flagged,
		Draft:        req.Draft,
		Forwarded:    req.Forwarded,
	})
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1.UpdateMessageResponse{Message: messageProto(m)}, nil
}

// BatchUpdateMessages applies one update to each message, skipping those that
// don't exist or that the caller can't see.
func (s *Service) BatchUpdateMessages(ctx context.Context, req *spoolyv1alpha1.BatchUpdateMessagesRequest) (*spoolyv1alpha1.BatchUpdateMessagesResponse, error) {
	if err := checkLabels(req.AddLabels, req.RemoveLabels); err != nil {
		return nil, err
	}
	u := db.MessageUpdate{
		Folder:       req.GetFolder(),
		AddLabels:    req.AddLabels,
		RemoveLabels: req.RemoveLabels,
		Seen:         req.Seen,
		Answered:     req.Answered,
		Flagged:      req.Flagged,
		Draft:        req.Draft,
		Forwarded:    req.Forwarded,
	}
	resp := &spoolyv1alpha1.BatchUpdateMessagesResponse{}
	for _, id := range req.Ids {
		_, err := s.requireMessage(ctx, id)
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, err = s.db.UpdateMessage(ctx, id, u)
		if errors.Is(err, db.ErrMessageNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, dbError(err)
		}
		resp.Updated++
	}
	return resp, nil
}

// A changes cursor is opaque to clients: it's the ID of the last event
// read, in decimal, but may become anything ListChanges returns.
func encodeCursor(eventID int64) string {
	return strconv.FormatInt(eventID, 10)
}

func decodeCursor(cursor string) (int64, error) {
	eventID, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || eventID < 0 {
		return 0, connect.Errorf(connect.CodeInvalidArgument, "cursor: pass one ListChanges returned")
	}
	return eventID, nil
}

// defaultChangesLimit is how many events a page of changes reads unless
// asked.
const defaultChangesLimit = 1000

// requireMailbox resolves a mailbox the caller has access to.
func (s *Service) requireMailbox(ctx context.Context, idOrAddress string) (*db.Mailbox, error) {
	a, _, err := s.resolveMailbox(ctx, idOrAddress)
	if err != nil {
		return nil, err
	}
	if err := s.Require(ctx, a.ID); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) ListChanges(ctx context.Context, req *spoolyv1alpha1.ListChangesRequest) (*spoolyv1alpha1.ListChangesResponse, error) {
	a, err := s.requireMailbox(ctx, req.MailboxIdOrAddress)
	if err != nil {
		return nil, err
	}
	// Without a cursor, from now: just where to start.
	if req.Cursor == "" {
		now, err := s.db.Now(ctx)
		if err != nil {
			return nil, dbError(err)
		}
		return &spoolyv1alpha1.ListChangesResponse{Created: []*spoolyv1.MessageSummary{}, Updated: []*spoolyv1.MessageSummary{}, Deleted: []string{}, Cursor: encodeCursor(now)}, nil
	}
	since, err := decodeCursor(req.Cursor)
	if err != nil {
		return nil, err
	}
	limit := defaultChangesLimit
	if req.Limit != nil {
		limit = int(*req.Limit)
	}
	c, err := s.db.ListChanges(ctx, a.ID, since, limit)
	if err != nil {
		return nil, dbError(err)
	}
	return &spoolyv1alpha1.ListChangesResponse{
		Created: summaryProtos(c.Created),
		Updated: summaryProtos(c.Updated),
		Deleted: c.Deleted,
		Cursor:  encodeCursor(c.LastID),
		HasMore: c.HasMore,
	}, nil
}

func (s *Service) GetMessage(ctx context.Context, req *spoolyv1.GetMessageRequest) (*spoolyv1.GetMessageResponse, error) {
	m, err := s.requireMessage(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return &spoolyv1.GetMessageResponse{Message: messageProto(m)}, nil
}

func (s *Service) GetMessageHeaders(ctx context.Context, req *spoolyv1.GetMessageHeadersRequest) (*spoolyv1.GetMessageHeadersResponse, error) {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	headers, err := s.readHeaders(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	resp := &spoolyv1.GetMessageHeadersResponse{Headers: make([]*spoolyv1.MessageHeader, 0, len(headers))}
	for _, h := range headers {
		resp.Headers = append(resp.Headers, &spoolyv1.MessageHeader{Name: h.Name, Value: h.Value})
	}
	return resp, nil
}

// readHeaders reads the header fields of message id, and nothing after
// them. Errors are connect errors.
func (s *Service) readHeaders(ctx context.Context, id string) ([]db.Header, error) {
	rc, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, dbError(err)
	}
	defer rc.Close()
	headers, err := mailparse.ReadHeaders(rc)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "reading header fields").WithCause(err)
	}
	return headers, nil
}

// ReadBody sets m's text and HTML bodies and header fields, reading just
// those from its raw message. Errors are connect errors.
func (s *Service) ReadBody(ctx context.Context, m *db.Message) error {
	var err error
	if m.Headers, err = s.readHeaders(ctx, m.ID); err != nil {
		return err
	}
	for _, b := range []struct {
		mediaType string
		dst       *string
	}{{db.TextBody, &m.Text}, {db.HTMLBody, &m.HTML}} {
		part, ok := m.Parts.Body(b.mediaType)
		if !ok {
			continue
		}
		r, err := s.openPart(ctx, m.ID, part)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return connect.NewError(connect.CodeInternal, "reading message body").WithCause(err)
		}
		*b.dst = strings.ToValidUTF8(string(content), "\uFFFD")
	}
	return nil
}

// openPart returns the decoded content of a part of message id, reading
// only its bytes. Errors are connect errors.
func (s *Service) openPart(ctx context.Context, id string, part db.Part) (io.ReadCloser, error) {
	rc, err := s.store.GetRange(ctx, id, part.Offset, part.Length)
	if err != nil {
		return nil, dbError(err)
	}
	r, err := mailparse.ReadPart(rc)
	if err != nil {
		rc.Close()
		return nil, connect.NewError(connect.CodeInternal, "parsing message part").WithCause(err)
	}
	return struct {
		io.Reader
		io.Closer
	}{r, rc}, nil
}

// OpenRaw returns a message and its raw source. The
// caller must close the source and check access first. Errors are connect
// errors.
func (s *Service) OpenRaw(ctx context.Context, id string) (*db.Message, io.ReadCloser, error) {
	m, err := s.db.GetMessage(ctx, id)
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

func (s *Service) GetMessageRaw(ctx context.Context, req *spoolyv1.GetMessageRawRequest, stream spoolyv1connect.MessageServiceGetMessageRawServerStream) error {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return err
	}
	var m *db.Message
	var rc io.ReadCloser
	var err error
	if req.Offset == nil && req.Length == nil {
		m, rc, err = s.OpenRaw(ctx, req.Id)
	} else {
		m, rc, err = s.openRawRange(ctx, req.Id, req.GetOffset(), req.Length)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	// Over REST, Vanguard copies response headers onto the HTTP response.
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		info.ResponseHeader().Set("Content-Disposition", ContentDisposition("attachment", EmlFilename(m)))
	}

	return sendChunks(rc, "message/rfc822", func(chunk *httpbody.HttpBody) error {
		return stream.Send(&spoolyv1.GetMessageRawResponse{Raw: chunk})
	})
}

// openRawRange returns a message and length bytes of its raw source from
// offset, or to the end with no length; past the end, none.
func (s *Service) openRawRange(ctx context.Context, id string, offset int64, length *int64) (*db.Message, io.ReadCloser, error) {
	m, err := s.db.GetMessage(ctx, id)
	if err != nil {
		return nil, nil, dbError(err)
	}
	n := max(m.Size-offset, 0)
	if length != nil {
		n = min(n, *length)
	}
	if n == 0 {
		return m, io.NopCloser(strings.NewReader("")), nil
	}
	rc, err := s.store.GetRange(ctx, id, offset, n)
	if err != nil {
		return nil, nil, dbError(err)
	}
	return m, rc, nil
}

// sendChunks sends r's content in chunks, the first with contentType, and
// always at least one.
func sendChunks(r io.Reader, contentType string, send func(*httpbody.HttpBody) error) error {
	buf := make([]byte, chunkSize)
	first := true
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 || first {
			chunk := &httpbody.HttpBody{Data: buf[:n]}
			if first {
				chunk.ContentType = contentType
				first = false
			}
			if err := send(chunk); err != nil {
				return err
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return nil
		}
		if readErr != nil {
			return connect.NewError(connect.CodeInternal, "reading message").WithCause(readErr)
		}
	}
}

// OpenPart returns part index of message m, decoded, with a content type
// to serve it as: text is decoded to UTF-8, so it says so. Only the part's
// bytes are read. The caller must close the content and check access
// first. Errors are connect errors.
func (s *Service) OpenPart(ctx context.Context, m *db.Message, index int) (db.Part, io.ReadCloser, error) {
	if index < 0 || index >= len(m.Parts) {
		return db.Part{}, nil, connect.Errorf(connect.CodeNotFound, "no part %d", index)
	}
	part := m.Parts[index]
	rc, err := s.openPart(ctx, m.ID, part)
	if err != nil {
		return db.Part{}, nil, err
	}
	if strings.HasPrefix(part.Type, "text/") {
		part.Type += "; charset=utf-8"
	}
	return part, rc, nil
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

func (s *Service) GetMessagePart(ctx context.Context, req *spoolyv1.GetMessagePartRequest, stream spoolyv1connect.MessageServiceGetMessagePartServerStream) error {
	m, err := s.requireMessage(ctx, req.Id)
	if err != nil {
		return err
	}
	part, rc, err := s.OpenPart(ctx, m, int(req.Index))
	if err != nil {
		return err
	}
	defer rc.Close()
	// Over REST, Vanguard copies response headers onto the HTTP response.
	// The API takes only API keys, never the dashboard's cookie, so a
	// browser can't be made to open a part, and its own disposition is safe
	// to pass on. Without one, named files download and the rest shows.
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		disposition := part.Disposition
		if disposition != "inline" && disposition != "attachment" {
			disposition = "inline"
			if part.Name != "" {
				disposition = "attachment"
			}
		}
		info.ResponseHeader().Set("Content-Disposition", ContentDisposition(disposition, part.Name))
		info.ResponseHeader().Set("X-Content-Type-Options", "nosniff")
	}
	return sendChunks(rc, part.Type, func(chunk *httpbody.HttpBody) error {
		return stream.Send(&spoolyv1.GetMessagePartResponse{Content: chunk})
	})
}

func (s *Service) DeleteMessage(ctx context.Context, req *spoolyv1.DeleteMessageRequest) (*spoolyv1.DeleteMessageResponse, error) {
	if _, err := s.requireMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	if err := s.PurgeMessage(ctx, req.Id); err != nil {
		return nil, err
	}
	return &spoolyv1.DeleteMessageResponse{}, nil
}

// EmptyFolder deletes for good every message of a mailbox in folder,
// which must be trash or junk, and returns how many it deleted.
func (s *Service) EmptyFolder(ctx context.Context, mailboxID, folder string) (int, error) {
	if folder != db.FolderTrash && folder != db.FolderJunk {
		return 0, connect.Errorf(connect.CodeInvalidArgument, "only trash and junk can be emptied, not %q", folder)
	}
	if err := s.Require(ctx, mailboxID); err != nil {
		return 0, err
	}
	f := db.View(folder, "")
	f.MailboxID, f.Limit = mailboxID, 500
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

// PurgeMessage deletes a message for good: its row and labels, and its raw
// source, now or, if it can be restored for a while, when Prune expunges it.
// Callers check access first.
func (s *Service) PurgeMessage(ctx context.Context, id string) error {
	restorable := s.opts.ExpungeAfter > 0
	if err := s.db.DeleteMessage(ctx, id, restorable); err != nil {
		return dbError(err)
	}
	if !restorable {
		s.deleteRaw(ctx, id)
	}
	return nil
}

// deleteRaw removes a message's raw source. Its row is gone, so it's
// unreachable already; leftover bytes from a failed delete are orphans, not
// corruption.
func (s *Service) deleteRaw(ctx context.Context, id string) {
	if err := s.store.Delete(ctx, id); err != nil {
		log.Printf("deleting raw message %s: %v", id, err)
	}
}

// Prune expunges messages deleted ExpungeAfter before now, removing their
// raw sources, then removes events older than EventsRetention, unless it's
// 0: then they're kept forever.
func (s *Service) Prune(ctx context.Context, now time.Time) error {
	expired, err := s.db.Expirable(ctx, now.Add(-s.opts.ExpungeAfter))
	if err != nil {
		return err
	}
	for _, m := range expired {
		s.deleteRaw(ctx, m.ID)
		if err := s.db.Expunge(ctx, m); err != nil {
			return err
		}
	}
	var n int64
	if s.opts.EventsRetention > 0 {
		if n, err = s.db.PruneEvents(ctx, now.Add(-s.opts.EventsRetention)); err != nil {
			return err
		}
	}
	if len(expired) > 0 || n > 0 {
		log.Printf("expunged %d deleted messages, pruned %d events", len(expired), n)
	}
	return nil
}

// ListDeleted returns a mailbox's messages deleted for good that can still
// be restored, most recently deleted first. Admins only: it's not in the
// API, but for the dashboard. Errors are connect errors.
func (s *Service) ListDeleted(ctx context.Context, mailboxIDOrAddress string) ([]*db.DeletedMessage, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	a, _, err := s.resolveMailbox(ctx, mailboxIDOrAddress)
	if err != nil {
		return nil, err
	}
	list, err := s.db.ListDeleted(ctx, a.ID)
	if err != nil {
		return nil, dbError(err)
	}
	return list, nil
}

// Restore brings back a message deleted for good, until it's expunged,
// under its ID, in its folder, with its flags and the labels that still
// exist. It's parsed again from its raw source, which is all the database
// kept besides its envelope, place, and state. Admins only: it's not in the
// API, but for the dashboard. Errors are connect errors.
func (s *Service) Restore(ctx context.Context, id string) (*db.Message, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	del, err := s.db.GetDeleted(ctx, id)
	if err != nil {
		return nil, dbError(err)
	}
	rc, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, dbError(err)
	}
	raw, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "reading raw message").WithCause(err)
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "parsing raw message").WithCause(err)
	}
	m := parsedMessage(del.ID, del.MailboxID, parsed, len(raw))
	m.MailFrom, m.RcptTo = del.MailFrom, del.RcptTo
	m.Folder, m.Flags, m.IdempotencyKey, m.ReceivedAt = del.Folder, del.Flags, del.IdempotencyKey, del.ReceivedAt
	// Labels deleted meanwhile are left off.
	var labels []string
	for _, l := range del.Labels {
		if _, err := s.db.GetLabel(ctx, l); err == nil {
			labels = append(labels, l)
		} else if !errors.Is(err, db.ErrLabelNotFound) {
			return nil, dbError(err)
		}
	}
	restored, err := s.db.RestoreMessage(ctx, m, labels)
	if err != nil {
		return nil, dbError(err)
	}
	return restored, nil
}

// parsedMessage is a message with ID id in a mailbox, with what's parsed
// from its raw source of size bytes.
func parsedMessage(id, mailboxID string, parsed *mailparse.Parsed, size int) db.Message {
	return db.Message{
		ID:        id,
		MailboxID: mailboxID,
		From:      parsed.From,
		To:        parsed.To,
		Cc:        parsed.Cc,
		Subject:   parsed.Subject,
		Date:      parsed.Date,
		Snippet:   parsed.Snippet,
		Size:      int64(size),
		Parts:     parsed.Parts,
		MessageID: parsed.MessageID,
		InReplyTo: parsed.InReplyTo,
	}
}
