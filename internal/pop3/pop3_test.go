package pop3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/service"
	"github.com/boxysh/boxy/internal/storage/memory"
)

// The first message has LF line endings and a line starting with ".".
const (
	message1 = "From: ann@example.com\nSubject: one\n\nline 1\n.hidden\nline 3\n"
	message2 = "Subject: two\r\n\r\nbody two\r\n"
	message3 = "Subject: three\r\n\r\nbody three\r\n"
)

type testEnv struct {
	t      *testing.T
	db     *db.DB
	addr   string // server address
	key    string // a valid API key
	ids    []string
	server *Server
}

func newTestEnv(t *testing.T, opts Options) *testEnv {
	t.Helper()
	ctx := t.Context()

	database, err := db.New(ctx, "sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	user, err := database.UpsertUser(ctx, "g1", "vh@example.com", "VH")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := database.CreateAPIKey(ctx, user.ID, "pop3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAccount(ctx, db.Account{ID: db.NewID(), Addresses: []string{"hello@boxy.sh"}}); err != nil {
		t.Fatal(err)
	}

	svc := service.New(database, memory.New(), service.Options{Admins: []string{"vh@example.com"}})
	env := &testEnv{t: t, db: database, key: key}
	for _, raw := range []string{message1, message2, message3} {
		m, err := svc.Store(ctx, service.Incoming{AccountIDOrAddress: "hello@boxy.sh", Raw: []byte(raw)})
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		env.ids = append(env.ids, m.ID)
		time.Sleep(2 * time.Millisecond) // distinct received times, so the maildrop keeps delivery order
	}

	env.server, err = New(database, svc, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env.server.authDelay = 0
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go env.server.Serve(l)
	env.addr = l.Addr().String()
	return env
}

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (e *testEnv) dial() *client {
	e.t.Helper()
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	c := &client{t: e.t, conn: conn, r: bufio.NewReader(conn)}
	if greeting := c.line(); !strings.HasPrefix(greeting, "+OK") {
		e.t.Fatalf("greeting = %q", greeting)
	}
	return c
}

func (c *client) line() string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("reading: %v", err)
	}
	if !strings.HasSuffix(line, "\r\n") {
		c.t.Fatalf("line %q doesn't end in CRLF", line)
	}
	return strings.TrimSuffix(line, "\r\n")
}

// cmd sends a command and returns the status line.
func (c *client) cmd(line string) string {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatal(err)
	}
	return c.line()
}

// ok sends a command that must succeed.
func (c *client) ok(line string) string {
	c.t.Helper()
	status := c.cmd(line)
	if !strings.HasPrefix(status, "+OK") {
		c.t.Fatalf("%s: %s", line, status)
	}
	return status
}

// multi sends a command with a multi-line response and returns its lines,
// undoing dot-stuffing.
func (c *client) multi(line string) []string {
	c.t.Helper()
	c.ok(line)
	var lines []string
	for {
		l := c.line()
		if l == "." {
			return lines
		}
		lines = append(lines, strings.TrimPrefix(l, "."))
	}
}

func (c *client) login(e *testEnv) {
	c.t.Helper()
	c.ok("USER hello@boxy.sh")
	c.ok("PASS " + e.key)
}

