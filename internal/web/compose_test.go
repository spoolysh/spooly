package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
	"github.com/spoolysh/spooly/internal/service"
)

// relay takes every recipient but nobody@, which it refuses for good, and
// later@, for now, and keeps what it's handed.
type relay struct {
	mu  sync.Mutex
	raw []string
}

func (r *relay) Name() string { return "test" }

func (r *relay) Send(_ context.Context, _ string, to []string, raw []byte) (*delivery.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := &delivery.Result{Reply: "2.0.0 Ok", Refused: map[string]*delivery.Error{}}
	for _, rcpt := range to {
		switch {
		case strings.HasPrefix(rcpt, "nobody@"):
			res.Refused[rcpt] = &delivery.Error{Detail: "550 5.1.1 user unknown"}
		case strings.HasPrefix(rcpt, "later@"):
			res.Refused[rcpt] = &delivery.Error{Temporary: true, Detail: "450 4.2.1 try later"}
		}
	}
	r.raw = append(r.raw, string(raw))
	return res, nil
}

func TestCompose(t *testing.T) {
	rl := &relay{}
	u := newTestUIWith(t, rl)
	resp, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}, Name: new("Hello")})
	if err != nil {
		t.Fatal(err)
	}
	if body := u.get("/compose?mailbox=hello@spooly.sh", 200); !strings.Contains(body, "Hello &lt;hello@spooly.sh&gt;") || strings.Contains(body, "Sending is off") {
		t.Errorf("compose page: %s", body)
	}

	form := url.Values{
		"mailbox": {resp.Mailbox.Id}, "to": {"Ann <ann@example.com>, nobody@example.com"},
		"subject": {"Héllo"}, "body": {"Hi\nthere"}, "key": {"k1"},
	}
	rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var sent struct{ URL, ID string }
	json.Unmarshal(rec.Body.Bytes(), &sent)
	sent.ID = strings.TrimPrefix(sent.URL, "/") // kept in Sent: the message's page
	if len(rl.raw) != 1 {
		t.Fatalf("relay got %d messages", len(rl.raw))
	}
	raw := rl.raw[0]
	for _, want := range []string{`From: "Hello" <hello@spooly.sh>`, `To: "Ann" <ann@example.com>, <nobody@example.com>`, "Subject: =?utf-8?q?H=C3=A9llo?=", "\r\n\r\nHi\r\nthere"} {
		if !strings.Contains(raw, want) {
			t.Errorf("sent message lacks %q:\n%s", want, raw)
		}
	}

	// The message shows how each delivery went, with a retry for the failed one.
	page := u.get("/"+sent.ID, 200)
	for _, want := range []string{"Delivery", "ann@example.com", ">Sent<", ">Failed<", "550 5.1.1 user unknown", "retryDelivery("} {
		if !strings.Contains(page, want) {
			t.Errorf("message page lacks %q", want)
		}
	}
	// The list too, with the recipients for whom it was.
	if list := u.get("/?folder=sent", 200); !strings.Contains(list, "1 of 2 not delivered") || !strings.Contains(list, "to ann@example.com, nobody@example.com") {
		t.Errorf("sent list: %s", list)
	}

	deliveries, err := u.db.ListDeliveries(t.Context(), db.DeliveryFilter{Status: db.DeliveryFailed})
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("failed deliveries: %v, %v", deliveries, err)
	}
	if rec := u.do("POST", "/ui/deliveries/"+deliveries[0].ID+"/retry", "", "", true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"failed"`) {
		t.Errorf("retry: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/deliveries/"+deliveries[0].ID+"/retry", "", "", false); rec.Code != 401 {
		t.Errorf("retry signed out: %d", rec.Code)
	}

	// Its page tells what happened, step by step, and the message links to it.
	if !strings.Contains(page, `href="/deliveries/`+deliveries[0].ID+`"`) {
		t.Error("the message page doesn't link the delivery")
	}
	steps := u.get("/deliveries/"+deliveries[0].ID, 200)
	for _, want := range []string{"nobody@example.com", ">Héllo<", "What happened", "550 5.1.1 user unknown", "try 1", "retried by hand", "try 2", "retryDelivery()"} {
		if !strings.Contains(steps, want) {
			t.Errorf("delivery page lacks %q", want)
		}
	}
	u.get("/deliveries/00000000000000000000000000", 404)

	form.Set("to", "not an address")
	if rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true); rec.Code != 400 || !strings.Contains(rec.Body.String(), "To:") {
		t.Errorf("bad To: %d %s", rec.Code, rec.Body)
	}
}

func TestComposeWithoutTransport(t *testing.T) {
	u := newTestUI(t)
	if body := u.get("/compose", 200); !strings.Contains(body, "Sending is off") {
		t.Error("compose page doesn't say sending is off")
	}
}

func TestDeliveriesPage(t *testing.T) {
	u := newTestUIWith(t, &relay{})
	resp, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"mailbox": {resp.Mailbox.Id}, "to": {"ann@example.com, nobody@example.com, later@example.com"}, "subject": {"Plans"}, "key": {"k1"}}
	if rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true); rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}

	all := u.get("/deliveries", 200)
	for _, want := range []string{"ann@example.com", "nobody@example.com", ">Plans<", "from hello@spooly.sh", "550 5.1.1 user unknown", "retryDelivery(", "/deliveries?status=failed"} {
		if !strings.Contains(all, want) {
			t.Errorf("deliveries page lacks %q", want)
		}
	}
	failed := u.get("/deliveries?status=failed&mailbox=hello@spooly.sh", 200)
	if !strings.Contains(failed, "nobody@example.com") || strings.Contains(failed, "ann@example.com") {
		t.Error("?status=failed shows more than the failed delivery")
	}
	deferredPage := u.get("/deliveries?status=deferred", 200)
	if !strings.Contains(deferredPage, "later@example.com") || strings.Contains(deferredPage, "ann@example.com") || strings.Contains(deferredPage, "nobody@example.com") {
		t.Error("?status=deferred shows more than the deferred delivery")
	}
	if body := u.get("/deliveries?status=bounced", 200); !strings.Contains(body, "No bounced deliveries.") {
		t.Error("an empty status doesn't say so")
	}
	u.get("/deliveries?mailbox=nobody@spooly.sh", 404)

	// Only admins see it.
	maria, err := u.db.UpsertUser(t.Context(), "g2", "maria@example.com", "Maria")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	u.sessions.SetSession(rec, maria.ID)
	req := httptest.NewRequest("GET", "/deliveries", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	out := httptest.NewRecorder()
	u.mux.ServeHTTP(out, req)
	if out.Code != 404 {
		t.Errorf("a user who isn't an admin: %d", out.Code)
	}
	if !strings.Contains(u.get("/", 200), `href="/deliveries"`) {
		t.Error("admins have no Deliveries tab")
	}
}

// Deliveries page both ways, newest first, and queued, deferred, and
// failed always have a tab.
func TestDeliveriesPaging(t *testing.T) {
	u := newTestUIWith(t, &relay{})
	resp, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	var to []string
	for i := range deliveriesPerPage + 10 {
		to = append(to, fmt.Sprintf("r%03d@example.com", i))
	}
	form := url.Values{"mailbox": {resp.Mailbox.Id}, "to": {strings.Join(to[:50], ", ")}, "key": {"k1"}}
	if rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true); rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	form.Set("to", strings.Join(to[50:], ", "))
	form.Set("key", "k2")
	if rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true); rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}

	first := u.get("/deliveries", 200)
	if !strings.Contains(first, "r059@") || strings.Contains(first, "r009@") || strings.Contains(first, "&larr; Newer") {
		t.Error("the first page isn't the newest 50")
	}
	for _, s := range []string{"queued", "deferred", "failed"} {
		if !strings.Contains(first, "?status="+s) {
			t.Errorf("no %s tab", s)
		}
	}
	if strings.Contains(first, "?status=bounced") {
		t.Error("an empty status has a tab")
	}
	older := link(t, first, "Older &rarr;")
	second := u.get(older, 200)
	if !strings.Contains(second, "r000@") || strings.Contains(second, "r010@") || strings.Contains(second, "Older &rarr;") {
		t.Error("the second page isn't the oldest 10")
	}
	back := u.get(link(t, second, "&larr; Newer"), 200)
	if !strings.Contains(back, "r010@") || !strings.Contains(back, "r059@") || !strings.Contains(back, "Older &rarr;") || strings.Contains(back, "&larr; Newer") {
		t.Error("Newer doesn't go back to the newest 50")
	}
}

// link returns the href of the link whose text is text.
func link(t *testing.T, page, text string) string {
	t.Helper()
	i := strings.Index(page, text)
	if i < 0 {
		t.Fatalf("no %q link", text)
	}
	start := strings.LastIndex(page[:i], `href="`) + len(`href="`)
	end := strings.Index(page[start:], `"`)
	return html.UnescapeString(page[start : start+end])
}

// Without a copy kept, sending goes to the delivery's page, and lists show
// the message as not kept.
func TestComposeWithoutSentCopy(t *testing.T) {
	u := newTestUIOptions(t, service.Options{Transport: &relay{}, NoSentCopy: true})
	resp, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"mailbox": {resp.Mailbox.Id}, "to": {"ann@example.com"}, "subject": {"Receipt"}, "key": {"k1"}}
	rec := u.do("POST", "/ui/messages/send", "application/x-www-form-urlencoded", form.Encode(), true)
	var sent struct{ URL string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &sent) != nil || !strings.HasPrefix(sent.URL, "/deliveries/") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if page := u.get(sent.URL, 200); !strings.Contains(page, "Receipt") || !strings.Contains(page, "not kept") || !strings.Contains(page, ">Sent<") {
		t.Errorf("delivery page: %s", page)
	}
	if list := u.get("/deliveries", 200); !strings.Contains(list, "Receipt") || !strings.Contains(list, "(not kept)") {
		t.Error("deliveries page doesn't show the subject as not kept")
	}
	if inbox := u.get("/?folder=sent", 200); strings.Contains(inbox, "Receipt") {
		t.Error("Sent shows a message sent without a copy")
	}
}
