package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/boxysh/boxy/internal/auth"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

type Handler struct {
	db       *db.DB
	sessions *auth.SessionManager
	svc      *service.Service
	hostname string
	version  string
	login    *template.Template            // standalone pages
	pages    map[string]*template.Template // layout-composed pages
}

// NewHandler builds the web UI. version is the server build version ("dev"
// for local builds); it is shown to signed-in users only.
func NewHandler(database *db.DB, sessions *auth.SessionManager, svc *service.Service, hostname, version string) *Handler {
	h := &Handler{
		db:       database,
		sessions: sessions,
		svc:      svc,
		hostname: strings.TrimRight(hostname, "/"),
		version:  version,
		login:    template.Must(template.ParseFS(templateFS, "templates/login.html")),
		pages:    map[string]*template.Template{},
	}
	funcs := template.FuncMap{
		"version":    func() string { return h.version },
		"isSystem":   db.IsSystemLabel,
		"formatSize": formatSize,
		"labelTitle": labelTitle,
	}
	for _, p := range []string{"messages", "message", "accounts", "labels", "apikeys"} {
		h.pages[p] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(
			templateFS,
			"templates/layout.html",
			"templates/"+p+".html",
		))
	}
	return h
}

// RegisterRoutes registers the dashboard pages and the endpoints they call.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.MessagesPage)
	mux.HandleFunc("GET /messages/{id}", h.MessagePage)
	mux.HandleFunc("GET /messages/{id}/raw", h.rawMessage)
	mux.HandleFunc("GET /accounts", h.AccountsPage)
	mux.HandleFunc("GET /labels", h.LabelsPage)
	mux.HandleFunc("GET /keys", h.KeysPage)
	mux.HandleFunc("GET /auth/keys", h.listKeys)

	// Endpoints that change state are signed in with the session cookie, so
	// they reject cross-site requests: other sites can't act as the user.
	csrf := http.NewCrossOriginProtection()
	mux.Handle("POST /auth/keys", csrf.Handler(http.HandlerFunc(h.createKey)))
	mux.Handle("DELETE /auth/keys/{id}", csrf.Handler(http.HandlerFunc(h.deleteKey)))
	mux.Handle("POST /ui/accounts", csrf.Handler(http.HandlerFunc(h.createAccount)))
	mux.Handle("POST /ui/labels", csrf.Handler(http.HandlerFunc(h.createLabel)))
	mux.Handle("DELETE /ui/labels/{name}", csrf.Handler(http.HandlerFunc(h.deleteLabel)))
	mux.Handle("POST /ui/messages", csrf.Handler(http.HandlerFunc(h.uploadMessage)))
	mux.Handle("POST /ui/messages/{id}/labels", csrf.Handler(http.HandlerFunc(h.updateLabels)))
}

type layoutData struct {
	ActiveTab string
	Title     string
	User      *db.User
}

// Messages

const messagesPerPage = 50

type messageRow struct {
	ID          string
	From        string
	FromAddr    string
	Subject     string
	Snippet     string
	Recipient   string
	UserLabels  []string
	Unread      bool
	Starred     bool
	Attachments int
	ReceivedAt  string
	ReceivedISO string
}

type labelLink struct {
	Name   string // empty for all mail
	Title  string
	Unread int
	Active bool
}

type messagesData struct {
	layoutData
	Messages  []messageRow
	Labels    []labelLink
	Accounts  []db.Account
	AccountID string
	Label     string // "" for all mail
	Hostname  string
	Paged     bool
	NextAfter string
	Query     template.URL // account and label, for pagination links
}

