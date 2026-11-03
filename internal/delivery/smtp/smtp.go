// Package smtp sends mail through an SMTP relay, such as Google Workspace's
// SMTP relay service, or Postfix.
package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strconv"
	"time"

	"github.com/spoolysh/spooly/internal/delivery"
)

// Options configure a relay.
type Options struct {
	// Name names the transport in deliveries; "smtp" if empty.
	Name string
	Host string
	Port int
	// ImplicitTLS speaks TLS from the start, as on port 465; otherwise the
	// relay must offer STARTTLS. Mail never goes in the clear.
	ImplicitTLS bool
	// Username and Password log in with AUTH PLAIN, if Username is set.
	Username, Password string
	// Hello is the name to greet the relay with; "localhost" if empty.
	Hello string
	// TLS is the TLS configuration, for tests; nil verifies Host.
	TLS *tls.Config
	// Timeout bounds a whole send; a minute if 0.
	Timeout time.Duration
}

// Transport sends through the relay of its options.
type Transport struct {
	opts Options
}

func New(opts Options) *Transport {
	if opts.Name == "" {
		opts.Name = "smtp"
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	if opts.TLS == nil {
		opts.TLS = &tls.Config{ServerName: opts.Host}
	}
	return &Transport{opts: opts}
}

func (t *Transport) Name() string { return t.opts.Name }

// Send hands raw to the relay. The relay may refuse each recipient on its
// own; a refusal of the sender or the message is the whole attempt's.
// Before that, what fails, such as connecting or logging in, is temporary:
// fixed, the message goes.
func (t *Transport) Send(ctx context.Context, from string, to []string, raw []byte) (*delivery.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opts.Timeout)
	defer cancel()
	c, err := t.dial(ctx)
	if err != nil {
		return nil, temporary(err)
	}
	defer c.Close()

	if err := c.Mail(from); err != nil {
		return nil, refusal(err)
	}
	result := &delivery.Result{Refused: map[string]*delivery.Error{}}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			result.Refused[rcpt] = refusal(err)
		}
	}
	if len(result.Refused) == len(to) {
		return result, nil
	}
	reply, err := data(c, raw)
	if err != nil {
		return nil, refusal(err)
	}
	result.MessageID, result.Reply = queuedID(reply), reply
	c.Quit()
	return result, nil
}

// dial connects and greets the relay, secures the connection, and logs in.
func (t *Transport) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(t.opts.Host, strconv.Itoa(t.opts.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if t.opts.ImplicitTLS {
		conn = tls.Client(conn, t.opts.TLS)
	}
	c, err := smtp.NewClient(conn, t.opts.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if t.opts.Hello != "" {
		if err := c.Hello(t.opts.Hello); err != nil {
			c.Close()
			return nil, err
		}
	}
	if !t.opts.ImplicitTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, fmt.Errorf("%s offers no STARTTLS: mail would go in the clear", addr)
		}
		if err := c.StartTLS(t.opts.TLS); err != nil {
			c.Close()
			return nil, err
		}
	}
	if t.opts.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", t.opts.Username, t.opts.Password, t.opts.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("logging in: %w", err)
		}
	}
	return c, nil
}

// data sends the message and returns the relay's reply, which Client.Data
// drops: it often names the relay's ID for the message.
func data(c *smtp.Client, raw []byte) (string, error) {
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return "", err
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(354)
	c.Text.EndResponse(id)
	if err != nil {
		return "", err
	}
	w := c.Text.DotWriter()
	if _, err := w.Write(raw); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	_, reply, err := c.Text.ReadResponse(250)
	return reply, err
}

// queuedID finds the relay's ID for a message in its reply to DATA: the
// Message-ID it assigned, as in "Ok <id@host>", or Postfix's "queued as
// 4F9A2C0D1E". Empty if it names none.
func queuedID(reply string) string {
	if m := replyMessageID.FindStringSubmatch(reply); m != nil {
		return m[1]
	}
	if m := queuedAs.FindStringSubmatch(reply); m != nil {
		return m[1]
	}
	return ""
}

var (
	replyMessageID = regexp.MustCompile(`<([^<>\s]+@[^<>\s]+)>`)
	queuedAs       = regexp.MustCompile(`(?i)queued as ([0-9A-Za-z._-]+)`)
)

// refusal makes an SMTP error a delivery error: temporary if the reply was
// 4xx or there was none, as when the connection dropped.
func refusal(err error) *delivery.Error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return &delivery.Error{Temporary: reply.Code < 500, Detail: fmt.Sprintf("%d %s", reply.Code, reply.Msg)}
	}
	return temporary(err)
}

func temporary(err error) *delivery.Error {
	return &delivery.Error{Temporary: true, Detail: err.Error()}
}
