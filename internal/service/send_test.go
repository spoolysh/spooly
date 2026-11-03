package service

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	spoolyv1alpha1 "github.com/spoolysh/spooly/gen/spooly/v1alpha1"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
)

// fakeTransport keeps what it's handed, and refuses what refuse says.
type fakeTransport struct {
	mu     sync.Mutex
	sends  []fakeSend
	refuse map[string]*delivery.Error // by recipient
	down   bool                       // fails every attempt, for now
}

type fakeSend struct {
	from string
	to   []string
	raw  string
}

func (f *fakeTransport) Name() string { return "fake" }

func (f *fakeTransport) Send(_ context.Context, from string, to []string, raw []byte) (*delivery.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, &delivery.Error{Temporary: true, Detail: "421 4.3.0 try later"}
	}
	res := &delivery.Result{MessageID: "fake-1", Reply: "2.0.0 Ok", Refused: map[string]*delivery.Error{}}
	var took []string
	for _, rcpt := range to {
		if e := f.refuse[rcpt]; e != nil {
			res.Refused[rcpt] = e
		} else {
			took = append(took, rcpt)
		}
	}
	if len(took) > 0 {
		f.sends = append(f.sends, fakeSend{from, took, string(raw)})
	}
	return res, nil
}

func (f *fakeTransport) sent() []fakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sends)
}

const outgoing = "From: Hello <hello@spooly.sh>\r\nTo: Ann <ann@example.com>\r\nCc: bob@example.com\r\n" +
	"Bcc: secret@example.com\r\nSubject: Hi\r\n\r\nHello there.\r\n"

func newSender(t *testing.T) (*Service, context.Context, *fakeTransport) {
	t.Helper()
	return newSenderWith(t, Options{})
}

func newSenderWith(t *testing.T, opts Options) (*Service, context.Context, *fakeTransport) {
	t.Helper()
	ft := &fakeTransport{refuse: map[string]*delivery.Error{}}
	opts.Transport = ft
	srv, svc, ctx := newTestServerWith(t, opts)
	createMailbox(t, srv, "hello@spooly.sh")
	return svc, ctx, ft
}

