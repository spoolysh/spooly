package imap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "modernc.org/sqlite"

	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/service"
	"github.com/spoolysh/spooly/internal/storage/memory"
)

type testEnv struct {
	t    *testing.T
	db   *db.DB
	svc  *service.Service
	addr string
	key  string
	ids  []string // delivered messages, oldest first
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := t.Context()

	database, err := db.New(ctx, "sqlite", "file:"+t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	user, err := database.UpsertUser(ctx, "g1", "vh@example.com", "VH")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := database.CreateAPIKey(ctx, user.ID, "imap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAccount(ctx, db.Account{ID: db.NewID(), Addresses: []string{"hello@spooly.sh"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateLabel(ctx, "billing"); err != nil {
		t.Fatal(err)
	}

	e := &testEnv{t: t, db: database, svc: service.New(database, memory.New(), service.Options{Admins: []string{"vh@example.com"}}), key: key}
	for i := range 3 {
		e.deliver(fmt.Sprintf("Message %d", i))
	}

	s := New(database, e.svc, Options{})
	s.authDelay, s.pollInterval = 0, 20*time.Millisecond
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	e.addr = l.Addr().String()
	return e
}

func (e *testEnv) deliver(subject string) string {
	e.t.Helper()
	raw := "From: Ann <ann@example.com>\r\nTo: hello@spooly.sh\r\nSubject: " + subject + "\r\n\r\nHello, " + subject + "\r\n"
	m, err := e.svc.Store(e.t.Context(), service.Incoming{AccountIDOrAddress: "hello@spooly.sh", Raw: []byte(raw)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.ids = append(e.ids, m.ID)
	return m.ID
}

func (e *testEnv) dial(handler *imapclient.UnilateralDataHandler) *imapclient.Client {
	e.t.Helper()
	c, err := imapclient.DialInsecure(e.addr, &imapclient.Options{UnilateralDataHandler: handler})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	if err := c.Login("hello@spooly.sh", e.key).Wait(); err != nil {
		e.t.Fatalf("Login: %v", err)
	}
	return c
}

// state is a message's folder and labels, then its flags in parentheses if
// it has any: "inbox,billing (seen,flagged)".
func (e *testEnv) state(id string) string {
	e.t.Helper()
	m, err := e.db.GetMessage(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var flags []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"seen", m.Seen}, {"answered", m.Answered}, {"flagged", m.Flagged}, {"draft", m.Draft}, {"forwarded", m.Forwarded}} {
		if f.set {
			flags = append(flags, f.name)
		}
	}
	state := strings.Join(append([]string{m.Folder}, m.Labels...), ",")
	if len(flags) > 0 {
		state += " (" + strings.Join(flags, ",") + ")"
	}
	return state
}

func selectBox(t *testing.T, c *imapclient.Client, name string) *goimap.SelectData {
	t.Helper()
	data, err := c.Select(name, nil).Wait()
	if err != nil {
		t.Fatalf("Select %s: %v", name, err)
	}
	return data
}

func fetchAll(t *testing.T, c *imapclient.Client, options *goimap.FetchOptions) []*imapclient.FetchMessageBuffer {
	t.Helper()
	msgs, err := c.Fetch(goimap.SeqSet{{Start: 1, Stop: 0}}, options).Collect() // 1:*
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return msgs
}

func subjects(msgs []*imapclient.FetchMessageBuffer) string {
	var s []string
	for _, m := range msgs {
		s = append(s, m.Envelope.Subject)
	}
	return strings.Join(s, ",")
}

func TestLogin(t *testing.T) {
	e := newTestEnv(t)
	c, err := imapclient.DialInsecure(e.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("hello@spooly.sh", "wrong").Wait(); err == nil {
		t.Error("login with a wrong key succeeded")
	}
	if err := c.Login("Hello+imap@spooly.sh", e.key).Wait(); err != nil {
		t.Errorf("login with a plus address: %v", err)
	}
}

func TestList(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range list {
		name := l.Mailbox
		for _, a := range l.Attrs {
			if a != goimap.MailboxAttrHasNoChildren && a != goimap.MailboxAttrSubscribed {
				name += " " + string(a)
			}
		}
		got = append(got, name)
	}
	want := `INBOX,Archive \Archive,Sent \Sent,Drafts \Drafts,Junk \Junk,Trash \Trash,billing`
	if strings.Join(got, ",") != want {
		t.Errorf("LIST = %s\nwant   %s", strings.Join(got, ","), want)
	}

	if err := c.Create("projects", nil).Wait(); err != nil {
		t.Errorf("Create: %v", err)
	}
	if _, err := e.db.GetLabel(t.Context(), "projects"); err != nil {
		t.Errorf("Create didn't make a label: %v", err)
	}
	if err := c.Create("Not A Label", nil).Wait(); err == nil {
		t.Error("Create accepted a name that isn't a label")
	}
	if err := c.Delete("billing").Wait(); err == nil {
		t.Error("Delete removed a shared label")
	}
}

func TestReadAndFlags(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)

	sel := selectBox(t, c, "INBOX")
	if sel.NumMessages != 3 || sel.UIDValidity == 0 {
		t.Errorf("SELECT = %+v", sel)
	}
	msgs := fetchAll(t, c, &goimap.FetchOptions{UID: true, Flags: true, Envelope: true, RFC822Size: true})
	if subjects(msgs) != "Message 0,Message 1,Message 2" || len(msgs[0].Flags) != 0 || msgs[0].RFC822Size == 0 {
		t.Errorf("FETCH = %s, flags %v", subjects(msgs), msgs[0].Flags)
	}
	if !(msgs[0].UID < msgs[1].UID && msgs[1].UID < msgs[2].UID && goimap.UID(sel.UIDNext) > msgs[2].UID) {
		t.Errorf("UIDs %d %d %d, UIDNEXT %d", msgs[0].UID, msgs[1].UID, msgs[2].UID, sel.UIDNext)
	}

	// Reading the body without PEEK marks it read.
	body, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{BodySection: []*goimap.FetchItemBodySection{{}}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body[0].BodySection[0].Bytes), "Hello, Message 0") || !slices.Contains(body[0].Flags, goimap.FlagSeen) {
		t.Errorf("BODY[] = %q, flags %v", body[0].BodySection[0].Bytes, body[0].Flags)
	}
	if got := e.state(e.ids[0]); got != "inbox (seen)" {
		t.Errorf("after reading: %s", got)
	}

	if _, err := c.Store(goimap.SeqSetNum(2), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagFlagged, goimap.FlagSeen}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[1]); got != "inbox (seen,flagged)" {
		t.Errorf("after +FLAGS: %s", got)
	}
	if _, err := c.Store(goimap.SeqSetNum(2), &goimap.StoreFlags{Op: goimap.StoreFlagsDel, Flags: []goimap.Flag{goimap.FlagSeen}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[1]); got != "inbox (flagged)" {
		t.Errorf("after -FLAGS: %s", got)
	}
}

// Header-only fetches, as mail clients make for their lists, read just the
// header block and still answer in full.
func TestFetchHeaders(t *testing.T) {
	e := newTestEnv(t)
	raw := "From: Ann <ann@example.com>\r\nTo: hello@spooly.sh\r\nSubject: Re: plan\r\n" +
		"Message-ID: <reply@example.com>\r\nIn-Reply-To: <plan@example.com> <other@example.com>\r\n\r\nSounds good.\r\n"
	if _, err := e.svc.Store(t.Context(), service.Incoming{AccountIDOrAddress: "hello@spooly.sh", Raw: []byte(raw)}); err != nil {
		t.Fatal(err)
	}
	c := e.dial(nil)
	selectBox(t, c, "INBOX")

	msgs, err := c.Fetch(goimap.SeqSetNum(4), &goimap.FetchOptions{
		Envelope: true,
		BodySection: []*goimap.FetchItemBodySection{
			{Specifier: goimap.PartSpecifierHeader, Peek: true},
			{Specifier: goimap.PartSpecifierHeader, HeaderFields: []string{"Subject"}, Peek: true},
		},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	env := msgs[0].Envelope
	if env.Subject != "Re: plan" || env.MessageID != "reply@example.com" || strings.Join(env.InReplyTo, ",") != "plan@example.com,other@example.com" {
		t.Errorf("ENVELOPE = %+v", env)
	}
	header, fields := string(msgs[0].BodySection[0].Bytes), string(msgs[0].BodySection[1].Bytes)
	if !strings.HasPrefix(header, "From: Ann") || !strings.HasSuffix(header, "\r\n\r\n") || strings.Contains(header, "Sounds good") {
		t.Errorf("BODY[HEADER] = %q", header)
	}
	if fields != "Subject: Re: plan\r\n\r\n" {
		t.Errorf("BODY[HEADER.FIELDS (SUBJECT)] = %q", fields)
	}

	// The whole message still comes whole.
	full, err := c.Fetch(goimap.SeqSetNum(4), &goimap.FetchOptions{BodySection: []*goimap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(full[0].BodySection[0].Bytes); got != raw {
		t.Errorf("BODY[] = %q", got)
	}
}

func TestMoveCopyExpunge(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")

	// Delete in a mail client: move to Trash.
	if _, err := c.Move(goimap.SeqSetNum(1), "Trash").Wait(); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := e.state(e.ids[0]); got != "trash" {
		t.Errorf("after moving to Trash: %s", got)
	}
	if got := subjects(fetchAll(t, c, &goimap.FetchOptions{Envelope: true})); got != "Message 1,Message 2" {
		t.Errorf("INBOX after move = %s", got)
	}

	// Copy to a label folder: the message is in both.
	copied, err := c.Copy(goimap.SeqSetNum(1), "billing").Wait()
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := e.state(e.ids[1]); got != "inbox,billing" {
		t.Errorf("after copy: %s", got)
	}
	if len(copied.DestUIDs) == 0 {
		t.Error("COPY didn't report UIDs")
	}

	// \Deleted and EXPUNGE in INBOX archives.
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[1]); got != "archive,billing" {
		t.Errorf("after expunge in INBOX: %s", got)
	}

	// The archived message is in Archive.
	selectBox(t, c, "Archive")
	if got := subjects(fetchAll(t, c, &goimap.FetchOptions{Envelope: true})); got != "Message 1" {
		t.Errorf("Archive = %s", got)
	}

	// Back to the inbox from the trash: it gets a new, higher UID there.
	sel := selectBox(t, c, "INBOX")
	selectBox(t, c, "Trash")
	if _, err := c.Move(goimap.SeqSetNum(1), "INBOX").Wait(); err != nil {
		t.Fatal(err)
	}
	sel2 := selectBox(t, c, "INBOX")
	msgs := fetchAll(t, c, &goimap.FetchOptions{UID: true, Envelope: true})
	last := msgs[len(msgs)-1]
	if last.Envelope.Subject != "Message 0" || last.UID < goimap.UID(sel.UIDNext) || sel2.UIDValidity != sel.UIDValidity {
		t.Errorf("restored message: %s UID %d, UIDNEXT was %d", last.Envelope.Subject, last.UID, sel.UIDNext)
	}

	// Deleting in the trash deletes for good, row and raw source.
	if _, err := c.Move(goimap.SeqSetNum(1), "Trash").Wait(); err != nil { // Message 2
		t.Fatal(err)
	}
	selectBox(t, c, "Trash")
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if expunged, err := c.Expunge().Collect(); err != nil || len(expunged) != 1 {
		t.Errorf("EXPUNGE in Trash = %v, %v", expunged, err)
	}
	if _, err := e.db.GetMessage(t.Context(), e.ids[2]); !errors.Is(err, db.ErrMessageNotFound) {
		t.Errorf("message after emptying the trash: %v", err)
	}
	if _, _, err := e.svc.OpenRaw(t.Context(), e.ids[2]); err == nil {
		t.Error("raw source survived emptying the trash")
	}
}

// Deleting takes effect at once, as with Gmail's auto-expunge: the message
// leaves the folder without an EXPUNGE, and the client hears at its next
// command.
func TestAutoExpunge(t *testing.T) {
	e := newTestEnv(t)
	expunged := make(chan uint32, 10)
	c := e.dial(&imapclient.UnilateralDataHandler{Expunge: func(seq uint32) { expunged <- seq }})
	selectBox(t, c, "INBOX")
	if _, err := c.Store(goimap.SeqSetNum(2), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[1]); got != "archive" {
		t.Errorf("right after deleting in INBOX: %s", got)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-expunged:
		if seq != 2 {
			t.Errorf("EXPUNGE %d, want 2", seq)
		}
	case <-time.After(2 * time.Second):
		t.Error("no EXPUNGE after NOOP")
	}
	if got := subjects(fetchAll(t, c, &goimap.FetchOptions{Envelope: true})); got != "Message 0,Message 2" {
		t.Errorf("INBOX = %s", got)
	}
}

// Trashed mail leaves every other folder while keeping its labels and
// flags, and comes back with a new UID when restored, as with Gmail.
func TestTrashHides(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagFlagged}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Copy(goimap.SeqSetNum(1), "billing").Wait(); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	selectBox(t, c, "billing")
	before := fetchAll(t, c, &goimap.FetchOptions{UID: true, Envelope: true})
	if len(before) != 1 {
		t.Fatalf("billing has %d messages, want 1", len(before))
	}
	selectBox(t, c, "INBOX")
	if _, err := c.Move(goimap.SeqSetNum(1), "Trash").Wait(); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if data := selectBox(t, c, "billing"); data.NumMessages != 0 {
		t.Errorf("billing has %d messages with its one in the trash, want 0", data.NumMessages)
	}

	selectBox(t, c, "Trash")
	if msgs := fetchAll(t, c, &goimap.FetchOptions{Flags: true}); len(msgs) != 1 || !slices.Contains(msgs[0].Flags, goimap.FlagFlagged) {
		t.Fatalf("Trash = %+v, want the message, still flagged", msgs)
	}
	if _, err := c.Move(goimap.SeqSetNum(1), "INBOX").Wait(); err != nil {
		t.Fatalf("Move: %v", err)
	}
	selectBox(t, c, "billing")
	after := fetchAll(t, c, &goimap.FetchOptions{UID: true, Envelope: true})
	if subjects(after) != subjects(before) || after[0].UID <= before[0].UID {
		t.Errorf("billing after restoring = %s UID %d, want %s with a UID above %d", subjects(after), after[0].UID, subjects(before), before[0].UID)
	}
}

// \Answered is the answered flag, set either way.
func TestAnswered(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagAnswered}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	msgs := fetchAll(t, c, &goimap.FetchOptions{Flags: true})
	if !slices.Contains(msgs[0].Flags, goimap.FlagAnswered) || slices.Contains(msgs[1].Flags, goimap.FlagAnswered) {
		t.Errorf("flags = %v, %v; want only the first answered", msgs[0].Flags, msgs[1].Flags)
	}
	if got := e.state(e.ids[0]); got != "inbox (answered)" {
		t.Errorf("after +FLAGS: %s", got)
	}
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsDel, Silent: true, Flags: []goimap.Flag{goimap.FlagAnswered}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[0]); got != "inbox" {
		t.Errorf("after -FLAGS: %s", got)
	}
}

// Drafts saved by a client go to the Drafts folder with \Draft, and
// discarding one deletes it for good.
func TestDrafts(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	raw := "From: hello@spooly.sh\r\nTo: ann@example.com\r\nSubject: Half-written reply\r\n\r\nDear Ann,\r\n"
	cmd := c.Append("Drafts", int64(len(raw)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagSeen, goimap.FlagDraft}})
	cmd.Write([]byte(raw))
	cmd.Close()
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("Append: %v", err)
	}
	selectBox(t, c, "Drafts")
	msgs := fetchAll(t, c, &goimap.FetchOptions{Envelope: true, Flags: true})
	if len(msgs) != 1 || msgs[0].Envelope.Subject != "Half-written reply" || !slices.Contains(msgs[0].Flags, goimap.FlagDraft) {
		t.Fatalf("Drafts = %+v", msgs)
	}
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatal(err)
	}
	all, err := e.db.ListMessages(t.Context(), db.MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("%d messages after discarding the draft, want the 3 delivered", len(all))
	}
}

