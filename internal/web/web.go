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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	xhtml "golang.org/x/net/html"

	"github.com/spoolysh/spooly/internal/auth"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/service"
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
		"isFolder":   db.IsFolder,
		"formatSize": formatSize,
		"labelTitle": labelTitle,
		"listURL":    listURL,
		"sub":        func(a, b int) int { return a - b },
	}
	for _, p := range []string{"messages", "message", "mailboxes", "mailbox", "labels", "users", "apikeys"} {
		h.pages[p] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(
			templateFS,
			"templates/layout.html",
			"templates/"+p+".html",
		))
	}
	return h
}

// RegisterRoutes registers the dashboard pages and the endpoints they call.
// Each runs as the signed-in user, so the service checks their access.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	page := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, h.withUser(f)) }
	page("GET /{$}", h.MessagesPage)
	// Messages open at /{id}, and their source at /{id}?raw. Fixed paths
	// such as /mailboxes are more specific, so they win, and anything that
	// isn't a message ID is a 404. A second path segment would clash with
	// the API's /v1/ and /spooly.v1.*Service/ subtrees.
	page("GET /{id}", h.MessagePage)
	page("GET /mailboxes", h.MailboxesPage)
	page("GET /mailboxes/{address}", h.MailboxPage)
	page("GET /labels", h.LabelsPage)
	page("GET /users", h.UsersPage)
	page("GET /keys", h.KeysPage)
	page("GET /auth/keys", h.listKeys)

	// Endpoints that change state are signed in with the session cookie, so
	// they reject cross-site requests: other sites can't act as the user.
	csrf := http.NewCrossOriginProtection()
	action := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, csrf.Handler(h.withUser(f))) }
	action("POST /auth/keys", h.createKey)
	action("DELETE /auth/keys/{id}", h.deleteKey)
	action("POST /ui/mailboxes", h.createMailbox)
	action("POST /ui/mailboxes/{id}", h.updateMailbox)
	action("POST /ui/mailboxes/{id}/access", h.share)
	action("DELETE /ui/mailboxes/{id}/access/{user}", h.unshare)
	action("POST /ui/labels", h.createLabel)
	action("DELETE /ui/labels/{name}", h.deleteLabel)
	action("POST /ui/messages/{id}/update", h.updateMessage)
	action("POST /ui/messages/update", h.updateMessages)
	action("POST /ui/messages/delete", h.deleteMessages)
	action("POST /ui/mailboxes/{id}/empty", h.emptyFolder)
	action("DELETE /ui/users/{id}/keys", h.revokeKeys)
	action("DELETE /ui/users/{id}/access", h.removeAccess)
}

// withUser runs next as the signed-in user, if there is one.
func (h *Handler) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userID, ok := h.sessions.GetUserID(r); ok {
			r = r.WithContext(auth.ContextWithUserID(r.Context(), userID))
		}
		next.ServeHTTP(w, r)
	})
}

// visibleMailboxes lists the mailboxes the signed-in user has access to.
func (h *Handler) visibleMailboxes(r *http.Request, user *db.User) ([]db.Mailbox, error) {
	f := db.MailboxFilter{Limit: 1000}
	if !h.svc.IsAdmin(user.Email) {
		f.UserID = user.ID
	}
	return h.db.ListMailboxes(r.Context(), f)
}

// canAccess reports whether the signed-in user has access to a mailbox.
func (h *Handler) canAccess(r *http.Request, mailboxID string) (bool, error) {
	err := h.svc.Require(r.Context(), mailboxID)
	if connect.CodeOf(err) == connect.CodeNotFound || connect.CodeOf(err) == connect.CodeUnauthenticated {
		return false, nil
	}
	return err == nil, err
}

type layoutData struct {
	ActiveTab string
	Title     string
	User      *db.User
	Admin     bool
}

// Messages

const messagesPerPage = 50

