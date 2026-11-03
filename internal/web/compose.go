package web

import (
	"bytes"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"strings"

	"github.com/spoolysh/spooly/internal/db"
)

type composeData struct {
	layoutData
	Mailboxes []db.Mailbox
	Mailbox   string // the address to send from, preselected
	Sends     bool   // whether there's a transport to send with
}

// ComposePage writes a new message, from ?mailbox= if given.
func (h *Handler) ComposePage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	mailboxes, err := h.visibleMailboxes(r, user)
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	h.render(w, "compose", composeData{
		layoutData: layoutData{ActiveTab: "messages", Title: "New message", User: user, Admin: h.svc.IsAdmin(user.Email)},
		Mailboxes:  mailboxes,
		Mailbox:    strings.ToLower(r.URL.Query().Get("mailbox")),
		Sends:      h.svc.Sends(),
	})
}

// sendMessage sends a plain-text message from a form: mailbox (its ID), to,
// cc, bcc, subject, body, and key, an idempotency key the page makes, so a
// second click doesn't send it twice.
func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	mailboxID := r.FormValue("mailbox")
	ok, err := h.canAccess(r, mailboxID)
	if err != nil {
		writeError(w, "sendMessage", err)
		return
	}
	if !ok {
		http.Error(w, "mailbox not found", http.StatusNotFound)
		return
	}
	a, err := h.db.GetMailbox(r.Context(), mailboxID)
	if err != nil {
		writeError(w, "sendMessage", err)
		return
	}
	var recipients [3][]*mail.Address
	for i, field := range []string{"To", "Cc", "Bcc"} {
		if v := strings.TrimSpace(r.FormValue(strings.ToLower(field))); v != "" {
			if recipients[i], err = mail.ParseAddressList(v); err != nil {
				http.Error(w, fmt.Sprintf("%s: %v", field, err), http.StatusBadRequest)
				return
			}
		}
	}
	raw := buildMessage(&mail.Address{Name: a.Name, Address: a.Address()}, recipients, r.FormValue("subject"), r.FormValue("body"))
	m, _, err := h.svc.Send(r.Context(), raw, nil, r.FormValue("key"))
	if err != nil {
		writeError(w, "sendMessage", err)
		return
	}
	writeJSON(w, map[string]string{"id": m.ID})
}

// buildMessage writes a plain-text message: to, cc, and bcc are its
// recipients, in that order.
func buildMessage(from *mail.Address, recipients [3][]*mail.Address, subject, body string) []byte {
	var b bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&b, "%s: %s\r\n", name, value) }
	header("From", from.String())
	for i, name := range []string{"To", "Cc", "Bcc"} {
		if list := recipients[i]; len(list) > 0 {
			s := make([]string, len(list))
			for j, a := range list {
				s[j] = a.String()
			}
			header(name, strings.Join(s, ", "))
		}
	}
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	qp.Close()
	return b.Bytes()
}

// retryDelivery tries a deferred or failed delivery again now.
func (h *Handler) retryDelivery(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	d, err := h.svc.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, "retryDelivery", err)
		return
	}
	writeJSON(w, map[string]string{"status": d.Status})
}

// deliverySummary says how sending a message went, for a list: whether any
// delivery failed, is still to go, or how far they all got.
func deliverySummary(list []*db.Delivery) (summary string, problem bool) {
	pending, failed, delivered := 0, 0, 0
	for _, d := range list {
		switch d.Status {
		case db.DeliveryQueued, db.DeliveryDeferred:
			pending++
		case db.DeliveryBounced, db.DeliveryComplained, db.DeliveryFailed:
			failed++
		case db.DeliveryDelivered:
			delivered++
		}
	}
	switch {
	case len(list) == 0:
		return "", false
	case failed == len(list):
		return "Not delivered", true
	case failed > 0:
		return fmt.Sprintf("%d of %d not delivered", failed, len(list)), true
	case pending > 0:
		return "Sending", false
	case delivered == len(list):
		return "Delivered", false
	default:
		return "Sent", false
	}
}

// deliveryClass colors a delivery status: green once it went, amber while
// it's on its way, red if it didn't.
func deliveryClass(status string) string {
	switch status {
	case db.DeliverySent, db.DeliveryDelivered:
		return "bg-green-50 text-green-700 border-green-200"
	case db.DeliveryBounced, db.DeliveryComplained, db.DeliveryFailed:
		return "bg-red-50 text-red-700 border-red-200"
	default:
		return "bg-amber-50 text-amber-800 border-amber-200"
	}
}

// deliveriesOf returns the deliveries of messages, by message.
func (h *Handler) deliveriesOf(r *http.Request, ids []string) (map[string][]*db.Delivery, error) {
	out := map[string][]*db.Delivery{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := h.db.ListDeliveries(r.Context(), db.DeliveryFilter{MessageIDs: ids})
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		out[d.MessageID] = append(out[d.MessageID], d)
	}
	return out, nil
}
