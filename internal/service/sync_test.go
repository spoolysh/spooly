package service

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
)

type summaryJSON struct {
	ID      string   `json:"id"`
	Folder  string   `json:"folder"`
	Labels  []string `json:"labels"`
	Seen    bool     `json:"seen"`
	Flagged bool     `json:"flagged"`
	Subject string   `json:"subject"`
}

type listingJSON struct {
	Messages []summaryJSON `json:"messages"`
}

// seqListingJSON is a list by seq.
type seqListingJSON struct {
	Messages []struct {
		Seq     uint32      `json:"seq"`
		Message summaryJSON `json:"message"`
	} `json:"messages"`
	Since   time.Time `json:"since"`
	NextSeq uint32    `json:"nextSeq"`
}

func (l *seqListingJSON) ids() []string {
	var ids []string
	for _, e := range l.Messages {
		ids = append(ids, e.Message.ID)
	}
	return ids
}

type changesJSON struct {
	Created []summaryJSON `json:"created"`
	Updated []summaryJSON `json:"updated"`
	Deleted []string      `json:"deleted"`
	Cursor  string        `json:"cursor"`
	HasMore bool          `json:"hasMore"`
}

// rpc calls a spooly.v1alpha1 method, such as "ChangeService/ListChanges",
// over Connect: none is mapped to REST. Connect leaves out empty fields, so
// nothing of a reused out survives.
func rpc(t *testing.T, srv *httptest.Server, method string, req map[string]any, want int, out any) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero()
	}
	call(t, srv, "POST", "/spooly.v1alpha1."+method, "application/json", string(body), want, out)
}

// listChanges lists a mailbox's changes since cursor.
func listChanges(t *testing.T, srv *httptest.Server, mailbox, cursor string, limit, want int, out any) {
	t.Helper()
	req := map[string]any{"mailbox_id_or_address": mailbox}
	if cursor != "" {
		req["cursor"] = cursor
	}
	if limit != 0 {
		req["limit"] = limit
	}
	rpc(t, srv, "ChangeService/ListChanges", req, want, out)
}

// listBySeq lists a@spooly.sh's messages in view, a folder or label=name,
// by seq.
func listBySeq(t *testing.T, srv *httptest.Server, view string, afterSeq uint32, limit int) seqListingJSON {
	t.Helper()
	req := map[string]any{"mailbox_id_or_address": "a@spooly.sh", "folder": view}
	if label, ok := strings.CutPrefix(view, "label="); ok {
		req = map[string]any{"mailbox_id_or_address": "a@spooly.sh", "label": label}
	}
	if afterSeq != 0 {
		req["after"] = afterSeq
	}
	if limit != 0 {
		req["limit"] = limit
	}
	var l seqListingJSON
	rpc(t, srv, "MessageService/ListMessagesBySeq", req, 200, &l)
	return l
}

// startCursor takes a mailbox's cursor for changes from now, as a sync
// client does before listing it.
func startCursor(t *testing.T, srv *httptest.Server, mailbox string) string {
	t.Helper()
	var start changesJSON
	listChanges(t, srv, mailbox, "", 0, 200, &start)
	if len(start.Created) != 0 || len(start.Updated) != 0 || len(start.Deleted) != 0 || start.Cursor == "" {
		t.Fatalf("starting changes = %+v", start)
	}
	return start.Cursor
}

