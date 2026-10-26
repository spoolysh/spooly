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

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/gen/boxy/v1/boxyv1connect"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/storage"
	"github.com/boxysh/boxy/internal/storage/memory"
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

// newTestServer serves the API as cmd/boxy does, minus authentication.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	if err := New(newTestDB(t), memory.New()).mount(mux, func(h http.Handler) http.Handler { return h }); err != nil {
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
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	From        struct{ Name, Addr string }
	Subject     string            `json:"subject"`
	Snippet     string            `json:"snippet"`
	Size        string            `json:"size"` // int64 is a JSON string
	Labels      []string          `json:"labels"`
	Metadata    map[string]string `json:"metadata"`
	Attachments []any             `json:"attachments"`
	Body        *struct {
		Text string `json:"text"`
		HTML string `json:"html"`
	} `json:"body"`
}

const testMessage = "From: Ann <ann@example.com>\r\nTo: hello@boxy.sh\r\nSubject: Hi %d\r\n\r\nHello %d\r\n"

func rawMessage(n int) string {
	return fmt.Sprintf(testMessage, n, n)
}

func createAccount(t *testing.T, srv *httptest.Server, addr string) string {
	t.Helper()
	var a struct{ ID string }
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addr": "`+addr+`"}`, 200, &a)
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
		ID, Addr, Name string
	}
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addr": "Hello@Boxy.sh", "name": "Hello"}`, 200, &a)
	if a.Addr != "hello@boxy.sh" || a.Name != "Hello" || len(a.ID) != 26 || strings.ToLower(a.ID) != a.ID {
		t.Errorf("created %+v", a)
	}

	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addr": "hello@boxy.sh"}`, 409, nil)
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addr": "hello+x@boxy.sh"}`, 400, nil)
	call(t, srv, "POST", "/v1/accounts", "application/json", `{"addr": "not an address"}`, 400, nil)

	for _, idOrAddr := range []string{a.ID, "hello@boxy.sh", "HELLO+news@boxy.sh"} {
		var got struct{ ID string }
		call(t, srv, "GET", "/v1/accounts/"+url.PathEscape(idOrAddr), "", "", 200, &got)
		if got.ID != a.ID {
			t.Errorf("GET %s = %s, want %s", idOrAddr, got.ID, a.ID)
		}
	}
	call(t, srv, "GET", "/v1/accounts/nobody@boxy.sh", "", "", 404, nil)

	second := createAccount(t, srv, "second@boxy.sh")
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
	accountID := createAccount(t, srv, "hello@boxy.sh")

	m := deliver(t, srv, "hello+news@boxy.sh", "sender=bounce@example.com&idempotency_key=k1", 1)
	if m.AccountID != accountID || m.Recipient != "hello+news@boxy.sh" || m.Sender != "bounce@example.com" {
		t.Errorf("envelope = %+v", m)
	}
	if m.Subject != "Hi 1" || m.From.Addr != "ann@example.com" || m.Snippet != "Hello 1" {
		t.Errorf("parsed = %+v", m)
	}
	if strings.Join(m.Labels, ",") != "inbox,unread" {
		t.Errorf("labels = %v", m.Labels)
	}
	if m.Size != fmt.Sprint(len(rawMessage(1))) {
		t.Errorf("size = %s", m.Size)
	}
	if m.Body != nil {
		t.Errorf("CreateMessage returned a body")
	}

	// A retry with the same key returns the stored message.
	if again := deliver(t, srv, "hello@boxy.sh", "idempotency_key=k1", 2); again.ID != m.ID {
		t.Errorf("retry stored a new message %s, want %s", again.ID, m.ID)
	}

	// By account ID, the recipient is the account's address.
	var byID messageJSON
	call(t, srv, "POST", "/v1/messages?account_id_or_addr="+accountID, "message/rfc822", rawMessage(3), 200, &byID)
	if byID.Recipient != "hello@boxy.sh" {
		t.Errorf("recipient by ID = %q", byID.Recipient)
	}

	call(t, srv, "POST", "/v1/accounts/nobody@boxy.sh/messages", "message/rfc822", rawMessage(4), 404, nil)
	call(t, srv, "POST", "/v1/messages", "message/rfc822", rawMessage(5), 400, nil)
	call(t, srv, "POST", "/v1/accounts/hello@boxy.sh/messages", "message/rfc822", "", 400, nil)

	var full struct {
		messageJSON
	}
	call(t, srv, "GET", "/v1/messages/"+m.ID, "", "", 200, &full)
	if full.Body == nil || strings.TrimSpace(full.Body.Text) != "Hello 1" {
		t.Errorf("body = %+v", full.Body)
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
	createAccount(t, srv, "hello@boxy.sh")

	client := boxyv1connect.NewMessageServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL)))
	stream, err := client.CreateMessage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw := rawMessage(1)
	first := &boxyv1.CreateMessageRequest{
		AccountIdOrAddr: "hello@boxy.sh",
		Sender:          "bounce@example.com",
		Raw:             &httpbody.HttpBody{ContentType: "message/rfc822", Data: []byte(raw[:10])},
	}
	if err := stream.Send(first); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&boxyv1.CreateMessageRequest{Raw: &httpbody.HttpBody{Data: []byte(raw[10:])}}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		t.Fatalf("CloseAndReceive: %v", err)
	}
	if resp.Message.Subject != "Hi 1" || resp.Message.Size != int64(len(raw)) || resp.Message.Sender != "bounce@example.com" {
		t.Errorf("message = %v", resp.Message)
	}
}

