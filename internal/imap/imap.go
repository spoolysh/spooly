// Package imap serves accounts over IMAP4rev1 (RFC 3501), with the IDLE,
// MOVE, UIDPLUS, and SPECIAL-USE extensions, on top of go-imap's server.
//
// The username is an account's address and the password is an API key, as
// for POP3. Folders are labels, as in Gmail: INBOX, Starred, Sent, Drafts,
// Spam, Trash, every user label, and All Mail. A message with two labels is
// in both folders, though spam and trash show only in their own. \Seen is
// the absence of the unread label, and \Flagged, \Answered, and \Draft are
// the starred, answered, and draft labels. Copying adds the destination's
// label; moving also removes the source's. Deleting works as in Gmail, at
// once rather than on expunge: the message loses the folder's label, or in
// All Mail goes to the trash, and in Trash and Drafts it's deleted for good.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/service"
)

// Options configure a Server.
type Options struct {
	// TLS enables STARTTLS. When it's set, logins need TLS: STARTTLS first,
	// or a listener that is already TLS. Without it, logins are allowed in
	// plaintext; only do that on a trusted network.
	TLS *tls.Config
}

// Server serves IMAP sessions.
type Server struct {
	db  *db.DB
	svc *service.Service
	srv *imapserver.Server
	// authDelay slows down guessing after a failed login.
	authDelay time.Duration
	// pollInterval is how often IDLE checks for changes.
	pollInterval time.Duration
}

// New returns a server.
func New(database *db.DB, svc *service.Service, opts Options) *Server {
	s := &Server{db: database, svc: svc, authDelay: time.Second, pollInterval: 5 * time.Second}
	s.srv = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{s: s}, nil, nil
		},
		Caps: goimap.CapSet{
			goimap.CapIMAP4rev1:  {},
			goimap.CapUIDPlus:    {},
			goimap.CapMove:       {},
			goimap.CapSpecialUse: {},
		},
		TLSConfig:    opts.TLS,
		InsecureAuth: opts.TLS == nil,
		Logger:       log.Default(),
	})
	return s
}

// Serve accepts connections on l until it is closed. Wrap l with
// tls.NewListener to serve IMAPS (port 993).
func (s *Server) Serve(l net.Listener) error {
	err := s.srv.Serve(l)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close stops the server.
func (s *Server) Close() error {
	return s.srv.Close()
}

// folder is a mailbox as clients see it.
type folder struct {
	name  string // IMAP name
	key   string // db mailbox key: a label, or db.AllMail
	attrs []goimap.MailboxAttr
}

// systemFolders are listed first, in this order. Clients find them by their
// special-use attributes (RFC 6154), whatever their names.
var systemFolders = []folder{
	{name: "INBOX", key: db.LabelInbox},
	{name: "Starred", key: db.LabelStarred, attrs: []goimap.MailboxAttr{goimap.MailboxAttrFlagged}},
	{name: "Sent", key: db.LabelSent, attrs: []goimap.MailboxAttr{goimap.MailboxAttrSent}},
	{name: "Drafts", key: db.LabelDraft, attrs: []goimap.MailboxAttr{goimap.MailboxAttrDrafts}},
	{name: "Spam", key: db.LabelSpam, attrs: []goimap.MailboxAttr{goimap.MailboxAttrJunk}},
	{name: "Trash", key: db.LabelTrash, attrs: []goimap.MailboxAttr{goimap.MailboxAttrTrash}},
	{name: "All Mail", key: db.AllMail, attrs: []goimap.MailboxAttr{goimap.MailboxAttrAll}},
}

const delim = '/'

var (
	errNoMailbox = &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeNonExistent, Text: "No such mailbox"}
	errNoCopy    = &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeTryCreate, Text: "No such mailbox"}
	errSame      = &goimap.Error{Type: goimap.StatusResponseTypeNo, Text: "Source and destination mailboxes are identical"}
)