func entryIDs(entries []summaryJSON) []string {
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// A sync client lists a folder in arrival order, then follows the changes
// from the listing's cursor.
func TestSync(t *testing.T) {
	srv := newTestServer(t)
	createMailbox(t, srv, "a@spooly.sh")
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)
	const base = "/v1/mailboxes/a@spooly.sh/"
	start := startCursor(t, srv, "a@spooly.sh")
	var ids []string
	for i := range 3 {
		ids = append(ids, deliver(t, srv, "a@spooly.sh", "", i).ID)
	}

	inbox := listBySeq(t, srv, "inbox", 0, 0)
	if !slices.Equal(inbox.ids(), ids) {
		t.Fatalf("inbox = %+v", inbox)
	}
	first, second, last := inbox.Messages[0].Seq, inbox.Messages[1].Seq, inbox.Messages[2].Seq
	if !(0 < first && first < second && second < last) {
		t.Errorf("folder seqs %d %d %d", first, second, last)
	}
	if inbox.NextSeq <= last || inbox.Since.IsZero() {
		t.Errorf("inbox next seq %d after %d, since %v", inbox.NextSeq, last, inbox.Since)
	}
	if page := listBySeq(t, srv, "inbox", 0, 2); !slices.Equal(page.ids(), ids[:2]) {
		t.Errorf("page 1 = %v", page.ids())
	}
	if page := listBySeq(t, srv, "inbox", second, 0); !slices.Equal(page.ids(), ids[2:]) {
		t.Errorf("page 2 = %v", page.ids())
	}

	// Every change since the start is every message; none since then.
	var changes changesJSON
	listChanges(t, srv, "a@spooly.sh", start, 0, 200, &changes)
	if !slices.Equal(entryIDs(changes.Created), ids) || len(changes.Updated) != 0 || len(changes.Deleted) != 0 || changes.Cursor == start || changes.HasMore {
		t.Errorf("changes since the start = %+v", changes)
	}
	synced := changes.Cursor
	listChanges(t, srv, "a@spooly.sh", synced, 0, 200, &changes)
	if len(changes.Created) != 0 || len(changes.Updated) != 0 || len(changes.Deleted) != 0 || changes.Cursor != synced {
		t.Errorf("changes since = %+v", changes)
	}
	listChanges(t, srv, "a@spooly.sh", "not-a-cursor", 0, 400, nil)

	// Archive one, label two at once, and delete one of those.
	call(t, srv, "PATCH", "/v1/messages/"+ids[0], "application/json", `{"folder": "archive"}`, 200, nil)
	var batch struct{ Updated int }
	rpc(t, srv, "MessageService/BatchUpdateMessages", map[string]any{"ids": []string{ids[1], ids[2], "00000000000000000000000000"}, "add_labels": []string{"billing"}}, 200, &batch)
	if batch.Updated != 2 {
		t.Errorf("batch updated %d, want 2", batch.Updated)
	}
	call(t, srv, "DELETE", "/v1/messages/"+ids[2], "", "", 200, nil)

	listChanges(t, srv, "a@spooly.sh", synced, 0, 200, &changes)
	if len(changes.Created) != 0 || !slices.Equal(entryIDs(changes.Updated), ids[:2]) || !slices.Equal(changes.Deleted, ids[2:]) || changes.HasMore {
		t.Fatalf("changes = %+v", changes)
	}
	if archived := changes.Updated[0]; archived.Folder != "archive" {
		t.Errorf("archived = %+v", archived)
	}
	if labeled := changes.Updated[1]; strings.Join(labeled.Labels, ",") != "billing" {
		t.Errorf("labeled = %+v", labeled)
	}
	// In the archive, the archived message came last.
	if archive := listBySeq(t, srv, "archive", 0, 0); len(archive.Messages) != 1 || archive.Messages[0].Seq <= last {
		t.Errorf("archive = %+v, want a seq above %d", archive.Messages, last)
	}
	all := changes.Cursor

	// An event at a time, the same changes, each message once.
	seen := map[string]bool{}
	for since := synced; ; {
		listChanges(t, srv, "a@spooly.sh", since, 1, 200, &changes)
		for _, id := range append(append(entryIDs(changes.Created), entryIDs(changes.Updated)...), changes.Deleted...) {
			seen[id] = true
		}
		since = changes.Cursor
		if !changes.HasMore {
			break
		}
	}
	if len(seen) != 3 || changes.Cursor != all {
		t.Errorf("paged changes = %v up to %s, want all 3 up to %s", seen, changes.Cursor, all)
	}

	// A label's listing leaves out junk and trash; back from the trash, the
	// message has a new, higher seq there.
	billing := listBySeq(t, srv, "label=billing", 0, 0)
	if !slices.Equal(billing.ids(), ids[1:2]) {
		t.Fatalf("billing = %v", billing.ids())
	}
	before := billing.Messages[0].Seq
	if !billing.Since.After(inbox.Since) {
		t.Errorf("billing since %v, not after the mailbox's %v", billing.Since, inbox.Since)
	}
	call(t, srv, "PATCH", "/v1/messages/"+ids[1], "application/json", `{"folder": "trash"}`, 200, nil)
	if billing = listBySeq(t, srv, "label=billing", 0, 0); len(billing.Messages) != 0 {
		t.Errorf("billing with its message in the trash = %v", billing.ids())
	}
	call(t, srv, "PATCH", "/v1/messages/"+ids[1], "application/json", `{"folder": "inbox"}`, 200, nil)
	if billing = listBySeq(t, srv, "label=billing", 0, 0); len(billing.Messages) != 1 || billing.Messages[0].Seq <= before {
		t.Errorf("billing after restoring = %+v, want a label seq above %d", billing.Messages, before)
	}

	// A list by seq is a folder's or a label's, not both.
	rpc(t, srv, "MessageService/ListMessagesBySeq", map[string]any{"mailbox_id_or_address": "a@spooly.sh"}, 400, nil)
	rpc(t, srv, "MessageService/ListMessagesBySeq", map[string]any{"mailbox_id_or_address": "a@spooly.sh", "folder": "inbox", "label": "billing"}, 400, nil)
	rpc(t, srv, "MessageService/ListMessagesBySeq", map[string]any{"mailbox_id_or_address": "nobody@spooly.sh", "folder": "inbox"}, 404, nil)
	rpc(t, srv, "MessageService/ListMessagesBySeq", map[string]any{"mailbox_id_or_address": "a@spooly.sh", "label": "nolabel"}, 404, nil)
	var newest listingJSON
	call(t, srv, "GET", base+"messages", "", "", 200, &newest)
	if len(newest.Messages) == 0 || newest.Messages[0].Subject == "" {
		t.Errorf("newest = %+v", newest.Messages)
	}
	listChanges(t, srv, "nobody@spooly.sh", "", 0, 404, nil)
}