type messageRow struct {
	ID          string
	From        string
	FromAddress string
	Subject     string
	Snippet     string
	RcptTo      string
	Tags        []string // shown on the row: its labels, and inbox outside the inbox
	Unread      bool
	Starred     bool
	Answered    bool
	Forwarded   bool
	Attachments int
	ReceivedAt  string
	ReceivedISO string
}

// view is what a message list shows, as its URL says, with the API's
// parameters: ?folder=, ?label=, and ?flagged=true. Without any, it's the
// inbox; ?folder= with no value is All Mail.
type view struct {
	Folder  string // "" for All Mail: every folder but junk and trash
	Label   string
	Flagged bool
}

// viewOf reads the view a list's URL asks for. ok is false for an unknown
// folder.
func viewOf(q url.Values) (v view, ok bool) {
	v = view{Folder: q.Get("folder"), Label: q.Get("label"), Flagged: q.Get("flagged") == "true"}
	if !q.Has("folder") && v.Label == "" && !v.Flagged {
		v.Folder = db.FolderInbox
	}
	return v, v.Folder == "" || db.IsFolder(v.Folder)
}

// filter selects v's messages.
func (v view) filter() db.MessageFilter {
	f := db.View(v.Folder, v.Label)
	if v.Flagged {
		f.Flagged = new(true)
	}
	return f
}

// Title is how v is shown: its label, Starred, or its folder.
func (v view) Title() string {
	switch {
	case v.Label != "":
		return labelTitle(v.Label)
	case v.Flagged:
		return "Starred"
	default:
		return labelTitle(v.Folder)
	}
}

// query sets v's parameters in q. The inbox needs none, so All Mail has an
// empty folder.
func (v view) query(q url.Values) {
	if v == (view{Folder: db.FolderInbox}) {
		return
	}
	if v.Folder != "" || (v.Label == "" && !v.Flagged) {
		q.Set("folder", v.Folder)
	}
	if v.Label != "" {
		q.Set("label", v.Label)
	}
	if v.Flagged {
		q.Set("flagged", "true")
	}
}

// viewLink links to a view in the list's navigation: All Mail, a folder,
// starred mail, or a label.
type viewLink struct {
	View   view
	Unread int
	Active bool
	// FirstLabel marks the first label, which a divider separates from the
	// folders above it.
	FirstLabel bool
}

type messagesData struct {
	layoutData
	Messages  []messageRow
	Views     []viewLink
	Mailboxes []db.Mailbox
	Mailbox   string // the filtered mailbox's address; "" for all
	MailboxID string // the filtered mailbox's ID; "" for all
	View      view
	Labels    []db.Label // for the toolbar's label menu
	Hostname  string
	Base      template.URL // the list's URL without a page cursor
	// The page shows messages Start to End of Total, newest first. Newer and
	// Older link to the neighboring pages; they're empty at either end.
	Start, End, Total int
	Paged             bool
	Newer, Older      template.URL
}

