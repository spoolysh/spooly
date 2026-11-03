package web

import (
	"bytes"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

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
	m, deliveries, err := h.svc.Send(r.Context(), raw, nil, r.FormValue("key"))
	if err != nil {
		writeError(w, "sendMessage", err)
		return
	}
	// The message, if kept in Sent; otherwise how it's getting there.
	next := "/deliveries"
	switch {
	case m != nil:
		next = "/" + m.ID
	case len(deliveries) == 1:
		next = "/deliveries/" + deliveries[0].ID
	}
	writeJSON(w, map[string]string{"url": next})
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

// deliveriesPerPage is how many deliveries the deliveries page shows.
const deliveriesPerPage = 50

type deliveryRow struct {
	*db.Delivery
	Mailbox string // its address
	Title   string // its subject, or (no subject)
	Kept    bool   // whether its message is in the mailbox, to link to
	Updated string
}

type statusLink struct {
	Status string // "" for all
	Count  int
	Active bool
}

type deliveriesData struct {
	layoutData
	Deliveries []deliveryRow
	Statuses   []statusLink
	Mailboxes  []db.Mailbox
	Mailbox    string // the filtered mailbox's address; "" for all
	Status     string
	Newer      string // the URL of the newer page, if any
	Older      string // the URL of the older page, if any
	Sends      bool
}

// DeliveriesPage lists deliveries, newest first, for admins to see how
// sending goes: ?status= and ?mailbox= filter them, and pages are cursors:
// ?before=ID for older ones, ?after=ID for newer.
func (h *Handler) DeliveriesPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	if !h.svc.IsAdmin(user.Email) {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	mailboxes, err := h.visibleMailboxes(r, user)
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	addresses := make(map[string]string, len(mailboxes))
	var mailboxID string
	address := strings.ToLower(q.Get("mailbox"))
	for _, a := range mailboxes {
		addresses[a.ID] = a.Address()
		if address != "" && a.Address() == address {
			mailboxID = a.ID
		}
	}
	if address != "" && mailboxID == "" {
		http.NotFound(w, r)
		return
	}
	status := q.Get("status")
	// One more than a page tells whether there's another page that way.
	f := db.DeliveryFilter{MailboxID: mailboxID, Status: status, Newest: true, Limit: deliveriesPerPage + 1}
	after, before := q.Get("after"), q.Get("before")
	if isID(after) {
		f.After, f.Newest = after, false
	} else if isID(before) {
		f.Before = before
	}
	list, err := h.db.ListDeliveries(r.Context(), f)
	if err != nil {
		h.internalError(w, "listing deliveries", err)
		return
	}
	more := len(list) > deliveriesPerPage
	if more {
		list = list[:deliveriesPerPage]
	}
	if !f.Newest {
		slices.Reverse(list)
	}
	counts, err := h.db.CountDeliveries(r.Context(), mailboxID)
	if err != nil {
		h.internalError(w, "counting deliveries", err)
		return
	}

	data := deliveriesData{
		layoutData: layoutData{ActiveTab: "deliveries", Title: "Deliveries", User: user, Admin: true},
		Mailboxes:  mailboxes, Mailbox: address, Status: status, Sends: h.svc.Sends(),
	}
	page := func(status, before, after string) string {
		v := url.Values{}
		if status != "" {
			v.Set("status", status)
		}
		if address != "" {
			v.Set("mailbox", address)
		}
		if before != "" {
			v.Set("before", before)
		}
		if after != "" {
			v.Set("after", after)
		}
		if len(v) == 0 {
			return "/deliveries"
		}
		return "/deliveries?" + strings.ReplaceAll(v.Encode(), "%40", "@")
	}
	if len(list) > 0 {
		newer, older := page(status, "", list[0].ID), page(status, list[len(list)-1].ID, "")
		switch {
		case isID(after): // paged forward from older ones
			data.Older = older
			if more {
				data.Newer = newer
			}
		case isID(before): // paged back from newer ones
			data.Newer = newer
			if more {
				data.Older = older
			}
		case more:
			data.Older = older
		}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	// Queued, deferred, and failed are always there, what's still to go and
	// what didn't; the rest once they have any.
	data.Statuses = append(data.Statuses, statusLink{Count: total, Active: status == ""})
	for _, s := range deliveryStatuses {
		always := s == db.DeliveryQueued || s == db.DeliveryDeferred || s == db.DeliveryFailed
		if always || counts[s] > 0 || s == status {
			data.Statuses = append(data.Statuses, statusLink{Status: s, Count: counts[s], Active: s == status})
		}
	}
	kept := map[string]bool{}
	for _, d := range list {
		if _, ok := kept[d.MessageID]; !ok {
			_, err := h.db.GetMessage(r.Context(), d.MessageID)
			kept[d.MessageID] = err == nil
		}
	}
	for _, d := range list {
		data.Deliveries = append(data.Deliveries, deliveryRow{
			Delivery: d, Mailbox: addresses[d.MailboxID], Title: subjectOrNone(d.Subject), Kept: kept[d.MessageID], Updated: formatTime(d.UpdatedAt),
		})
	}
	h.render(w, "deliveries", data)
}

// deliveryStatuses are the statuses in the order a delivery goes through
// them.
var deliveryStatuses = []string{
	db.DeliveryQueued, db.DeliveryDeferred, db.DeliverySent, db.DeliveryDelayed,
	db.DeliveryDelivered, db.DeliveryBounced, db.DeliveryComplained, db.DeliveryFailed,
}

type deliveryData struct {
	layoutData
	Delivery *db.Delivery
	Events   []db.DeliveryEvent
	// Retention is how long events are kept, in days.
	Retention int
	Mailbox   string // its address
	Title     string // its subject, or (no subject)
	Kept      bool   // whether its message is in the mailbox, to link to
}

// DeliveryPage shows a delivery, with what happened to it, to
// whoever can see its mailbox.
func (h *Handler) DeliveryPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	id := r.PathValue("id")
	if !isID(id) {
		http.NotFound(w, r)
		return
	}
	d, events, err := h.svc.Delivery(r.Context(), id)
	if connect.CodeOf(err) == connect.CodeNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting delivery", err)
		return
	}
	admin := h.svc.IsAdmin(user.Email)
	tab := "messages" // where others see deliveries: on their messages
	if admin {
		tab = "deliveries"
	}
	data := deliveryData{
		layoutData: layoutData{ActiveTab: tab, Title: "Delivery to " + d.Recipient, User: user, Admin: admin},
		Delivery:   d,
		Events:     events,
		Retention:  int(h.svc.EventsRetention().Hours() / 24),
	}
	if a, err := h.db.GetMailbox(r.Context(), d.MailboxID); err == nil {
		data.Mailbox = a.Address()
	}
	data.Title = subjectOrNone(d.Subject)
	if _, err := h.db.GetMessage(r.Context(), d.MessageID); err == nil {
		data.Kept = true
	}
	h.render(w, "delivery", data)
}