func (h *Handler) MessagesPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	q := r.URL.Query()
	label := db.LabelInbox
	if q.Has("label") {
		label = q.Get("label")
	}
	accountID := q.Get("account")
	after := q.Get("after")

	messages, err := h.db.ListMessages(r.Context(), db.MessageFilter{
		AccountID: accountID, Label: label, After: after, Limit: messagesPerPage + 1,
	})
	if err != nil {
		h.internalError(w, "listing messages", err)
		return
	}
	accounts, err := h.db.ListAccounts(r.Context(), 1000, "")
	if err != nil {
		h.internalError(w, "listing accounts", err)
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}

	data := messagesData{
		layoutData: layoutData{ActiveTab: "messages", Title: labelTitle(label), User: user},
		Accounts:   accounts,
		AccountID:  accountID,
		Label:      label,
		Hostname:   h.hostname,
		Paged:      after != "",
		Query:      template.URL(url.Values{"account": {accountID}, "label": {label}}.Encode()),
	}
	for _, l := range append([]db.Label{{Name: ""}}, labels...) {
		if l.Name == db.LabelUnread {
			continue // shown as counts and bold rows instead
		}
		link := labelLink{Name: l.Name, Title: labelTitle(l.Name), Active: l.Name == label}
		if l.Name == db.LabelInbox {
			// Unread in the inbox, like a mail client's badge.
			if link.Unread, err = h.db.CountMessages(r.Context(), accountID, l.Name, db.LabelUnread); err != nil {
				h.internalError(w, "counting messages", err)
				return
			}
		}
		data.Labels = append(data.Labels, link)
	}
	if len(messages) > messagesPerPage {
		messages = messages[:messagesPerPage]
		data.NextAfter = messages[len(messages)-1].ID
	}
	for _, m := range messages {
		row := messageRow{
			ID:          m.ID,
			From:        m.From.Name,
			FromAddr:    m.From.Addr,
			Subject:     m.Subject,
			Snippet:     m.Snippet,
			Recipient:   m.Recipient,
			Attachments: len(m.Attachments),
			ReceivedAt:  formatTime(m.ReceivedAt),
			ReceivedISO: m.ReceivedAt.Local().Format(time.DateTime),
		}
		if row.From == "" {
			row.From = m.From.Addr
		}
		for _, l := range m.Labels {
			switch {
			case l == db.LabelUnread:
				row.Unread = true
			case l == db.LabelStarred:
				row.Starred = true
			case !db.IsSystemLabel(l):
				row.UserLabels = append(row.UserLabels, l)
			}
		}
		data.Messages = append(data.Messages, row)
	}
	h.render(w, "messages", data)
}

type messageData struct {
	layoutData
	Message    *db.Message
	Account    *db.Account
	UserLabels []string
	AllLabels  []db.Label
	Has        map[string]bool
	Date       string
	ReceivedAt string
	HTMLDoc    string // the HTML body with remote content blocked
	HTMLDocAll string // the HTML body as sent
}

func (h *Handler) MessagePage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	id := r.PathValue("id")
	m, err := h.db.GetMessage(r.Context(), id, true)
	if errors.Is(err, db.ErrMessageNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting message", err)
		return
	}
	// Opening a message reads it, as in a mail client. The API leaves
	// unread alone; clients remove it themselves.
	if hasLabel(m, db.LabelUnread) {
		updated, err := h.db.UpdateMessage(r.Context(), id, db.MessageUpdate{RemoveLabels: []string{db.LabelUnread}})
		if err != nil {
			h.internalError(w, "marking message read", err)
			return
		}
		m.Labels = updated.Labels
	}

	account, err := h.db.GetAccount(r.Context(), m.AccountID)
	if err != nil {
		h.internalError(w, "getting account", err)
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}

	data := messageData{
		layoutData: layoutData{ActiveTab: "messages", Title: subjectOrNone(m.Subject), User: user},
		Message:    m,
		Account:    account,
		AllLabels:  labels,
		Has:        map[string]bool{},
		ReceivedAt: m.ReceivedAt.Local().Format(time.DateTime),
	}
	if m.Date != nil {
		data.Date = m.Date.Local().Format(time.DateTime)
	}
	for _, l := range m.Labels {
		data.Has[l] = true
		if !db.IsSystemLabel(l) {
			data.UserLabels = append(data.UserLabels, l)
		}
	}
	if m.HTML != "" {
		// The body is shown in a sandboxed iframe that can't run scripts.
		// The CSP also blocks remote images and styles until the user loads
		// them, so opening a message doesn't tell the sender; links open in
		// a new tab.
		const head = `<base target="_blank"><meta http-equiv="Content-Security-Policy" content="script-src 'none'; default-src 'none'; `
		data.HTMLDoc = head + `img-src data:; style-src 'unsafe-inline'">` + m.HTML
		data.HTMLDocAll = head + `img-src * data:; style-src * 'unsafe-inline'; font-src *">` + m.HTML
	}
	h.render(w, "message", data)
}