func (h *Handler) MessagesPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	q := r.URL.Query()
	v, ok := viewOf(q)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The mailbox is filtered by address, so URLs read well:
	// /?mailbox=hello@spooly.sh.
	address := strings.ToLower(q.Get("mailbox"))
	var mailboxID string
	if address != "" {
		a, err := h.db.ResolveMailbox(r.Context(), address)
		if err == nil {
			err = h.svc.Require(r.Context(), a.ID)
		}
		if errors.Is(err, db.ErrMailboxNotFound) || connect.CodeOf(err) == connect.CodeNotFound {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			h.internalError(w, "getting mailbox", err)
			return
		}
		mailboxID, address = a.ID, a.Address()
	}
	visible, err := h.svc.VisibleMailboxes(r.Context())
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	// Pages are cursors: ?after=ID for older messages, ?before=ID for newer.
	f := v.filter()
	f.MailboxID, f.MailboxIDs, f.Limit = mailboxID, visible, messagesPerPage
	if after := q.Get("after"); isID(after) {
		f.After = after
	} else if before := q.Get("before"); isID(before) {
		f.Before = before
	}

	messages, err := h.db.ListMessages(r.Context(), f)
	if err != nil {
		h.internalError(w, "listing messages", err)
		return
	}
	all := f
	all.After, all.Before = "", ""
	total, err := h.db.CountFiltered(r.Context(), all)
	if err != nil {
		h.internalError(w, "counting messages", err)
		return
	}
	newer := 0
	if len(messages) > 0 {
		above := all
		above.Before = messages[0].ID
		if newer, err = h.db.CountFiltered(r.Context(), above); err != nil {
			h.internalError(w, "counting messages", err)
			return
		}
	}
	mailboxes, err := h.visibleMailboxes(r, user)
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}

	data := messagesData{
		layoutData: layoutData{ActiveTab: "messages", Title: v.Title(), User: user, Admin: h.svc.IsAdmin(user.Email)},
		Mailboxes:  mailboxes,
		Mailbox:    address,
		MailboxID:  mailboxID,
		View:       v,
		Labels:     labels,
		Hostname:   h.hostname,
		Base:       listURL(address, v),
		Paged:      f.After != "" || f.Before != "",
		Total:      total,
	}
	if len(messages) > 0 {
		data.Start, data.End = newer+1, newer+len(messages)
		if newer > 0 {
			data.Newer = data.Base + "&before=" + template.URL(messages[0].ID)
		}
		if data.End < total {
			data.Older = data.Base + "&after=" + template.URL(messages[len(messages)-1].ID)
		}
	}
	// All Mail, the folders with starred mail after the inbox, then the
	// labels.
	views := []view{{}, {Folder: db.FolderInbox}, {Flagged: true}}
	for _, folder := range db.Folders[1:] {
		views = append(views, view{Folder: folder})
	}
	for _, l := range labels {
		views = append(views, view{Label: l.Name})
	}
	firstLabel := len(views) - len(labels)
	for i, lv := range views {
		link := viewLink{View: lv, Active: lv == v, FirstLabel: i == firstLabel}
		if lv.Folder == db.FolderInbox || i >= firstLabel {
			// Unread in the inbox and in each label, like a mail client's
			// badges.
			unread := lv.filter()
			unread.MailboxID, unread.MailboxIDs, unread.Seen = mailboxID, visible, new(bool)
			if link.Unread, err = h.db.CountFiltered(r.Context(), unread); err != nil {
				h.internalError(w, "counting messages", err)
				return
			}
		}
		data.Views = append(data.Views, link)
	}
	for _, m := range messages {
		row := messageRow{
			ID:          m.ID,
			From:        m.From.Name,
			FromAddress: m.From.Address,
			Subject:     m.Subject,
			Snippet:     m.Snippet,
			RcptTo:      m.RcptTo,
			Attachments: countAttachments(m.Parts),
			ReceivedAt:  formatTime(m.ReceivedAt),
			ReceivedISO: m.ReceivedAt.Local().Format(time.DateTime),
			Unread:      !m.Seen,
			Starred:     m.Flagged,
			Answered:    m.Answered,
			Forwarded:   m.Forwarded,
		}
		if row.From == "" {
			row.From = m.From.Address
		}
		// Inbox tells, outside the inbox, what isn't archived.
		if m.Folder == db.FolderInbox && v.Folder != db.FolderInbox {
			row.Tags = append(row.Tags, db.FolderInbox)
		}
		for _, l := range m.Labels {
			if l != v.Label { // every row in this view has it
				row.Tags = append(row.Tags, l)
			}
		}
		data.Messages = append(data.Messages, row)
	}
	h.render(w, "messages", data)
}

type messageData struct {
	layoutData
	Message    *db.Message
	Mailbox    *db.Mailbox
	AllLabels  []db.Label
	Has        map[string]bool // the message's labels
	Date       string
	ReceivedAt string
	HTMLDoc    string // the HTML body with remote content blocked
	HTMLDocAll string // the HTML body as sent
	HasRemote  bool   // whether the HTML body loads remote images or styles
	// Attachments are the indexes of the parts to list as attachments: not
	// a body, nor an image the HTML body shows.
	Attachments []int
}

