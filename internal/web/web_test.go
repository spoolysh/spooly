package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/internal/auth"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/service"
	"github.com/boxysh/boxy/internal/storage/memory"
)

const testMessage = "From: Ann <ann@example.com>\r\nTo: hello@boxy.sh\r\nSubject: Hello there\r\n" +
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
	// Mount the API too, as cmd/boxy does, so routes that clash with it panic.
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

	if body := u.get("/", 200); !strings.Contains(body, "No accounts yet") {
		t.Errorf("empty messages page: %s", body)
	}

	if rec := u.do("POST", "/ui/accounts", "application/x-www-form-urlencoded", "address=hello@boxy.sh&name=Hello", true); rec.Code != 200 {
		t.Fatalf("create account: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/accounts", "application/x-www-form-urlencoded", "address=hello@boxy.sh", true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate account: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/labels", "application/x-www-form-urlencoded", "name=billing", true); rec.Code != http.StatusNoContent {
		t.Fatalf("create label: %d %s", rec.Code, rec.Body)
	}

	m, err := u.svc.Store(t.Context(), service.Incoming{AccountIDOrAddress: "hello@boxy.sh", Raw: []byte(testMessage)})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	id := m.ID

	if body := u.get("/", 200); !strings.Contains(body, "Hello there") || !strings.Contains(body, "Unread: ") {
		t.Errorf("inbox doesn't list the unread message")
	}
	if body := u.get("/?label=&account=hello@boxy.sh", 200); !strings.Contains(body, "Hello there") {
		t.Error("account filter by address doesn't list its message")
	}
	u.get("/?account=nobody@boxy.sh", 404)
	u.get("/accounts", 200)
	u.get("/labels", 200)
	u.get("/keys", 200)

	body := u.get("/"+id, 200)
	if !strings.Contains(body, "sandbox=") || !strings.Contains(body, "img-src data:;") {
		t.Errorf("message page doesn't sandbox the HTML body")
	}
	if strings.Contains(body, `<img src="https://tracker.example`) {
		t.Errorf("HTML body is not escaped into srcdoc")
	}
	if _, err := u.db.UpdateMessage(t.Context(), id, db.MessageUpdate{Metadata: map[string]string{
		"ticket_id": "T-123", "crm": "https://crm.example.com/c/1", "script": "javascript:alert(1)",
	}}); err != nil {
		t.Fatal(err)
	}
	body = u.get("/"+id, 200)
	if !strings.Contains(body, "T-123") || !strings.Contains(body, `href="https://crm.example.com/c/1"`) || strings.Contains(body, `href="javascript:`) {
		t.Errorf("message page doesn't show metadata, or links a non-web value")
	}
	// Opening a message marks it read.
	got, err := u.db.GetMessage(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Labels, ",") != "inbox" {
		t.Errorf("labels after opening = %v", got.Labels)
	}

	rec := u.do("POST", "/ui/messages/"+id+"/labels", "application/json", `{"add_labels": ["billing"], "remove_labels": ["inbox"]}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "billing") {
		t.Errorf("update labels: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/messages/"+id+"/labels", "application/json", `{"add_labels": ["nope"]}`, true); rec.Code != 400 {
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
	u.get("/aaaaaaaaaaaaaaaaaaaaa", 404)
	// Paths that aren't message IDs are 404s, even signed out.
	if rec := u.do("GET", "/robots.txt", "", "", false); rec.Code != 404 {
		t.Errorf("/robots.txt: %d", rec.Code)
	}
	u.get("/accounts", 200)

	if rec := u.do("DELETE", "/ui/labels/billing", "", "", true); rec.Code != http.StatusNoContent {
		t.Errorf("delete label: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("DELETE", "/ui/labels/inbox", "", "", true); rec.Code != http.StatusConflict {
		t.Errorf("delete system label: %d %s", rec.Code, rec.Body)
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
	if got := folderTitle("draft"); got != "Drafts" {
		t.Errorf(`folderTitle("draft") = %q, want "Drafts"`, got)
	}
}

func TestAttachments(t *testing.T) {
	u := newTestUI(t)
	if _, err := u.svc.CreateAccount(u.asAdmin(), &boxyv1.CreateAccountRequest{Addresses: []string{"hello@boxy.sh"}}); err != nil {
		t.Fatal(err)
	}
	raw := "Subject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"a.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n" +
		"--b\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"page.html\"\r\n\r\n<script>alert(1)</script>\r\n--b--\r\n"
	m, err := u.svc.Store(t.Context(), service.Incoming{AccountIDOrAddress: "hello@boxy.sh", Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}

	if body := u.get("/"+m.ID, 200); !strings.Contains(body, "?attachment=1&amp;download") {
		t.Error("message page doesn't link attachments")
	}
	for _, tc := range []struct {
		query, disposition string
	}{
		{"attachment=0", `inline; filename=a.png`},
		{"attachment=0&download", `attachment; filename=a.png`},
		{"attachment=1", `attachment; filename=page.html`}, // HTML never shows inline
	} {
		rec := u.do("GET", "/"+m.ID+"?"+tc.query, "", "", true)
		if rec.Code != 200 || rec.Header().Get("Content-Disposition") != tc.disposition {
			t.Errorf("%s: %d, Content-Disposition %q, want %q", tc.query, rec.Code, rec.Header().Get("Content-Disposition"), tc.disposition)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: not sandboxed", tc.query)
		}
	}
	if rec := u.do("GET", "/"+m.ID+"?attachment=2", "", "", true); rec.Code != 404 {
		t.Errorf("missing attachment: %d", rec.Code)
	}
	if rec := u.do("GET", "/"+m.ID+"?attachment=0", "", "", false); rec.Code != 401 {
		t.Errorf("signed out: %d", rec.Code)
	}
}

func TestPagingAndBulkActions(t *testing.T) {
	u := newTestUI(t)
	if _, err := u.svc.CreateAccount(u.asAdmin(), &boxyv1.CreateAccountRequest{Addresses: []string{"hello@boxy.sh"}}); err != nil {
		t.Fatal(err)
	}
	var ids []string // oldest first
	for i := range 120 {
		raw := fmt.Sprintf("Subject: message %d\r\n\r\nbody\r\n", i)
		m, err := u.svc.Store(t.Context(), service.Incoming{AccountIDOrAddress: "hello@boxy.sh", Raw: []byte(raw)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	// All Mail leaves out spam and trash, as the IMAP folder does.
	body := fmt.Sprintf(`{"ids": [%q, %q], "add_labels": ["trash"], "remove_labels": ["inbox"]}`, ids[118], ids[119])
	if rec := u.do("POST", "/ui/messages/labels", "application/json", body, true); rec.Code != 200 {
		t.Fatalf("trash: %d %s", rec.Code, rec.Body)
	}
	if all := u.get("/?label=", 200); !strings.Contains(all, "1&ndash;50 of 118") || strings.Contains(all, "message 119<") {
		t.Errorf("All Mail includes trashed messages")
	}
	// Outside the inbox, rows show an Inbox tag; inside, they don't.
	tag := `border-brand-200">Inbox</span>`
	if all := u.get("/?label=", 200); !strings.Contains(all, tag) {
		t.Error("All Mail doesn't tag inbox messages")
	}
	if inbox := u.get("/", 200); strings.Contains(inbox, tag) {
		t.Error("Inbox tags its own messages")
	}
	if trash := u.get("/?label=trash", 200); !strings.Contains(trash, "1&ndash;2 of 2") {
		t.Errorf("Trash doesn't list the trashed messages")
	}
	body = fmt.Sprintf(`{"ids": [%q, %q], "add_labels": ["inbox"], "remove_labels": ["trash"]}`, ids[118], ids[119])
	if rec := u.do("POST", "/ui/messages/labels", "application/json", body, true); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}

	first := u.get("/", 200)
	if !strings.Contains(first, "1&ndash;50 of 120") || !strings.Contains(first, "after="+ids[70]) || strings.Contains(first, "before=") {
		t.Errorf("first page lacks its range or older link")
	}
	second := u.get("/?label=inbox&after="+ids[70], 200)
	if !strings.Contains(second, "51&ndash;100 of 120") || !strings.Contains(second, "before="+ids[69]) || !strings.Contains(second, "after="+ids[20]) {
		t.Errorf("second page lacks its range or links")
	}
	// Newer from the second page is the first page again.
	back := u.get("/?label=inbox&before="+ids[69], 200)
	if !strings.Contains(back, "1&ndash;50 of 120") || !strings.Contains(back, "message 119") || strings.Contains(back, "message 69<") {
		t.Errorf("newer page isn't the first page")
	}
	last := u.get("/?label=inbox&after="+ids[20], 200)
	if !strings.Contains(last, "101&ndash;120 of 120") || strings.Contains(last, "after=") {
		t.Errorf("last page has an older link or wrong range")
	}

	body = fmt.Sprintf(`{"ids": [%q, %q], "remove_labels": ["inbox"], "add_labels": ["starred"]}`, ids[0], ids[1])
	if rec := u.do("POST", "/ui/messages/labels", "application/json", body, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"updated":2`) {
		t.Fatalf("bulk: %d %s", rec.Code, rec.Body)
	}
	for _, id := range ids[:2] {
		m, err := u.db.GetMessage(t.Context(), id, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(m.Labels, ","); got != "unread,starred" {
			t.Errorf("labels after bulk = %s", got)
		}
	}
	if rec := u.do("POST", "/ui/messages/labels", "application/json", `{"ids": [], "add_labels": ["starred"]}`, true); rec.Code != 400 {
		t.Errorf("empty bulk: %d", rec.Code)
	}
	if rec := u.do("POST", "/ui/messages/labels", "application/json", fmt.Sprintf(`{"ids": [%q], "add_labels": ["nope"]}`, ids[2]), true); rec.Code != 400 {
		t.Errorf("unknown label: %d", rec.Code)
	}
}

// Trash and spam can be deleted for good, a selection or all of an
// account's at once.
func TestDeleteForever(t *testing.T) {
	u := newTestUI(t)
	hello, err := u.svc.CreateAccount(u.asAdmin(), &boxyv1.CreateAccountRequest{Addresses: []string{"hello@boxy.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.svc.CreateAccount(u.asAdmin(), &boxyv1.CreateAccountRequest{Addresses: []string{"other@boxy.sh"}}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, to := range []string{"hello@boxy.sh", "hello@boxy.sh", "hello@boxy.sh", "other@boxy.sh"} {
		m, err := u.svc.Store(t.Context(), service.Incoming{AccountIDOrAddress: to, Raw: fmt.Appendf(nil, "Subject: message %d\r\n\r\nbody\r\n", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	label := func(id, l string) {
		t.Helper()
		if _, err := u.db.UpdateMessage(t.Context(), id, db.MessageUpdate{AddLabels: []string{l}, RemoveLabels: []string{db.LabelInbox}}); err != nil {
			t.Fatal(err)
		}
	}
	label(ids[0], db.LabelTrash)
	label(ids[1], db.LabelTrash)
	label(ids[2], db.LabelSpam)
	label(ids[3], db.LabelTrash)

	if page := u.get("/?label=trash", 200); !strings.Contains(page, "Choose an account to empty its trash.") || strings.Contains(page, "Empty Trash") {
		t.Error("Trash for all accounts offers to empty it")
	}
	accountID := hello.Account.Id
	if page := u.get("/?account=hello@boxy.sh&label=trash", 200); !strings.Contains(page, "Empty Trash") || !strings.Contains(page, "/ui/accounts/"+accountID+"/empty") || !strings.Contains(page, "Delete forever") {
		t.Error("an account's Trash lacks Empty Trash or Delete forever")
	}

	if rec := u.do("POST", "/ui/messages/delete", "application/json", fmt.Sprintf(`{"ids": [%q]}`, ids[0]), true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, _, err := u.svc.OpenRaw(t.Context(), ids[0]); err == nil {
		t.Error("deleted message still opens")
	}

	if rec := u.do("POST", "/ui/accounts/"+accountID+"/empty", "application/json", `{"label": "inbox"}`, true); rec.Code != 400 {
		t.Errorf("emptying the inbox: %d", rec.Code)
	}
	if rec := u.do("POST", "/ui/accounts/"+accountID+"/empty", "application/json", `{"label": "trash"}`, true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("empty trash: %d %s", rec.Code, rec.Body)
	}
	// Only that account's trash goes: its spam and the other's trash stay.
	for i, want := range []bool{false, false, true, true} {
		_, err := u.db.GetMessage(t.Context(), ids[i], false)
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

// A user who isn't an admin sees only the accounts shared with them.
func TestSharing(t *testing.T) {
	u := newTestUI(t)
	ctx := t.Context()
	admin := u.asAdmin()
	for _, address := range []string{"vh@boxy.sh", "support@boxy.sh"} {
		if _, err := u.svc.CreateAccount(admin, &boxyv1.CreateAccountRequest{Addresses: []string{address}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"vh@boxy.sh", "support@boxy.sh"} {
		if _, err := u.svc.Store(ctx, service.Incoming{AccountIDOrAddress: address, Raw: []byte("Subject: to " + address + "\r\n\r\nhi\r\n")}); err != nil {
			t.Fatal(err)
		}
	}
	maria, err := u.db.UpsertUser(ctx, "g2", "maria@example.com", "Maria")
	if err != nil {
		t.Fatal(err)
	}
	support, err := u.db.GetAccountByAddress(ctx, "support@boxy.sh")
	if err != nil {
		t.Fatal(err)
	}

	// The admin shares support@ with Maria, and adds an address.
	if rec := u.do("POST", "/ui/accounts/"+support.ID+"/access", "application/x-www-form-urlencoded", "email=Maria@Example.com", true); rec.Code != http.StatusNoContent {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/accounts/"+support.ID+"/access", "application/x-www-form-urlencoded", "email=nobody@example.com", true); rec.Code != 404 {
		t.Errorf("share with someone who never signed in: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/accounts/"+support.ID, "application/json", `{"addresses": ["support@boxy.sh", "contact@domain2.com"]}`, true); rec.Code != 200 {
		t.Fatalf("add address: %d %s", rec.Code, rec.Body)
	}
	if body := u.get("/accounts/support@boxy.sh", 200); !strings.Contains(body, "maria@example.com") || !strings.Contains(body, "contact@domain2.com") {
		t.Error("account page lacks the share or the address")
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
	if !strings.Contains(inbox, "to support@boxy.sh") || strings.Contains(inbox, "to vh@boxy.sh") {
		t.Error("Maria's inbox shows the wrong accounts")
	}
	if code := asMaria("GET", "/?account=vh@boxy.sh", "").Code; code != 404 {
		t.Errorf("Maria filtering by vh@: %d", code)
	}
	if code := asMaria("GET", "/accounts/vh@boxy.sh", "").Code; code != 404 {
		t.Errorf("Maria opening vh@'s page: %d", code)
	}
	accounts := asMaria("GET", "/accounts", "").Body.String()
	if !strings.Contains(accounts, "support@boxy.sh") || strings.Contains(accounts, "vh@boxy.sh") || strings.Contains(accounts, "Create account") {
		t.Error("Maria's accounts page is wrong")
	}
	// Access is full access: Maria can rename support@, but not create
	// accounts.
	if code := asMaria("POST", "/ui/accounts/"+support.ID, `{"name": "Support"}`).Code; code != 200 {
		t.Errorf("Maria renaming support@: %d", code)
	}
	req := httptest.NewRequest("POST", "/ui/accounts", strings.NewReader("address=maria@boxy.sh"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(rec.Result().Cookies()[0])
	created := httptest.NewRecorder()
	u.mux.ServeHTTP(created, req)
	if created.Code != 403 {
		t.Errorf("Maria creating an account: %d %s", created.Code, created.Body)
	}
	// Labels are managed by admins only.
	if code := asMaria("GET", "/labels", "").Code; code != 404 {
		t.Errorf("Maria opening Labels: %d", code)
	}
	if strings.Contains(inbox, `href="/labels"`) {
		t.Error("Maria's navigation links to Labels")
	}
	if code := asMaria("GET", "/accounts/support@boxy.sh", "").Code; code != 200 {
		t.Errorf("Maria opening support@'s page: %d", code)
	}
}