// A range of a raw message reads just those bytes.
func TestGetMessageRawRange(t *testing.T) {
	srv := newTestServer(t)
	createMailbox(t, srv, "hello@spooly.sh")
	m := deliver(t, srv, "hello@spooly.sh", "", 1)
	raw := rawMessage(1)

	get := func(query string) string {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + "/v1/messages/" + m.ID + "/raw" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET raw%s: %d %s", query, resp.StatusCode, b)
		}
		return string(b)
	}
	for query, want := range map[string]string{
		"?offset=0&length=4": raw[:4],
		"?offset=6":          raw[6:],
		"?length=10":         raw[:10],
		"?offset=1000":       "",
	} {
		if got := get(query); got != want {
			t.Errorf("raw%s = %q, want %q", query, got, want)
		}
	}
	call(t, srv, "GET", "/v1/messages/"+m.ID+"/raw?length=0", "", "", 400, nil)
}

// A message deleted for good is found no more, but reported as deleted, and
// an admin can restore it until it's expunged. Then changes since before
// the pruned events can't be listed.
func TestDeleteRestoreExpunge(t *testing.T) {
	srv, svc, admin := newTestServerWith(t, Options{ExpungeAfter: time.Hour, EventsRetention: 2 * time.Hour})
	createMailbox(t, srv, "a@spooly.sh")
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)
	const base = "/v1/mailboxes/a@spooly.sh/"

	m := deliver(t, srv, "a@spooly.sh", "folder=archive&labels=billing&seen=true", 1)
	before := startCursor(t, srv, "a@spooly.sh")
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 200, nil)

	call(t, srv, "GET", "/v1/messages/"+m.ID, "", "", 404, nil)
	var list listJSON
	call(t, srv, "GET", "/v1/messages", "", "", 200, &list)
	if len(list) != 0 {
		t.Errorf("deleted message listed: %d messages", len(list))
	}
	var changes changesJSON
	listChanges(t, srv, "a@spooly.sh", before, 0, 200, &changes)
	if !slices.Equal(changes.Deleted, []string{m.ID}) || len(changes.Created) != 0 || len(changes.Updated) != 0 {
		t.Fatalf("changes = %+v", changes)
	}

	// An admin lists it and restores it, with its folder, flags, and labels.
	// That's not in the API.
	call(t, srv, "POST", "/v1/messages/"+m.ID+":restore", "application/json", `{}`, 404, nil)
	deleted, err := svc.ListDeleted(admin, "a@spooly.sh")
	if err != nil || len(deleted) != 1 || deleted[0].ID != m.ID || deleted[0].Folder != "archive" || deleted[0].Subject != "Hi 1" {
		t.Fatalf("deleted = %+v, %v", deleted, err)
	}
	if _, err := svc.ListDeleted(t.Context(), "a@spooly.sh"); err == nil {
		t.Error("listed deleted messages without signing in")
	}
	restored, err := svc.Restore(admin, m.ID)
	if err != nil || restored.ID != m.ID || restored.Folder != "archive" || !restored.Seen || strings.Join(restored.Labels, ",") != "billing" || restored.Subject != "Hi 1" {
		t.Errorf("restored = %+v, %v", restored, err)
	}
	if _, err := svc.Restore(admin, m.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("restoring twice: %v", err)
	}
	listChanges(t, srv, "a@spooly.sh", before, 0, 200, &changes)
	// Deleted and restored since, it came back.
	if !slices.Equal(entryIDs(changes.Created), []string{m.ID}) || len(changes.Updated) != 0 || len(changes.Deleted) != 0 {
		t.Errorf("changes after restoring = %+v", changes)
	}

	// Deleted again, then expunged: its raw source goes, and it can't be
	// restored anymore. It's still reported as deleted.
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 200, nil)
	if err := svc.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if deleted, _ = svc.ListDeleted(admin, "a@spooly.sh"); len(deleted) != 1 {
		t.Fatalf("expunged too soon: deleted = %+v", deleted)
	}
	if err := svc.Prune(t.Context(), time.Now().Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if deleted, _ = svc.ListDeleted(admin, "a@spooly.sh"); len(deleted) != 0 {
		t.Errorf("deleted after expunging = %+v", deleted)
	}
	if _, err := svc.Restore(admin, m.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("restoring after expunging: %v", err)
	}
	if _, err := svc.store.Get(t.Context(), m.ID); err == nil {
		t.Error("raw source survived expunging")
	}
	listChanges(t, srv, "a@spooly.sh", before, 0, 200, &changes)
	if !slices.Equal(changes.Deleted, []string{m.ID}) {
		t.Errorf("changes after expunging = %+v", changes)
	}

	// Past the events' retention, changes since before them can't be listed.
	if err := svc.Prune(t.Context(), time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	listChanges(t, srv, "a@spooly.sh", before, 0, 400, nil)
	// Since the last event, nothing pruned was missed.
	listChanges(t, srv, "a@spooly.sh", changes.Cursor, 0, 200, nil)
	// Listed again, the client follows changes from then.
	relisted := startCursor(t, srv, "a@spooly.sh")
	listChanges(t, srv, "a@spooly.sh", relisted, 0, 200, &changes)
	if len(changes.Created) != 0 || len(changes.Updated) != 0 || len(changes.Deleted) != 0 {
		t.Errorf("changes after listing again = %+v", changes)
	}
}

// Without a time to restore it, a deleted message's raw source goes at once.
func TestDeleteExpungesAtOnce(t *testing.T) {
	srv, svc, admin := newTestServerWith(t, Options{EventsRetention: time.Hour})
	createMailbox(t, srv, "a@spooly.sh")
	m := deliver(t, srv, "a@spooly.sh", "", 1)
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 200, nil)
	if _, err := svc.store.Get(t.Context(), m.ID); err == nil {
		t.Error("raw source survived deleting")
	}
	if _, err := svc.Restore(admin, m.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("restoring: %v", err)
	}
}