func (h *Handler) MessagePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isID(id) {
		http.NotFound(w, r)
		return
	}
	// ?part=N[&download] serves a part, such as an attachment, numbered as
	// in the API; otherwise ?raw shows the source and ?download saves it as
	// a .eml file.
	q := r.URL.Query()
	if q.Has("part") {
		h.part(w, r)
		return
	}
	if q.Has("raw") || q.Has("download") {
		h.rawMessage(w, r)
		return
	}
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	m, err := h.db.GetMessage(r.Context(), id)
	if errors.Is(err, db.ErrMessageNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting message", err)
		return
	}
	if ok, err := h.canAccess(r, m.MailboxID); err != nil || !ok {
		if err != nil {
			h.internalError(w, "checking access", err)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	if err := h.svc.ReadBody(r.Context(), m); err != nil {
		h.internalError(w, "reading message", err)
		return
	}
	// Opening a message reads it, as in a mail client. The API leaves
	// seen alone; clients set it themselves.
	if !m.Seen {
		seen := true
		if _, err := h.db.UpdateMessage(r.Context(), id, db.MessageUpdate{Seen: &seen}); err != nil {
			h.internalError(w, "marking message read", err)
			return
		}
		m.Seen = true
	}

	mailbox, err := h.db.GetMailbox(r.Context(), m.MailboxID)
	if err != nil {
		h.internalError(w, "getting mailbox", err)
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}

	data := messageData{
		layoutData: layoutData{ActiveTab: "messages", Title: shorten(subjectOrNone(m.Subject), 80), User: user, Admin: h.svc.IsAdmin(user.Email)},
		Message:    m,
		Mailbox:    mailbox,
		AllLabels:  labels,
		Has:        map[string]bool{},
		ReceivedAt: m.ReceivedAt.Local().Format(time.DateTime),
	}
	if m.Date != nil {
		data.Date = m.Date.Local().Format(time.DateTime)
	}
	for _, l := range m.Labels {
		data.Has[l] = true
	}
	html, shown := inlineParts(m)
	for _, i := range m.Parts.AttachmentIndexes() {
		if !shown[i] {
			data.Attachments = append(data.Attachments, i)
		}
	}
	if html != "" {
		// The body is shown in a sandboxed iframe that can't run scripts.
		// The CSP also blocks remote images and styles until the user loads
		// them, so opening a message doesn't tell the sender; links open in
		// a new tab. Images embedded in the message load from Spooly ('self').
		const head = `<base target="_blank"><meta http-equiv="Content-Security-Policy" content="script-src 'none'; default-src 'none'; `
		data.HTMLDoc = head + `img-src 'self' data:; style-src 'unsafe-inline'">` + html
		data.HTMLDocAll = head + `img-src * data:; style-src * 'unsafe-inline'; font-src *">` + html
		data.HasRemote = hasRemoteContent(html)
	}
	h.render(w, "message", data)
}

// countAttachments counts the parts a list shows a paperclip for: not a
// body, nor what looks like an image the HTML body embeds (one with a
// Content-ID that isn't an attachment). The message page checks the HTML
// itself.
func countAttachments(parts db.Parts) int {
	n := 0
	for _, p := range parts.Attachments() {
		if !(p.CID != "" && strings.HasPrefix(p.Type, "image/") && p.Disposition != "attachment") {
			n++
		}
	}
	return n
}

// cidURL matches a cid: URL (RFC 2392), as in <img src="cid:logo@x"> or
// CSS url(cid:logo@x).
var cidURL = regexp.MustCompile(`(?i)\bcid:([^"'\s)>]+)`)

// inlineParts returns m's HTML body with its cid: URLs pointing at the parts
// they name, and which parts those are. Unknown cid: URLs are left as they
// are.
func inlineParts(m *db.Message) (string, map[int]bool) {
	byCID := map[string]int{}
	for i, p := range m.Parts {
		if _, ok := byCID[p.CID]; p.CID != "" && !ok {
			byCID[p.CID] = i
		}
	}
	shown := map[int]bool{}
	if len(byCID) == 0 {
		return m.HTML, shown
	}
	html := cidURL.ReplaceAllStringFunc(m.HTML, func(u string) string {
		cid, err := url.PathUnescape(u[len("cid:"):])
		if err != nil {
			return u
		}
		i, ok := byCID[cid]
		if !ok {
			return u
		}
		shown[i] = true
		return "/" + m.ID + "?part=" + strconv.Itoa(i)
	})
	return html, shown
}

// remoteURL matches a URL on another host: http:, https:, or
// protocol-relative.
var remoteURL = regexp.MustCompile(`(?i)^\s*(https?:)?//`)

// cssRemoteURL matches a remote url(...) or @import in CSS.
var cssRemoteURL = regexp.MustCompile(`(?i)(url\(\s*['"]?|@import\s+['"])\s*(https?:)?//`)

// hasRemoteContent reports whether an HTML body loads anything the iframe
// blocks until remote content is allowed: images, styles, or fonts from
// another host.
func hasRemoteContent(s string) bool {
	z := xhtml.NewTokenizer(strings.NewReader(s))
	inStyle := false
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return false
		case xhtml.TextToken:
			if inStyle && cssRemoteURL.Match(z.Text()) {
				return true
			}
		case xhtml.EndTagToken:
			if name, _ := z.TagName(); string(name) == "style" {
				inStyle = false
			}
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, more := z.TagName()
			if string(name) == "style" {
				inStyle = true
			}
			var rel, href string
			for more {
				var key, val []byte
				key, val, more = z.TagAttr()
				switch string(key) {
				case "src", "background", "poster":
					if remoteURL.Match(val) {
						return true
					}
				case "srcset":
					for _, c := range strings.Split(string(val), ",") {
						if remoteURL.MatchString(c) {
							return true
						}
					}
				case "style":
					if cssRemoteURL.Match(val) {
						return true
					}
				case "rel":
					rel = strings.ToLower(string(val))
				case "href":
					href = string(val)
				}
			}
			if string(name) == "link" && strings.Contains(rel, "stylesheet") && remoteURL.MatchString(href) {
				return true
			}
		}
	}
}