func hasLabel(m *db.Message, label string) bool {
	for _, l := range m.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// rawMessage serves a message's source as plain text, so the browser shows
// it rather than downloading or rendering it.
func (h *Handler) rawMessage(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.sessions.GetUserID(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	rc, err := h.svc.OpenRaw(r.Context(), id)
	if connect.CodeOf(err) == connect.CodeNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting raw message", err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.eml"`)
	}
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("sending raw message %s: %v", id, err)
	}
}

// Accounts

type accountRow struct {
	db.Account
	Unread    int
	CreatedAt string
}

type accountsData struct {
	layoutData
	Accounts []accountRow
	Hostname string
}

func (h *Handler) AccountsPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	accounts, err := h.db.ListAccounts(r.Context(), 1000, "")
	if err != nil {
		h.internalError(w, "listing accounts", err)
		return
	}
	data := accountsData{layoutData: layoutData{ActiveTab: "accounts", Title: "Accounts", User: user}, Hostname: h.hostname}
	for _, a := range accounts {
		n, err := h.db.CountMessages(r.Context(), a.ID, db.LabelInbox, db.LabelUnread)
		if err != nil {
			h.internalError(w, "counting messages", err)
			return
		}
		data.Accounts = append(data.Accounts, accountRow{Account: a, Unread: n, CreatedAt: a.CreatedAt.Local().Format(time.DateTime)})
	}
	h.render(w, "accounts", data)
}

// Labels

type labelRow struct {
	Name      string
	System    bool
	Count     int
	CreatedAt string
}

type labelsData struct {
	layoutData
	Labels []labelRow
}

func (h *Handler) LabelsPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}
	data := labelsData{layoutData: layoutData{ActiveTab: "labels", Title: "Labels", User: user}}
	for _, l := range labels {
		n, err := h.db.CountMessages(r.Context(), "", l.Name)
		if err != nil {
			h.internalError(w, "counting messages", err)
			return
		}
		row := labelRow{Name: l.Name, System: l.System, Count: n}
		if l.CreatedAt != nil {
			row.CreatedAt = l.CreatedAt.Local().Format(time.DateTime)
		}
		data.Labels = append(data.Labels, row)
	}
	h.render(w, "labels", data)
}

// API keys

type keyRow struct {
	ID        string
	Label     string
	Prefix    string
	CreatedAt string
}

type keysData struct {
	layoutData
	Keys     []keyRow
	Hostname string
}

func (h *Handler) KeysPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	keys, err := h.db.ListAPIKeys(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "listing keys", err)
		return
	}

	data := keysData{layoutData: layoutData{ActiveTab: "keys", Title: "API Keys", User: user}, Hostname: h.hostname}
	for _, k := range keys {
		data.Keys = append(data.Keys, keyRow{
			ID:        k.ID,
			Label:     k.Label,
			Prefix:    k.KeyPrefix,
			CreatedAt: k.CreatedAt.Local().Format(time.DateTime),
		})
	}
	h.render(w, "apikeys", data)
}

// Auth helpers

func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) *db.User {
	userID, ok := h.sessions.GetUserID(r)
	if !ok {
		h.renderStandalone(w, "login.html", nil)
		return nil
	}

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil {
		h.sessions.ClearSession(w)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return nil
	}
	return user
}

// Render helpers

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	t, ok := h.pages[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("rendering %s: %v", name, err)
	}
}

func (h *Handler) renderStandalone(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.login.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("rendering %s: %v", name, err)
	}
}

func (h *Handler) internalError(w http.ResponseWriter, op string, err error) {
	log.Printf("%s: %v", op, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// writeError answers a dashboard action with the status for a service
// error, as plain text.
func writeError(w http.ResponseWriter, op string, err error) {
	status := http.StatusInternalServerError
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument:
		status = http.StatusBadRequest
	case connect.CodeNotFound:
		status = http.StatusNotFound
	case connect.CodeAlreadyExists, connect.CodeFailedPrecondition:
		status = http.StatusConflict
	case connect.CodeResourceExhausted:
		status = http.StatusRequestEntityTooLarge
	}
	if status == http.StatusInternalServerError {
		log.Printf("%s: %v", op, err)
		http.Error(w, "internal error", status)
		return
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		http.Error(w, cerr.Message(), status)
		return
	}
	http.Error(w, err.Error(), status)
}

// labelTitle is how a label is shown: system labels capitalized, "" as all
// mail.
func labelTitle(name string) string {
	switch {
	case name == "":
		return "All mail"
	case db.IsSystemLabel(name):
		return strings.ToUpper(name[:1]) + name[1:]
	default:
		return name
	}
}

func subjectOrNone(s string) string {
	if s == "" {
		return "(no subject)"
	}
	return s
}

// formatTime shows today's times as a clock time and older ones as a date.
func formatTime(t time.Time) string {
	t = t.Local()
	now := time.Now()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("2006-01-02")
	}
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
