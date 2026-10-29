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

-- Each account counts up a sequence. A message is stamped with the next
-- number whenever it joins a label (message_labels.seq) or comes into view
-- outside spam and trash (messages.seq), so within any label, later arrivals
-- always have higher numbers. IMAP uses them as UIDs.
CREATE TABLE IF NOT EXISTS accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    next_seq INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL
);

-- An account receives mail for any of its addresses. The first, by
-- position, is shown for the account.
CREATE TABLE IF NOT EXISTS account_addresses (
    address TEXT PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    position INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_account_addresses_account_id ON account_addresses(account_id, position);

-- Who has access to an account besides admins, who reach every account.
CREATE TABLE IF NOT EXISTS account_users (
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    user_id INTEGER NOT NULL REFERENCES users(id),
    PRIMARY KEY (account_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_account_users_user_id ON account_users(user_id);

-- System labels are rows too, with no created_at, so every label a message
-- carries is in this table.
CREATE TABLE IF NOT EXISTS labels (
    name TEXT PRIMARY KEY,
    system BOOLEAN NOT NULL,
    created_at TIMESTAMP
);

INSERT INTO labels (name, system) VALUES
    ('inbox', TRUE), ('unread', TRUE), ('starred', TRUE), ('answered', TRUE), ('sent', TRUE),
    ('draft', TRUE), ('spam', TRUE), ('trash', TRUE);

-- Raw messages are in object storage, keyed by message ID: a nanoid.
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    seq INTEGER NOT NULL,
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
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
    -- The Message-ID, and the parent's: the first In-Reply-To ID. Without
    -- angle brackets; empty when missing. A reply's parent_msg_id is its
    -- parent's msg_id, which Boxy looks up for replies and threads.
    msg_id TEXT NOT NULL DEFAULT '',
    parent_msg_id TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL,
    idempotency_key TEXT,
    received_at TIMESTAMP NOT NULL,
    UNIQUE (account_id, idempotency_key)
);

-- Lists are newest received first, so they read messages in that order.
CREATE INDEX IF NOT EXISTS idx_messages_received ON messages(received_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_account_received ON messages(account_id, received_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_account_msg_id ON messages(account_id, msg_id);

CREATE TABLE IF NOT EXISTS message_labels (
    message_id TEXT NOT NULL REFERENCES messages(id),
    label TEXT NOT NULL REFERENCES labels(name),
    seq INTEGER NOT NULL,
    PRIMARY KEY (message_id, label)
);

CREATE INDEX IF NOT EXISTS idx_message_labels_label ON message_labels(label, message_id);

-- +goose Down
DROP TABLE IF EXISTS message_labels;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS account_users;
DROP TABLE IF EXISTS account_addresses;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