func TestSession(t *testing.T) {
	e := newTestEnv(t, Options{})
	c := e.dial()

	if capa := c.multi("CAPA"); !slices.Contains(capa, "USER") || !slices.Contains(capa, "UIDL") || slices.Contains(capa, "STLS") {
		t.Errorf("CAPA = %v", capa)
	}
	c.ok("USER hello@boxy.sh")
	if got := c.cmd("PASS wrong"); !strings.HasPrefix(got, "-ERR [AUTH]") {
		t.Errorf("wrong key: %s", got)
	}
	if got := c.cmd("STAT"); !strings.HasPrefix(got, "-ERR") {
		t.Errorf("STAT before login: %s", got)
	}
	// A plus address logs in to its account.
	c.ok("USER Hello+pop@boxy.sh")
	c.ok("PASS " + e.key)

	size := len(message1) + len(message2) + len(message3)
	if got := c.ok("STAT"); got != "+OK 3 "+strconv.Itoa(size) {
		t.Errorf("STAT = %s", got)
	}
	if got := c.multi("UIDL"); !slices.Equal(got, []string{"1 " + e.ids[0], "2 " + e.ids[1], "3 " + e.ids[2]}) {
		t.Errorf("UIDL = %v", got)
	}
	if got := c.ok("LIST 2"); got != "+OK 2 "+strconv.Itoa(len(message2)) {
		t.Errorf("LIST 2 = %s", got)
	}
	if got := c.cmd("RETR 9"); !strings.HasPrefix(got, "-ERR") {
		t.Errorf("RETR 9 = %s", got)
	}

	want := strings.Split(strings.TrimSuffix(message1, "\n"), "\n")
	if got := c.multi("RETR 1"); !slices.Equal(got, want) {
		t.Errorf("RETR 1 = %q, want %q", got, want)
	}
	if got := c.multi("TOP 1 1"); !slices.Equal(got, want[:4]) {
		t.Errorf("TOP 1 1 = %q, want %q", got, want[:4])
	}
	if got := c.multi("TOP 2 0"); !slices.Equal(got, []string{"Subject: two", ""}) {
		t.Errorf("TOP 2 0 = %q", got)
	}

	c.ok("DELE 2")
	if got := c.cmd("RETR 2"); !strings.HasPrefix(got, "-ERR") {
		t.Errorf("RETR of a deleted message: %s", got)
	}
	if got := c.ok("STAT"); !strings.HasPrefix(got, "+OK 2 ") {
		t.Errorf("STAT after DELE = %s", got)
	}
	c.ok("RSET")
	c.ok("DELE 2")
	c.ok("NOOP")
	c.ok("QUIT")

	// Keep (the default): the deleted message stays in the inbox; the
	// retrieved one is read; TOP alone doesn't read.
	e.wantLabels(e.ids[0], "inbox")
	e.wantLabels(e.ids[1], "inbox,unread")
	e.wantLabels(e.ids[2], "inbox,unread")
}