func noErr(text string) error {
	return &goimap.Error{Type: goimap.StatusResponseTypeNo, Text: text}
}

// msg is a message in the selected folder, as this session last reported it.
type msg struct {
	db.SeqMessage
	deleted bool // \Deleted, kept for the session until expunged
}

func (m *msg) flags() []goimap.Flag {
	flags := []goimap.Flag{}
	if !m.Unread {
		flags = append(flags, goimap.FlagSeen)
	}
	if m.Starred {
		flags = append(flags, goimap.FlagFlagged)
	}
	if m.Answered {
		flags = append(flags, goimap.FlagAnswered)
	}
	if m.Draft {
		flags = append(flags, goimap.FlagDraft)
	}
	if m.deleted {
		flags = append(flags, goimap.FlagDeleted)
	}
	return flags
}

type session struct {
	s       *Server
	account *db.Account
	// ctx carries the logged-in user, for service calls that check access.
	ctx context.Context

	// The selected folder: nil when none is. msgs is the session's view, in
	// sequence number order; it changes only when the client is told.
	selected    *folder
	uidValidity uint32
	uidNext     uint32
	msgs        []*msg
}

var _ imapserver.SessionMove = (*session)(nil)

func (c *session) Close() error { return nil }

// Login authenticates an account's address with an API key.
func (c *session) Login(username, password string) error {
	account, ctx, err := c.s.svc.LoginMail(context.Background(), username, password)
	if err != nil {
		time.Sleep(c.s.authDelay)
		return imapserver.ErrAuthFailed
	}
	c.account, c.ctx = account, ctx
	return nil
}

// folders returns every folder: the system ones, then a folder per user
// label.
func (c *session) folders() ([]folder, error) {
	labels, err := c.s.db.ListLabels(context.Background())
	if err != nil {
		return nil, err
	}
	folders := slices.Clone(systemFolders)
	for _, l := range labels {
		if !l.System {
			folders = append(folders, folder{name: l.Name, key: l.Name})
		}
	}
	return folders, nil
}

// folder finds a folder by its IMAP name. INBOX is case-insensitive.
func (c *session) folder(name string) (*folder, error) {
	folders, err := c.folders()
	if err != nil {
		return nil, err
	}
	for i := range folders {
		if folders[i].name == name || (folders[i].name == "INBOX" && strings.EqualFold(name, "INBOX")) {
			return &folders[i], nil
		}
	}
	return nil, errNoMailbox
}

