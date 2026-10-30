// Package pop3 serves accounts over POP3 (RFC 1939), with the CAPA (RFC
// 2449), STLS (RFC 2595), and AUTH PLAIN (RFC 5034) extensions.
//
// The username is an account's address and the password is an API key, so
// any key can open any account, as over the API. The maildrop is the newest
// messages of the account's inbox, numbered oldest first when the session
// starts; the unique ID of a message is its ID. When the client quits,
// retrieved messages are marked read, unless Options.KeepUnread is set, and
// deleted ones are handled as Options.Delete says: by default they are
// kept, as in a WildDuck patched to ignore DELE.
//
// Unlike RFC 1939, a maildrop isn't locked to one session: several people's
// clients poll the same account, as with WildDuck. Deleting only changes
// labels, which concurrent sessions can safely repeat.
package pop3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/service"
)

const (
	// idleTimeout is how long a session may wait for a command; RFC 1939
	// asks for at least 10 minutes.
	idleTimeout = 10 * time.Minute
	// maxLine bounds a command line. RFC 2449 allows 255 octets, but AUTH
	// PLAIN responses carry an API key, so leave room.
	maxLine = 4096
	// maxAuthFailures closes the connection after this many failed logins.
	maxAuthFailures = 3
)

// What DELE does to a message when the client quits.
const (
	DeleteKeep    = "keep"    // nothing: the message stays in the inbox
	DeleteArchive = "archive" // remove the inbox label
	DeleteTrash   = "trash"   // remove the inbox label and add trash
)

// DefaultMaxMessages is how many of the newest inbox messages a session
// lists by default, as in WildDuck.
const DefaultMaxMessages = 250

// Options configure a Server.
type Options struct {
	// TLS enables STLS. When it's set, logins need TLS: STLS first, or a
	// listener that is already TLS. Without it, logins are allowed in
	// plaintext; only do that on a trusted network.
	TLS *tls.Config
	// Delete is DeleteKeep (the default), DeleteArchive, or DeleteTrash.
	Delete string
	// MaxMessages limits the maildrop to the newest messages; 0 means
	// DefaultMaxMessages.
	MaxMessages int
	// KeepUnread leaves read status alone: retrieving or deleting a message
	// doesn't remove unread, so it reflects only what people open elsewhere.
	KeepUnread bool
}

// Server serves POP3 sessions.
type Server struct {
	db   *db.DB
	svc  *service.Service
	tls  *tls.Config
	opts Options
	// authDelay slows down guessing after a failed login.
	authDelay time.Duration
}

// New returns a server, or an error for invalid options.
func New(database *db.DB, svc *service.Service, opts Options) (*Server, error) {
	switch opts.Delete {
	case "":
		opts.Delete = DeleteKeep
	case DeleteKeep, DeleteArchive, DeleteTrash:
	default:
		return nil, fmt.Errorf("pop3: delete must be %s, %s, or %s, not %q", DeleteKeep, DeleteArchive, DeleteTrash, opts.Delete)
	}
	if opts.MaxMessages < 0 {
		return nil, fmt.Errorf("pop3: max messages must not be negative")
	}
	if opts.MaxMessages == 0 {
		opts.MaxMessages = DefaultMaxMessages
	}
	return &Server{db: database, svc: svc, tls: opts.TLS, opts: opts, authDelay: time.Second}, nil
}

