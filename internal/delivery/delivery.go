// Package delivery sends messages through a mail transport: an SMTP relay,
// Cloudflare, or another provider. A spool keeps each message's way to
// each recipient as a delivery, and retries those the transport couldn't
// take until it does or it's been too long.
package delivery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/storage"
)

// Transport hands messages to whatever delivers them.
type Transport interface {
	// Name names the transport in deliveries, such as "smtp".
	Name() string
	// Send hands raw to the transport for the envelope sender from and
	// recipients to. An error fails the attempt for every recipient, for
	// good if it's an *Error that isn't Temporary; otherwise the result
	// says which recipients the transport refused, the rest it took.
	Send(ctx context.Context, from string, to []string, raw []byte) (*Result, error)
}

// Result is what a transport says of a message it took.
type Result struct {
	// MessageID is the transport's ID for the message, if it gives one.
	MessageID string
	// Reply is what the transport said on taking it, such as "2.0.0 Ok".
	Reply string
	// Refused are the recipients it didn't take, and why.
	Refused map[string]*Error
}

// Error is a transport's refusal, such as an SMTP reply: temporary ones are
// tried again.
type Error struct {
	Temporary bool
	Detail    string // such as "550 5.1.1 user unknown"
}

func (e *Error) Error() string { return e.Detail }

// Retry timing: a deferred delivery is tried again after the next of
// backoff, the last repeating, until it's been MaxAge since it was queued.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}

// MaxAge is how long a delivery is retried before it fails.
const MaxAge = 24 * time.Hour

// lease is how long a claimed delivery is left to its spool before it's
// due again, should sending stop halfway.
const lease = 10 * time.Minute

// Spool sends the deliveries of stored messages through a transport.
type Spool struct {
	db        *db.DB
	store     storage.Storage
	transport Transport
}

func NewSpool(database *db.DB, store storage.Storage, transport Transport) *Spool {
	return &Spool{db: database, store: store, transport: transport}
}

// Transport names the spool's transport.
func (s *Spool) Transport() string { return s.transport.Name() }

// Send tries the due deliveries of a message now. How it went is in the
// deliveries; the error is for what kept it from trying.
func (s *Spool) Send(ctx context.Context, messageID string) error {
	claimed, err := s.db.ClaimDeliveries(ctx, messageID, time.Now(), lease, 1000)
	if err != nil {
		return err
	}
	return s.send(ctx, claimed)
}

// SendDue tries every delivery that's due, such as one deferred until now,
// a batch at a time.
func (s *Spool) SendDue(ctx context.Context) error {
	for {
		claimed, err := s.db.ClaimDeliveries(ctx, "", time.Now(), lease, 100)
		if err != nil || len(claimed) == 0 {
			return err
		}
		if err := s.send(ctx, claimed); err != nil {
			return err
		}
	}
}

// send tries claimed deliveries, one transport call per message.
func (s *Spool) send(ctx context.Context, claimed []*db.Delivery) error {
	byMessage := map[string][]*db.Delivery{}
	var order []string
	for _, d := range claimed {
		if byMessage[d.MessageID] == nil {
			order = append(order, d.MessageID)
		}
		byMessage[d.MessageID] = append(byMessage[d.MessageID], d)
	}
	for _, id := range order {
		if err := s.sendMessage(ctx, id, byMessage[id]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Spool) sendMessage(ctx context.Context, messageID string, deliveries []*db.Delivery) error {
	m, err := s.db.GetMessage(ctx, messageID)
	if errors.Is(err, db.ErrMessageNotFound) {
		return nil // deleted meanwhile, its deliveries with it
	}
	if err != nil {
		return err
	}
	raw, err := s.read(ctx, messageID)
	if err != nil {
		return err
	}
	to := make([]string, len(deliveries))
	for i, d := range deliveries {
		to[i] = d.Recipient
	}
	result, err := s.transport.Send(ctx, m.MailFrom, to, raw)
	for _, d := range deliveries {
		var refused *Error
		switch {
		case err != nil:
			if !errors.As(err, &refused) {
				refused = &Error{Temporary: true, Detail: err.Error()}
			}
		case result.Refused[d.Recipient] != nil:
			refused = result.Refused[d.Recipient]
		}
		a := db.DeliveryAttempt{Status: db.DeliverySent, Attempted: true}
		if result != nil {
			a.TransportMessageID, a.Detail = result.MessageID, result.Reply
		}
		if refused != nil {
			a.Status, a.Detail = retry(d, refused)
			if a.Status == db.DeliveryDeferred {
				next := time.Now().Add(backoff[min(d.Attempts, len(backoff)-1)])
				a.NextAttemptAt = &next
			}
		}
		if _, err := s.db.RecordDelivery(ctx, d.ID, a); err != nil {
			return err
		}
		if refused != nil {
			log.Printf("delivery %s of message %s to %s: %s: %s", d.ID, messageID, d.Recipient, a.Status, a.Detail)
		}
	}
	return nil
}

// retry says what a refusal makes of a delivery: deferred if temporary and
// there's time left, failed otherwise.
func retry(d *db.Delivery, refused *Error) (status, detail string) {
	switch {
	case !refused.Temporary:
		return db.DeliveryFailed, refused.Detail
	case time.Since(d.CreatedAt) >= MaxAge:
		return db.DeliveryFailed, fmt.Sprintf("gave up after %s: %s", MaxAge, refused.Detail)
	default:
		return db.DeliveryDeferred, refused.Detail
	}
}

func (s *Spool) read(ctx context.Context, messageID string) ([]byte, error) {
	r, err := s.store.Get(ctx, messageID)
	if err != nil {
		return nil, fmt.Errorf("reading message %s: %w", messageID, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Outgoing is a message prepared to send.
type Outgoing struct {
	Raw []byte
	// Bcc are the Bcc header's addresses, which Raw no longer has: they get
	// the message without the others seeing them.
	Bcc []string
}

// Prepare readies a message to send from an address of domain: it takes
// out the Bcc header, and adds a Message-ID and a Date if missing. The rest
// is left as it is.
func Prepare(raw []byte, domain string, now time.Time) (*Outgoing, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("reading the header: %w", err)
	}
	out := &Outgoing{}
	for _, v := range h.Values("Bcc") {
		if strings.TrimSpace(v) == "" {
			continue
		}
		list, err := addressParser.ParseList(v)
		if err != nil {
			return nil, fmt.Errorf("Bcc: %w", err)
		}
		for _, a := range list {
			out.Bcc = append(out.Bcc, a.Address)
		}
	}
	h.Del("Bcc")
	if !h.Has("Message-Id") {
		h.Add("Message-Id", "<"+db.NewID()+"@"+domain+">")
	}
	if !h.Has("Date") {
		h.Add("Date", now.Format(time.RFC1123Z))
	}
	var b bytes.Buffer
	if err := textproto.WriteHeader(&b, h); err != nil {
		return nil, err
	}
	if _, err := io.Copy(&b, br); err != nil {
		return nil, err
	}
	out.Raw = b.Bytes()
	return out, nil
}

// addressParser decodes encoded words in names, as in =?utf-8?q?Jos=C3=A9?=.
var addressParser = mail.AddressParser{WordDecoder: &mime.WordDecoder{}}
