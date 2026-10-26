package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/boxysh/boxy/internal/auth"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/service"
	"github.com/boxysh/boxy/internal/storage/memory"
)

const testMessage = "From: Ann <ann@example.com>\r\nTo: hello@boxy.sh\r\nSubject: Hello there\r\n" +
	"Content-Type: text/html\r\n\r\n<p>Hi <img src=\"https://tracker.example/x.png\"></p>\r\n"

type testUI struct {
	t      *testing.T
	db     *db.DB
	svc    *service.Service
	mux    *http.ServeMux
	cookie *http.Cookie
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

	svc := service.New(database, memory.New())
	mux := http.NewServeMux()
	NewHandler(database, sessions, svc, "http://example.test", "dev").RegisterRoutes(mux)
	return &testUI{t: t, db: database, svc: svc, mux: mux, cookie: rec.Result().Cookies()[0]}
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

	if rec := u.do("POST", "/ui/accounts", "application/x-www-form-urlencoded", "addr=hello@boxy.sh&name=Hello", true); rec.Code != 200 {
		t.Fatalf("create account: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/accounts", "application/x-www-form-urlencoded", "addr=hello@boxy.sh", true); rec.Code != http.StatusConflict {
		t.Errorf("duplicate account: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/labels", "application/x-www-form-urlencoded", "name=billing", true); rec.Code != http.StatusNoContent {
		t.Fatalf("create label: %d %s", rec.Code, rec.Body)
	}

	a, err := u.db.GetAccountByAddr(t.Context(), "hello@boxy.sh")
	if err != nil {
		t.Fatal(err)
	}
	rec := u.do("POST", "/ui/messages?account="+a.ID, "message/rfc822", testMessage, true)
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	m, err := u.db.ListMessages(t.Context(), db.MessageFilter{Limit: 1})
	if err != nil || len(m) != 1 {
		t.Fatalf("ListMessages: %v %v", m, err)
	}
	id := m[0].ID

	if body := u.get("/", 200); !strings.Contains(body, "Hello there") || !strings.Contains(body, "Unread: ") {
		t.Errorf("inbox doesn't list the unread message")
	}
	u.get("/?label=&account="+a.ID, 200)
	u.get("/accounts", 200)
	u.get("/labels", 200)
	u.get("/keys", 200)

	body := u.get("/messages/"+id, 200)
	if !strings.Contains(body, "sandbox=") || !strings.Contains(body, "img-src data:;") {
		t.Errorf("message page doesn't sandbox the HTML body")
	}
	if strings.Contains(body, `<img src="https://tracker.example`) {
		t.Errorf("HTML body is not escaped into srcdoc")
	}
	// Opening a message marks it read.
	got, err := u.db.GetMessage(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Labels, ",") != "inbox" {
		t.Errorf("labels after opening = %v", got.Labels)
	}

	rec = u.do("POST", "/ui/messages/"+id+"/labels", "application/json", `{"add_labels": ["billing"], "remove_labels": ["inbox"]}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "billing") {
		t.Errorf("update labels: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do("POST", "/ui/messages/"+id+"/labels", "application/json", `{"add_labels": ["nope"]}`, true); rec.Code != 400 {
		t.Errorf("unknown label: %d %s", rec.Code, rec.Body)
	}

	if raw := u.get("/messages/"+id+"/raw", 200); raw != testMessage {
		t.Errorf("raw = %q", raw)
	}
	if rec := u.do("GET", "/messages/"+id+"/raw", "", "", false); rec.Code != 401 {
		t.Errorf("signed-out raw: %d", rec.Code)
	}
	u.get("/messages/01h0000000000000000000000z", 404)

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
