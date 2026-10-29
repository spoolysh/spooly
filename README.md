# Boxy

Mail backend. Incoming mail is stored per account, raw and parsed, organized
with labels, and read through one API: REST, gRPC, gRPC-Web, and Connect.

> Early prototype: receiving mail, the API, the dashboard, POP3, and IMAP
> work; sending mail is next.

- **Accounts**: receive mail for any of their addresses, looked up by ID or
  address; plus addresses (`hello+news@`) reach `hello@`. Shared with the
  people who work them
- **Labels**: like Gmail, a message can have several; inbox, unread, and
  trash are labels too. Shared by all accounts
- **Raw first**: every message is kept as received (RFC 5322); headers,
  bodies, and attachments are read from it, only the part asked for
- **Any inbound**: a Cloudflare Email Routing worker first; anything that can
  POST the raw message works
- **POP3 and IMAP**: mail clients read an account, with an API key as the
  password; over IMAP, labels are folders
- **Dashboard**: read mail, change labels, and manage accounts, labels, and
  API keys, signed in with Google

## Quick start

```bash
export SESSION_SECRET="your-secret-here"
export GOOGLE_CLIENT_ID="your-client-id"
export GOOGLE_CLIENT_SECRET="your-client-secret"

just serve
# or: go run ./cmd/boxy serve --migrate --mail
```

Visit `http://localhost:8080` to sign in, create an account, and generate an
API key. Uses SQLite by default, with PostgreSQL available when you need a
separate database, and keeps raw messages in `./storage`. With Docker:
`cp .env.example .env`, fill it in, and `docker compose up`.

## Receiving mail

An inbound worker posts the raw message as the body, with the envelope in the
query string:

```bash
curl -X POST "https://boxy.example.com/v1/accounts/hello@boxy.sh/messages?mail_from=bounce@example.com" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: message/rfc822" \
  -H "Idempotency-Key: 9b2f…" \
  --data-binary @message.eml
```

The address in the path is the envelope recipient (Cloudflare's
`message.to`) and is kept as the message's `rcpt_to`. Plus addresses reach
the base account: `hello+news@boxy.sh` lands in `hello@boxy.sh` with
`rcpt_to` `hello+news@boxy.sh`. An unknown address gets `404`, so the worker
can reject the mail. Send an `Idempotency-Key` header to make retries safe: a repeat with the same key, in the same
account, returns the stored message.
New messages get the `inbox` and `unread` labels.

To mirror an existing mailbox, such as Gmail, pass each message's own
`labels` instead, repeated: `?labels=sent` stores sent mail,
`?labels=inbox&labels=starred` a read, starred inbox message. Labels must
exist; create your own first. Use the source's message ID as the
idempotency key, such as `gmail-18f3c2a9b7d41e05`, so syncing again never
duplicates, and its date as
`received_at`, so mail sorts by when it really arrived.

A Cloudflare Email Routing worker that delivers to Boxy. Cloudflare gives no
delivery ID, so the idempotency key is the recipient and Message-ID, which
stay the same when the sending server retries. A hash of the raw bytes is
only the fallback: Cloudflare adds its own headers, which may differ between
attempts.

```js
async function idempotencyKey(message, raw) {
  const id = message.headers.get("Message-ID")?.trim();
  const source = id ? new TextEncoder().encode(`${message.to}\n${id}`) : raw;
  const hash = await crypto.subtle.digest("SHA-256", source);
  return [...new Uint8Array(hash)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export default {
  async email(message, env) {
    const raw = await new Response(message.raw).arrayBuffer();
    const to = encodeURIComponent(message.to);
    const query = new URLSearchParams({ mail_from: message.from });
    const resp = await fetch(`${env.BOXY_URL}/v1/accounts/${to}/messages?${query}`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${env.BOXY_KEY}`,
        "Content-Type": "message/rfc822",
        "Idempotency-Key": await idempotencyKey(message, raw),
      },
      body: raw,
    });
    if (resp.status === 404) message.setReject("Unknown address");
    else if (!resp.ok) throw new Error(`Boxy answered ${resp.status}`); // Cloudflare retries
  },
};
```

## Labels

A label is just its name: up to 64 lowercase letters and digits, separated by
single `_` or `-` (`billing`, `customer-issues`). Labels are shared by all
accounts, so `billing` means the same across every address. The system
labels `inbox`, `unread`, `starred`, `answered`, `sent`, `draft`,
`spam`, and `trash` always exist. Combine a label with an account to get,
say, one address's inbox:
`GET /v1/accounts/support@boxy.sh/messages?label=inbox`. As in Gmail, spam
and trash are listed only under `label=spam` and `label=trash`, so a trashed
message drops out of `starred` and `billing` until it's restored.

Common actions are label changes:

| Action | Change |
|---|---|
| Mark read | remove `unread` |
| Archive | remove `inbox` |
| Star | add `starred` |
| Trash | add `trash`, remove `inbox` |

```bash
curl -X PATCH "https://boxy.example.com/v1/messages/$ID" \
  -H "Authorization: Bearer $KEY" \
  -d '{"remove_labels": ["unread", "inbox"]}'