func (e *testEnv) wantLabels(id, want string) {
	e.t.Helper()
	m, err := e.db.GetMessage(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	if got := strings.Join(m.Labels, ","); got != want {
		e.t.Errorf("labels of %s = %s, want %s", id, got, want)
	}
}

func TestDeleteModes(t *testing.T) {
	for mode, want := range map[string]string{
		DeleteArchive: "",
		DeleteTrash:   "trash",
	} {
		t.Run(mode, func(t *testing.T) {
			e := newTestEnv(t, Options{Delete: mode})
			c := e.dial()
			c.login(e)
			c.multi("RETR 1")
			c.ok("DELE 1")
			c.ok("DELE 2")
			c.ok("QUIT")
			e.wantLabels(e.ids[0], want)
			e.wantLabels(e.ids[1], want)
			e.wantLabels(e.ids[2], "inbox,unread")
		})
	}

	// Without QUIT, nothing changes.
	e := newTestEnv(t, Options{Delete: DeleteArchive})
	c := e.dial()
	c.login(e)
	c.ok("DELE 1")
	c.conn.Close()
	time.Sleep(50 * time.Millisecond)
	e.wantLabels(e.ids[0], "inbox,unread")

	if _, err := New(e.db, nil, Options{Delete: "drop"}); err == nil {
		t.Error("New accepted an unknown delete mode")
	}
}

func TestKeepUnread(t *testing.T) {
	e := newTestEnv(t, Options{KeepUnread: true, Delete: DeleteTrash})
	c := e.dial()
	c.login(e)
	c.multi("RETR 1")
	c.multi("RETR 2")
	c.ok("DELE 2")
	c.ok("QUIT")
	e.wantLabels(e.ids[0], "inbox,unread")
	e.wantLabels(e.ids[1], "unread,trash")
}

func TestMaxMessages(t *testing.T) {
	e := newTestEnv(t, Options{MaxMessages: 2})
	c := e.dial()
	c.login(e)
	// The newest two, numbered oldest first.
	if got := c.multi("UIDL"); !slices.Equal(got, []string{"1 " + e.ids[1], "2 " + e.ids[2]}) {
		t.Errorf("UIDL = %v", got)
	}
}

// Several clients can use the same account at once, each downloading every
// message.
func TestConcurrentSessions(t *testing.T) {
	e := newTestEnv(t, Options{})
	first, second := e.dial(), e.dial()
	first.login(e)
	second.login(e)
	for _, c := range []*client{first, second} {
		if got := c.multi("RETR 1"); len(got) == 0 {
			t.Fatal("empty RETR")
		}
		c.ok("DELE 1")
	}
	first.ok("QUIT")
	second.ok("QUIT")
	e.wantLabels(e.ids[0], "inbox")

	third := e.dial()
	third.login(e)
	if got := third.ok("STAT"); !strings.HasPrefix(got, "+OK 3 ") {
		t.Errorf("STAT after others deleted = %s", got)
	}
}

func TestAuthPlain(t *testing.T) {
	e := newTestEnv(t, Options{})
	plain := base64.StdEncoding.EncodeToString([]byte("\x00hello@boxy.sh\x00" + e.key))

	c := e.dial()
	c.ok("AUTH PLAIN " + plain)
	c.ok("STAT")

	c3 := newTestEnv(t, Options{}).dial()
	if got := c3.cmd("AUTH PLAIN"); got != "+ " {
		t.Fatalf("AUTH PLAIN without initial response: %q", got)
	}
	if got := c3.cmd(base64.StdEncoding.EncodeToString([]byte("\x00hello@boxy.sh\x00wrong"))); !strings.HasPrefix(got, "-ERR [AUTH]") {
		t.Errorf("wrong key: %s", got)
	}
}

func TestTooManyFailuresCloses(t *testing.T) {
	e := newTestEnv(t, Options{})
	c := e.dial()
	for range maxAuthFailures {
		c.ok("USER hello@boxy.sh")
		c.cmd("PASS wrong")
	}
	c.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.r.ReadByte(); err == nil {
		t.Error("connection still open after failed logins")
	}
}

func TestSTLS(t *testing.T) {
	cert := selfSigned(t)
	e := newTestEnv(t, Options{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}})
	c := e.dial()

	capa := c.multi("CAPA")
	if !slices.Contains(capa, "STLS") || slices.Contains(capa, "USER") {
		t.Errorf("CAPA before TLS = %v", capa)
	}
	if got := c.cmd("USER hello@boxy.sh"); !strings.HasPrefix(got, "-ERR [AUTH]") {
		t.Errorf("USER before TLS: %s", got)
	}
	c.ok("STLS")
	tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	c.conn, c.r = tlsConn, bufio.NewReader(tlsConn)

	if capa := c.multi("CAPA"); slices.Contains(capa, "STLS") || !slices.Contains(capa, "USER") {
		t.Errorf("CAPA after TLS = %v", capa)
	}
	c.login(e)
	c.ok("STAT")
}

func TestWriteMessage(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		lines int
		want  string
	}{
		{"A: b\n\nx\n.y\n", -1, "A: b\r\n\r\nx\r\n..y\r\n"},
		{"A: b\r\n\r\nx\r\ny\r\n", 1, "A: b\r\n\r\nx\r\n"},
		{"A: b\n\nno final newline", -1, "A: b\r\n\r\nno final newline\r\n"},
		{"A: b\n", 0, "A: b\r\n"},
		{"A: b\n\n" + strings.Repeat("z", 10000) + "\n", -1, "A: b\r\n\r\n" + strings.Repeat("z", 10000) + "\r\n"},
	} {
		var buf bytes.Buffer
		w := bufio.NewWriter(&buf)
		if err := writeMessage(w, strings.NewReader(tc.raw), tc.lines); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		if buf.String() != tc.want {
			t.Errorf("writeMessage(%q, %d) = %q, want %q", tc.raw, tc.lines, buf.String(), tc.want)
		}
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