// viewable are the attachment types the browser may show rather than
// download: they can't run scripts. Everything else, HTML and SVG
// included, downloads, so it never runs on the dashboard's origin.
var viewable = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
	"text/plain":      true,
}

// part serves part N of message {id} for ?part=N: shown in the browser if
// its type is viewable, else downloaded, as is any with ?download.
func (h *Handler) part(w http.ResponseWriter, r *http.Request) {
	if !h.canRead(w, r) {
		return
	}
	index, err := strconv.Atoi(r.URL.Query().Get("part"))
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	m, err := h.db.GetMessage(r.Context(), r.PathValue("id"))
	if err != nil {
		h.internalError(w, "getting message", err)
		return
	}
	a, content, err := h.svc.OpenPart(r.Context(), m, index)
	if connect.CodeOf(err) == connect.CodeNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting part", err)
		return
	}

	mediaType, _, _ := strings.Cut(a.Type, ";")
	disposition := "attachment"
	if viewable[mediaType] && !r.URL.Query().Has("download") {
		disposition = "inline"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", a.Type)
	hdr.Set("Content-Disposition", service.ContentDisposition(disposition, a.Name))
	hdr.Set("Content-Length", strconv.FormatInt(a.Size, 10))
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Belt and braces: even a mislabeled file can't run scripts. Chrome
	// won't show a sandboxed PDF, so PDFs go without.
	if mediaType != "application/pdf" {
		hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	}
	defer content.Close()
	io.Copy(w, content)
}

