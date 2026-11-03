# Spooly

Mail backend. Incoming mail is stored per mailbox, raw and parsed, organized
with labels, and read through one API: REST, gRPC, gRPC-Web, and Connect.

> Early prototype: receiving mail, the API, and the dashboard work, POP3
> and IMAP through the bridge, and sending through an SMTP relay or
> Cloudflare, in alpha.

- **Mailboxes**: receive mail for any of their addresses, looked up by ID or
  address; plus addresses (`hello+billing@`) reach `hello@`, labeled
  `billing`. Shared with the people who work them
- **Folders, labels, and flags**: as in Proton Mail, a message is in one
  folder (inbox, archive, sent, drafts, junk, trash) and has any labels;
  flags say its state: read, answered, starred, draft, forwarded. Labels are
  shared by all mailboxes
- **Raw first**: every message is kept as received (RFC 5322); headers,
  bodies, and attachments are read from it, only the part asked for
- **Any inbound**: a Cloudflare Email Routing worker first; anything that can
  POST the raw message works
- **Sending**: through an SMTP relay or Cloudflare, with each recipient's
  delivery tracked and retried
- **POP3 and IMAP**: mail clients read a mailbox through the
  [bridge](#mail-clients), with an API key as the password
- **Dashboard**: read mail, move it and label it, and manage mailboxes,
  labels, and API keys, signed in with Google

## Quick start

```bash
export SESSION_SECRET="your-secret-here"
export GOOGLE_CLIENT_ID="your-client-id"
export GOOGLE_CLIENT_SECRET="your-client-secret"

just serve
# or: go run ./cmd/spooly serve --migrate
```

Visit `http://localhost:8080` to sign in, create a mailbox, and generate an
API key. Uses SQLite by default, with PostgreSQL available when you need a
separate database, and keeps raw messages in `./storage`. With Docker:
`cp .env.example .env`, fill it in, and `docker compose up`.

## Receiving mail

An inbound worker posts the raw message as the body, with the envelope in the
query string:

```bash
curl -X POST "https://spooly.example.com/v1/mailboxes/hello@spooly.sh/messages?mail_from=bounce@example.com" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: message/rfc822" \
  -H "Idempotency-Key: 9b2f…" \
  --data-binary @message.eml
```

The address in the path is the envelope recipient (Cloudflare's
`message.to`) and is kept as the message's `rcpt_to`. Plus addresses reach
the base mailbox: `hello+news@spooly.sh` lands in `hello@spooly.sh` with
`rcpt_to` `hello+news@spooly.sh`. A plus address's tag is also a label, if
one by that name exists: mail to `hello+billing@spooly.sh` is labeled
`billing`. An unknown address gets `404`, so the worker can reject the mail.
Send an `Idempotency-Key` header to make retries safe: a repeat with the
same key, in the same mailbox, returns the stored message. New messages go
to the inbox, with no labels or flags.

To mirror an existing mailbox, such as Gmail, pass each message's own
`folder`, `labels` (repeated), and flags instead: `?folder=sent&seen=true`
stores sent mail, `?labels=billing&seen=true&flagged=true` a read, starred
inbox message labeled `billing`. Labels must exist; create your own first.
Use the source's message ID as the idempotency key, such as
`gmail-18f3c2a9b7d41e05`, so syncing again never duplicates, and its date as
`received_at`, so mail sorts by when it really arrived.

A Cloudflare Email Routing worker that delivers to Spooly. Cloudflare gives no
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
    const resp = await fetch(`${env.SPOOLY_URL}/v1/mailboxes/${to}/messages?${query}`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${env.SPOOLY_KEY}`,
        "Content-Type": "message/rfc822",
        "Idempotency-Key": await idempotencyKey(message, raw),
      },
      body: raw,
    });
    if (resp.status === 404) message.setReject("Unknown address");
    else if (!resp.ok) throw new Error(`Spooly answered ${resp.status}`); // Cloudflare retries
  },
};
```

## Sending mail

Sending is in alpha, over Connect or gRPC only: `MessageService/SendMessage`
under `spooly.v1alpha1`, and `DeliveryService` for how it went. The
dashboard writes plain-text messages with **New message**.

```bash
curl -X POST "https://spooly.example.com/spooly.v1alpha1.MessageService/SendMessage" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Idempotency-Key: 7c41…" \
  -d '{"raw": "'"$(base64 < message.eml | tr -d '\n')"'"}'
```

The message is raw RFC 5322. Its From says which mailbox sends it: an
address of a mailbox you can see, or a plus address of one. Its Bcc header
is taken out, and a
Message-ID and a Date are added if missing; recipients are its To, Cc, and
Bcc, unless `rcpt_to` lists them. It's stored in the mailbox's sent folder,
read, as it went out, and handed to the mail transport at once. The
response has the message and a delivery to each recipient; a refusal
doesn't fail the call. With the same `Idempotency-Key`, a retry returns them
and sends nothing again.

Each delivery is `queued` until handed over, then `sent` once the transport
took it, `deferred` while it's retried (after 1 minute, then 5, 15, 30, an
hour, 2, then every 4 hours), or `failed` if the transport refused it for
good or for 24 hours. `ListDeliveries` lists them by message, mailbox, or
status, `GetDelivery` gets one with its events, what happened to it: each
try and the transport's reply, and retries by hand; and `RetryDelivery`
tries a deferred or failed one again now. The dashboard shows the same: on
the message, on each delivery's page, and to admins under Deliveries.
Deleting a message deletes its deliveries, cancelling those still to go.

Spooly retries nothing on its own: deferred deliveries go when `POST /tick`
is called, with an admin's key. It runs the jobs that are due: `deliveries`
each time, and `prune` (expunging deleted messages, pruning old events) at
most hourly, and answers what each did; `?job=deliveries` runs just one.
Have a scheduler call it every minute, such as Cloud Scheduler on Cloud Run:

```bash
gcloud scheduler jobs create http spooly-tick --schedule="* * * * *" \
  --uri="https://spooly.example.com/tick" --http-method=POST \
  --headers="Authorization=Bearer $KEY"
```

What Spooly sends carries `X-Spooly-Message-Id`, its ID for the message. A
copy that comes back to the same mailbox, such as a mail client saving it to
Sent, or Gmail's Sent copy synced in, is that message, not a new one.

With `SAVE_TO_SENT=false`, as for an app sending notifications, no copy is
kept: the response has no message, only the deliveries, which keep its
sender and subject. Its raw source stays in storage only until they're
done, and it carries no `X-Spooly-Message-Id`, so a copy that comes back is
a message of its own. A repeated `Idempotency-Key` still finds the
deliveries, and sends nothing again.
The events are `delivery.created` and `delivery.updated`, in the same
stream as messages': kept for `EVENTS_RETENTION`, after which a delivery
keeps only its last status and reply.

The transport is `MAIL_TRANSPORT`:

- `smtp`: any SMTP relay, such as Google Workspace's SMTP relay service
  (`smtp-relay.gmail.com`), which sends from any address of your domains.
  Port 465 speaks TLS from the start; any other must offer STARTTLS, so mail
  never goes in the clear, unless the relay is this machine.
- `cloudflare`: Cloudflare Email Service, over its SMTP submission with an
  API token. The domain must be onboarded to Email Sending, which needs the
  Workers Paid plan.

Either way, publish SPF and DKIM for the relay, and DMARC, or mail goes to
spam.

To try sending locally, catch it with [Mailpit](https://mailpit.axllent.org),
which shows what arrives at http://localhost:8025, and refuses recipients
outside `--smtp-allowed-recipients`, so failed deliveries can be seen too:

```bash
mailpit --smtp-allowed-recipients '@example\.(com|org)$'
MAIL_TRANSPORT=smtp SMTP_HOST=localhost SMTP_PORT=1025 just serve
```

## Folders, labels, and flags

As in Proton Mail, a message's folder says where it is, its labels what it's
about, and its flags what state it's in.

A message is in exactly one folder: `inbox`, `archive`, `sent`, `drafts`,
`junk`, or `trash`. Combine a folder with a mailbox to get, say, one
address's inbox: `GET /v1/mailboxes/support@spooly.sh/messages?folder=inbox`.
Without a folder, lists show all mail but junk and trash, as Gmail's All
Mail; those are listed only under `folder=junk` and `folder=trash`.

A label is just its name: up to 64 lowercase letters and digits, separated by
single `_` or `-` (`billing`, `customer-issues`). A message can have several,
and keeps them in any folder: a trashed message drops out of `billing` until
it's restored, and is back there then. Labels are shared by all mailboxes, so
`billing` means the same across every address. No label can be named after a
folder or a flag, or `spam`, `unread`, and `starred`.

Flags are named as JMAP's keywords and match IMAP's flags, and a message
keeps them wherever it goes:

| Flag | Set when | IMAP |
|---|---|---|
| `seen` | Read | `\Seen` |
| `answered` | Replied to | `\Answered` |
| `flagged` | Starred | `\Flagged` |
| `draft` | Not finished | `\Draft` |
| `forwarded` | Forwarded | `$Forwarded` |

Lists filter by `seen` and `flagged`: `?seen=false` for unread mail,
`?flagged=true` for starred.

Common actions:

| Action | Change |
|---|---|
| Mark read | `seen`: true |
| Archive | `folder`: `archive` |
| Star | `flagged`: true |
| Trash | `folder`: `trash` |
| Label | `add_labels`: `["billing"]` |

```bash
curl -X PATCH "https://spooly.example.com/v1/messages/$ID" \
  -H "Authorization: Bearer $KEY" \
  -d '{"folder": "archive", "seen": true}'
```

Trash is only a folder: Spooly keeps trashed mail until it's deleted for good,
with `DELETE /v1/messages/{id}`, from a mail client, or with the dashboard's
**Delete forever**, and **Empty Trash** or **Delete all junk** for one
mailbox. Deleting is for everyone with access to the mailbox: the message is
found no more, and [lists of changes](#syncing) report it deleted. For
`EXPUNGE_AFTER` (7 days by default), an admin can still restore it, which
brings it back under its ID, in its folder, with its flags and the labels
that still exist. Then it's expunged: its raw source is removed, and it's
gone for good. With `EXPUNGE_AFTER=0`, that's at once.

## Syncing

A client that keeps its own copy of a mailbox, such as an IMAP bridge or
a service working the mail, lists it once and then follows its changes.
Every change to a message, deleting it included, is logged as an event,
numbered in the order changes commit, and the cursor a client passes back
says where it resumes; it's opaque, so pass it back as it came. Events
are the organization's, one stream for every mailbox: webhooks will send
them all, and a mailbox's changes are its own of them.

Syncing is over Connect or gRPC only, not mapped to REST, and in alpha
(`spooly.v1alpha1`), so it may still change:
`MessageService/ListMessagesBySeq` and `ChangeService/ListChanges`.

1. Take a cursor to start from: `ListChanges` without one, as
   `POST /spooly.v1alpha1.ChangeService/ListChanges` with
   `{"mailbox_id_or_address": "hello@spooly.sh"}`, has no changes, only the
   `cursor` for changes from now. Taken before listing, it misses no change
   made while the client lists.
2. List each folder in the order messages came there, with
   `ListMessagesBySeq` and `{"mailbox_id_or_address": "hello@spooly.sh",
   "folder": "inbox"}`; a message is in exactly one folder, and its summary
   has its labels. Each comes with its `seq` there; page with `after`, the
   last one's seq. A label lists the same way, with `label` for `folder`.
3. Follow the changes from the cursor: `ListChanges` with
   `"cursor": "01j9z3…"`. The response
   has, each once, the messages `created` since, delivered or restored, and
   those `updated`, there before and changed, both as they are now; the IDs
   of the ones `deleted`, even those created since; and the `cursor` to pass
   next time. `has_more` says there's another page right away. A change
   made while listing can show again: a created message may be listed
   already, and a deleted one never seen, so apply created and updated
   messages alike, and ignore deletions of unknown ones. `limit` counts
   the organization's events read, other mailboxes' included, so a page
   can have fewer changes.

Lists and changes show messages as summaries: ID, mailbox, folder, labels,
flags, sender, subject, snippet, date, size, number of attachments, and
`received_at`. Get a message for its recipients, envelope, and parts.

A change shows as soon as it's committed. Events are numbered as they
commit, so none commits behind one a client has seen: a client resuming
from its cursor misses none.

Every change is kept as an event, named as webhooks will send it:
`message.created`, `message.updated` (with what changed: the folder moved
from and to, labels added and removed, flags), `message.deleted`, and
`message.restored`. An update that changes nothing, such as adding a label
the message has, is no change. Events are kept for `EVENTS_RETENTION` (45
days by default; `0` keeps them forever), then pruned. A cursor from before the newest event
pruned fails with `FAILED_PRECONDITION`: start over, from step 1.

A message's seq in a folder or label is higher than that of any message
that came there before it: moved to the archive, or back from the trash, it
gets a new one. The IMAP bridge uses seqs as UIDs.

Messages can be updated a thousand at a time, as `PATCH` does each, over
Connect or gRPC only, in alpha for now:

```bash
curl -X POST "https://spooly.example.com/spooly.v1alpha1.MessageService/BatchUpdateMessages" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"ids": ["01j9z3…", "..."], "seen": true}'
```

## Mail clients

Mail clients read mailboxes over POP3 and IMAP through the
[bridge](https://github.com/spoolysh/bridge), a separate, stateless
server that calls this API with each client's API key. It runs wherever
raw TCP gets through, such as a small VM next to a Cloud Run deployment, or
inside a desktop app.

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/mailboxes` | Create a mailbox (admins only): `addresses` (the first is primary), `name` |
| `GET` | `/v1/mailboxes` | List mailboxes, newest first. Query: `limit` (1–100, default 50), `after` (last ID of the previous page) |
| `GET` | `/v1/mailboxes/{id_or_address}` | Get a mailbox by ID or any of its addresses |
| `PATCH` | `/v1/mailboxes/{id_or_address}` | Update a mailbox (admins only): `name`, `addresses` (replaces them, in order) |
| `POST` | `/v1/messages` | Store an incoming message. Body: raw message (`message/rfc822`). Query: `mailbox_id_or_address`, `mail_from`, `received_at`, `folder`, `labels`, `seen`, `answered`, `flagged`, `draft`, `forwarded`. Header: `Idempotency-Key` |
| `POST` | `/v1/mailboxes/{mailbox_id_or_address}/messages` | Same, for one mailbox |
| `GET` | `/v1/messages/{id}` | Get a message, with its `parts` listed |
| `GET` | `/v1/messages` | List messages as summaries, newest received first. Query: `mailbox_id_or_address`, `folder`, `label`, `seen`, `flagged`, `limit`, `after` |
| `GET` | `/v1/mailboxes/{mailbox_id_or_address}/messages` | Same, for one mailbox |
| `PATCH` | `/v1/messages/{id}` | Update a message: `folder`, `add_labels`, `remove_labels`, and the flags `seen`, `answered`, `flagged`, `draft`, and `forwarded` |
| `DELETE` | `/v1/messages/{id}` | Delete a message for good, with its raw source |
| `GET` | `/v1/messages/{id}/headers` | Get a message's header fields |
| `GET` | `/v1/messages/{id}/parts/{index}` | Get a part, decoded, by its position in `parts` (from 0): the text or HTML body, or an attachment |
| `GET` | `/v1/messages/{id}/raw` | Get a message's raw source. Query: `offset`, `length`, for just those bytes |
| `GET` | `/v1/labels` | List labels |
| `POST` | `/v1/labels` | Create a label (admins only) |
| `DELETE` | `/v1/labels/{name}` | Delete a label (admins only) |

Mailboxes and messages have [ULID](https://github.com/ulid/spec)
IDs, lowercase, such as `01j9z3kq7v0m4t2x8r5b6n1c9d`: 26 characters that
sort by when they were made. Mailboxes are listed newest
created first, and messages newest received first: by
`received_at`, which the relay can set, so imported old mail sorts by its
real date. Lists page by ID: pass the last ID as `after`; a page shorter
than `limit` is the last one.

A message's header fields are at
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

A mailbox's address can't contain `+`, since `+` starts a tag. The API
never changes `seen` on its own; the dashboard sets it when you open a
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
| `DATABASE_URL` | `file:spooly.db` for SQLite, `postgres://user:pass@host/db` for PostgreSQL | `file:spooly.db` |
| `STORAGE_BACKEND` | Where raw messages go: `local`, `s3`, `gcs`, or `memory` (lost on restart) | `local` |
| `LOCAL_STORAGE_DIR` | Directory for `local` storage | `./storage` |
| `S3_BUCKET` | Bucket for `s3`: Amazon S3, Cloudflare R2, MinIO; credentials from the usual `AWS_*` variables | — |
| `GCS_BUCKET` | Bucket for `gcs`; credentials from Application Default Credentials | — |
| `EVENTS_RETENTION` | How long events are kept: for lists of changes, and as deliveries' history; at least `EXPUNGE_AFTER`, or `0` to keep them forever | `1080h` (45 days) |
| `EXPUNGE_AFTER` | How long a message deleted for good can still be restored by an admin, its raw source kept; `0` removes it at once | `168h` (7 days) |
| `MAIL_TRANSPORT` | How mail is sent: `smtp`, `cloudflare`, or empty to turn sending off | — (off) |
| `SMTP_HOST`, `SMTP_PORT` | The relay for `smtp`; port 465 is TLS from the start, any other must offer STARTTLS | —, `587` |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | The relay's login, if it takes one | — |
| `SAVE_TO_SENT` | Keep a copy of sent mail in the mailbox's Sent folder; `false` keeps only each recipient's delivery | `true` |
| `CLOUDFLARE_API_TOKEN` | An API token with Email Sending: Edit, for `cloudflare` | — |
| `ADMIN_EMAILS` | Comma-separated sign-in emails of admins, who manage mailboxes, labels, and users, and reach every mailbox | — (no admins) |
| `SESSION_SECRET` | HMAC key for signing session cookies | required |
| `GOOGLE_CLIENT_ID` | Google OAuth 2.0 client ID | required |
| `GOOGLE_CLIENT_SECRET` | Google OAuth 2.0 client secret | required |
| `ALLOWED_DOMAINS` | Comma-separated list of email domains that can sign in | — (all allowed) |

Migrations are managed by [goose](https://github.com/pressly/goose): run
`spooly migrate`, or start with `spooly serve --migrate`.

## Access

Users sign in to the dashboard with Google, and each creates their own API
keys, which act as them. Admins, listed in `ADMIN_EMAILS`, reach every mailbox
and manage them: they create mailboxes, change their addresses, and share
them by the email someone signs in with (they sign in once first). Everyone
else works with the mail of the mailboxes shared with them. Without access,
a mailbox and its mail don't exist for a user: lists leave them out and
lookups answer 404. Labels are shared by every mailbox, so admins create and
delete them; everyone applies them.

The dashboard's Users page, for admins, lists everyone who has signed in,
the mailboxes shared with them, and their API keys. To offboard someone,
revoke their keys, which also signs out their mail clients, and remove
their access.

A mailbox receives mail for every one of its addresses, which are in
order: the first is primary, shown for the mailbox. Each address belongs to
one mailbox. A relay that forwards a whole domain to Spooly, such as a
Cloudflare catch-all rule, works:
mail to an address no mailbox has is rejected before it's read. The relay
uses an admin's API key, so it can deliver to every mailbox.

Mail clients log in to a mailbox with one of its addresses and the API key
of someone with access.

## Development

API definitions are in [proto/spooly/v1](proto/spooly/v1), and those still
taking shape in [proto/spooly/v1alpha1](proto/spooly/v1alpha1), which breaking
changes checks skip. Code is generated with [buf](https://buf.build):

```bash
just generate   # buf generate: Go + Connect into gen/, OpenAPI into public/
just lint       # buf lint
just test       # go test ./...
just build      # build the spooly binary
```

| Package | |
|---|---|
| [cmd/spooly](cmd/spooly) | Entry point: `serve` and `migrate` |
| [internal/service](internal/service) | The API: Mailbox, Label, and Message services, and the alpha Change and Delivery services, mounted for Connect, gRPC, and REST |
| [internal/mailparse](internal/mailparse) | Parses raw messages into header fields, bodies, a snippet, and attachments |
| [internal/db](internal/db) | SQLite and PostgreSQL storage, with migrations |
| [internal/storage](internal/storage) | Raw message storage: local disk, S3-compatible, GCS, or memory |
| [internal/delivery](internal/delivery) | Sends mail through a transport, SMTP or Cloudflare, retrying each recipient's delivery |
| [internal/web](internal/web) | The dashboard |
| [internal/auth](internal/auth) | Google sign-in and session cookies |
| [spoolytest](spoolytest) | Runs the API in-process for other modules' tests, such as the bridge's |