```

Trash is only a label: Boxy keeps trashed mail until it's deleted for good,
with `DELETE /v1/messages/{id}`, from a mail client, or with the dashboard's
**Delete forever**, and **Empty Trash** or **Delete all spam** for one
account. Deleting removes the raw source too, for everyone
with access to the account, and can't be undone.

## POP3

Mail clients log in with the account's address as the username and an API
key as the password, over plain POP3 with STLS, or POP3S. The maildrop is the
newest 250 messages of the account's inbox, and the unique ID (UIDL) of a
message is its ID. Retrieving a message marks it read for everyone, as with
WildDuck; set `POP3_MARK_READ=false` to have POP3 leave read status alone.

What a client's delete does is set by `POP3_DELETE`, applied when it quits:

| `POP3_DELETE` | Deleted messages |
|---|---|
| `keep` (default) | Stay in the inbox: `DELE` is accepted and ignored |
| `archive` | Lose the `inbox` label |
| `trash` | Lose `inbox`, get `trash` |

`keep` matches a WildDuck patched to ignore `DELE`, so clients set to delete
after downloading can't remove mail that others read through the API.

Several people's clients can use the same account at once, as with WildDuck:
unlike RFC 1939, a session doesn't lock the maildrop. Each client downloads
every message once, tracking them by UIDL. With `archive` or `trash`, the
first client to delete a message removes it from everyone else's maildrop. Any
API key can log in to any account, as over the API. Without a TLS
certificate, logins are plaintext; use that only on a trusted network.

## IMAP

Mail clients log in as for POP3: the account's address and an API key.
Folders are labels, as in Gmail, so a message with two labels is in both:

| Folder | Holds | Special use |
|---|---|---|
| `INBOX` | `inbox` | |
| `Starred` | `starred` | `\Flagged` |
| `Sent` | `sent` | `\Sent` |
| `Drafts` | `draft` | `\Drafts` |
| `Spam` | `spam` | `\Junk` |
| `Trash` | `trash` | `\Trash` |
| `All Mail` | Everything but spam and trash | `\All` |
| `billing`, ... | Each of your labels, under its name | |

Clients find the special folders by their special-use attributes (RFC 6154),
whatever the names. `\Seen` is the absence of `unread`, and `\Flagged`,
`\Answered`, and `\Draft` are `starred`, `answered`, and `draft`, so reading,
flagging, or replying in a mail client shows in the dashboard and for
everyone else, and the other way around. Like `unread`, `answered` is a
state rather than a folder: mail clients set it when you reply, and the
dashboard shows it as ↩ on the message.

- **Spam and trash** show only in their own folders, as in Gmail. A trashed
  message keeps its other labels, so it's back in Starred or `billing` once
  restored, with a new UID there. The dashboard works the same way.
- **Copying** to a folder adds its label; **moving** also removes the
  source's. Moving to Trash or Spam takes the message out of the inbox.
- **Deleting** (`\Deleted`) works as in Gmail. It takes effect at once,
  without waiting for an expunge: the message loses that folder's label, or
  from All Mail goes to the trash. In Trash and Drafts, it's deleted for
  good, raw source included.
- **Drafts** a client saves go to the Drafts folder with `\Draft`;
  discarding one deletes it.
- **Creating** a folder creates a label, which every account then has.
  Folders can't be deleted or renamed from a mail client, since labels are
  shared by all accounts; use the dashboard.
- **Saving** a message (`APPEND`), such as a copy of sent mail, stores it with
  the folder's label.
- **New mail and changes** reach clients that have a folder open within a few
  seconds (IDLE).

Messages are numbered per folder (IMAP UIDs) from a per-account sequence:
a message gets the next number whenever it joins a label, so every folder's
numbers only grow, as IMAP requires.

### Where the mail protocols run

| Command | Runs | Where |
|---|---|---|
| `boxy serve` | API and dashboard | Anywhere, such as Cloud Run |
| `boxy serve --mail` | API, dashboard, and mail protocols | A single server, or local development (`just serve`) |
| `boxy mail` | Mail protocols only: POP3 and IMAP | A host that takes raw TCP, such as a small VM; no Google or session settings |

Cloud Run takes only HTTP, so pair `boxy serve` there with `boxy mail` on a
VM.

Both need the same PostgreSQL database and S3 or GCS bucket, so mail
delivered through the API shows up in POP3 and IMAP at once.

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/accounts` | Create an account (admins only): `addresses` (the first is primary), `name` |
| `GET` | `/v1/accounts` | List accounts, newest first. Query: `limit` (1–100, default 50), `after` (last ID of the previous page) |
| `GET` | `/v1/accounts/{id_or_address}` | Get an account by ID or any of its addresses |
| `PATCH` | `/v1/accounts/{id_or_address}` | Update an account (admins only): `name`, `addresses` (replaces them, in order) |
| `POST` | `/v1/messages` | Store an incoming message. Body: raw message (`message/rfc822`). Query: `account_id_or_address`, `mail_from`, `received_at`, `labels`. Header: `Idempotency-Key` |
| `POST` | `/v1/accounts/{account_id_or_address}/messages` | Same, for one account |
| `GET` | `/v1/messages/{id}` | Get a message, with its `parts` listed |
| `GET` | `/v1/messages` | List messages, newest received first. Query: `account_id_or_address`, `label`, `limit`, `after` |
| `GET` | `/v1/accounts/{account_id_or_address}/messages` | Same, for one account |
| `PATCH` | `/v1/messages/{id}` | Update a message: `add_labels`, `remove_labels`, `metadata` (merged; an empty value deletes a key) |
| `DELETE` | `/v1/messages/{id}` | Delete a message for good, with its raw source |
| `GET` | `/v1/messages/{id}/headers` | Get a message's header fields |
| `GET` | `/v1/messages/{id}/parts/{index}` | Get a part, decoded, by its position in `parts` (from 0): the text or HTML body, or an attachment |
| `GET` | `/v1/messages/{id}/raw` | Get a message's raw source |
| `GET` | `/v1/labels` | List labels |
| `POST` | `/v1/labels` | Create a user label (admins only) |
| `DELETE` | `/v1/labels/{name}` | Delete a user label (admins only) |