// canRead checks that the signed-in user can read message {id}, answering
// the request if not.
func (h *Handler) canRead(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := auth.UserIDFromContext(r.Context()); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	m, err := h.db.GetMessage(r.Context(), r.PathValue("id"))
	if errors.Is(err, db.ErrMessageNotFound) {
		http.NotFound(w, r)
		return false
	}
	if err != nil {
		h.internalError(w, "getting message", err)
		return false
	}
	ok, err := h.canAccess(r, m.MailboxID)
	if err != nil {
		h.internalError(w, "checking access", err)
		return false
	}
	if !ok {
		http.NotFound(w, r)
	}
	return ok
}

var idPattern = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

// isID reports whether s looks like a message ID: a ULID, lowercase.
func isID(s string) bool {
	return idPattern.MatchString(s)
}

// rawMessage serves a message's source: as plain text, so the browser shows
// it rather than rendering it, or with ?download, as a .eml file to save.
func (h *Handler) rawMessage(w http.ResponseWriter, r *http.Request) {
	if !h.canRead(w, r) {
		return
	}
	id := r.PathValue("id")
	m, rc, err := h.svc.OpenRaw(r.Context(), id)
	if connect.CodeOf(err) == connect.CodeNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting raw message", err)
		return
	}
	defer rc.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Inline or not, a saved copy is named after the subject.
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", service.ContentDisposition("attachment", service.EmlFilename(m)))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", service.ContentDisposition("inline", service.EmlFilename(m)))
	}
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("sending raw message %s: %v", id, err)
	}
}

// Mailboxes

type mailboxRow struct {
	db.Mailbox
	Unread    int
	CreatedAt string
}

type mailboxesData struct {
	layoutData
	Mailboxes []mailboxRow
	Hostname  string
}

// MailboxesPage lists the mailboxes. Mailboxes are managed by admins only;
// others reach the mailboxes shared with them from the messages page.
func (h *Handler) MailboxesPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	if !h.svc.IsAdmin(user.Email) {
		http.NotFound(w, r)
		return
	}
	mailboxes, err := h.visibleMailboxes(r, user)
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	data := mailboxesData{layoutData: layoutData{ActiveTab: "mailboxes", Title: "Mailboxes", User: user, Admin: h.svc.IsAdmin(user.Email)}, Hostname: h.hostname}
	for _, a := range mailboxes {
		f := db.View(db.FolderInbox, "")
		f.MailboxID, f.Seen = a.ID, new(bool)
		n, err := h.db.CountFiltered(r.Context(), f)
		if err != nil {
			h.internalError(w, "counting messages", err)
			return
		}
		data.Mailboxes = append(data.Mailboxes, mailboxRow{Mailbox: a, Unread: n, CreatedAt: a.CreatedAt.Local().Format(time.DateTime)})
	}
	h.render(w, "mailboxes", data)
}

type mailboxData struct {
	layoutData
	Mailbox *db.Mailbox
	Users   []db.User // who it's shared with
}

