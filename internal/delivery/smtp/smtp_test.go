package smtp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spoolysh/spooly/internal/delivery"
)

// relay is a fake SMTP relay. It refuses temp@ for now and nobody@ for
// good, takes the rest, and keeps what it took.
type relay struct {
	addr     string
	implicit bool // TLS from the start, else STARTTLS if offered
	starttls bool
	cert     tls.Certificate
	mu       sync.Mutex
	from     string
	rcpts    []string
	data     string
	listener net.Listener
}

func newRelay(t *testing.T, implicit, starttls bool) (*relay, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSigned(t)
	r := &relay{implicit: implicit, starttls: starttls, cert: cert}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	r.listener, r.addr = l, l.Addr().String()
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go r.serve(conn)
		}
	}()
	return r, pool
}

func (r *relay) serve(conn net.Conn) {
	defer conn.Close()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	say := func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
	say("220 fake ESMTP")
	secure := r.implicit
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.Fields(cmd + " ")[0])
		switch {
		case verb == "EHLO":
			if r.starttls && !secure {
				say("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 AUTH PLAIN")
			}
		case verb == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{r.cert}})
			if tc.Handshake() != nil {
				return
			}
			conn, secure = tc, true
			rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		case verb == "AUTH":
			creds, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, "AUTH PLAIN "))
			if string(creds) != "\x00user\x00secret" {
				say("535 5.7.8 bad credentials")
				continue
			}
			say("235 2.7.0 ok")
		case verb == "MAIL":
			r.mu.Lock()
			r.from, r.rcpts = cmd, nil
			r.mu.Unlock()
			say("250 2.1.0 ok")
		case verb == "RCPT":
			switch {
			case strings.Contains(cmd, "<temp@"):
				say("450 4.2.1 try later")
			case strings.Contains(cmd, "<nobody@"):
				say("550 5.1.1 user unknown")
			default:
				r.mu.Lock()
				r.rcpts = append(r.rcpts, cmd)
				r.mu.Unlock()
				say("250 2.1.5 ok")
			}
		case verb == "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			r.mu.Lock()
			r.data = b.String()
			r.mu.Unlock()
			say("250 2.0.0 Ok: queued as ABC123")
		case verb == "QUIT":
			say("221 bye")
			return
		default:
			say("502 5.5.2 unknown")
		}
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func transport(t *testing.T, r *relay, pool *x509.CertPool, password string) *Transport {
	t.Helper()
	host, port, _ := net.SplitHostPort(r.addr)
	p, _ := strconv.Atoi(port)
	return New(Options{
		Host: host, Port: p, ImplicitTLS: r.implicit, Username: "user", Password: password,
		TLS: &tls.Config{ServerName: host, RootCAs: pool}, Timeout: 5 * time.Second,
	})
}

const message = "From: a@spooly.sh\r\nSubject: hi\r\n\r\nhello\r\n"

func TestSend(t *testing.T) {
	for _, implicit := range []bool{true, false} {
		r, pool := newRelay(t, implicit, true)
		tr := transport(t, r, pool, "secret")
		res, err := tr.Send(context.Background(), "a@spooly.sh",
			[]string{"b@example.com", "temp@example.com", "nobody@example.com", "c@example.com"}, []byte(message))
		if err != nil {
			t.Fatalf("implicit %t: %v", implicit, err)
		}
		if res.MessageID != "ABC123" || !strings.Contains(res.Reply, "queued as ABC123") {
			t.Errorf("implicit %t: result %+v", implicit, res)
		}
		if e := res.Refused["temp@example.com"]; e == nil || !e.Temporary || !strings.HasPrefix(e.Detail, "450 ") {
			t.Errorf("temp@: %+v", e)
		}
		if e := res.Refused["nobody@example.com"]; e == nil || e.Temporary || !strings.HasPrefix(e.Detail, "550 ") {
			t.Errorf("nobody@: %+v", e)
		}
		if len(res.Refused) != 2 {
			t.Errorf("refused %v", res.Refused)
		}
		r.mu.Lock()
		if len(r.rcpts) != 2 || !strings.Contains(r.from, "<a@spooly.sh>") || r.data != message {
			t.Errorf("relay got from %q, rcpts %q, data %q", r.from, r.rcpts, r.data)
		}
		r.mu.Unlock()
	}
}

func TestEveryRecipientRefused(t *testing.T) {
	r, pool := newRelay(t, true, false)
	res, err := transport(t, r, pool, "secret").Send(context.Background(), "a@spooly.sh", []string{"nobody@example.com"}, []byte(message))
	if err != nil || len(res.Refused) != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data != "" {
		t.Error("sent data with no recipient")
	}
}

// What goes wrong before the relay looks at the message is temporary: fixed,
// the message goes.
func TestSetupErrorsAreTemporary(t *testing.T) {
	r, pool := newRelay(t, false, false) // no STARTTLS
	_, err := transport(t, r, pool, "secret").Send(context.Background(), "a@spooly.sh", []string{"b@example.com"}, []byte(message))
	var e *delivery.Error
	if !errors.As(err, &e) || !e.Temporary || !strings.Contains(e.Detail, "STARTTLS") {
		t.Errorf("without STARTTLS: %v", err)
	}

	// On this machine, it may go in the clear.
	tr := transport(t, r, pool, "")
	tr.opts.Username, tr.opts.Plaintext = "", true
	if _, err := tr.Send(context.Background(), "a@spooly.sh", []string{"b@example.com"}, []byte(message)); err != nil {
		t.Errorf("in the clear on this machine: %v", err)
	}

	r, pool = newRelay(t, true, false)
	_, err = transport(t, r, pool, "wrong").Send(context.Background(), "a@spooly.sh", []string{"b@example.com"}, []byte(message))
	if !errors.As(err, &e) || !e.Temporary || !strings.Contains(e.Detail, "535") {
		t.Errorf("wrong password: %v", err)
	}

	tr = New(Options{Host: "127.0.0.1", Port: 1, ImplicitTLS: true, Timeout: time.Second})
	if _, err := tr.Send(context.Background(), "a@spooly.sh", []string{"b@example.com"}, []byte(message)); !errors.As(err, &e) || !e.Temporary {
		t.Errorf("unreachable: %v", err)
	}
}

func TestIsLocal(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "smtp-relay.gmail.com": false, "10.0.0.5": false} {
		if IsLocal(host) != want {
			t.Errorf("IsLocal(%q) = %t", host, !want)
		}
	}
}

func TestQueuedID(t *testing.T) {
	for reply, want := range map[string]string{
		"2.0.0 Ok: queued as 4F9A2C0D1E":      "4F9A2C0D1E",
		"2.0.0 Ok <0101abc@smtp.example.net>": "0101abc@smtp.example.net",
		"2.0.0 OK 1700000000 x1-20020a17090":  "",
	} {
		if got := queuedID(reply); got != want {
			t.Errorf("queuedID(%q) = %q, want %q", reply, got, want)
		}
	}
}
