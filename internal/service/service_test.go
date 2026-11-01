package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/genproto/googleapis/api/httpbody"
	_ "modernc.org/sqlite"

	spoolyv1 "github.com/spoolysh/spooly/gen/spooly/v1"
	"github.com/spoolysh/spooly/gen/spooly/v1/spoolyv1connect"
	"github.com/spoolysh/spooly/internal/auth"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/storage"
	"github.com/spoolysh/spooly/internal/storage/memory"
)

func newTestDB(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.New(t.Context(), "sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })

	if err := database.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

// newTestServer serves the API as cmd/spooly does, minus authentication.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	database := newTestDB(t)
	admin, err := database.UpsertUser(t.Context(), "admin", "admin@example.com", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	// Every request acts as the admin.
	asAdmin := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(auth.ContextWithUserID(r.Context(), admin.ID)))
		})
	}
	mux := http.NewServeMux()
	if err := New(database, memory.New(), Options{Admins: []string{"admin@example.com"}}).mount(mux, asAdmin); err != nil {
		t.Fatalf("mount: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// call makes a REST request and decodes a JSON response into out, failing
// unless the status is want.
func call(t *testing.T, srv *httptest.Server, method, path, contentType, body string, want int, out any) {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: decoding %s: %v", method, path, b, err)
		}
	}
}

type messageJSON struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	MailFrom  string `json:"mail_from"`
	RcptTo    string `json:"rcpt_to"`
	From      struct{ Name, Address string }
	Subject   string   `json:"subject"`
	Snippet   string   `json:"snippet"`
	Size      string   `json:"size"` // int64 is a JSON string
	Folder    string   `json:"folder"`
	Labels    []string `json:"labels"`
	Seen      bool     `json:"seen"`
	Flagged   bool     `json:"flagged"`
	Answered  bool     `json:"answered"`
	Draft     bool     `json:"draft"`
	Forwarded bool     `json:"forwarded"`
	Parts     []struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Size        string `json:"size"`
		CID         string `json:"cid"`
		Disposition string `json:"disposition"`
	} `json:"parts"`
}

const testMessage = "From: Ann <ann@example.com>\r\nTo: hello@spooly.sh\r\nSubject: Hi %d\r\n\r\nHello %d\r\n"

func rawMessage(n int) string {
	return fmt.Sprintf(testMessage, n, n)
}

func createAccount(t *testing.T, srv *httptest.Server, address string) string {
	t.Helper()
	var a struct{ ID string }
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addresses": ["`+address+`"]}`, 200, &a)
	return a.ID
}

func deliver(t *testing.T, srv *httptest.Server, to, query string, n int) messageJSON {
	t.Helper()
	var m messageJSON
	call(t, srv, "POST", "/v1/accounts/"+to+"/messages?"+query, "message/rfc822", rawMessage(n), 200, &m)
	return m
}

func TestAccounts(t *testing.T) {
	srv := newTestServer(t)

	var a struct {
		ID        string
		Addresses []string
		Name      string
	}
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addresses": ["Hello@Spooly.sh"], "name": "Hello"}`, 200, &a)
	if strings.Join(a.Addresses, ",") != "hello@spooly.sh" || a.Name != "Hello" || a.ID != "1" {
		t.Errorf("created %+v", a)
	}

	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addresses": ["hello@spooly.sh"]}`, 409, nil)
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addresses": ["hello+x@spooly.sh"]}`, 400, nil)
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addresses": ["not an address"]}`, 400, nil)

	for _, idOrAddress := range []string{a.ID, "hello@spooly.sh", "HELLO+news@spooly.sh"} {
		var got struct{ ID string }
		call(t, srv, "GET", "/v1/accounts/"+url.PathEscape(idOrAddress), "", "", 200, &got)
		if got.ID != a.ID {
			t.Errorf("GET %s = %s, want %s", idOrAddress, got.ID, a.ID)
		}
	}
	call(t, srv, "GET", "/v1/accounts/nobody@spooly.sh", "", "", 404, nil)

	second := createAccount(t, srv, "second@spooly.sh")
	var page []struct{ ID string }
	call(t, srv, "GET", "/v1/accounts?limit=1", "", "", 200, &page)
	if len(page) != 1 || page[0].ID != second {
		t.Fatalf("first page = %+v, want newest %s", page, second)
	}
	call(t, srv, "GET", "/v1/accounts?limit=1&after="+second, "", "", 200, &page)
	if len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("second page = %+v, want %s", page, a.ID)
	}
}

