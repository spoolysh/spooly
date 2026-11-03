package web

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
)

// relay takes every recipient but nobody@, and keeps what it's handed.
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
		if strings.HasPrefix(rcpt, "nobody@") {
			res.Refused[rcpt] = &delivery.Error{Detail: "550 5.1.1 user unknown"}
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
	var sent struct{ ID string }
	json.Unmarshal(rec.Body.Bytes(), &sent)
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
