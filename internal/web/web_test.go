package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/internal/auth"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/service"
	"github.com/spoolysh/spooly/internal/storage/memory"
)

const testMessage = "From: Ann <ann@example.com>\r\nTo: hello@spooly.sh\r\nSubject: Hello there\r\n" +
	"Content-Type: text/html\r\n\r\n<p>Hi <img src=\"https://tracker.example/x.png\"></p>\r\n"

type testUI struct {
	t        *testing.T
	db       *db.DB
	svc      *service.Service
	mux      *http.ServeMux
	cookie   *http.Cookie
	sessions *auth.SessionManager
	userID   string
}

func newTestUI(t *testing.T) *testUI {
	t.Helper()

	database, err := db.New(t.Context(), "sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })
	if err := database.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	user, err := database.UpsertUser(t.Context(), "g1", "vh@example.com", "VH")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	sessions := auth.NewSessionManager("secret")
	rec := httptest.NewRecorder()
	sessions.SetSession(rec, user.ID)

	svc := service.New(database, memory.New(), service.Options{Admins: []string{"vh@example.com"}})
	mux := http.NewServeMux()
	NewHandler(database, sessions, svc, "http://example.test", "dev").RegisterRoutes(mux)
	// Mount the API too, as cmd/spooly does, so routes that clash with it
	// panic.
	if err := svc.Mount(mux); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return &testUI{t: t, db: database, svc: svc, mux: mux, cookie: rec.Result().Cookies()[0], sessions: sessions, userID: user.ID}
}

func (u *testUI) do(method, path, contentType, body string, signedIn bool) *httptest.ResponseRecorder {
	u.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if signedIn {
		req.AddCookie(u.cookie)
	}
	rec := httptest.NewRecorder()
	u.mux.ServeHTTP(rec, req)
	return rec
}

func (u *testUI) get(path string, want int) string {
	u.t.Helper()
	rec := u.do("GET", path, "", "", true)
	if rec.Code != want {
		u.t.Fatalf("GET %s: status %d, want %d; body: %s", path, rec.Code, want, rec.Body)
	}
	return rec.Body.String()
}

func TestPagesRender(t *testing.T) {
	u := newTestUI(t)

	// Signed out, every page shows the sign-in page.
	if body := u.do("GET", "/", "", "", false).Body.String(); !strings.Contains(body, "Sign in with Google") {
		t.Errorf("signed-out / doesn't show sign-in")
	}

	if body := u.get("/", 200); !strings.Contains(body, "No mailboxes yet") {
		t.Errorf("empty messages page: %s", body)
	}

	if rec := u.do("POST", "/ui/mailboxes", "application/x-www-form-urlencoded", "address=hello@spooly.sh&name=Hello", true); rec.Code != 200 {
		t.Fatalf("create mailbox: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/mailboxes", "application/x-www-form-urlencoded", "address=hello@spooly.sh", true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate mailbox: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/labels", "application/x-www-form-urlencoded", "name=billing", true); rec.Code != http.StatusNoContent {
		t.Fatalf("create label: %d %s", rec.Code, rec.Body)
	}

	m, err := u.svc.Store(t.Context(), service.Incoming{MailboxIDOrAddress: "hello@spooly.sh", Raw: []byte(testMessage)})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	id := m.ID

	if body := u.get("/", 200); !strings.Contains(body, "Hello there") || !strings.Contains(body, "Unread: ") {
		t.Errorf("inbox doesn't list the unread message")
	}
	if body := u.get("/?folder=&mailbox=hello@spooly.sh", 200); !strings.Contains(body, "Hello there") {
		t.Error("mailbox filter by address doesn't list its message")
	}
	u.get("/?mailbox=nobody@spooly.sh", 404)
	u.get("/?folder=nope", 404)
	u.get("/mailboxes", 200)
	u.get("/labels", 200)
	u.get("/users", 200)
	u.get("/keys", 200)

	body := u.get("/"+id, 200)
	if !strings.Contains(body, "sandbox=") || !strings.Contains(body, "img-src &#39;self&#39; data:;") {
		t.Errorf("message page doesn't sandbox the HTML body")
	}
	if !strings.Contains(body, `id="load-images"`) {
		t.Error("a message with remote images can't load them")
	}
	if strings.Contains(body, `<img src="https://tracker.example`) {
		t.Errorf("HTML body is not escaped into srcdoc")
	}
	if !strings.Contains(body, "Headers <span class=\"text-gray-400\">(4)</span>") || !strings.Contains(body, "text/html</dd>") {
		t.Errorf("message page doesn't list the header fields")
	}
	// Opening a message marks it read.
	got, err := u.db.GetMessage(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Folder != "inbox" || !got.Seen {
		t.Errorf("after opening: folder %q, seen %t", got.Folder, got.Seen)
	}

	rec := u.do("POST", "/ui/messages/"+id+"/update", "application/json", `{"folder": "archive", "add_labels": ["billing"], "seen": false, "flagged": true}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "billing") {
		t.Errorf("update: %d %s", rec.Code, rec.Body)
	}
	if got, err := u.db.GetMessage(t.Context(), id); err != nil || got.Folder != "archive" || got.Seen || !got.Flagged {
		t.Errorf("after update: %+v, %v", got, err)
	}
	if body := u.get("/?flagged=true", 200); !strings.Contains(body, "Hello there") || !strings.Contains(body, `title="Starred"`) {
		t.Error("Starred doesn't list the starred message")
	}
	if rec := u.do("POST", "/ui/messages/"+id+"/update", "application/json", `{"add_labels": ["nope"]}`, true); rec.Code != 400 {
		t.Errorf("unknown label: %d %s", rec.Code, rec.Body)
	}

	if raw := u.get("/"+id+"?raw", 200); raw != testMessage {
		t.Errorf("raw = %q", raw)
	}
	rec = u.do("GET", "/"+id+"?download", "", "", true)
	if rec.Code != 200 || rec.Body.String() != testMessage || rec.Header().Get("Content-Disposition") != `attachment; filename="Hello there.eml"` {
		t.Errorf("download: %d %q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	if rec := u.do("GET", "/"+id+"?raw", "", "", false); rec.Code != 401 {
		t.Errorf("signed-out raw: %d", rec.Code)
	}
	u.get("/00000000000000000000000000", 404)
	// Paths that aren't message IDs are 404s, even signed out.
	if rec := u.do("GET", "/robots.txt", "", "", false); rec.Code != 404 {
		t.Errorf("/robots.txt: %d", rec.Code)
	}
	u.get("/mailboxes", 200)

	if rec := u.do("DELETE", "/ui/labels/billing", "", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete label: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("DELETE", "/ui/labels/inbox", "", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("delete a folder as a label: %d %s", rec.Code, rec.Body)
	}
}

func TestActionsRejectCrossOrigin(t *testing.T) {
	u := newTestUI(t)
	req := httptest.NewRequest("POST", "/ui/labels", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(u.cookie)
	rec := httptest.NewRecorder()
	u.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", rec.Code)
	}
}

// A list's URL has the API's parameters, and reads back as the same view.
func TestListURL(t *testing.T) {
	for v, want := range map[view]string{
		{Folder: "inbox"}:                   "/",
		{}:                                  "/?folder=",
		{Folder: "archive"}:                 "/?folder=archive",
		{Label: "billing"}:                  "/?label=billing",
		{Flagged: true}:                     "/?flagged=true",
		{Folder: "inbox", Label: "billing"}: "/?folder=inbox&label=billing",
	} {
		got := string(listURL("", v))
		if got != want {
			t.Errorf("listURL(%+v) = %s, want %s", v, got, want)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		if back, ok := viewOf(u.Query()); !ok || back != v {
			t.Errorf("%s reads back as %+v, %t", got, back, ok)
		}
	}
	if got := listURL("hello@spooly.sh", view{Label: "billing"}); got != "/?label=billing&mailbox=hello@spooly.sh" {
		t.Errorf("with a mailbox: %s", got)
	}
	if _, ok := viewOf(url.Values{"folder": {"nope"}}); ok {
		t.Error("an unknown folder is a view")
	}
}

func TestLabelTitle(t *testing.T) {
	for name, want := range map[string]string{
		"":                 "All Mail",
		"inbox":            "Inbox",
		"billing":          "Billing",
		"customer-issues":  "Customer Issues",
		"q3_invoices-2026": "Q3 Invoices 2026",
	} {
		if got := labelTitle(name); got != want {
			t.Errorf("labelTitle(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAttachments(t *testing.T) {
	u := newTestUI(t)
	if _, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	raw := "Subject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"a.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n" +
		"--b\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"page.html\"\r\n\r\n<script>alert(1)</script>\r\n--b--\r\n"
	m, err := u.svc.Store(t.Context(), service.Incoming{MailboxIDOrAddress: "hello@spooly.sh", Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}

	if body := u.get("/"+m.ID, 200); !strings.Contains(body, "?part=2&amp;download") {
		t.Error("message page doesn't link attachments")
	}
	for _, tc := range []struct {
		query, disposition string
	}{
		{"part=1", `inline; filename=a.png`},
		{"part=1&download", `attachment; filename=a.png`},
		{"part=2", `attachment; filename=page.html`}, // HTML never shows inline
	} {
		rec := u.do("GET", "/"+m.ID+"?"+tc.query, "", "", true)
		if rec.Code != 200 || rec.Header().Get("Content-Disposition") != tc.disposition {
			t.Errorf("%s: %d, Content-Disposition %q, want %q", tc.query, rec.Code, rec.Header().Get("Content-Disposition"), tc.disposition)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: not sandboxed", tc.query)
		}
	}
	if rec := u.do("GET", "/"+m.ID+"?part=3", "", "", true); rec.Code != 404 {
		t.Errorf("missing part: %d", rec.Code)
	}
	if rec := u.do("GET", "/"+m.ID+"?part=1", "", "", false); rec.Code != 401 {
		t.Errorf("signed out: %d", rec.Code)
	}
}

// Images the HTML body embeds by cid: load from their parts, and aren't
// listed as attachments.
func TestInlineImages(t *testing.T) {
	u := newTestUI(t)
	if _, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	raw := "Subject: news\r\nContent-Type: multipart/related; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:logo@x\"><img src=\"cid:missing@x\"><div style=\"background:url(CID:bg%40x)\"></div>\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <bg@x>\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <unused@x>\r\nContent-Disposition: attachment; filename=\"unused.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n--b--\r\n"
	m, err := u.svc.Store(t.Context(), service.Incoming{MailboxIDOrAddress: "hello@spooly.sh", Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}

	body := u.get("/"+m.ID, 200)
	for _, want := range []string{
		"src=&#34;/" + m.ID + "?part=1&#34;",
		"url(/" + m.ID + "?part=2)",
		"cid:missing@x", // unknown, left as is
	} {
		if !strings.Contains(body, want) {
			t.Errorf("message page doesn't have %s", want)
		}
	}
	if strings.Contains(body, "?part=1&amp;download") || strings.Contains(body, "?part=2&amp;download") {
		t.Error("inline images are listed as attachments")
	}
	if !strings.Contains(body, "?part=3&amp;download") {
		t.Error("an image the HTML doesn't show isn't listed")
	}
	if strings.Contains(body, `id="load-images"`) {
		t.Error("a message without remote images offers to load them")
	}
}

func TestHasRemoteContent(t *testing.T) {
	for html, want := range map[string]bool{
		`<p>Hi</p>`:                                                 false,
		`<img src="/abc?part=1">`:                                   false,
		`<img src="data:image/png;base64,x">`:                       false,
		`<a href="https://example.com">x</a>`:                       false, // a link, not loaded
		`<img src="https://t.example/x.png">`:                       true,
		`<img SRC='//t.example/x.png'>`:                             true,
		`<img srcset="/a.png 1x, https://t.example/b.png 2x">`:      true,
		`<td background="http://t.example/bg.jpg">`:                 true,
		`<div style="background:url( 'https://t.example/bg.jpg')">`: true,
		`<style>@import "https://t.example/a.css";</style>`:         true,
		`<style>p { background: url(/abc?part=1) }</style>`:         false,
		`<link rel="stylesheet" href="https://t.example/a.css">`:    true,
		`<link rel="icon" href="https://t.example/f.ico">`:          false,
	} {
		if got := hasRemoteContent(html); got != want {
			t.Errorf("hasRemoteContent(%s) = %v, want %v", html, got, want)
		}
	}
}

func TestPagingAndBulkActions(t *testing.T) {
	u := newTestUI(t)
	if _, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	var ids []string // oldest first
	for i := range 120 {
		raw := fmt.Sprintf("Subject: message %d\r\n\r\nbody\r\n", i)
		m, err := u.svc.Store(t.Context(), service.Incoming{MailboxIDOrAddress: "hello@spooly.sh", Raw: []byte(raw)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	// All Mail leaves out junk and trash.
	body := fmt.Sprintf(`{"ids": [%q, %q], "folder": "trash"}`, ids[118], ids[119])
	if rec := u.do("POST", "/ui/messages/update", "application/json", body, true); rec.Code != 200 {
		t.Fatalf("trash: %d %s", rec.Code, rec.Body)
	}
	if all := u.get("/?folder=", 200); !strings.Contains(all, "1&ndash;50 of 118") || strings.Contains(all, "message 119<") {
		t.Errorf("All Mail includes trashed messages")
	}
	// Outside the inbox, rows show an Inbox tag; inside, they don't.
	tag := `border-brand-200">Inbox</span>`
	if all := u.get("/?folder=", 200); !strings.Contains(all, tag) {
		t.Error("All Mail doesn't tag inbox messages")
	}
	if inbox := u.get("/", 200); strings.Contains(inbox, tag) {
		t.Error("Inbox tags its own messages")
	}
	if trash := u.get("/?folder=trash", 200); !strings.Contains(trash, "1&ndash;2 of 2") {
		t.Errorf("Trash doesn't list the trashed messages")
	}
	body = fmt.Sprintf(`{"ids": [%q, %q], "folder": "inbox"}`, ids[118], ids[119])
	if rec := u.do("POST", "/ui/messages/update", "application/json", body, true); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}

	first := u.get("/", 200)
	if !strings.Contains(first, "1&ndash;50 of 120") || !strings.Contains(first, "after="+ids[70]) || strings.Contains(first, "before=") {
		t.Errorf("first page lacks its range or older link")
	}
	second := u.get("/?folder=inbox&after="+ids[70], 200)
	if !strings.Contains(second, "51&ndash;100 of 120") || !strings.Contains(second, "before="+ids[69]) || !strings.Contains(second, "after="+ids[20]) {
		t.Errorf("second page lacks its range or links")
	}
	// Newer from the second page is the first page again.
	back := u.get("/?folder=inbox&before="+ids[69], 200)
	if !strings.Contains(back, "1&ndash;50 of 120") || !strings.Contains(back, "message 119") || strings.Contains(back, "message 69<") {
		t.Errorf("newer page isn't the first page")
	}
	last := u.get("/?folder=inbox&after="+ids[20], 200)
	if !strings.Contains(last, "101&ndash;120 of 120") || strings.Contains(last, "after=") {
		t.Errorf("last page has an older link or wrong range")
	}

	body = fmt.Sprintf(`{"ids": [%q, %q], "folder": "archive", "flagged": true}`, ids[0], ids[1])
	if rec := u.do("POST", "/ui/messages/update", "application/json", body, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"updated":2`) {
		t.Fatalf("bulk: %d %s", rec.Code, rec.Body)
	}
	for _, id := range ids[:2] {
		m, err := u.db.GetMessage(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Folder != "archive" || !m.Flagged || m.Seen {
			t.Errorf("after bulk: folder %q, flagged %t, seen %t", m.Folder, m.Flagged, m.Seen)
		}
	}
	if rec := u.do("POST", "/ui/messages/update", "application/json", `{"ids": [], "flagged": true}`, true); rec.Code != 400 {
		t.Errorf("empty bulk: %d", rec.Code)
	}
	if rec := u.do("POST", "/ui/messages/update", "application/json", fmt.Sprintf(`{"ids": [%q], "add_labels": ["nope"]}`, ids[2]), true); rec.Code != 400 {
		t.Errorf("unknown label: %d", rec.Code)
	}
}

// Trash and junk can be deleted for good, a selection or all of an
// mailbox's at once.
func TestDeleteForever(t *testing.T) {
	u := newTestUI(t)
	hello, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"hello@spooly.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.svc.CreateMailbox(u.asAdmin(), &spoolyv1.CreateMailboxRequest{Addresses: []string{"other@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, to := range []string{"hello@spooly.sh", "hello@spooly.sh", "hello@spooly.sh", "other@spooly.sh"} {
		m, err := u.svc.Store(t.Context(), service.Incoming{MailboxIDOrAddress: to, Raw: fmt.Appendf(nil, "Subject: message %d\r\n\r\nbody\r\n", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	move := func(id, folder string) {
		t.Helper()
		if _, err := u.db.UpdateMessage(t.Context(), id, db.MessageUpdate{Folder: folder}); err != nil {
			t.Fatal(err)
		}
	}
	move(ids[0], db.FolderTrash)
	move(ids[1], db.FolderTrash)
	move(ids[2], db.FolderJunk)
	move(ids[3], db.FolderTrash)

	if page := u.get("/?folder=trash", 200); !strings.Contains(page, "Choose a mailbox to empty its trash.") || strings.Contains(page, "Empty Trash") {
		t.Error("Trash for all mailboxes offers to empty it")
	}
	mailboxID := hello.Mailbox.Id
	if page := u.get("/?mailbox=hello@spooly.sh&folder=trash", 200); !strings.Contains(page, "Empty Trash") || !strings.Contains(page, "/ui/mailboxes/"+mailboxID+"/empty") || !strings.Contains(page, "Delete forever") {
		t.Error("a mailbox's Trash lacks Empty Trash or Delete forever")
	}

	if rec := u.do("POST", "/ui/messages/delete", "application/json", fmt.Sprintf(`{"ids": [%q]}`, ids[0]), true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, _, err := u.svc.OpenRaw(t.Context(), ids[0]); err == nil {
		t.Error("deleted message still opens")
	}

	if rec := u.do("POST", "/ui/mailboxes/"+mailboxID+"/empty", "application/json", `{"folder": "inbox"}`, true); rec.Code != 400 {
		t.Errorf("emptying the inbox: %d", rec.Code)
	}
	if rec := u.do("POST", "/ui/mailboxes/"+mailboxID+"/empty", "application/json", `{"folder": "trash"}`, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("empty trash: %d %s", rec.Code, rec.Body)
	}
	// Only that mailbox's trash goes: its junk and the other's trash stay.
	for i, want := range []bool{false, false, true, true} {
		_, err := u.db.GetMessage(t.Context(), ids[i])
		if got := err == nil; got != want {
			t.Errorf("message %d exists = %v, want %v", i, got, want)
		}
	}
}

// asAdmin is a context acting as the signed-in user, an admin, for calling
// the service directly.
func (u *testUI) asAdmin() context.Context {
	return auth.ContextWithUserID(u.t.Context(), u.userID)
}

// A user who isn't an admin sees only the mailboxes shared with them.
func TestSharing(t *testing.T) {
	u := newTestUI(t)
	ctx := t.Context()
	admin := u.asAdmin()
	for _, address := range []string{"vh@spooly.sh", "support@spooly.sh"} {
		if _, err := u.svc.CreateMailbox(admin, &spoolyv1.CreateMailboxRequest{Addresses: []string{address}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"vh@spooly.sh", "support@spooly.sh"} {
		if _, err := u.svc.Store(ctx, service.Incoming{MailboxIDOrAddress: address, Raw: []byte("Subject: to " + address + "\r\n\r\nhi\r\n")}); err != nil {
			t.Fatal(err)
		}
	}
	maria, err := u.db.UpsertUser(ctx, "g2", "maria@example.com", "Maria")
	if err != nil {
		t.Fatal(err)
	}
	support, err := u.db.GetMailboxByAddress(ctx, "support@spooly.sh")
	if err != nil {
		t.Fatal(err)
	}

	// The admin shares support@ with Maria, and adds an address.
	if rec := u.do("POST", "/ui/mailboxes/"+support.ID+"/access", "application/x-www-form-urlencoded", "email=Maria@Example.com", true); rec.Code != http.StatusNoContent {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/mailboxes/"+support.ID+"/access", "application/x-www-form-urlencoded", "email=nobody@example.com", true); rec.Code != 404 {
		t.Errorf("share with someone who never signed in: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/mailboxes/"+support.ID, "application/json", `{"addresses": ["support@spooly.sh", "contact@domain2.com"]}`, true); rec.Code != 200 {
		t.Fatalf("add address: %d %s", rec.Code, rec.Body)
	}
	if body := u.get("/mailboxes/support@spooly.sh", 200); !strings.Contains(body, "maria@example.com") || !strings.Contains(body, "contact@domain2.com") {
		t.Error("mailbox page lacks the share or the address")
	}

	// Maria signs in: she sees support@ only.
	rec := httptest.NewRecorder()
	u.sessions.SetSession(rec, maria.ID)
	asMaria := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(rec.Result().Cookies()[0])
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		out := httptest.NewRecorder()
		u.mux.ServeHTTP(out, req)
		return out
	}
	inbox := asMaria("GET", "/", "").Body.String()
	if !strings.Contains(inbox, "to support@spooly.sh") || strings.Contains(inbox, "to vh@spooly.sh") {
		t.Error("Maria's inbox shows the wrong mailboxes")
	}
	if code := asMaria("GET", "/?mailbox=vh@spooly.sh", "").Code; code != 404 {
		t.Errorf("Maria filtering by vh@: %d", code)
	}
	// Mailboxes, labels, and users are managed by admins only: Maria works
	// with support@'s mail, but can't change the mailbox or share it.
	for _, path := range []string{"/mailboxes", "/mailboxes/support@spooly.sh", "/mailboxes/vh@spooly.sh", "/labels", "/users"} {
		if code := asMaria("GET", path, "").Code; code != 404 {
			t.Errorf("Maria opening %s: %d", path, code)
		}
	}
	for _, link := range []string{`href="/mailboxes"`, `href="/labels"`, `href="/users"`} {
		if strings.Contains(inbox, link) {
			t.Errorf("Maria's navigation has %s", link)
		}
	}
	if code := asMaria("POST", "/ui/mailboxes/"+support.ID, `{"name": "Support"}`).Code; code != 403 {
		t.Errorf("Maria renaming support@: %d", code)
	}
	if code := asMaria("DELETE", "/ui/users/"+u.userID+"/keys", "").Code; code != 403 {
		t.Errorf("Maria revoking keys: %d", code)
	}
	req := httptest.NewRequest("POST", "/ui/mailboxes", strings.NewReader("address=maria@spooly.sh"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(rec.Result().Cookies()[0])
	created := httptest.NewRecorder()
	u.mux.ServeHTTP(created, req)
	if created.Code != 403 {
		t.Errorf("Maria creating a mailbox: %d %s", created.Code, created.Body)
	}

	// The admin sees Maria on Users, and offboards her: her keys and her
	// access go.
	if _, _, err := u.db.CreateAPIKey(ctx, maria.ID, "laptop"); err != nil {
		t.Fatal(err)
	}
	users := u.get("/users", 200)
	if !strings.Contains(users, "maria@example.com") || !strings.Contains(users, `href="/mailboxes/support@spooly.sh"`) || !strings.Contains(users, "Revoke keys") {
		t.Error("Users lacks Maria, her mailbox, or Revoke keys")
	}
	if rec := u.do("DELETE", "/ui/users/"+maria.ID+"/keys", "", "", true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"revoked":1`) {
		t.Errorf("revoke keys: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("DELETE", "/ui/users/"+maria.ID+"/access", "", "", true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"removed":1`) {
		t.Errorf("remove access: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("DELETE", "/ui/users/999/keys", "", "", true); rec.Code != 404 {
		t.Errorf("revoking an unknown user's keys: %d", rec.Code)
	}
	if keys, err := u.db.ListAPIKeys(ctx, maria.ID); err != nil || len(keys) != 0 {
		t.Errorf("Maria's keys after revoking: %d, %v", len(keys), err)
	}
	if strings.Contains(asMaria("GET", "/", "").Body.String(), "to support@spooly.sh") {
		t.Error("Maria still sees support@ after removing her access")
	}
}
