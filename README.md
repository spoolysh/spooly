# Boxy

Mail backend. Incoming mail is stored per mailbox, raw and parsed, and read
through one API: REST, gRPC, gRPC-Web, and Connect.

> Early prototype: only the API definition exists so far.

- **Mailboxes**: one per address, looked up by ID or address
- **Raw first**: every message is kept as received (RFC 5322); headers,
  bodies, and attachments are parsed from it
- **Any inbound**: a Cloudflare Email Routing worker first; anything that can
  POST the raw message works

## Receiving mail

An inbound worker posts the raw message as the body, with the envelope in the
query string:

```bash
curl -X POST "https://boxy.example.com/v1/mailboxes/hello@boxy.sh/messages?sender=bounce@example.com" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: message/rfc822" \
  --data-binary @message.eml
```

An unknown address gets `404`, so the worker can reject the mail. Pass
`idempotency_key` to make retries safe: a repeat returns the stored message.

## API

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/mailboxes` | Create a mailbox |
| `GET` | `/v1/mailboxes` | List mailboxes, newest first. Query: `limit` (1–100, default 50), `after` (last ID of the previous page) |
| `GET` | `/v1/mailboxes/{id_or_addr}` | Get a mailbox by ID or address |
| `POST` | `/v1/messages` | Store an incoming message. Body: raw message (`message/rfc822`). Query: `mailbox_id_or_addr`, `sender`, `received_at`, `idempotency_key` |
| `GET` | `/v1/messages` | List messages, newest first. Query: `mailbox_id_or_addr`, `limit`, `after` |
| `GET` | `/v1/mailboxes/{mailbox_id_or_addr}/messages` | Same, for one mailbox |
| `POST` | `/v1/mailboxes/{mailbox_id_or_addr}/messages` | Same, for one mailbox |
| `GET` | `/v1/messages/{id}` | Get a message with its body |
| `GET` | `/v1/messages/{id}/raw` | Get a message's raw source |

IDs are lowercase [ULIDs](https://github.com/ulid/spec), so they sort by the
time a mailbox or message was created. Lists page by ID: pass the last ID as
`after`; a page shorter than `limit` is the last one.

`sender` is the SMTP envelope sender (`MAIL FROM`, Cloudflare's
`message.from`), not the `From` header; it's empty for bounces.

The OpenAPI spec is generated to `public/openapi.yaml`.

## Development

API definitions are in [proto/boxy/v1](proto/boxy/v1). Code is generated with
[buf](https://buf.build):

```bash
buf lint        # lint proto files
buf generate    # Go + Connect into gen/, OpenAPI into public/
```