func TestCreateMessageREST(t *testing.T) {
	srv := newTestServer(t)
	accountID := createAccount(t, srv, "hello@spooly.sh")

	// post delivers rawMessage(n) to an address with an Idempotency-Key.
	post := func(to, query, key string, n, want int) messageJSON {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/accounts/"+to+"/messages"+query, strings.NewReader(rawMessage(n)))
		req.Header.Set("Content-Type", "message/rfc822")
		req.Header.Set("Idempotency-Key", key)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got messageJSON
		json.NewDecoder(resp.Body).Decode(&got)
		if resp.StatusCode != want {
			t.Fatalf("POST %s%s with Idempotency-Key %q: status %d, want %d", to, query, key, resp.StatusCode, want)
		}
		return got
	}

	m := post("hello+news@spooly.sh", "?mail_from=bounce@example.com", "k1", 1, 200)
	if m.AccountID != accountID || m.RcptTo != "hello+news@spooly.sh" || m.MailFrom != "bounce@example.com" {
		t.Errorf("envelope = %+v", m)
	}
	if m.Subject != "Hi 1" || m.From.Address != "ann@example.com" || m.Snippet != "Hello 1" {
		t.Errorf("parsed = %+v", m)
	}
	if m.Folder != "inbox" || len(m.Labels) != 0 || m.Seen || m.Flagged || m.Answered || m.Draft {
		t.Errorf("folder %q, labels %v, flags %+v", m.Folder, m.Labels, m)
	}
	if m.Size != fmt.Sprint(len(rawMessage(1))) {
		t.Errorf("size = %s", m.Size)
	}
	if len(m.Parts) != 1 || m.Parts[0].Type != "text/plain" || m.Parts[0].Size != "9" {
		t.Errorf("parts = %+v", m.Parts)
	}

	// A retry with the same key returns the stored message.
	if again := post("hello@spooly.sh", "", "k1", 2, 200); again.ID != m.ID {
		t.Errorf("retry stored a new message %s, want %s", again.ID, m.ID)
	}
	post("hello@spooly.sh", "", strings.Repeat("k", 129), 3, 400)

	// By account ID, rcpt_to is the account's address.
	var byID messageJSON
	call(t, srv, "POST", "/v1/messages?account_id_or_address="+accountID, "message/rfc822", rawMessage(3), 200, &byID)
	if byID.RcptTo != "hello@spooly.sh" {
		t.Errorf("rcpt_to by ID = %q", byID.RcptTo)
	}

	call(t, srv, "POST", "/v1/accounts/nobody@spooly.sh/messages", "message/rfc822", rawMessage(4), 404, nil)
	call(t, srv, "POST", "/v1/messages", "message/rfc822", rawMessage(5), 400, nil)
	call(t, srv, "POST", "/v1/accounts/hello@spooly.sh/messages", "message/rfc822", "", 400, nil)

	var got messageJSON
	call(t, srv, "GET", "/v1/messages/"+m.ID, "", "", 200, &got)
	if got.ID != m.ID || len(got.Parts) != 1 {
		t.Errorf("got %+v, want %+v", got, m)
	}
	var headers []struct{ Name, Value string }
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/headers", "", "", 200, &headers)
	if fmt.Sprint(headers) != "[{From Ann <ann@example.com>} {To hello@spooly.sh} {Subject Hi 1}]" {
		t.Errorf("headers = %v", headers)
	}

	resp, err := srv.Client().Get(srv.URL + "/v1/messages/" + m.ID + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != rawMessage(1) || resp.Header.Get("Content-Type") != "message/rfc822" {
		t.Errorf("raw = %q (%s)", raw, resp.Header.Get("Content-Type"))
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="Hi 1.eml"` {
		t.Errorf("raw Content-Disposition = %q", got)
	}
}

func TestCreateMessageConnectStream(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")

	client := spoolyv1connect.NewMessageServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL)))
	stream, err := client.CreateMessage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw := rawMessage(1)
	first := &spoolyv1.CreateMessageRequest{
		AccountIdOrAddress: "hello@spooly.sh",
		MailFrom:           "bounce@example.com",
		Raw:                &httpbody.HttpBody{ContentType: "message/rfc822", Data: []byte(raw[:10])},
	}
	if err := stream.Send(first); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&spoolyv1.CreateMessageRequest{Raw: &httpbody.HttpBody{Data: []byte(raw[10:])}}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		t.Fatalf("CloseAndReceive: %v", err)
	}
	if resp.Message.Subject != "Hi 1" || resp.Message.Size != int64(len(raw)) || resp.Message.MailFrom != "bounce@example.com" {
		t.Errorf("message = %v", resp.Message)
	}

	// The Idempotency-Key header works as over REST.
	var ids []string
	for range 2 {
		ctx, info := connect.NewClientContext(t.Context())
		info.RequestHeader().Set("Idempotency-Key", "k1")
		stream, err := client.CreateMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&spoolyv1.CreateMessageRequest{AccountIdOrAddress: "hello@spooly.sh", Raw: &httpbody.HttpBody{Data: []byte(raw)}}); err != nil {
			t.Fatal(err)
		}
		resp, err := stream.CloseAndReceive()
		if err != nil {
			t.Fatalf("CloseAndReceive: %v", err)
		}
		ids = append(ids, resp.Message.Id)
	}
	if ids[0] != ids[1] {
		t.Errorf("retry with Idempotency-Key stored a new message: %v", ids)
	}
}

func TestListMessages(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "a@spooly.sh")
	createAccount(t, srv, "b@spooly.sh")
	var ids []string
	for i := range 3 {
		ids = append(ids, deliver(t, srv, "a@spooly.sh", "", i).ID)
	}
	other := deliver(t, srv, "b@spooly.sh", "", 9)

	var all []messageJSON
	call(t, srv, "GET", "/v1/messages", "", "", 200, &all)
	if len(all) != 4 || all[0].ID != other.ID {
		t.Fatalf("all = %d messages, first %s", len(all), all[0].ID)
	}

	var page []messageJSON
	call(t, srv, "GET", "/v1/accounts/a@spooly.sh/messages?limit=2", "", "", 200, &page)
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("page 1 = %+v", page)
	}
	call(t, srv, "GET", "/v1/accounts/a@spooly.sh/messages?limit=2&after="+page[1].ID, "", "", 200, &page)
	if len(page) != 1 || page[0].ID != ids[0] {
		t.Fatalf("page 2 = %+v", page)
	}

	call(t, srv, "PATCH", "/v1/messages/"+ids[0], "application/json", `{"folder": "archive"}`, 200, nil)
	call(t, srv, "GET", "/v1/messages?account_id_or_address=a@spooly.sh&folder=inbox", "", "", 200, &page)
	if len(page) != 2 {
		t.Errorf("inbox after archiving one = %d messages", len(page))
	}
	call(t, srv, "GET", "/v1/messages?label=nothing", "", "", 200, &page)
	if len(page) != 0 {
		t.Errorf("unknown label listed %d messages", len(page))
	}
	call(t, srv, "GET", "/v1/messages?folder=nothing", "", "", 400, nil)

	// Junk and trash are listed only under their own folders.
	call(t, srv, "PATCH", "/v1/messages/"+ids[1], "application/json", `{"folder": "trash", "flagged": true}`, 200, nil)
	call(t, srv, "PATCH", "/v1/messages/"+ids[2], "application/json", `{"folder": "junk"}`, 200, nil)
	for query, want := range map[string]int{
		"":                           1,
		"&folder=inbox":              0,
		"&folder=archive":            1,
		"&flagged=true":              0,
		"&seen=false":                1,
		"&folder=trash&flagged=true": 1,
		"&folder=junk":               1,
		"&folder=trash":              1,
	} {
		call(t, srv, "GET", "/v1/messages?account_id_or_address=a@spooly.sh"+query, "", "", 200, &page)
		if len(page) != want {
			t.Errorf("%q listed %d messages, want %d", query, len(page), want)
		}
	}
}

func TestUpdateMessage(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	m := deliver(t, srv, "hello@spooly.sh", "", 1)
	path := "/v1/messages/" + m.ID

	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)

	var got messageJSON
	call(t, srv, "PATCH", path, "application/json",
		`{"folder": "archive", "add_labels": ["billing"], "seen": true, "flagged": true}`, 200, &got)
	if got.Folder != "archive" || strings.Join(got.Labels, ",") != "billing" || !got.Seen || !got.Flagged || got.Answered {
		t.Errorf("folder %q, labels %v, flags %+v", got.Folder, got.Labels, got)
	}

	// No folder, labels, or flags is no change.
	var merged messageJSON
	call(t, srv, "PATCH", path, "application/json", `{}`, 200, &merged)
	if merged.Folder != "archive" || len(merged.Labels) != 1 || !merged.Seen || !merged.Flagged {
		t.Errorf("labels or flags changed: %+v", merged)
	}

	// A flag set to false is cleared; the others stay.
	var unread messageJSON
	call(t, srv, "PATCH", path, "application/json", `{"seen": false, "forwarded": true}`, 200, &unread)
	if unread.Seen || !unread.Flagged || !unread.Forwarded {
		t.Errorf("after seen false: %+v", unread)
	}

	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["nope"]}`, 400, nil)
	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["billing"], "remove_labels": ["billing"]}`, 400, nil)
	call(t, srv, "PATCH", path, "application/json", `{"folder": "nope"}`, 400, nil)
	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["Not A Slug"]}`, 400, nil)
	call(t, srv, "PATCH", "/v1/messages/aaaaaaaaaaaaaaaaaaaaa", "application/json", `{}`, 404, nil)

	// Deleting a label removes it from its messages.
	call(t, srv, "DELETE", "/v1/labels/billing", "", "", 200, nil)
	call(t, srv, "GET", path, "", "", 200, &got)
	if len(got.Labels) != 0 || !got.Flagged {
		t.Errorf("after deleting billing: %+v", got)
	}
}