Accounts are numbered in order from 1, and messages have 21-character
[nanoid](https://github.com/ai/nanoid) IDs. Accounts are listed newest
created first, and messages newest received first: by
`received_at`, which the relay can set, so imported old mail sorts by its
real date. Lists page by ID: pass the last ID as `after`; a page shorter
than `limit` is the last one.

Messages carry `metadata`, your own key-value data such as
`{"ticket_id": "T-123"}`: up to 50 keys of 1–40 characters, values up to 500.
It isn't part of the email. The email's own header fields are at
`/v1/messages/{id}/headers`: every field exactly as received, in order, as `{"name", "value"}`, including the ones parsed into `from`, `to`,
`cc`, `subject`, and `date`. Values are unfolded but not decoded, so
`=?utf-8?B?...?=` stays as written; the parsed fields are decoded.

A message's `parts` are its MIME parts, every one but multiparts, in the
order of the raw message, named as in JMAP: `type` (such as
`application/pdf`), `disposition` (`inline`, `attachment`, or empty),
`name` (the file name), `cid` (the Content-ID the HTML refers to as
`cid:...` for inline images), and `size` (decoded). The text body is the first
`text/plain` part whose disposition isn't `attachment` and that has no name;
the HTML body, the same for `text/html`. Everything else is an attachment:

```js
const body = type => parts.findIndex(p => p.type === type && p.disposition !== "attachment" && !p.name)
```

Get a part by its index at `/v1/messages/{id}/parts/{index}`: decoded, text
in UTF-8, with its content type and its disposition naming the file (without
one, named parts download and the rest show). Only that
part is read from storage.

`mail_from` and `rcpt_to` are the SMTP envelope (`MAIL FROM` and `RCPT TO`,
Cloudflare's `message.from` and `message.to`, named as in JMAP's
`envelope`), not the `From`, `Sender`, and `To` headers; `mail_from` is empty
for bounces.

An account's address can't contain `+`, since `+` starts a tag. The API
never changes `unread` on its own; the dashboard removes it when you open a
message. Raw messages, up to 40 MiB each, are kept in object storage under the
message ID. The database keeps what lists and filters need (addresses,
subject, snippet) and where each body and attachment is in the raw message;
opening a message reads only its header and bodies, and downloading an
attachment reads only that attachment.

The OpenAPI spec is generated to `public/openapi.yaml` and served at
`/openapi.yaml`.

## Environment variables

| Variable | Description | Default |
|---|---|---|
| `PORT` | Server listen port | `8080` |
| `HOSTNAME` | Public base URL | `http://localhost:$PORT` |
| `DATABASE_URL` | `file:boxy.db` for SQLite, `postgres://user:pass@host/db` for PostgreSQL | `file:boxy.db` |
| `STORAGE_BACKEND` | Where raw messages go: `local`, `s3`, `gcs`, or `memory` (lost on restart) | `local` |
| `LOCAL_STORAGE_DIR` | Directory for `local` storage | `./storage` |
| `S3_BUCKET` | Bucket for `s3`: Amazon S3, Cloudflare R2, MinIO; credentials from the usual `AWS_*` variables | — |
| `GCS_BUCKET` | Bucket for `gcs`; credentials from Application Default Credentials | — |
| `POP3_ADDR` | POP3 listen address for `serve --mail` and `mail`, with STLS when a certificate is set; empty disables POP3 | `:1110` |
| `POP3S_ADDR` | POP3 over TLS listen address, such as `:995` | — |
| `MAIL_TLS_CERT`, `MAIL_TLS_KEY` | PEM certificate and key files for the mail protocols (STARTTLS, POP3S, IMAPS); logins then need TLS | — |
| `POP3_DELETE` | What a client's delete does: `keep`, `archive`, or `trash` | `keep` |
| `POP3_MARK_READ` | Whether retrieving or deleting a message over POP3 marks it read | `true` |
| `POP3_MAX_MESSAGES` | How many of the newest inbox messages POP3 lists | `250` |
| `IMAP_ADDR` | IMAP listen address for `serve --mail` and `mail`, with STARTTLS when a certificate is set; empty disables IMAP | `:1143` |
| `IMAPS_ADDR` | IMAP over TLS listen address, such as `:993` | — |
| `ADMIN_EMAILS` | Comma-separated sign-in emails of admins, who manage accounts, labels, and users, and reach every account | — (no admins) |
| `SESSION_SECRET` | HMAC key for signing session cookies | required |
| `GOOGLE_CLIENT_ID` | Google OAuth 2.0 client ID | required |
| `GOOGLE_CLIENT_SECRET` | Google OAuth 2.0 client secret | required |
| `ALLOWED_DOMAINS` | Comma-separated list of email domains that can sign in | — (all allowed) |

Migrations are managed by [goose](https://github.com/pressly/goose): run
`boxy migrate`, or start with `boxy serve --migrate`. See
[Where the mail protocols run](#where-the-mail-protocols-run) for
`serve --mail` and `mail`.

## Access

Users sign in to the dashboard with Google, and each creates their own API
keys, which act as them. Admins, listed in `ADMIN_EMAILS`, reach every account
and manage them: they create accounts, change their addresses, and share
them by the email someone signs in with (they sign in once first). Everyone
else works with the mail of the accounts shared with them. Without access,
an account and its mail don't exist for a user: lists leave them out and
lookups answer 404. Labels are shared by every account, so admins create and
delete them; everyone applies them.

The dashboard's Users page, for admins, lists everyone who has signed in,
the accounts shared with them, and their API keys. To offboard someone,
revoke their keys, which also signs out their mail clients, and remove
their access.

An account receives mail for every one of its addresses, which are in
order: the first is primary, shown for the account. Each address belongs to
one account. A relay that forwards a whole domain to Boxy, such as a
Cloudflare catch-all rule, works:
mail to an address no account has is rejected before it's read. The relay
uses an admin's API key, so it can deliver to every account.

Mail clients log in to an account with one of its addresses and the API key
of someone with access.

## Development

API definitions are in [proto/boxy/v1](proto/boxy/v1). Code is generated with
[buf](https://buf.build):

```bash
just generate   # buf generate: Go + Connect into gen/, OpenAPI into public/
just lint       # buf lint
just test       # go test ./...
just build      # build the boxy binary
```

| Package | |
|---|---|
| [cmd/boxy](cmd/boxy) | Entry point: `serve`, `mail`, and `migrate` |
| [internal/service](internal/service) | The API: Account, Label, and Message services, mounted for Connect, gRPC, and REST |
| [internal/mailparse](internal/mailparse) | Parses raw messages into header fields, bodies, a snippet, and attachments |
| [internal/db](internal/db) | SQLite and PostgreSQL storage, with migrations |
| [internal/pop3](internal/pop3) | The POP3 server |
| [internal/imap](internal/imap) | The IMAP server, on [go-imap](https://github.com/emersion/go-imap) |
| [internal/storage](internal/storage) | Raw message storage: local disk, S3-compatible, GCS, or memory |
| [internal/web](internal/web) | The dashboard |
| [internal/auth](internal/auth) | Google sign-in and session cookies |
