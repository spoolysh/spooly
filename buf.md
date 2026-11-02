# Spooly

Self-hosted mail backend with multi-protocol API (gRPC, gRPC-Web, Connect, REST).

## Services

`spooly.v1.MailboxService` — create, list, get, and update mailboxes.

| RPC | Method | Path |
|---|---|---|
| `CreateMailbox` | `POST` | `/v1/mailboxes` |
| `ListMailboxes` | `GET` | `/v1/mailboxes` |
| `GetMailbox` | `GET` | `/v1/mailboxes/{id_or_address}` |
| `UpdateMailbox` | `PATCH` | `/v1/mailboxes/{id_or_address}` |

`spooly.v1.MessageService` — deliver, list, get, label, and delete messages, and read their headers, parts, and raw source.

| RPC | Method | Path |
|---|---|---|
| `CreateMessage` | `POST` | `/v1/messages`, `/v1/mailboxes/{mailbox_id_or_address}/messages` |
| `GetMessage` | `GET` | `/v1/messages/{id}` |
| `ListMessages` | `GET` | `/v1/messages`, `/v1/mailboxes/{mailbox_id_or_address}/messages` |
| `UpdateMessage` | `PATCH` | `/v1/messages/{id}` |
| `DeleteMessage` | `DELETE` | `/v1/messages/{id}` |
| `GetMessageHeaders` | `GET` | `/v1/messages/{id}/headers` |
| `GetMessagePart` | `GET` | `/v1/messages/{id}/parts/{index}` |
| `GetMessageRaw` | `GET` | `/v1/messages/{id}/raw` |

`spooly.v1.LabelService` — create, list, and delete labels.

| RPC | Method | Path |
|---|---|---|
| `CreateLabel` | `POST` | `/v1/labels` |
| `ListLabels` | `GET` | `/v1/labels` |
| `DeleteLabel` | `DELETE` | `/v1/labels/{name}` |

## Links

- [GitHub](https://github.com/spoolysh/spooly)