// MailboxPage shows a mailbox's addresses and who it's shared with, and
// lets admins change them.
func (h *Handler) MailboxPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	if !h.svc.IsAdmin(user.Email) {
		http.NotFound(w, r)
		return
	}
	a, err := h.db.ResolveMailbox(r.Context(), strings.ToLower(r.PathValue("address")))
	if errors.Is(err, db.ErrMailboxNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.internalError(w, "getting mailbox", err)
		return
	}
	if ok, err := h.canAccess(r, a.ID); err != nil || !ok {
		if err != nil {
			h.internalError(w, "checking access", err)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	users, err := h.svc.MailboxUsers(r.Context(), a.ID)
	if err != nil {
		h.internalError(w, "listing access", err)
		return
	}
	h.render(w, "mailbox", mailboxData{
		layoutData: layoutData{ActiveTab: "mailboxes", Title: a.Address(), User: user, Admin: h.svc.IsAdmin(user.Email)},
		Mailbox:    a,
		Users:      users,
	})
}

// Users

type userRow struct {
	ID        string
	Email     string
	Name      string
	Admin     bool
	Self      bool
	Mailboxes []string // addresses of the mailboxes shared with them
	Keys      int
	JoinedAt  string
}

type usersData struct {
	layoutData
	Users []userRow
}

// UsersPage lists everyone who has signed in, what they reach, and lets
// admins take it away.
func (h *Handler) UsersPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	if !h.svc.IsAdmin(user.Email) {
		http.NotFound(w, r)
		return
	}
	users, err := h.svc.ListUsers(r.Context())
	if err != nil {
		h.internalError(w, "listing users", err)
		return
	}
	mailboxes, err := h.visibleMailboxes(r, user)
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	addresses := make(map[string]string, len(mailboxes))
	for _, a := range mailboxes {
		addresses[a.ID] = a.Address()
	}
	data := usersData{layoutData: layoutData{ActiveTab: "users", Title: "Users", User: user, Admin: true}}
	for _, u := range users {
		row := userRow{ID: u.ID, Email: u.Email, Name: u.Name, Admin: u.Admin, Self: u.ID == user.ID, Keys: u.Keys,
			JoinedAt: u.CreatedAt.Local().Format(time.DateTime)}
		for _, id := range u.MailboxIDs {
			row.Mailboxes = append(row.Mailboxes, addresses[id])
		}
		slices.Sort(row.Mailboxes)
		data.Users = append(data.Users, row)
	}
	h.render(w, "users", data)
}

// Labels

type labelRow struct {
	Name      string
	Count     int
	CreatedAt string
}

type labelsData struct {
	layoutData
	Labels []labelRow
}

// LabelsPage manages labels. It's for admins: labels are shared by every
// mailbox.
func (h *Handler) LabelsPage(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	if !h.svc.IsAdmin(user.Email) {
		http.NotFound(w, r)
		return
	}
	labels, err := h.db.ListLabels(r.Context())
	if err != nil {
		h.internalError(w, "listing labels", err)
		return
	}
	visible, err := h.svc.VisibleMailboxes(r.Context())
	if err != nil {
		h.internalError(w, "listing mailboxes", err)
		return
	}
	data := labelsData{layoutData: layoutData{ActiveTab: "labels", Title: "Labels", User: user, Admin: h.svc.IsAdmin(user.Email)}}
	for _, l := range labels {
		// Counted in the mailboxes the user can see only.
		n, err := h.db.CountFiltered(r.Context(), db.MessageFilter{MailboxIDs: visible, Label: l.Name})
		if err != nil {
			h.internalError(w, "counting messages", err)
			return
		}
		data.Labels = append(data.Labels, labelRow{Name: l.Name, Count: n, CreatedAt: l.CreatedAt.Local().Format(time.DateTime)})
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

	data := keysData{layoutData: layoutData{ActiveTab: "keys", Title: "API Keys", User: user, Admin: h.svc.IsAdmin(user.Email)}, Hostname: h.hostname}
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
	case connect.CodePermissionDenied:
		status = http.StatusForbidden
	case connect.CodeUnauthenticated:
		status = http.StatusUnauthorized
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

// listURL links to the message list for a mailbox's address ("" for all)
// and a view, the inbox by default. It keeps "@" as is, so the URL reads like
// the address, as /?label=billing&mailbox=hello@spooly.sh.
func listURL(address string, v ...view) template.URL {
	q := url.Values{}
	if address != "" {
		q.Set("mailbox", address)
	}
	if len(v) > 0 {
		v[0].query(q)
	}
	if len(q) == 0 {
		return "/"
	}
	return template.URL("/?" + strings.ReplaceAll(q.Encode(), "%40", "@"))
}

// labelTitle is how a label is shown: "customer-issues" as "Customer
// Issues", and "" as all mail. Label names are lowercase ASCII words, so
// each word's first byte is its first letter.
func labelTitle(name string) string {
	if name == "" {
		return "All Mail"
	}
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func subjectOrNone(s string) string {
	if s == "" {
		return "(no subject)"
	}
	return s
}

// shorten cuts s to at most n characters, ending with an ellipsis if cut.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
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
