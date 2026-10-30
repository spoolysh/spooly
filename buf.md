# Spooly

Self-hosted mail backend with multi-protocol API (gRPC, gRPC-Web, Connect, REST).

## Services

`spooly.v1.AccountService` — create, list, get, and update accounts.

| RPC | Method | Path |
|---|---|---|
| `CreateAccount` | `POST` | `/v1/accounts` |
| `ListAccounts` | `GET` | `/v1/accounts` |
| `GetAccount` | `GET` | `/v1/accounts/{id_or_address}` |
| `UpdateAccount` | `PATCH` | `/v1/accounts/{id_or_address}` |

`spooly.v1.MessageService` — deliver, list, get, label, and delete messages, and read their headers, parts, and raw source.

| RPC | Method | Path |
|---|---|---|
| `CreateMessage` | `POST` | `/v1/messages`, `/v1/accounts/{account_id_or_address}/messages` |
| `GetMessage` | `GET` | `/v1/messages/{id}` |
| `ListMessages` | `GET` | `/v1/messages`, `/v1/accounts/{account_id_or_address}/messages` |
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