// Serve accepts connections on l until it is closed. Wrap l with
// tls.NewListener to serve POP3S (port 995).
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.serveConn(conn)
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	_, isTLS := conn.(*tls.Conn)
	sess := &session{s: s, conn: conn, tls: isTLS}
	sess.setConn(conn)

	sess.reply("+OK Spooly POP3 ready")
	for sess.w.Flush() == nil {
		conn.SetReadDeadline(time.Now().Add(idleTimeout))
		line, err := sess.readLine()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
				log.Printf("pop3 %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
		cmd, arg, _ := strings.Cut(line, " ")
		if quit := sess.handle(strings.ToUpper(cmd), arg); quit {
			sess.w.Flush()
			return
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

type session struct {
	s    *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	tls  bool

	user     string // from USER, waiting for PASS
	failures int

	account   *db.Account // set once logged in
	messages  []db.MaildropEntry
	deleted   []bool
	retrieved []bool
}

func (c *session) setConn(conn net.Conn) {
	c.conn = conn
	c.r = bufio.NewReaderSize(conn, maxLine)
	c.w = bufio.NewWriter(conn)
}

func (c *session) reply(format string, args ...any) {
	fmt.Fprintf(c.w, format+"\r\n", args...)
}

// readLine reads a command line without its line ending.
func (c *session) readLine() (string, error) {
	line, err := c.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("command line too long")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// handle runs one command and reports whether the session is over.
func (c *session) handle(cmd, arg string) (quit bool) {
	if cmd == "QUIT" {
		c.quit()
		return true
	}
	if cmd == "CAPA" {
		c.capa()
		return false
	}
	if c.account == nil {
		switch cmd {
		case "STLS":
			c.startTLS()
		case "USER":
			if c.needTLS() {
				return false
			}
			c.user = strings.TrimSpace(arg)
			c.reply("+OK send the API key as the password")
		case "PASS":
			if c.needTLS() {
				return false
			}
			if c.user == "" {
				c.reply("-ERR send USER first")
				return false
			}
			user := c.user
			c.user = ""
			return c.login(user, arg)
		case "AUTH":
			return c.auth(arg)
		default:
			c.reply("-ERR unknown command or not logged in")
		}
		return false
	}

	switch cmd {
	case "STAT":
		n, size := 0, int64(0)
		for i, m := range c.messages {
			if !c.deleted[i] {
				n++
				size += m.Size
			}
		}
		c.reply("+OK %d %d", n, size)
	case "LIST", "UIDL":
		c.list(cmd, arg)
	case "RETR":
		if i, ok := c.message(arg); ok {
			c.send(i, -1)
		}
	case "TOP":
		msg, lines, _ := strings.Cut(strings.TrimSpace(arg), " ")
		n, err := strconv.Atoi(strings.TrimSpace(lines))
		if err != nil || n < 0 {
			c.reply("-ERR usage: TOP msg lines")
			return false
		}
		if i, ok := c.message(msg); ok {
			c.send(i, n)
		}
	case "DELE":
		if i, ok := c.message(arg); ok {
			c.deleted[i] = true
			c.reply("+OK message %d deleted", i+1)
		}
	case "RSET":
		clear(c.deleted)
		c.reply("+OK %d messages", len(c.messages))
	case "NOOP":
		c.reply("+OK")
	default:
		c.reply("-ERR unknown command")
	}
	return false
}

func (c *session) capa() {
	c.reply("+OK capability list follows")
	c.reply("TOP")
	c.reply("UIDL")
	c.reply("RESP-CODES")
	c.reply("AUTH-RESP-CODE")
	c.reply("PIPELINING")
	if c.account == nil {
		if c.s.tls != nil && !c.tls {
			c.reply("STLS")
		}
		if !c.authNeedsTLS() {
			c.reply("USER")
			c.reply("SASL PLAIN")
		}
	}
	c.reply("EXPIRE NEVER")
	c.reply("IMPLEMENTATION Spooly")
	c.reply(".")
}

func (c *session) authNeedsTLS() bool {
	return c.s.tls != nil && !c.tls
}

// needTLS answers a login attempt that needs STLS first.
func (c *session) needTLS() bool {
	if c.authNeedsTLS() {
		c.reply("-ERR [AUTH] use STLS first: logins need TLS")
		return true
	}
	return false
}

func (c *session) startTLS() {
	if c.s.tls == nil || c.tls {
		c.reply("-ERR STLS not available")
		return
	}
	// Commands pipelined after STLS must not be read as if encrypted.
	if c.r.Buffered() > 0 {
		c.reply("-ERR STLS must be the last command sent")
		return
	}
	c.reply("+OK begin TLS negotiation")
	if err := c.w.Flush(); err != nil {
		return
	}
	tlsConn := tls.Server(c.conn, c.s.tls)
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		c.conn.Close()
		return
	}
	c.setConn(tlsConn)
	c.tls = true
	c.user = ""
}

// auth handles AUTH, with PLAIN as the only mechanism.
func (c *session) auth(arg string) (quit bool) {
	mech, initial, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if mech == "" {
		// The pre-SASL form lists mechanisms, as some clients expect.
		c.reply("+OK")
		if !c.authNeedsTLS() {
			c.reply("PLAIN")
		}
		c.reply(".")
		return false
	}
	if !strings.EqualFold(mech, "PLAIN") {
		c.reply("-ERR unsupported mechanism")
		return false
	}
	if c.needTLS() {
		return false
	}
	if initial == "" {
		c.reply("+ ")
		if err := c.w.Flush(); err != nil {
			return true
		}
		line, err := c.readLine()
		if err != nil {
			return true
		}
		initial = line
	}
	if initial == "*" {
		c.reply("-ERR authentication cancelled")
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(initial)
	if err != nil {
		c.reply("-ERR invalid base64")
		return false
	}
	// authzid \0 authcid \0 password; an authzid other than the user isn't
	// supported.
	parts := bytes.Split(decoded, []byte{0})
	if len(parts) != 3 || (len(parts[0]) > 0 && !bytes.Equal(parts[0], parts[1])) {
		c.reply("-ERR invalid PLAIN response")
		return false
	}
	return c.login(string(parts[1]), string(parts[2]))
}

// login authenticates address with an API key and opens its maildrop.
func (c *session) login(address, key string) (quit bool) {
	ctx := context.Background()
	account, _, err := c.s.svc.LoginMail(ctx, address, key)
	if err != nil {
		c.failures++
		time.Sleep(c.s.authDelay)
		c.reply("-ERR [AUTH] invalid address or API key")
		return c.failures >= maxAuthFailures
	}
	c.account = account

	c.messages, err = c.s.db.ListMaildrop(ctx, account.ID, db.LabelInbox, c.s.opts.MaxMessages)
	if err != nil {
		log.Printf("pop3: listing %s: %v", account.Address(), err)
		c.reply("-ERR [SYS/TEMP] can't open the maildrop")
		return true
	}
	c.deleted = make([]bool, len(c.messages))
	c.retrieved = make([]bool, len(c.messages))
	c.reply("+OK %s has %d messages", account.Address(), len(c.messages))
	return false
}

// message parses a message number, answering an error if it doesn't name
// a message that isn't deleted.
func (c *session) message(arg string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 || n > len(c.messages) || c.deleted[n-1] {
		c.reply("-ERR no such message")
		return 0, false
	}
	return n - 1, true
}

func (c *session) list(cmd, arg string) {
	line := func(i int) string {
		if cmd == "UIDL" {
			return fmt.Sprintf("%d %s", i+1, c.messages[i].ID)
		}
		return fmt.Sprintf("%d %d", i+1, c.messages[i].Size)
	}
	if strings.TrimSpace(arg) != "" {
		if i, ok := c.message(arg); ok {
			c.reply("+OK %s", line(i))
		}
		return
	}
	c.reply("+OK")
	for i := range c.messages {
		if !c.deleted[i] {
			c.reply("%s", line(i))
		}
	}
	c.reply(".")
}

// send answers RETR (bodyLines < 0) or TOP with message i.
func (c *session) send(i, bodyLines int) {
	m := c.messages[i]
	_, rc, err := c.s.svc.OpenRaw(context.Background(), m.ID)
	if err != nil {
		log.Printf("pop3: opening %s: %v", m.ID, err)
		c.reply("-ERR [SYS/TEMP] can't read the message")
		return
	}
	defer rc.Close()

	c.reply("+OK %d octets", m.Size)
	if err := writeMessage(c.w, rc, bodyLines); err != nil {
		// The response is already under way, so the connection can't
		// recover.
		log.Printf("pop3: sending %s: %v", m.ID, err)
		c.conn.Close()
		return
	}
	c.reply(".")
	if bodyLines < 0 {
		c.retrieved[i] = true
	}
}

// quit ends the session, applying reads and deletions if logged in.
func (c *session) quit() {
	if c.account == nil {
		c.reply("+OK bye")
		return
	}
	ctx := context.Background()
	failed := 0
	for i, m := range c.messages {
		var u db.MessageUpdate
		if c.deleted[i] && c.s.opts.Delete != DeleteKeep {
			u.RemoveLabels = append(u.RemoveLabels, db.LabelInbox)
			if c.s.opts.Delete == DeleteTrash {
				u.AddLabels = []string{db.LabelTrash}
			}
		}
		if (c.retrieved[i] || len(u.RemoveLabels) > 0) && !c.s.opts.KeepUnread {
			u.RemoveLabels = append(u.RemoveLabels, db.LabelUnread)
		}
		if len(u.RemoveLabels) == 0 && len(u.AddLabels) == 0 {
			continue
		}
		if _, err := c.s.db.UpdateMessage(ctx, m.ID, u); err != nil && !errors.Is(err, db.ErrMessageNotFound) {
			log.Printf("pop3: updating %s: %v", m.ID, err)
			failed++
		}
	}
	if failed > 0 {
		c.reply("-ERR [SYS/TEMP] %d messages weren't updated", failed)
		return
	}
	c.reply("+OK bye")
}

// writeMessage copies a raw message to w as a POP3 multi-line response body:
// CRLF line endings, with lines starting with "." dot-stuffed. bodyLines >= 0
// stops after the header and that many body lines, for TOP. The terminating
// "." line is the caller's.
func writeMessage(w *bufio.Writer, r io.Reader, bodyLines int) error {
	br := bufio.NewReader(r)
	atLineStart, inBody, lines := true, false, 0
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			complete := chunk[len(chunk)-1] == '\n'
			content := chunk
			if complete {
				content = bytes.TrimSuffix(bytes.TrimSuffix(chunk, []byte("\n")), []byte("\r"))
			}
			if atLineStart {
				if inBody && bodyLines >= 0 && lines >= bodyLines {
					return nil
				}
				if len(content) > 0 && content[0] == '.' {
					w.WriteByte('.')
				}
			}
			w.Write(content)
			if complete {
				w.WriteString("\r\n")
				if inBody {
					lines++
				} else if atLineStart && len(content) == 0 {
					inBody = true // the blank line that ends the header
				}
			}
			atLineStart = complete
		}
		if err == io.EOF {
			if !atLineStart {
				w.WriteString("\r\n")
			}
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}
