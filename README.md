# Boxy

Mail backend. Incoming mail is stored per account, raw and parsed, organized
with labels, and read through one API: REST, gRPC, gRPC-Web, and Connect.

> Early prototype: only the API definition exists so far.

- **Accounts**: one per address, looked up by ID or address; plus addresses
  (`hello+news@`) reach `hello@`
- **Labels**: like Gmail, a message can have several; inbox, unread, and
  trash are labels too. Shared by all accounts
- **Raw first**: every message is kept as received (RFC 5322); headers,
  bodies, and attachments are parsed from it
- **Any inbound**: a Cloudflare Email Routing worker first; anything that can
  POST the raw message works

## Receiving mail

An inbound worker posts the raw message as the body, with the envelope in the
query string:

```bash
curl -X POST "https://boxy.example.com/v1/accounts/hello@boxy.sh/messages?sender=bounce@example.com" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: message/rfc822" \
  --data-binary @message.eml
```

The address in the path is the envelope recipient (Cloudflare's
`message.to`) and is kept as the message's `recipient`. Plus addresses reach
the base account: `hello+news@boxy.sh` lands in `hello@boxy.sh` with
`recipient` `hello+news@boxy.sh`. An unknown address gets `404`, so the worker
can reject the mail. Pass
`idempotency_key` to make retries safe: a repeat returns the stored message.
New messages get the `inbox` and `unread` labels.

## Labels

A label is just its name: up to 64 lowercase letters and digits, separated by
single `_` or `-` (`billing`, `customer-issues`). Labels are shared by all
accounts, so `billing` means the same across every address. The system labels
`inbox`, `unread`, `starred`, `spam`, and `trash` always exist. Combine a
label with an account to get, say, one address's inbox:
`GET /v1/accounts/support@boxy.sh/messages?label=inbox`.

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

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/accounts` | Create an account |
| `GET` | `/v1/accounts` | List accounts, newest first. Query: `limit` (1–100, default 50), `after` (last ID of the previous page) |
| `GET` | `/v1/accounts/{id_or_addr}` | Get an account by ID or address |
| `POST` | `/v1/messages` | Store an incoming message. Body: raw message (`message/rfc822`). Query: `account_id_or_addr`, `sender`, `received_at`, `idempotency_key` |
| `POST` | `/v1/accounts/{account_id_or_addr}/messages` | Same, for one account |
| `GET` | `/v1/messages` | List messages, newest first. Query: `account_id_or_addr`, `label`, `limit`, `after` |
| `GET` | `/v1/accounts/{account_id_or_addr}/messages` | Same, for one account |
| `GET` | `/v1/messages/{id}` | Get a message with its body |
| `GET` | `/v1/messages/{id}/raw` | Get a message's raw source |
| `PATCH` | `/v1/messages/{id}` | Update a message: `add_labels`, `remove_labels`, `metadata` (merged; an empty value deletes a key) |
| `GET` | `/v1/labels` | List labels |
| `POST` | `/v1/labels` | Create a user label |
| `DELETE` | `/v1/labels/{name}` | Delete a user label |

IDs are lowercase [ULIDs](https://github.com/ulid/spec), so they sort by the
time an account or message was created. Lists page by ID: pass the
last ID as `after`; a page shorter than `limit` is the last one.

Messages carry `metadata`, your own key-value data such as
`{"ticket_id": "T-123"}`: up to 50 keys of 1–40 characters, values up to 500.

`sender` and `recipient` are the SMTP envelope (`MAIL FROM` and `RCPT TO`,
Cloudflare's `message.from` and `message.to`), not the `From` and `To`
headers; `sender` is empty for bounces.

The OpenAPI spec is generated to `public/openapi.yaml`.

## Development

API definitions are in [proto/boxy/v1](proto/boxy/v1). Code is generated with
[buf](https://buf.build):

```bash
buf lint        # lint proto files
buf generate    # Go + Connect into gen/, OpenAPI into public/
```
