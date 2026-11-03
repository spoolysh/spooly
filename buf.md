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

## Alpha

Over Connect or gRPC only, and still taking shape: they may change before
moving to `spooly.v1`.

`spooly.v1alpha1.MessageService` — list a folder or label by seq, for sync
clients such as an IMAP bridge, update messages in batches, and send mail.

| RPC | |
|---|---|
| `ListMessagesBySeq` | A folder or label in arrival order, with each message's seq |
| `BatchUpdateMessages` | One update to up to 1000 messages |
| `SendMessage` | Send a raw message from the mailbox of its From, with a delivery to each recipient |

`spooly.v1alpha1.DeliveryService` — how sent messages are getting to their
recipients.

| RPC | |
|---|---|
| `ListDeliveries` | Deliveries by message, mailbox, or status |
| `RetryDelivery` | Try a deferred or failed delivery again now |

`spooly.v1alpha1.ChangeService` — follow what changes in a mailbox.

| RPC | |
|---|---|
| `ListChanges` | Messages created, updated, and deleted since a cursor |

## Links

- [GitHub](https://github.com/spoolysh/spooly)