func TestListMessages(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "a@boxy.sh")
	createAccount(t, srv, "b@boxy.sh")
	var ids []string
	for i := range 3 {
		ids = append(ids, deliver(t, srv, "a@boxy.sh", "", i).ID)
	}
	other := deliver(t, srv, "b@boxy.sh", "", 9)

	var all []messageJSON
	call(t, srv, "GET", "/v1/messages", "", "", 200, &all)
	if len(all) != 4 || all[0].ID != other.ID {
		t.Fatalf("all = %d messages, first %s", len(all), all[0].ID)
	}

	var page []messageJSON
	call(t, srv, "GET", "/v1/accounts/a@boxy.sh/messages?limit=2", "", "", 200, &page)
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("page 1 = %+v", page)
	}
	call(t, srv, "GET", "/v1/accounts/a@boxy.sh/messages?limit=2&after="+page[1].ID, "", "", 200, &page)
	if len(page) != 1 || page[0].ID != ids[0] {
		t.Fatalf("page 2 = %+v", page)
	}

	call(t, srv, "PATCH", "/v1/messages/"+ids[0], "application/json", `{"remove_labels": ["inbox"]}`, 200, nil)
	call(t, srv, "GET", "/v1/messages?account_id_or_addr=a@boxy.sh&label=inbox", "", "", 200, &page)
	if len(page) != 2 {
		t.Errorf("inbox after archiving one = %d messages", len(page))
	}
	call(t, srv, "GET", "/v1/messages?label=nothing", "", "", 200, &page)
	if len(page) != 0 {
		t.Errorf("unknown label listed %d messages", len(page))
	}
}

func TestUpdateMessage(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@boxy.sh")
	m := deliver(t, srv, "hello@boxy.sh", "", 1)
	path := "/v1/messages/" + m.ID

	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)

	var got messageJSON
	call(t, srv, "PATCH", path, "application/json",
		`{"add_labels": ["billing", "starred"], "remove_labels": ["unread", "inbox"], "metadata": {"ticket_id": "T-1", "x": "y"}}`, 200, &got)
	if strings.Join(got.Labels, ",") != "starred,billing" {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.Metadata["ticket_id"] != "T-1" || got.Metadata["x"] != "y" {
		t.Errorf("metadata = %v", got.Metadata)
	}

	// Metadata merges; an empty value deletes a key. No labels is no change.
	var merged messageJSON // json.Unmarshal would merge into got's map
	call(t, srv, "PATCH", path, "application/json", `{"metadata": {"x": "", "y": "z"}}`, 200, &merged)
	if len(merged.Metadata) != 2 || merged.Metadata["y"] != "z" || merged.Metadata["ticket_id"] != "T-1" {
		t.Errorf("merged metadata = %v", merged.Metadata)
	}
	if len(merged.Labels) != 2 {
		t.Errorf("labels changed to %v", merged.Labels)
	}

	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["nope"]}`, 400, nil)
	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["inbox"], "remove_labels": ["inbox"]}`, 400, nil)
	call(t, srv, "PATCH", path, "application/json", `{"add_labels": ["Not A Slug"]}`, 400, nil)
	call(t, srv, "PATCH", "/v1/messages/01h0000000000000000000000z", "application/json", `{}`, 404, nil)

	var many strings.Builder
	many.WriteString(`{"metadata": {`)
	for i := range 50 {
		fmt.Fprintf(&many, `"k%d": "v",`, i)
	}
	many.WriteString(`"last": "v"}}`)
	call(t, srv, "PATCH", path, "application/json", many.String(), 400, nil)

	// Deleting a label removes it from its messages.
	call(t, srv, "DELETE", "/v1/labels/billing", "", "", 200, nil)
	call(t, srv, "GET", path, "", "", 200, &got)
	if strings.Join(got.Labels, ",") != "starred" {
		t.Errorf("labels after deleting billing = %v", got.Labels)
	}
}