func (c *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *goimap.ListOptions) error {
	folders, err := c.folders()
	if err != nil {
		return err
	}
	for _, f := range folders {
		if options.SelectSpecialUse && len(f.attrs) == 0 {
			continue
		}
		if !slices.ContainsFunc(patterns, func(p string) bool { return imapserver.MatchList(f.name, delim, ref, p) }) {
			continue
		}
		// Special-use attributes go out even without RETURN (SPECIAL-USE),
		// which RFC 6154 allows and older clients rely on.
		data := &goimap.ListData{
			Mailbox: f.name,
			Delim:   delim,
			Attrs:   append([]goimap.MailboxAttr{goimap.MailboxAttrHasNoChildren, goimap.MailboxAttrSubscribed}, f.attrs...),
		}
		if options.ReturnStatus != nil {
			if data.Status, err = c.status(&f, options.ReturnStatus); err != nil {
				return err
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

func (c *session) Status(name string, options *goimap.StatusOptions) (*goimap.StatusData, error) {
	f, err := c.folder(name)
	if err != nil {
		return nil, err
	}
	return c.status(f, options)
}

func (c *session) status(f *folder, options *goimap.StatusOptions) (*goimap.StatusData, error) {
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, f.key)
	if err != nil {
		return nil, err
	}
	data := &goimap.StatusData{Mailbox: f.name, UIDNext: goimap.UID(mb.NextSeq), UIDValidity: uidValidity(mb)}
	if options.NumMessages {
		n := uint32(len(mb.Messages))
		data.NumMessages = &n
	}
	if options.NumUnseen {
		n := uint32(0)
		for _, m := range mb.Messages {
			if m.Unread {
				n++
			}
		}
		data.NumUnseen = &n
	}
	if options.NumDeleted {
		n := uint32(0)
		data.NumDeleted = &n
	}
	if options.Size {
		var size int64
		for _, m := range mb.Messages {
			size += m.Size
		}
		data.Size = &size
	}
	return data, nil
}

func (c *session) Select(name string, options *goimap.SelectOptions) (*goimap.SelectData, error) {
	f, err := c.folder(name)
	if err != nil {
		return nil, err
	}
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, f.key)
	if err != nil {
		return nil, err
	}
	c.selected, c.uidValidity, c.uidNext = f, uidValidity(mb), mb.NextSeq
	c.msgs = make([]*msg, len(mb.Messages))
	var firstUnseen uint32
	for i, m := range mb.Messages {
		c.msgs[i] = &msg{SeqMessage: m}
		if m.Unread && firstUnseen == 0 {
			firstUnseen = uint32(i) + 1
		}
	}
	flags := []goimap.Flag{goimap.FlagSeen, goimap.FlagFlagged, goimap.FlagAnswered, goimap.FlagDraft, goimap.FlagDeleted}
	return &goimap.SelectData{
		Flags:             flags,
		PermanentFlags:    flags,
		NumMessages:       uint32(len(c.msgs)),
		FirstUnseenSeqNum: firstUnseen,
		UIDNext:           goimap.UID(mb.NextSeq),
		UIDValidity:       uidValidity(mb),
	}, nil
}

func (c *session) Unselect() error {
	c.selected, c.msgs = nil, nil
	return nil
}

// Create makes a user label. Labels are shared by all accounts, so it
// appears in every account's folder list.
func (c *session) Create(name string, options *goimap.CreateOptions) error {
	if _, err := c.folder(name); err == nil {
		return &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	// The API checks label names in its interceptor; here, check the same
	// rules directly.
	req := &boxyv1.CreateLabelRequest{Name: name}
	if err := protovalidate.Validate(req); err != nil {
		return noErr("Folder names are labels: lowercase letters and digits, separated by - or _")
	}
	if _, err := c.s.svc.CreateLabel(c.ctx, req); err != nil {
		return noErr("Can't create the folder: " + err.Error())
	}
	return nil
}

// Delete refuses: a label is shared by all accounts, so deleting it from
// one mail client would remove it everywhere. Delete labels in the dashboard.
func (c *session) Delete(name string) error {
	return noErr("Folders are labels shared by all accounts; delete them in the Boxy dashboard")
}

func (c *session) Rename(name, newName string, options *goimap.RenameOptions) error {
	return noErr("Folders can't be renamed")
}

// Every folder is always subscribed.
func (c *session) Subscribe(name string) error   { return nil }
func (c *session) Unsubscribe(name string) error { return nil }

// labelsFor returns what putting a message into folder f means: the labels
// to add and remove.
func labelsFor(f *folder) (add, remove []string) {
	switch f.key {
	case db.AllMail:
		// Out of spam and trash, where All Mail can't show it.
		return nil, []string{db.LabelSpam, db.LabelTrash}
	case db.LabelTrash, db.LabelSpam:
		return []string{f.key}, []string{db.LabelInbox}
	default:
		return []string{f.key}, nil
	}
}

// removeFrom returns what deleting a message from folder f means, as in
// Gmail: from Trash or Drafts it's deleted for good; from All Mail it goes
// to the trash; from any other folder it loses that folder's label.
func removeFrom(f *folder) (add, remove []string, forGood bool) {
	switch f.key {
	case db.LabelTrash, db.LabelDraft:
		return nil, nil, true
	case db.AllMail:
		return []string{db.LabelTrash}, []string{db.LabelInbox}, false
	default:
		return nil, []string{f.key}, false
	}
}

// takeOut deletes a message from the selected folder, as removeFrom says.
func (c *session) takeOut(id string) error {
	add, remove, forGood := removeFrom(c.selected)
	if forGood {
		return c.s.svc.PurgeMessage(context.Background(), id)
	}
	return c.update(id, add, remove)
}

func (c *session) update(id string, add, remove []string) error {
	// Don't remove a label that is also added (moving Spam to Spam's own
	// rule, for one): the add wins.
	remove = slices.DeleteFunc(slices.Clone(remove), func(l string) bool { return slices.Contains(add, l) })
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	_, err := c.s.db.UpdateMessage(context.Background(), id, db.MessageUpdate{AddLabels: add, RemoveLabels: remove})
	if errors.Is(err, db.ErrMessageNotFound) {
		return nil
	}
	return err
}

func (c *session) Append(name string, r goimap.LiteralReader, options *goimap.AppendOptions) (*goimap.AppendData, error) {
	f, err := c.folder(name)
	if err != nil {
		return nil, errNoCopy
	}
	raw, err := io.ReadAll(io.LimitReader(r, service.MaxMessageSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > service.MaxMessageSize {
		return nil, &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeTooBig, Text: "Message too large"}
	}

	labels, _ := labelsFor(f)
	labels = slices.Clone(labels)
	if labels == nil {
		labels = []string{} // All Mail: no labels, not the defaults
	}
	if !slices.Contains(options.Flags, goimap.FlagSeen) {
		labels = append(labels, db.LabelUnread)
	}
	if slices.Contains(options.Flags, goimap.FlagFlagged) {
		labels = append(labels, db.LabelStarred)
	}
	if slices.Contains(options.Flags, goimap.FlagAnswered) {
		labels = append(labels, db.LabelAnswered)
	}
	if slices.Contains(options.Flags, goimap.FlagDraft) && !slices.Contains(labels, db.LabelDraft) {
		labels = append(labels, db.LabelDraft)
	}
	m, err := c.s.svc.Store(context.Background(), service.Incoming{
		AccountIDOrAddress: c.account.ID,
		Raw:                raw,
		ReceivedAt:         options.Time,
		Labels:             labels,
	})
	if err != nil {
		return nil, noErr("Can't store the message: " + err.Error())
	}
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, f.key)
	if err != nil {
		return nil, err
	}
	data := &goimap.AppendData{UIDValidity: uidValidity(mb)}
	for _, mm := range mb.Messages {
		if mm.ID == m.ID {
			data.UID = goimap.UID(mm.Seq)
		}
	}
	return data, nil
}

// resolve returns the indexes into c.msgs that numSet names, in order.
func (c *session) resolve(numSet goimap.NumSet) []int {
	var out []int
	switch set := numSet.(type) {
	case goimap.SeqSet:
		max := uint32(len(c.msgs))
		for i := range c.msgs {
			seq := uint32(i) + 1
			for _, r := range set {
				start, stop := staticRange(r.Start, r.Stop, max)
				if seq >= start && seq <= stop {
					out = append(out, i)
					break
				}
			}
		}
	case goimap.UIDSet:
		max := uint32(0)
		if len(c.msgs) > 0 {
			max = c.msgs[len(c.msgs)-1].Seq
		}
		for i, m := range c.msgs {
			for _, r := range set {
				start, stop := staticRange(uint32(r.Start), uint32(r.Stop), max)
				if m.Seq >= start && m.Seq <= stop {
					out = append(out, i)
					break
				}
			}
		}
	}
	return out
}

// staticRange resolves "*" (0) to max and orders the range.
func staticRange(start, stop, max uint32) (uint32, uint32) {
	if start == 0 {
		start = max
	}
	if stop == 0 {
		stop = max
	}
	if start > stop {
		start, stop = stop, start
	}
	return start, stop
}

// uidValidity is a folder's UIDVALIDITY. Its UIDs are sequence numbers,
// which are reused only when its label is deleted and created again, and
// that changes when its numbering began.
func uidValidity(f *db.SeqList) uint32 {
	return uint32(f.Since.Unix())
}

// envelope parses the ENVELOPE of a raw message from its header.
func envelope(raw []byte) *goimap.Envelope {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil
	}
	return imapserver.ExtractEnvelope(h)
}

// raw reads a message's raw source, or with headerOnly just its header
// block, up to and including the blank line that ends it.
func (c *session) raw(m *msg, headerOnly bool) ([]byte, error) {
	_, rc, err := c.s.svc.OpenRaw(context.Background(), m.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if !headerOnly {
		return io.ReadAll(rc)
	}
	br := bufio.NewReader(rc)
	var header []byte
	for {
		line, err := br.ReadBytes('\n')
		header = append(header, line...)
		if err == io.EOF || (err == nil && len(bytes.TrimRight(line, "\r\n")) == 0) {
			return header, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// headerOnly reports whether everything options asks for is in a message's
// header block, as mail clients fetch for their message lists: ENVELOPE,
// BODY[HEADER], or BODY[HEADER.FIELDS (...)].
func headerOnly(options *goimap.FetchOptions) bool {
	if options.BodyStructure != nil || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0 {
		return false
	}
	for _, bs := range options.BodySection {
		if bs.Specifier != goimap.PartSpecifierHeader || len(bs.Part) > 0 {
			return false
		}
	}
	return true
}

func (c *session) Fetch(w *imapserver.FetchWriter, numSet goimap.NumSet, options *goimap.FetchOptions) error {
	markSeen := slices.ContainsFunc(options.BodySection, func(bs *goimap.FetchItemBodySection) bool { return !bs.Peek })
	needRaw := options.Envelope || options.BodyStructure != nil || len(options.BodySection) > 0 ||
		len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	headerOnly := headerOnly(options)

	for _, i := range c.resolve(numSet) {
		m := c.msgs[i]
		var raw []byte
		if needRaw {
			var err error
			if raw, err = c.raw(m, headerOnly); err != nil {
				// Gone from storage: skip it rather than fail the
				// whole FETCH.
				log.Printf("imap: reading %s: %v", m.ID, err)
				continue
			}
		}
		seen := false
		if markSeen && m.Unread {
			if err := c.update(m.ID, nil, []string{db.LabelUnread}); err != nil {
				return err
			}
			m.Unread, seen = false, true
		}

		rw := w.CreateMessage(uint32(i) + 1)
		rw.WriteUID(goimap.UID(m.Seq))
		if options.Flags || seen {
			rw.WriteFlags(m.flags())
		}
		if options.InternalDate {
			rw.WriteInternalDate(m.ReceivedAt)
		}
		if options.RFC822Size {
			rw.WriteRFC822Size(m.Size)
		}
		if options.Envelope {
			rw.WriteEnvelope(envelope(raw))
		}
		if options.BodyStructure != nil {
			rw.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
		}
		for _, bs := range options.BodySection {
			buf := imapserver.ExtractBodySection(bytes.NewReader(raw), bs)
			if err := writeAll(rw.WriteBodySection(bs, int64(len(buf))), buf); err != nil {
				return err
			}
		}
		for _, bs := range options.BinarySection {
			buf := imapserver.ExtractBinarySection(bytes.NewReader(raw), bs)
			if err := writeAll(rw.WriteBinarySection(bs, int64(len(buf))), buf); err != nil {
				return err
			}
		}
		for _, bss := range options.BinarySectionSize {
			rw.WriteBinarySectionSize(bss, imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), bss))
		}
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writeAll(wc io.WriteCloser, buf []byte) error {
	_, err := wc.Write(buf)
	if cerr := wc.Close(); err == nil {
		err = cerr
	}
	return err
}

func (c *session) Store(w *imapserver.FetchWriter, numSet goimap.NumSet, flags *goimap.StoreFlags, options *goimap.StoreOptions) error {
	has := func(f goimap.Flag) bool {
		return slices.ContainsFunc(flags.Flags, func(g goimap.Flag) bool { return strings.EqualFold(string(f), string(g)) })
	}
	// apply returns whether flag f is set after the store, given cur.
	apply := func(cur bool, f goimap.Flag) bool {
		switch flags.Op {
		case goimap.StoreFlagsSet:
			return has(f)
		case goimap.StoreFlagsAdd:
			return cur || has(f)
		case goimap.StoreFlagsDel:
			return cur && !has(f)
		}
		return cur
	}
	idx := c.resolve(numSet)
	for _, i := range idx {
		m := c.msgs[i]
		unread := !apply(!m.Unread, goimap.FlagSeen)
		starred := apply(m.Starred, goimap.FlagFlagged)
		answered := apply(m.Answered, goimap.FlagAnswered)
		draft := apply(m.Draft, goimap.FlagDraft)
		deleted := apply(m.deleted, goimap.FlagDeleted)
		var add, remove []string
		change := func(label string, was, now bool) {
			switch {
			case now && !was:
				add = append(add, label)
			case was && !now:
				remove = append(remove, label)
			}
		}
		change(db.LabelUnread, m.Unread, unread)
		change(db.LabelStarred, m.Starred, starred)
		change(db.LabelAnswered, m.Answered, answered)
		change(db.LabelDraft, m.Draft, draft)
		if err := c.update(m.ID, add, remove); err != nil {
			return err
		}
		// Deleting takes effect at once, as with Gmail's auto-expunge: the
		// message leaves the folder now, and clients hear of it at their
		// next command that may expunge.
		if deleted && !m.deleted {
			if err := c.takeOut(m.ID); err != nil {
				return err
			}
		}
		m.Unread, m.Starred, m.Answered, m.Draft, m.deleted = unread, starred, answered, draft, deleted
	}
	if flags.Silent {
		return nil
	}
	for _, i := range idx {
		rw := w.CreateMessage(uint32(i) + 1)
		rw.WriteUID(goimap.UID(c.msgs[i].Seq))
		rw.WriteFlags(c.msgs[i].flags())
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// copyTo puts the messages at idx into folder dest, also removing the labels
// in leave, and returns their UIDs there: none, and nil, for messages that
// spam or trash keeps out of dest.
func (c *session) copyTo(idx []int, destName string, leave ...string) (*goimap.CopyData, error) {
	dest, err := c.folder(destName)
	if err != nil {
		return nil, errNoCopy
	}
	if c.selected != nil && dest.key == c.selected.key {
		return nil, errSame
	}
	add, remove := labelsFor(dest)
	// In one update, so that a message moved out of the trash is seen
	// leaving it and gets its new UIDs.
	remove = append(slices.Clone(remove), leave...)
	for _, i := range idx {
		if err := c.update(c.msgs[i].ID, add, remove); err != nil {
			return nil, err
		}
	}
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, dest.key)
	if err != nil {
		return nil, err
	}
	uids := map[string]uint32{}
	for _, m := range mb.Messages {
		uids[m.ID] = m.Seq
	}
	data := &goimap.CopyData{UIDValidity: uidValidity(mb)}
	for _, i := range idx {
		if uid, ok := uids[c.msgs[i].ID]; ok {
			data.SourceUIDs.AddNum(goimap.UID(c.msgs[i].Seq))
			data.DestUIDs.AddNum(goimap.UID(uid))
		}
	}
	if len(data.DestUIDs) == 0 {
		return nil, nil
	}
	return data, nil
}

func (c *session) Copy(numSet goimap.NumSet, dest string) (*goimap.CopyData, error) {
	return c.copyTo(c.resolve(numSet), dest)
}

func (c *session) Move(w *imapserver.MoveWriter, numSet goimap.NumSet, dest string) error {
	idx := c.resolve(numSet)
	// Out of this folder too: its label goes. All Mail has none; moving to
	// Spam or Trash takes the messages out of it.
	var leave []string
	if c.selected.key != db.AllMail {
		leave = []string{c.selected.key}
	}
	data, err := c.copyTo(idx, dest, leave...)
	if err != nil {
		return err
	}
	if err := w.WriteCopyData(data); err != nil {
		return err
	}
	gone, err := c.left(idx)
	if err != nil {
		return err
	}
	return c.expunge(gone, w.WriteExpunge)
}

// Expunge reports the \Deleted messages gone. Storing \Deleted already took
// them out of the folder; that's done again here, harmlessly, in case it
// failed then.
func (c *session) Expunge(w *imapserver.ExpungeWriter, uids *goimap.UIDSet) error {
	var idx []int
	for i, m := range c.msgs {
		if !m.deleted || (uids != nil && !uids.Contains(goimap.UID(m.Seq))) {
			continue
		}
		if err := c.takeOut(m.ID); err != nil {
			return err
		}
		idx = append(idx, i)
	}
	gone, err := c.left(idx)
	if err != nil {
		return err
	}
	return c.expunge(gone, w.WriteExpunge)
}

// left returns which of the messages at idx are no longer in the selected
// folder.
func (c *session) left(idx []int) (map[int]bool, error) {
	gone := map[int]bool{}
	if len(idx) == 0 {
		return gone, nil
	}
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, c.selected.key)
	if err != nil {
		return nil, err
	}
	present := map[uint32]bool{}
	for _, m := range mb.Messages {
		present[m.Seq] = true
	}
	for _, i := range idx {
		if !present[c.msgs[i].Seq] {
			gone[i] = true
		}
	}
	return gone, nil
}

// expunge drops the messages at the given indexes from the session's view,
// reporting each with writeExpunge, highest sequence number first so the
// others stay valid.
func (c *session) expunge(gone map[int]bool, writeExpunge func(uint32) error) error {
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if !gone[i] {
			continue
		}
		if err := writeExpunge(uint32(i) + 1); err != nil {
			return err
		}
		c.msgs = slices.Delete(c.msgs, i, i+1)
	}
	return nil
}

func (c *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if c.selected == nil {
		return nil
	}
	mb, err := c.s.db.ListBySeq(context.Background(), c.account.ID, c.selected.key)
	if err != nil {
		return err
	}
	current := make(map[uint32]db.SeqMessage, len(mb.Messages))
	for _, m := range mb.Messages {
		current[m.Seq] = m
	}

	// Messages that left the folder. Until the client may hear of expunges,
	// they stay in the view.
	if allowExpunge {
		gone := map[int]bool{}
		for i, m := range c.msgs {
			if _, ok := current[m.Seq]; !ok {
				gone[i] = true
			}
		}
		if err := c.expunge(gone, w.WriteExpunge); err != nil {
			return err
		}
	}

	// Flag changes made elsewhere: the dashboard, the API, other clients.
	for i, m := range c.msgs {
		cur, ok := current[m.Seq]
		if !ok || (cur.Unread == m.Unread && cur.Starred == m.Starred && cur.Answered == m.Answered && cur.Draft == m.Draft) {
			continue
		}
		m.Unread, m.Starred, m.Answered, m.Draft = cur.Unread, cur.Starred, cur.Answered, cur.Draft
		if err := w.WriteMessageFlags(uint32(i)+1, goimap.UID(m.Seq), m.flags()); err != nil {
			return err
		}
	}

	// New arrivals have UIDs above any the view has.
	last := uint32(0)
	if len(c.msgs) > 0 {
		last = c.msgs[len(c.msgs)-1].Seq
	}
	added := false
	for _, m := range mb.Messages {
		if m.Seq > last {
			c.msgs = append(c.msgs, &msg{SeqMessage: m})
			added = true
		}
	}
	if added {
		return w.WriteNumMessages(uint32(len(c.msgs)))
	}
	return nil
}

func (c *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	ticker := time.NewTicker(c.s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if err := c.Poll(w, true); err != nil {
				return err
			}
		}
	}
}