func TestDeleteMessage(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	m := deliver(t, srv, "hello@spooly.sh", "", 1)
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 200, nil)
	call(t, srv, "GET", "/v1/messages/"+m.ID, "", "", 404, nil)
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 404, nil)
	call(t, srv, "DELETE", "/v1/messages/not-an-id", "", "", 400, nil)
}

func TestLabels(t *testing.T) {
	srv := newTestServer(t)

	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "zeta"}`, 200, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "alpha-1"}`, 200, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "alpha-1"}`, 409, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "Bad Name"}`, 400, nil)
	// Folders and flags aren't labels, and no label reads like one.
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "inbox"}`, 400, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "starred"}`, 400, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "spam"}`, 400, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "seen"}`, 400, nil)

	var labels []struct {
		Name      string
		CreatedAt string `json:"created_at"`
	}
	call(t, srv, "GET", "/v1/labels", "", "", 200, &labels)
	var names []string
	for _, l := range labels {
		names = append(names, l.Name)
		if l.CreatedAt == "" {
			t.Errorf("%s has no created_at", l.Name)
		}
	}
	if got := strings.Join(names, ","); got != "alpha-1,zeta" {
		t.Errorf("labels = %s", got)
	}

	call(t, srv, "DELETE", "/v1/labels/inbox", "", "", 404, nil)
	call(t, srv, "DELETE", "/v1/labels/nothing", "", "", 404, nil)
}