func send(t *testing.T, svc *Service, ctx context.Context, raw string, rcptTo ...string) *spoolyv1alpha1.SendMessageResponse {
	t.Helper()
	resp, err := svc.SendMessage(ctx, &spoolyv1alpha1.SendMessageRequest{RcptTo: rcptTo, Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func statuses(list []*spoolyv1alpha1.Delivery) map[string]string {
	out := map[string]string{}
	for _, d := range list {
		out[d.Recipient] = d.Status
	}
	return out
}

func TestSendMessage(t *testing.T) {
	svc, ctx, ft := newSender(t)
	resp := send(t, svc, ctx, outgoing)

	m := resp.Message
	if m.Folder != db.FolderSent || !m.Seen || m.MailFrom != "hello@spooly.sh" {
		t.Errorf("stored as %s, seen %t, mail_from %q", m.Folder, m.Seen, m.MailFrom)
	}
	want := map[string]string{"ann@example.com": "sent", "bob@example.com": "sent", "secret@example.com": "sent"}
	if got := statuses(resp.Deliveries); !maps(got, want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
	for _, d := range resp.Deliveries {
		if d.Transport != "fake" || d.TransportMessageId != "fake-1" || d.Attempts != 1 || d.Detail != "2.0.0 Ok" || d.MessageId != m.Id {
			t.Errorf("delivery %+v", d)
		}
	}

	sends := ft.sent()
	if len(sends) != 1 {
		t.Fatalf("%d sends, want 1", len(sends))
	}
	s := sends[0]
	if s.from != "hello@spooly.sh" || !slices.Equal(s.to, []string{"ann@example.com", "bob@example.com", "secret@example.com"}) {
		t.Errorf("sent from %q to %v", s.from, s.to)
	}
	// Bcc goes by the envelope only; Message-ID and Date are added.
	if strings.Contains(s.raw, "Bcc:") || strings.Contains(s.raw, "secret@") {
		t.Errorf("sent raw shows Bcc:\n%s", s.raw)
	}
	if !strings.Contains(s.raw, "Message-Id: <") || !strings.Contains(s.raw, "@spooly.sh>") || !strings.Contains(s.raw, "Date: ") {
		t.Errorf("sent raw lacks Message-Id or Date:\n%s", s.raw)
	}
	// What's stored is what was sent.
	r, err := svc.store.Get(ctx, m.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if stored, _ := io.ReadAll(r); string(stored) != s.raw {
		t.Errorf("stored:\n%s\nsent:\n%s", stored, s.raw)
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSendMessageRcptTo(t *testing.T) {
	svc, ctx, ft := newSender(t)
	resp := send(t, svc, ctx, outgoing, "Only@Example.com")
	if got := statuses(resp.Deliveries); !maps(got, map[string]string{"only@example.com": "sent"}) {
		t.Errorf("deliveries %v", got)
	}
	if s := ft.sent(); len(s) != 1 || !slices.Equal(s[0].to, []string{"only@example.com"}) {
		t.Errorf("sent %+v", s)
	}
}

func TestSendMessageRefused(t *testing.T) {
	svc, ctx, ft := newSender(t)
	ft.refuse["ann@example.com"] = &delivery.Error{Detail: "550 5.1.1 user unknown"}
	ft.refuse["bob@example.com"] = &delivery.Error{Temporary: true, Detail: "450 4.2.1 try later"}
	resp := send(t, svc, ctx, outgoing)
	want := map[string]string{"ann@example.com": "failed", "bob@example.com": "deferred", "secret@example.com": "sent"}
	if got := statuses(resp.Deliveries); !maps(got, want) {
		t.Fatalf("deliveries %v, want %v", got, want)
	}
	var deferred, failed *spoolyv1alpha1.Delivery
	for _, d := range resp.Deliveries {
		switch d.Status {
		case "deferred":
			deferred = d
		case "failed":
			failed = d
		}
	}
	if deferred.NextAttemptAt == nil || deferred.Detail != "450 4.2.1 try later" || failed.NextAttemptAt != nil || failed.Detail != "550 5.1.1 user unknown" {
		t.Errorf("deferred %+v, failed %+v", deferred, failed)
	}

	// Retried once the transport takes it.
	delete(ft.refuse, "bob@example.com")
	r, err := svc.RetryDelivery(ctx, &spoolyv1alpha1.RetryDeliveryRequest{Id: deferred.Id})
	if err != nil || r.Delivery.Status != "sent" || r.Delivery.Attempts != 2 {
		t.Fatalf("retry: %+v, %v", r, err)
	}
	if s := ft.sent(); len(s) != 2 || !slices.Equal(s[1].to, []string{"bob@example.com"}) {
		t.Errorf("sent %+v", s)
	}
	// Its events tell what happened, the retry by hand included.
	got, err := svc.GetDelivery(ctx, &spoolyv1alpha1.GetDeliveryRequest{Id: deferred.Id})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, e := range got.Events {
		steps = append(steps, fmt.Sprintf("%s %d %s", e.Status, e.Attempt, e.Detail))
	}
	wantSteps := "queued 0 ; deferred 1 450 4.2.1 try later; queued 0 retried by hand; sent 2 2.0.0 Ok"
	if strings.Join(steps, "; ") != wantSteps || got.Events[3].TransportMessageId != "fake-1" {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(steps, "; "), wantSteps)
	}

	// Only deferred and failed ones are retried.
	if _, err := svc.RetryDelivery(ctx, &spoolyv1alpha1.RetryDeliveryRequest{Id: deferred.Id}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("retrying a sent delivery: %v", err)
	}
}

// A transport that's down defers every delivery, and they go once it's up.
func TestSendMessageTransportDown(t *testing.T) {
	svc, ctx, ft := newSender(t)
	ft.down = true
	resp := send(t, svc, ctx, outgoing)
	for _, d := range resp.Deliveries {
		if d.Status != "deferred" || d.Detail != "421 4.3.0 try later" {
			t.Errorf("delivery %+v", d)
		}
	}
	ft.down = false
	for _, d := range resp.Deliveries {
		if _, err := svc.RetryDelivery(ctx, &spoolyv1alpha1.RetryDeliveryRequest{Id: d.Id}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := svc.ListDeliveries(ctx, &spoolyv1alpha1.ListDeliveriesRequest{MessageIds: []string{resp.Message.Id}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Deliveries {
		if d.Status != "sent" {
			t.Errorf("after retry: %+v", d)
		}
	}
}

func TestSendMessageIdempotent(t *testing.T) {
	svc, ctx, ft := newSender(t)
	m1, d1, err := svc.Send(ctx, []byte(outgoing), nil, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	m2, d2, err := svc.Send(ctx, []byte(outgoing), nil, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if m1.ID != m2.ID || len(d1) != 3 || len(d2) != 3 || len(ft.sent()) != 1 {
		t.Errorf("repeat: %s, %s; %d and %d deliveries; %d sends", m1.ID, m2.ID, len(d1), len(d2), len(ft.sent()))
	}
}

func TestSendMessageInvalid(t *testing.T) {
	svc, ctx, ft := newSender(t)
	for name, tc := range map[string]struct {
		raw  string
		want connect.Code
	}{
		"no mailbox's From": {strings.Replace(outgoing, "hello@spooly.sh", "ceo@spooly.sh", 1), connect.CodeNotFound},
		"no From":           {"To: ann@example.com\r\nSubject: Hi\r\n\r\nHello.\r\n", connect.CodeInvalidArgument},
		"no recipients":     {"From: hello@spooly.sh\r\nSubject: Hi\r\n\r\nHello.\r\n", connect.CodeInvalidArgument},
	} {
		_, err := svc.SendMessage(ctx, &spoolyv1alpha1.SendMessageRequest{Raw: []byte(tc.raw)})
		if connect.CodeOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
	// A plus address of the mailbox's is its own.
	send(t, svc, ctx, strings.Replace(outgoing, "hello@spooly.sh", "hello+news@spooly.sh", 1))
	if len(ft.sent()) != 1 {
		t.Errorf("sent %d from a plus address", len(ft.sent()))
	}
}

func TestSendMessageOff(t *testing.T) {
	srv, svc, ctx := newTestServerWith(t, Options{})
	createMailbox(t, srv, "hello@spooly.sh")
	_, err := svc.SendMessage(ctx, &spoolyv1alpha1.SendMessageRequest{Raw: []byte(outgoing)})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "MAIL_TRANSPORT") {
		t.Errorf("without a transport: %v", err)
	}
}

func TestListDeliveries(t *testing.T) {
	svc, ctx, ft := newSender(t)
	ft.refuse["ann@example.com"] = &delivery.Error{Detail: "550 no"}
	first := send(t, svc, ctx, outgoing)
	second := send(t, svc, ctx, outgoing)

	list := func(req *spoolyv1alpha1.ListDeliveriesRequest) []*spoolyv1alpha1.Delivery {
		t.Helper()
		resp, err := svc.ListDeliveries(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Deliveries
	}
	if got := list(&spoolyv1alpha1.ListDeliveriesRequest{MessageIds: []string{first.Message.Id}}); len(got) != 3 {
		t.Errorf("one message's: %d", len(got))
	}
	if got := list(&spoolyv1alpha1.ListDeliveriesRequest{MessageIds: []string{first.Message.Id, second.Message.Id}}); len(got) != 6 {
		t.Errorf("two messages': %d", len(got))
	}
	failed := "failed"
	if got := list(&spoolyv1alpha1.ListDeliveriesRequest{Status: &failed}); len(got) != 2 {
		t.Errorf("failed: %d", len(got))
	}
	mailbox, limit := "hello@spooly.sh", int32(4)
	page := list(&spoolyv1alpha1.ListDeliveriesRequest{MailboxIdOrAddress: &mailbox, Limit: &limit})
	rest := list(&spoolyv1alpha1.ListDeliveriesRequest{MailboxIdOrAddress: &mailbox, After: &page[3].Id})
	if len(page) != 4 || len(rest) != 2 {
		t.Errorf("pages of %d and %d", len(page), len(rest))
	}

	// Deleting a message deletes its deliveries.
	if _, err := svc.DeleteMessage(ctx, &spoolyv1.DeleteMessageRequest{Id: first.Message.Id}); err != nil {
		t.Fatal(err)
	}
	if got := list(&spoolyv1alpha1.ListDeliveriesRequest{MessageIds: []string{first.Message.Id}}); len(got) != 0 {
		t.Errorf("after deleting the message: %d", len(got))
	}
}

// A copy of a sent message that comes back to its mailbox, as Gmail's Sent
// copy or a mail client's, is that message; elsewhere, it's a new one.
func TestSentCopy(t *testing.T) {
	svc, ctx, ft := newSender(t)
	sent := send(t, svc, ctx, outgoing).Message
	sentCopy := ft.sent()[0].raw
	if !strings.Contains(sentCopy, "X-Spooly-Message-Id: "+sent.Id+"\r\n") {
		t.Fatalf("sent message lacks its ID:\n%s", sentCopy)
	}

	back, err := svc.Store(ctx, Incoming{MailboxIDOrAddress: "hello@spooly.sh", Raw: []byte(sentCopy), Folder: db.FolderSent})
	if err != nil || back.ID != sent.Id {
		t.Errorf("the sentCopy is %v, %v; want the sent message %s", back, err, sent.Id)
	}
	list, err := svc.db.ListMessages(ctx, db.MessageFilter{Limit: 10})
	if err != nil || len(list) != 1 {
		t.Errorf("%d messages after the sentCopy came back, %v", len(list), err)
	}

	if _, err := svc.db.CreateMailbox(ctx, db.Mailbox{ID: db.NewID(), Addresses: []string{"other@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.Store(ctx, Incoming{MailboxIDOrAddress: "other@spooly.sh", Raw: []byte(sentCopy)})
	if err != nil || other.ID == sent.Id {
		t.Errorf("in another mailbox, the sentCopy is %v, %v", other, err)
	}
}

// RunJobs sends what's due each time, and prunes at most hourly.
func TestRunJobs(t *testing.T) {
	svc, ctx, ft := newSender(t)
	ft.down = true
	resp := send(t, svc, ctx, outgoing)
	ft.down = false
	runs := func(only ...string) string {
		t.Helper()
		list, err := svc.RunJobs(ctx, only...)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, run := range list {
			out = append(out, fmt.Sprintf("%s %t %s", run.Job, run.Ran, run.Detail))
		}
		return strings.Join(out, "; ")
	}

	// Deferred a minute ahead, they're not due yet; prune is, the first time.
	if got := runs(); got != "deliveries true tried 0 deliveries; prune true expunged deleted messages and pruned old events" {
		t.Errorf("first run: %s", got)
	}
	past := time.Now().Add(-time.Second)
	for _, d := range resp.Deliveries {
		if _, err := svc.db.RecordDelivery(ctx, d.Id, db.DeliveryAttempt{Status: db.DeliveryDeferred, NextAttemptAt: &past}); err != nil {
			t.Fatal(err)
		}
	}
	if got := runs(); got != "deliveries true tried 3 deliveries; prune false ran within the last 1h0m0s" || len(ft.sent()) != 1 {
		t.Errorf("second run: %s; %d sends", got, len(ft.sent()))
	}
	if got := runs(JobPrune); got != "prune false ran within the last 1h0m0s" {
		t.Errorf("prune only: %s", got)
	}
}

// A scheduler calls POST /tick.
func TestTick(t *testing.T) {
	srv, _, _ := newTestServerWith(t, Options{})
	var out struct {
		Runs []JobRun `json:"runs"`
	}
	call(t, srv, "POST", "/tick?job=deliveries", "", "", 200, &out)
	if len(out.Runs) != 1 || out.Runs[0].Job != "deliveries" || out.Runs[0].Ran || !strings.Contains(out.Runs[0].Detail, "MAIL_TRANSPORT") {
		t.Errorf("POST /tick?job=deliveries = %+v", out.Runs)
	}
	call(t, srv, "POST", "/tick", "", "", 200, &out)
	if len(out.Runs) != 2 {
		t.Errorf("POST /tick = %+v", out.Runs)
	}
	call(t, srv, "POST", "/tick?job=nope", "", "", 400, nil)
	call(t, srv, "GET", "/tick", "", "", 405, nil)
}

// With no copy kept in Sent, nothing lands in the mailbox: the raw source
// is kept only until its deliveries are done, and they keep its sender and
// subject.
func TestSendWithoutSentCopy(t *testing.T) {
	svc, ctx, ft := newSenderWith(t, Options{NoSentCopy: true})
	ft.refuse["bob@example.com"] = &delivery.Error{Temporary: true, Detail: "450 4.2.1 try later"}
	resp := send(t, svc, ctx, outgoing)
	if resp.Message != nil {
		t.Errorf("a message came back: %v", resp.Message.Id)
	}
	if list, err := svc.db.ListMessages(ctx, db.MessageFilter{Limit: 10}); err != nil || len(list) != 0 {
		t.Errorf("%d messages in the mailbox, %v", len(list), err)
	}
	want := map[string]string{"ann@example.com": "sent", "bob@example.com": "deferred", "secret@example.com": "sent"}
	if got := statuses(resp.Deliveries); !maps(got, want) {
		t.Fatalf("deliveries %v, want %v", got, want)
	}
	id := resp.Deliveries[0].MessageId
	for _, d := range resp.Deliveries {
		if d.MailFrom != "hello@spooly.sh" || d.Subject != "Hi" || d.MessageId != id {
			t.Errorf("delivery %+v", d)
		}
	}
	if strings.Contains(ft.sent()[0].raw, "X-Spooly-Message-Id") {
		t.Error("an unkept message carries its ID")
	}

	// Kept while bob's delivery waits, and gone once it's done.
	stored := func() bool {
		t.Helper()
		r, err := svc.store.Get(ctx, id)
		if err == nil {
			r.Close()
		}
		return err == nil
	}
	if !stored() {
		t.Fatal("raw source gone while a delivery waits")
	}
	var bob string
	for _, d := range resp.Deliveries {
		if d.Recipient == "bob@example.com" {
			bob = d.Id
		}
	}
	ft.refuse["bob@example.com"] = &delivery.Error{Detail: "550 5.1.1 user unknown"}
	if r, err := svc.RetryDelivery(ctx, &spoolyv1alpha1.RetryDeliveryRequest{Id: bob}); err != nil || r.Delivery.Status != "failed" {
		t.Fatalf("retry: %+v, %v", r, err)
	}
	if stored() {
		t.Error("raw source kept after the last delivery was done")
	}
	// Retried after that, there's nothing to send.
	if r, err := svc.RetryDelivery(ctx, &spoolyv1alpha1.RetryDeliveryRequest{Id: bob}); err != nil || r.Delivery.Status != "failed" || r.Delivery.Detail != "the message is no longer kept" {
		t.Errorf("retry after release: %+v, %v", r, err)
	}

	// A repeat of a send sends nothing again.
	sends := len(ft.sent())
	_, first, err := svc.Send(ctx, []byte(outgoing), []string{"carol@example.com"}, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	m, again, err := svc.Send(ctx, []byte(outgoing), []string{"carol@example.com"}, "key-1")
	if err != nil || m != nil || len(again) != 1 || again[0].ID != first[0].ID || len(ft.sent()) != sends+1 {
		t.Errorf("repeat: %v, %v, %v; %d sends", m, again, err, len(ft.sent())-sends)
	}
}