func TestLabels(t *testing.T) {
	srv := newTestServer(t)

	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "zeta"}`, 200, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "alpha-1"}`, 200, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "alpha-1"}`, 409, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "inbox"}`, 409, nil)
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "Bad Name"}`, 400, nil)

	var labels []struct {
		Name      string
		System    bool
		CreatedAt *string `json:"created_at"`
	}
	call(t, srv, "GET", "/v1/labels", "", "", 200, &labels)
	var names []string
	for _, l := range labels {
		names = append(names, l.Name)
		if l.System != (l.CreatedAt == nil) {
			t.Errorf("%s: system %v, created_at %v", l.Name, l.System, l.CreatedAt)
		}
	}
	if got := strings.Join(names, ","); got != "inbox,unread,starred,sent,spam,trash,alpha-1,zeta" {
		t.Errorf("labels = %s", got)
	}

	call(t, srv, "DELETE", "/v1/labels/inbox", "", "", 400, nil)
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
	svc := New(database, store)
	if _, err := database.CreateAccount(t.Context(), db.Account{ID: db.NewID(), Addr: "hello@boxy.sh"}); err != nil {
		t.Fatal(err)
	}

	in := Incoming{AccountIDOrAddr: "hello@boxy.sh", IdempotencyKey: "k", Raw: []byte(rawMessage(1))}
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
	createAccount(t, srv, "hello@boxy.sh")
	raw := "Subject: big\r\n\r\n" + strings.Repeat("0123456789abcdef", 3*chunkSize/16+5)
	var m messageJSON
	call(t, srv, "POST", "/v1/accounts/hello@boxy.sh/messages", "message/rfc822", raw, 200, &m)

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

func TestGetAttachment(t *testing.T) {
	srv := newTestServer(t)
	createAccount(t, srv, "hello@boxy.sh")
	raw := "Subject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment; filename*=iso-8859-1''caf%E9.txt\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n--b--\r\n"
	var m messageJSON
	call(t, srv, "POST", "/v1/accounts/hello@boxy.sh/messages", "message/rfc822", raw, 200, &m)

	for _, tc := range []struct {
		index, contentType, disposition, body string
	}{
		{"0", "text/plain; charset=utf-8", "attachment; filename*=utf-8''caf%C3%A9.txt", "café"},
		{"1", "application/pdf", `attachment; filename=invoice.pdf`, "%PDF-1.4"},
	} {
		resp, err := srv.Client().Get(srv.URL + "/v1/messages/" + m.ID + "/attachments/" + tc.index)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != tc.body {
			t.Errorf("attachment %s: %d %q", tc.index, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != tc.contentType {
			t.Errorf("attachment %s: Content-Type %q, want %q", tc.index, got, tc.contentType)
		}
		if got := resp.Header.Get("Content-Disposition"); got != tc.disposition {
			t.Errorf("attachment %s: Content-Disposition %q, want %q", tc.index, got, tc.disposition)
		}
	}
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/attachments/2", "", "", 404, nil)
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/attachments/-1", "", "", 400, nil)
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