// Without a retention, events are kept forever; deleted messages are still
// expunged.
func TestEventsKeptForever(t *testing.T) {
	srv, svc, admin := newTestServerWith(t, Options{ExpungeAfter: time.Hour})
	createMailbox(t, srv, "a@spooly.sh")
	m := deliver(t, srv, "a@spooly.sh", "", 1)
	call(t, srv, "DELETE", "/v1/messages/"+m.ID, "", "", 200, nil)
	before, err := svc.db.ListEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Prune(t.Context(), time.Now().AddDate(10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	after, err := svc.db.ListEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || len(after) == 0 {
		t.Errorf("%d events before pruning, %d after", len(before), len(after))
	}
	if _, err := svc.Restore(admin, m.ID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("restoring an expunged message: %v", err)
	}
}

// Every change is an event, named as webhooks would send it, under a ULID in
// the order of the changes; an update records what it changed, and one
// that changes nothing is no event.
func TestEvents(t *testing.T) {
	srv, svc, admin := newTestServerWith(t, Options{ExpungeAfter: time.Hour, EventsRetention: time.Hour})
	createMailbox(t, srv, "a@spooly.sh")
	call(t, srv, "POST", "/v1/labels", "application/json", `{"name": "billing"}`, 200, nil)
	m := deliver(t, srv, "a@spooly.sh", "", 1)
	path := "/v1/messages/" + m.ID

	update := `{"folder": "archive", "add_labels": ["billing"], "seen": true}`
	call(t, srv, "PATCH", path, "application/json", update, 200, nil)
	call(t, srv, "PATCH", path, "application/json", update, 200, nil) // no change
	call(t, srv, "DELETE", "/v1/labels/billing", "", "", 200, nil)
	call(t, srv, "DELETE", path, "", "", 200, nil)
	if _, err := svc.Restore(admin, m.ID); err != nil {
		t.Fatal(err)
	}

	events, err := svc.db.ListEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		if e.ObjectID != m.ID {
			t.Errorf("event %d is for %s", e.ID, e.ObjectID)
		}
		desc := e.Type
		if e.Data != nil {
			desc += " " + string(e.Data)
		}
		got = append(got, desc)
	}
	want := []string{
		"message.created",
		`message.updated {"folder":["inbox","archive"],"add_labels":["billing"],"seen":true}`,
		`message.updated {"remove_labels":["billing"]}`,
		`message.deleted {"mailbox_id":"` + m.MailboxID + `"}`,
		"message.restored",
	}
	if !slices.Equal(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Numbered in the order of the changes.
	for i, e := range events {
		if e.ID != int64(i)+1 {
			t.Errorf("event %d has ID %d", i+1, e.ID)
		}
	}
}

// Changes show as soon as they're committed, and a reader following them
// while others write misses none.
func TestChangesWhileWriting(t *testing.T) {
	srv := newTestServer(t)
	createMailbox(t, srv, "a@spooly.sh")
	start := startCursor(t, srv, "a@spooly.sh")
	first := deliver(t, srv, "a@spooly.sh", "", 0)
	var changes changesJSON
	listChanges(t, srv, "a@spooly.sh", start, 0, 200, &changes)
	if !slices.Equal(entryIDs(changes.Created), []string{first.ID}) {
		t.Fatalf("changes right after delivering = %+v", changes)
	}

	const writers = 20
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { deliver(t, srv, "a@spooly.sh", "", i+1) })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	seen := map[string]bool{first.ID: true}
	cursor := changes.Cursor
	for finished := false; ; {
		select {
		case <-done:
			finished = true
		default:
		}
		listChanges(t, srv, "a@spooly.sh", cursor, 1, 200, &changes)
		for _, m := range changes.Created {
			seen[m.ID] = true
		}
		cursor = changes.Cursor
		if finished && !changes.HasMore && len(changes.Created) == 0 {
			break
		}
	}
	if len(seen) != writers+1 {
		t.Errorf("followed %d messages, want %d", len(seen), writers+1)
	}
}

// Events are the organization's, but a mailbox's changes are only its own:
// its messages, changed or deleted. The cursor still moves past the others.
func TestChangesPerMailbox(t *testing.T) {
	srv := newTestServer(t)
	createMailbox(t, srv, "a@spooly.sh")
	createMailbox(t, srv, "b@spooly.sh")
	start := startCursor(t, srv, "a@spooly.sh")
	mine := deliver(t, srv, "a@spooly.sh", "", 1)
	theirs := deliver(t, srv, "b@spooly.sh", "", 2)
	gone := deliver(t, srv, "b@spooly.sh", "", 3)
	call(t, srv, "DELETE", "/v1/messages/"+gone.ID, "", "", 200, nil)
	call(t, srv, "PATCH", "/v1/messages/"+theirs.ID, "application/json", `{"seen": true}`, 200, nil)

	var a, b changesJSON
	listChanges(t, srv, "a@spooly.sh", start, 0, 200, &a)
	if !slices.Equal(entryIDs(a.Created), []string{mine.ID}) || len(a.Updated) != 0 || len(a.Deleted) != 0 {
		t.Errorf("a's changes = %+v", a)
	}
	listChanges(t, srv, "b@spooly.sh", start, 0, 200, &b)
	// Created and changed since, theirs is created; gone, created and
	// deleted, is still deleted: a client listing since may have it.
	if !slices.Equal(entryIDs(b.Created), []string{theirs.ID}) || len(b.Updated) != 0 || !slices.Equal(b.Deleted, []string{gone.ID}) {
		t.Errorf("b's changes = %+v", b)
	}
	if a.Cursor != b.Cursor {
		t.Errorf("cursors %s and %s: both read the organization's events", a.Cursor, b.Cursor)
	}
}
