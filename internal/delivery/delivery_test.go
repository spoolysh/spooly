package delivery

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/storage/memory"
)

func TestPrepare(t *testing.T) {
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	raw := "From: a@spooly.sh\r\nTo: b@example.com\r\nBcc: =?utf-8?q?Jos=C3=A9?= <jose@example.com>, c@example.com\r\nSubject: Hi\r\n\r\nBody\r\n"
	out, err := Prepare([]byte(raw), "spooly.sh", now)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Raw)
	if strings.Contains(got, "Bcc") || strings.Contains(got, "jose@") {
		t.Errorf("Bcc kept:\n%s", got)
	}
	if strings.Join(out.Bcc, ",") != "jose@example.com,c@example.com" {
		t.Errorf("Bcc = %v", out.Bcc)
	}
	if !strings.Contains(got, "@spooly.sh>\r\n") || !strings.Contains(got, "Date: Tue, 03 Nov 2026 10:00:00 +0000\r\n") {
		t.Errorf("no Message-Id or Date:\n%s", got)
	}
	if !strings.Contains(got, "From: a@spooly.sh\r\nTo: b@example.com\r\n") || !strings.HasSuffix(got, "\r\n\r\nBody\r\n") {
		t.Errorf("changed the rest:\n%s", got)
	}

	// A message that has them keeps its own.
	raw = "From: a@spooly.sh\r\nMessage-ID: <mine@spooly.sh>\r\nDate: Mon, 02 Nov 2026 09:00:00 +0000\r\n\r\nBody\r\n"
	out, err = Prepare([]byte(raw), "spooly.sh", now)
	if err != nil || string(out.Raw) != raw || out.Bcc != nil {
		t.Errorf("Prepare(%q) = %q, %v, %v", raw, out.Raw, out.Bcc, err)
	}
}

// fake takes everything, or nothing while down.
type fake struct {
	down bool
	sent int
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Send(context.Context, string, []string, []byte) (*Result, error) {
	if f.down {
		return nil, &Error{Temporary: true, Detail: "421 later"}
	}
	f.sent++
	return &Result{}, nil
}

// SendDue sends what's due, and leaves what isn't.
func TestSendDue(t *testing.T) {
	ctx := t.Context()
	database, err := db.New(ctx, "sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := database.CreateMailbox(ctx, db.Mailbox{ID: db.NewID(), Addresses: []string{"a@spooly.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	m := db.Message{ID: db.NewID(), MailboxID: a.ID, MailFrom: "a@spooly.sh", Folder: db.FolderSent, ReceivedAt: time.Now()}
	if _, err := store.Put(ctx, m.ID, "message/rfc822", bytes.NewReader([]byte("Subject: hi\r\n\r\nhi\r\n"))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.SendMessage(ctx, m, nil, "fake", []string{"b@example.com", "c@example.com"}); err != nil {
		t.Fatal(err)
	}

	f := &fake{down: true}
	spool := NewSpool(database, store, f)
	if err := spool.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := database.ListDeliveries(ctx, db.DeliveryFilter{})
	for _, d := range list {
		if d.Status != db.DeliveryDeferred || d.Attempts != 1 || d.NextAttemptAt == nil || time.Until(*d.NextAttemptAt) < 50*time.Second {
			t.Errorf("after a failed try: %+v", d)
		}
	}
	// Deferred to later, they're not due yet.
	f.down = false
	if err := spool.SendDue(ctx); err != nil || f.sent != 0 {
		t.Errorf("sent %d before due, %v", f.sent, err)
	}
	for _, d := range list {
		if _, err := database.RetryDelivery(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.SendDue(ctx); err != nil || f.sent != 1 {
		t.Errorf("sent %d once due, %v", f.sent, err)
	}
	list, _ = database.ListDeliveries(ctx, db.DeliveryFilter{})
	for _, d := range list {
		if d.Status != db.DeliverySent || d.Attempts != 2 {
			t.Errorf("after sending: %+v", d)
		}
	}
}
