-- +goose Up
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    google_id TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    key_hash TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    label TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id);

-- A message coming into a folder (messages.folder_seq) or getting a label
-- (message_labels.seq) takes its mailbox's next seq, the order of arrivals
-- there: later arrivals always have higher seqs. An IMAP bridge uses them
-- as UIDs.
CREATE TABLE IF NOT EXISTS mailboxes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    next_seq INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL
);

-- A mailbox receives mail for any of its addresses. The first, by
-- position, is shown for the mailbox.
CREATE TABLE IF NOT EXISTS mailbox_addresses (
    address TEXT PRIMARY KEY,
    mailbox_id TEXT NOT NULL REFERENCES mailboxes(id),
    position INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mailbox_addresses_mailbox_id ON mailbox_addresses(mailbox_id, position);

-- Who has access to a mailbox besides admins, who reach every mailbox.
CREATE TABLE IF NOT EXISTS mailbox_users (
    mailbox_id TEXT NOT NULL REFERENCES mailboxes(id),
    user_id INTEGER NOT NULL REFERENCES users(id),
    PRIMARY KEY (mailbox_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_mailbox_users_user_id ON mailbox_users(user_id);

-- Labels say what a message is about, such as billing. A message can have
-- several, in any folder.
CREATE TABLE IF NOT EXISTS labels (
    name TEXT PRIMARY KEY,
    created_at TIMESTAMP NOT NULL
);

-- Raw messages are in object storage, keyed by message ID: a ULID, lowercase.
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    mailbox_id TEXT NOT NULL REFERENCES mailboxes(id),
    -- Where the message is: exactly one folder. Junk and trash show only in
    -- their own folders, as in Gmail.
    folder TEXT NOT NULL CHECK (folder IN ('inbox', 'archive', 'sent', 'drafts', 'junk', 'trash')),
    folder_seq INTEGER NOT NULL,
    -- The SMTP envelope: MAIL FROM, empty for bounces, and the RCPT TO it
    -- was delivered to.
    mail_from TEXT NOT NULL,
    rcpt_to TEXT NOT NULL,
    from_name TEXT NOT NULL,
    from_address TEXT NOT NULL,
    to_addresses TEXT NOT NULL,
    cc_addresses TEXT NOT NULL,
    subject TEXT NOT NULL,
    date TIMESTAMP,
    snippet TEXT NOT NULL,
    size INTEGER NOT NULL,
    -- The MIME parts but multiparts, in order: a JSON list of {type,
    -- disposition, name, cid, size, offset, length}. Offset and length are
    -- where the part's bytes, its header included, are in the raw message,
    -- so it's read without the rest. Header fields and body content stay in
    -- the raw message.
    parts TEXT NOT NULL,
    -- How many of the parts aren't a body, for lists to show.
    attachments INTEGER NOT NULL DEFAULT 0,
    -- The Message-ID, and the parent's: the first In-Reply-To ID. Without
    -- angle brackets; empty when missing. A reply's in_reply_to is its
    -- parent's message_id, which Spooly looks up for replies and threads.
    message_id TEXT NOT NULL DEFAULT '',
    in_reply_to TEXT NOT NULL DEFAULT '',
    -- Flags are the message's state, wherever it is: read, replied to,
    -- starred, an unfinished draft, and forwarded, as IMAP's \Seen,
    -- \Answered, \Flagged, \Draft, and $Forwarded.
    seen BOOLEAN NOT NULL DEFAULT FALSE,
    answered BOOLEAN NOT NULL DEFAULT FALSE,
    flagged BOOLEAN NOT NULL DEFAULT FALSE,
    draft BOOLEAN NOT NULL DEFAULT FALSE,
    forwarded BOOLEAN NOT NULL DEFAULT FALSE,
    idempotency_key TEXT,
    received_at TIMESTAMP NOT NULL,
    UNIQUE (mailbox_id, idempotency_key)
);

-- Lists are newest received first, so they read messages in that order.
CREATE INDEX IF NOT EXISTS idx_messages_received ON messages(received_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_mailbox_received ON messages(mailbox_id, received_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_mailbox_folder ON messages(mailbox_id, folder, received_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_mailbox_message_id ON messages(mailbox_id, message_id);
-- Sync clients list a folder in the order messages came into it.
CREATE INDEX IF NOT EXISTS idx_messages_mailbox_folder_seq ON messages(mailbox_id, folder, folder_seq);

CREATE TABLE IF NOT EXISTS message_labels (
    message_id TEXT NOT NULL REFERENCES messages(id),
    label TEXT NOT NULL REFERENCES labels(name),
    seq INTEGER NOT NULL,
    PRIMARY KEY (message_id, label)
);

CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label, message_id);
CREATE INDEX IF NOT EXISTS idx_message_labels_label_seq ON message_labels(label, seq);

-- A sent message's way to each recipient, through the mail transport: queued
-- until handed over, deferred while retrying, sent once the transport took
-- it, then what the transport tells of it later: delayed, delivered,
-- bounced, or complained. Failed when Spooly gave up. Deleting the message
-- deletes its deliveries, cancelling those still to go.
CREATE TABLE IF NOT EXISTS deliveries (
    id TEXT PRIMARY KEY,
    message_id TEXT NOT NULL REFERENCES messages(id),
    mailbox_id TEXT NOT NULL REFERENCES mailboxes(id),
    recipient TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'deferred', 'sent', 'delayed', 'delivered', 'bounced', 'complained', 'failed')),
    transport TEXT NOT NULL,
    -- The transport's ID for the message, to look it up there.
    transport_message_id TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    -- When to try next, while queued or deferred: a spool claims a
    -- delivery by moving it ahead, so another doesn't send it meanwhile.
    next_attempt_at TIMESTAMP,
    -- The last reply or reason, such as "550 5.1.1 user unknown".
    detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE (message_id, recipient)
);

CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries(next_attempt_at) WHERE status IN ('queued', 'deferred');
CREATE INDEX IF NOT EXISTS idx_deliveries_mailbox ON deliveries(mailbox_id, id);

-- Every change in the organization, by its place in commit order: lists
-- of changes resume after an ID. IDs come from state's events counter as
-- the transaction logging them commits: that row stays locked until then,
-- so no event commits with an ID below one already read. The type, named as
-- webhooks would send it, says which kind of object object_id is, such as
-- message.updated, and data, as JSON, what the type has to say: what an
-- update changed, say. A message.deleted event's data is the mailbox the
-- message was in, and until the message is expunged, under restore, what
-- restoring it needs besides its raw source, which stays in storage until
-- then. A delivery event's data is what the delivery became, and why: the
-- transport's reply, say; a delivery's events are its history.
CREATE TABLE IF NOT EXISTS events (
    id BIGINT PRIMARY KEY,
    type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    data TEXT,
    created_at TIMESTAMP NOT NULL
);

-- An object's events, such as a delivery's, which tell its history.
CREATE INDEX IF NOT EXISTS idx_events_object ON events(object_id, id);

CREATE INDEX IF NOT EXISTS idx_events_restorable ON events(object_id)
    WHERE type = 'message.deleted' AND json_extract(data, '$.restore') IS NOT NULL;

-- The organization's counters, by name: events, the last event's ID;
-- pruned, the newest event pruned. Changes since an event before it
-- can no longer be told.
CREATE TABLE IF NOT EXISTS state (
    name TEXT PRIMARY KEY,
    value BIGINT NOT NULL
);

INSERT INTO state (name, value) VALUES ('events', 0);

-- +goose Down
DROP TABLE IF EXISTS state;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS message_labels;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS mailbox_users;
DROP TABLE IF EXISTS mailbox_addresses;
DROP TABLE IF EXISTS mailboxes;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