// countingStore is a memory store that counts its objects.
type countingStore struct {
	*memory.Storage
	ids map[string]bool
}

func (s *countingStore) Put(ctx context.Context, id, contentType string, r io.Reader) (storage.Stored, error) {
	s.ids[id] = true
	return s.Storage.Put(ctx, id, contentType, r)
}

func (s *countingStore) Delete(ctx context.Context, id string) error {
	delete(s.ids, id)
	return s.Storage.Delete(ctx, id)
}

func TestStoreKeepsRawInStorage(t *testing.T) {
	database := newTestDB(t)
	store := &countingStore{memory.New(), map[string]bool{}}
	svc := New(database, store, Options{})
	if _, err := database.CreateAccount(t.Context(), db.Account{ID: db.NewID(), Addresses: []string{"hello@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}

	in := Incoming{AccountIDOrAddress: "hello@spooly.sh", IdempotencyKey: "k", Raw: []byte(rawMessage(1))}
	m, err := svc.Store(t.Context(), in)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if len(store.ids) != 1 || !store.ids[m.ID] {
		t.Fatalf("stored objects %v, want only %s", store.ids, m.ID)
	}

	// A retry writes bytes under a new ID, then removes them.
	if again, err := svc.Store(t.Context(), in); err != nil || again.ID != m.ID {
		t.Fatalf("retry = %v, %v", again, err)
	}
	if len(store.ids) != 1 {
		t.Errorf("retry left objects %v", store.ids)
	}

	_, rc, err := svc.OpenRaw(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != rawMessage(1) {
		t.Errorf("raw = %q", b)
	}

	// A row whose bytes are gone reads as not found, not as an empty message.
	store.Delete(t.Context(), m.ID)
	if _, _, err := svc.OpenRaw(t.Context(), m.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("OpenRaw without bytes = %v, want not found", err)
	}
}

func TestRawMessageLargerThanAChunk(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	raw := "Subject: big\r\n\r\n" + strings.Repeat("0123456789abcdef", 3*chunkSize/16+5)
	var m messageJSON
	call(t, srv, "POST", "/v1/accounts/hello@spooly.sh/messages", "message/rfc822", raw, 200, &m)

	resp, err := srv.Client().Get(srv.URL + "/v1/messages/" + m.ID + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != raw {
		t.Errorf("raw is %d bytes, want %d", len(got), len(raw))
	}
}

func TestGetMessagePart(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	raw := "Subject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename*=iso-8859-1''caf%E9.txt\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n--b--\r\n"
	var m messageJSON
	call(t, srv, "POST", "/v1/accounts/hello@spooly.sh/messages", "message/rfc822", raw, 200, &m)
	var kinds []string
	for _, p := range m.Parts {
		kinds = append(kinds, p.Disposition+":"+p.Name+":"+p.CID)
	}
	if got := strings.Join(kinds, ","); got != "::,attachment:café.txt:,inline::logo@x,attachment:invoice.pdf:" {
		t.Errorf("parts = %s", got)
	}

	for _, tc := range []struct {
		index, contentType, disposition, body string
	}{
		{"0", "text/plain; charset=utf-8", "inline", "body"},
		{"1", "text/plain; charset=utf-8", "attachment; filename*=utf-8''caf%C3%A9.txt", "café"},
		{"2", "image/png", "inline", "\x89PNG"},
		{"3", "application/pdf", `attachment; filename=invoice.pdf`, "%PDF-1.4"},
	} {
		resp, err := srv.Client().Get(srv.URL + "/v1/messages/" + m.ID + "/parts/" + tc.index)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != tc.body {
			t.Errorf("part %s: %d %q", tc.index, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != tc.contentType {
			t.Errorf("part %s: Content-Type %q, want %q", tc.index, got, tc.contentType)
		}
		if got := resp.Header.Get("Content-Disposition"); got != tc.disposition {
			t.Errorf("part %s: Content-Disposition %q, want %q", tc.index, got, tc.disposition)
		}
	}
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/parts/4", "", "", 404, nil)
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/parts/-1", "", "", 400, nil)
}

func TestEmlFilename(t *testing.T) {
	for subject, want := range map[string]string{
		"Can't sign in":             "Can't sign in.eml",
		"Re: Invoice 1/2 <urgent>?": "Re_ Invoice 1_2 _urgent__.eml",
		"Счёт\tза  октябрь":         "Счёт за октябрь.eml",
		"...hidden.":                "hidden.eml",
		"":                          "01m5w8mp868fgkg22shbhjph9s.eml",
		"   ":                       "01m5w8mp868fgkg22shbhjph9s.eml",
		strings.Repeat("я", 150):    strings.Repeat("я", 100) + ".eml",
	} {
		if got := EmlFilename(&db.Message{ID: "01m5w8mp868fgkg22shbhjph9s", Subject: subject}); got != want {
			t.Errorf("EmlFilename(%q) = %q, want %q", subject, got, want)
		}
	}
}

// TestAccess goes through API keys, as clients do: an admin, Alice with
// access to support@, and Bob with none.
func TestAccess(t *testing.T) {
	ctx := t.Context()
	database := newTestDB(t)
	keys := map[string]string{}
	userIDs := map[string]string{}
	for _, who := range []string{"admin", "alice", "bob"} {
		u, err := database.UpsertUser(ctx, who, who+"@example.com", who)
		if err != nil {
			t.Fatal(err)
		}
		userIDs[who] = u.ID
		if keys[who], _, err = database.CreateAPIKey(ctx, u.ID, who); err != nil {
			t.Fatal(err)
		}
	}

	svc := New(database, memory.New(), Options{Admins: []string{"Admin@Example.com"}})
	mux := http.NewServeMux()
	if err := svc.Mount(mux); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	as := func(who, method, path, contentType, body string, want int, out any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+keys[who])
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s %s: status %d, want %d; body: %s", who, method, path, resp.StatusCode, want, b)
		}
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Only admins create accounts.
	as("alice", "POST", "/v1/accounts", "application/json", `{"addresses": ["x@spooly.sh"]}`, 403, nil)
	var support, personal struct{ ID string }
	as("admin", "POST", "/v1/accounts", "application/json", `{"addresses": ["support@spooly.sh"]}`, 200, &support)
	as("admin", "POST", "/v1/accounts", "application/json", `{"addresses": ["vh@spooly.sh"]}`, 200, &personal)
	if err := svc.Share(auth.ContextWithUserID(ctx, userIDs["admin"]), support.ID, "Alice@Example.com"); err != nil {
		t.Fatal(err)
	}

	// The admin's key delivers to any account, and unknown addresses are
	// rejected; Alice delivers only to hers.
	var toSupport, toPersonal messageJSON
	as("admin", "POST", "/v1/accounts/support@spooly.sh/messages", "message/rfc822", rawMessage(1), 200, &toSupport)
	as("admin", "POST", "/v1/accounts/vh@spooly.sh/messages", "message/rfc822", rawMessage(2), 200, &toPersonal)
	as("admin", "POST", "/v1/accounts/nobody@spooly.sh/messages", "message/rfc822", rawMessage(3), 404, nil)
	as("alice", "POST", "/v1/accounts/vh@spooly.sh/messages", "message/rfc822", rawMessage(3), 404, nil)

	// Alice sees support@ only; vh@ doesn't exist for her. Bob sees nothing.
	var accounts []struct{ Addresses []string }
	as("alice", "GET", "/v1/accounts", "", "", 200, &accounts)
	if len(accounts) != 1 || accounts[0].Addresses[0] != "support@spooly.sh" {
		t.Errorf("alice sees accounts %+v", accounts)
	}
	var messages []messageJSON
	as("alice", "GET", "/v1/messages", "", "", 200, &messages)
	if len(messages) != 1 || messages[0].ID != toSupport.ID {
		t.Errorf("alice sees messages %+v", messages)
	}
	as("alice", "GET", "/v1/accounts/vh@spooly.sh", "", "", 404, nil)
	as("alice", "GET", "/v1/messages/"+toPersonal.ID, "", "", 404, nil)
	as("alice", "GET", "/v1/messages/"+toSupport.ID+"/raw", "", "", 200, nil)
	as("alice", "PATCH", "/v1/messages/"+toSupport.ID, "application/json", `{"flagged": true}`, 200, nil)
	as("alice", "DELETE", "/v1/labels/inbox", "", "", 403, nil)
	as("alice", "POST", "/v1/labels", "application/json", `{"name": "mine"}`, 403, nil)
	as("admin", "POST", "/v1/labels", "application/json", `{"name": "mine"}`, 200, nil)
	as("alice", "GET", "/v1/labels", "", "", 200, nil)
	as("bob", "GET", "/v1/accounts", "", "", 200, &accounts)
	as("bob", "GET", "/v1/messages", "", "", 200, &messages)
	if len(accounts) != 0 || len(messages) != 0 {
		t.Errorf("bob sees %d accounts and %d messages", len(accounts), len(messages))
	}
	as("bob", "PATCH", "/v1/messages/"+toSupport.ID, "application/json", `{"flagged": true}`, 404, nil)
	var all []messageJSON
	as("admin", "GET", "/v1/messages", "", "", 200, &all)
	if len(all) != 2 {
		t.Errorf("admin sees %d messages, want 2", len(all))
	}

	// More addresses route mail to the account, each to one account only.
	// Only admins manage accounts; Alice can't, though she has access.
	as("alice", "PATCH", "/v1/accounts/support@spooly.sh", "application/json", `{"name": "Mine"}`, 403, nil)
	var updated struct{ Addresses []string }
	as("admin", "PATCH", "/v1/accounts/support@spooly.sh", "application/json", `{"addresses": ["support@spooly.sh", "contact@domain2.com", "Help@Spooly.sh"]}`, 200, &updated)
	if strings.Join(updated.Addresses, ",") != "support@spooly.sh,contact@domain2.com,help@spooly.sh" {
		t.Errorf("addresses = %v", updated.Addresses)
	}
	as("admin", "PATCH", "/v1/accounts/vh@spooly.sh", "application/json", `{"addresses": ["vh@spooly.sh", "contact@domain2.com"]}`, 409, nil)
	as("admin", "PATCH", "/v1/accounts/vh@spooly.sh", "application/json", `{"addresses": ["vh@spooly.sh", "vh+x@spooly.sh"]}`, 400, nil)
	as("admin", "PATCH", "/v1/accounts/vh@spooly.sh", "application/json", `{"addresses": ["vh@spooly.sh", "VH@spooly.sh"]}`, 400, nil)
	as("bob", "PATCH", "/v1/accounts/support@spooly.sh", "application/json", `{"name": "Mine"}`, 404, nil)
	var routed messageJSON
	as("admin", "POST", "/v1/accounts/contact+web@domain2.com/messages", "message/rfc822", rawMessage(4), 200, &routed)
	if routed.AccountID != support.ID || routed.RcptTo != "contact+web@domain2.com" {
		t.Errorf("mail to an extra address: account %s, rcpt_to %s", routed.AccountID, routed.RcptTo)
	}
	var found struct{ ID string }
	as("alice", "GET", "/v1/accounts/contact@domain2.com", "", "", 200, &found)
	if found.ID != support.ID {
		t.Errorf("lookup by an extra address found %s", found.ID)
	}

	// Reordering changes which address is shown; removing one rejects its
	// mail.
	as("admin", "PATCH", "/v1/accounts/support@spooly.sh", "application/json", `{"addresses": ["help@spooly.sh", "support@spooly.sh", "contact@domain2.com"]}`, 200, &updated)
	if updated.Addresses[0] != "help@spooly.sh" {
		t.Errorf("after reordering: %+v", updated)
	}
	as("admin", "PATCH", "/v1/accounts/help@spooly.sh", "application/json", `{"addresses": ["help@spooly.sh", "contact@domain2.com"]}`, 200, nil)
	as("admin", "POST", "/v1/accounts/support@spooly.sh/messages", "message/rfc822", rawMessage(5), 404, nil)

	// Mail clients log in with access only.
	if _, _, err := svc.LoginMail(ctx, "vh@spooly.sh", keys["alice"]); err == nil {
		t.Error("alice logged in to an account not shared with her")
	}
	if _, _, err := svc.LoginMail(ctx, "help@spooly.sh", keys["alice"]); err != nil {
		t.Errorf("alice's login: %v", err)
	}
}

// Lists go by received date, not delivery order: imported old mail sorts
// by its real date, and pages follow that order.
func TestListOrderByReceived(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	// Delivered in this order, received at these times.
	times := []string{
		"2026-10-20T10:00:00Z",
		"2026-10-25T10:00:00.5Z", // fractional seconds sort with whole ones
		"2026-10-01T09:00:00Z",   // imported: older than the rest
		"2026-10-25T10:00:00Z",
		"2026-10-25T10:00:00.25Z",
	}
	ids := map[string]string{}
	for i, at := range times {
		var m messageJSON
		call(t, srv, "POST", "/v1/accounts/hello@spooly.sh/messages?received_at="+at, "message/rfc822", rawMessage(i), 200, &m)
		ids[m.ID] = at
	}
	want := []string{"2026-10-25T10:00:00.5Z", "2026-10-25T10:00:00.25Z", "2026-10-25T10:00:00Z", "2026-10-20T10:00:00Z", "2026-10-01T09:00:00Z"}

	var got []string
	after := ""
	for page := 0; page < 5; page++ {
		var msgs []messageJSON
		path := "/v1/messages?limit=2"
		if after != "" {
			path += "&after=" + after
		}
		call(t, srv, "GET", path, "", "", 200, &msgs)
		for _, m := range msgs {
			got = append(got, ids[m.ID])
		}
		if len(msgs) < 2 {
			break
		}
		after = msgs[len(msgs)-1].ID
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v\nwant    %v", got, want)
	}
}

// A relay mirroring a mailbox stores sent and archived mail in its own
// folder, with its labels and flags, instead of the inbox.
func TestCreateMessageFolder(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")

	sent := deliver(t, srv, "hello@spooly.sh", "folder=sent", 1)
	if sent.Folder != "sent" || len(sent.Labels) != 0 {
		t.Errorf("sent mail: folder %q, labels %v", sent.Folder, sent.Labels)
	}
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)
	read := deliver(t, srv, "hello@spooly.sh", "folder=archive&labels=billing&seen=true&flagged=true&forwarded=true", 2)
	if read.Folder != "archive" || strings.Join(read.Labels, ",") != "billing" || !read.Seen || !read.Flagged || !read.Forwarded || read.Answered {
		t.Errorf("folder %q, labels %v, flags %+v", read.Folder, read.Labels, read)
	}
	for _, query := range []string{"labels=nope", "labels=Not%20A%20Slug", "folder=nope", "folder=billing"} {
		call(t, srv, "POST", "/v1/accounts/hello@spooly.sh/messages?"+query, "message/rfc822", rawMessage(3), 400, nil)
	}

	var inSent []messageJSON
	call(t, srv, "GET", "/v1/messages?folder=sent", "", "", 200, &inSent)
	if len(inSent) != 1 || inSent[0].ID != sent.ID {
		t.Errorf("Sent lists %d messages", len(inSent))
	}
}

// Mail to a plus address gets its tag's label, if there's one.
func TestPlusAddressLabel(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@spooly.sh")
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)

	m := deliver(t, srv, "hello+billing@spooly.sh", "", 1)
	if m.Folder != "inbox" || strings.Join(m.Labels, ",") != "billing" {
		t.Errorf("hello+billing@: folder %q, labels %v", m.Folder, m.Labels)
	}
	m = deliver(t, srv, "hello+billing@spooly.sh", "labels=billing", 2)
	if strings.Join(m.Labels, ",") != "billing" {
		t.Errorf("hello+billing@ labeled billing too: labels %v", m.Labels)
	}
	m = deliver(t, srv, "hello+news@spooly.sh", "", 3)
	if len(m.Labels) != 0 {
		t.Errorf("hello+news@ with no news label: labels %v", m.Labels)
	}
}