// Copying to one of Spooly's folders moves the message, which is in only
// one. A client that then deletes the original, as clients without MOVE
// do, leaves it where it went. Deleting from the archive trashes it.
func TestCopyToFolder(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")
	if _, err := c.Copy(goimap.SeqSetNum(1), "Archive").Wait(); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := e.state(e.ids[0]); got != "archive" {
		t.Errorf("after copy: %s", got)
	}
	deleted := &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}
	if _, err := c.Store(goimap.SeqSetNum(1), deleted, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[0]); got != "archive" {
		t.Errorf("after deleting the original: %s", got)
	}

	selectBox(t, c, "Archive")
	if _, err := c.Store(goimap.SeqSetNum(1), deleted, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[0]); got != "trash" {
		t.Errorf("after deleting from Archive: %s", got)
	}
}

// $Forwarded is the forwarded flag. Flags match whatever their case.
func TestForwarded(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")
	if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{"$forwarded"}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := e.state(e.ids[0]); got != "inbox (forwarded)" {
		t.Errorf("after +FLAGS: %s", got)
	}
	msgs := fetchAll(t, c, &goimap.FetchOptions{Flags: true})
	if !slices.Contains(msgs[0].Flags, goimap.FlagForwarded) {
		t.Errorf("flags = %v, want $Forwarded", msgs[0].Flags)
	}

	raw := "From: hello@spooly.sh\r\nSubject: Sent on\r\n\r\nSee below.\r\n"
	cmd := c.Append("Sent", int64(len(raw)), &goimap.AppendOptions{Flags: []goimap.Flag{"\\SEEN", "$forwarded"}})
	cmd.Write([]byte(raw))
	cmd.Close()
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	selectBox(t, c, "Sent")
	sent, err := c.Fetch(goimap.UIDSetNum(data.UID), &goimap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(sent) != 1 || !slices.Contains(sent[0].Flags, goimap.FlagSeen) || !slices.Contains(sent[0].Flags, goimap.FlagForwarded) {
		t.Errorf("appended flags = %+v, %v", sent, err)
	}
}

func TestSearch(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	selectBox(t, c, "INBOX")
	seen := true
	if _, err := e.db.UpdateMessage(t.Context(), e.ids[2], db.MessageUpdate{Seen: &seen}); err != nil {
		t.Fatal(err)
	}
	c.Noop().Wait()

	unseen, err := c.Search(&goimap.SearchCriteria{NotFlag: []goimap.Flag{goimap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := unseen.AllSeqNums(); !slices.Equal(got, []uint32{1, 2}) {
		t.Errorf("SEARCH UNSEEN = %v", got)
	}
	bySubject, err := c.UIDSearch(&goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: "Subject", Value: "message 1"}}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := bySubject.AllUIDs(); len(got) != 1 {
		t.Errorf("UID SEARCH SUBJECT = %v", got)
	}
	byText, err := c.Search(&goimap.SearchCriteria{Body: []string{"hello, message 2"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := byText.AllSeqNums(); !slices.Equal(got, []uint32{3}) {
		t.Errorf("SEARCH BODY = %v", got)
	}
}

func TestAppend(t *testing.T) {
	e := newTestEnv(t)
	c := e.dial(nil)
	raw := "From: hello@spooly.sh\r\nTo: ann@example.com\r\nSubject: Re: Message 0\r\n\r\nThanks!\r\n"
	cmd := c.Append("Sent", int64(len(raw)), &goimap.AppendOptions{Flags: []goimap.Flag{goimap.FlagSeen}})
	cmd.Write([]byte(raw))
	cmd.Close()
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	selectBox(t, c, "Sent")
	msgs := fetchAll(t, c, &goimap.FetchOptions{UID: true, Envelope: true, Flags: true})
	if len(msgs) != 1 || msgs[0].Envelope.Subject != "Re: Message 0" || msgs[0].UID != data.UID || !slices.Contains(msgs[0].Flags, goimap.FlagSeen) {
		t.Errorf("Sent after APPEND = %+v, APPEND UID %d", msgs, data.UID)
	}
}

// Changes made elsewhere reach a client that has the folder open: new mail,
// and labels changed in the dashboard or API.
func TestUpdates(t *testing.T) {
	e := newTestEnv(t)
	exists := make(chan uint32, 10)
	flags := make(chan uint32, 10)
	c := e.dial(&imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				exists <- *data.NumMessages
			}
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			flags <- msg.SeqNum
			for msg.Next() != nil {
			}
		},
	})
	selectBox(t, c, "INBOX")

	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}
	e.deliver("Message 3")
	select {
	case n := <-exists:
		if n != 4 {
			t.Errorf("EXISTS %d, want 4", n)
		}
	case <-time.After(2 * time.Second):
		t.Error("IDLE didn't report new mail")
	}
	flagged := true
	if _, err := e.db.UpdateMessage(t.Context(), e.ids[1], db.MessageUpdate{Flagged: &flagged}); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-flags:
		if seq != 2 {
			t.Errorf("FETCH FLAGS for %d, want 2", seq)
		}
	case <-time.After(2 * time.Second):
		t.Error("IDLE didn't report a flag change")
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
}

// Only users the account is shared with, and admins, can log in to it.
func TestLoginNeedsAccess(t *testing.T) {
	e := newTestEnv(t)
	ctx := t.Context()
	bob, err := e.db.UpsertUser(ctx, "g2", "bob@example.com", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := e.db.CreateAPIKey(ctx, bob.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	c, err := imapclient.DialInsecure(e.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("hello@spooly.sh", key).Wait(); err == nil {
		t.Fatal("logged in to an account not shared with the user")
	}

	a, err := e.db.GetAccountByAddress(ctx, "hello@spooly.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.AddUser(ctx, a.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("hello@spooly.sh", key).Wait(); err != nil {
		t.Fatalf("Login once shared: %v", err)
	}
	if sel := selectBox(t, c, "INBOX"); sel.NumMessages != 3 {
		t.Errorf("INBOX has %d messages, want 3", sel.NumMessages)
	}
}
