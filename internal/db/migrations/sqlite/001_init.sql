-- +goose Up
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    google_id TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    key_hash TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    label TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id);

CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    addr TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

-- System labels are rows too, with no created_at, so every label a message
-- carries is in this table.
CREATE TABLE IF NOT EXISTS labels (
    name TEXT PRIMARY KEY,
    system BOOLEAN NOT NULL,
    created_at TIMESTAMP
);

INSERT INTO labels (name, system) VALUES
    ('inbox', TRUE), ('unread', TRUE), ('starred', TRUE), ('sent', TRUE), ('spam', TRUE), ('trash', TRUE);

-- Raw messages are in object storage, keyed by message ID.
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id),
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
    from_name TEXT NOT NULL,
    from_addr TEXT NOT NULL,
    to_addrs TEXT NOT NULL,
    cc_addrs TEXT NOT NULL,
    subject TEXT NOT NULL,
    date TIMESTAMP,
    snippet TEXT NOT NULL,
    size INTEGER NOT NULL,
    attachments TEXT NOT NULL,
    text_body TEXT NOT NULL,
    html_body TEXT NOT NULL,
    metadata TEXT NOT NULL,
    idempotency_key TEXT,
    received_at TIMESTAMP NOT NULL,
    UNIQUE (account_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_messages_account_id ON messages(account_id, id);

CREATE TABLE IF NOT EXISTS messages_labels (
    message_id TEXT NOT NULL REFERENCES messages(id),
    label TEXT NOT NULL REFERENCES labels(name),
    PRIMARY KEY (message_id, label)
);

CREATE INDEX IF NOT EXISTS idx_messages_labels_label ON messages_labels(label, message_id);

-- +goose Down
DROP TABLE IF EXISTS messages_labels;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
