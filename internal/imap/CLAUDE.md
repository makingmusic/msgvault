# internal/imap

IMAP client that satisfies `gmail.API` so the same `internal/sync`
syncer can drive it. Built on `github.com/emersion/go-imap/v2`.

## Files

- `client.go` — connection lifecycle, mailbox enumeration, UID FETCH,
  trash/delete. Implements `gmailapi.API`.
- `config.go` — `*Config` (host/port/TLS/auth method) and the
  `imap[s|+starttls]://user@host:port` identifier format used as the
  `Source.identifier` value in the database.
- `auth.go` — password storage. Path is
  `<tokens_dir>/imap_<sha256-prefix>.json`. Atomic write, `0600`.
- `xoauth2.go` — `sasl.Client` for the XOAUTH2 mechanism. Used with a
  Microsoft access token from `internal/microsoft`; could in theory
  carry a Google token too, though `add-o365` is the only caller.
- `labels.go` — maps RFC 6154 mailbox attributes (`\Sent`, `\Trash`,
  `\Junk`, `\All`, `\Archive`) and well-known folder names (Inbox,
  Drafts, Spam, ...) to `system` vs `user` label types.

## API mapping

`gmail.API` was designed for Gmail; the IMAP impl fakes the rest:

- `GetProfile` → `STATUS INBOX (MESSAGES)`. `HistoryID` is always 0.
- `ListLabels` → `LIST "" "*"`, classified via `labels.classifyLabelType`.
- `ListMessages` → first call enumerates all mailboxes once into
  `messageListCache` (`client.go:443`), subsequent calls page through
  it via numeric offsets (`listPageSize = 500`). Page tokens are not
  durable across sessions — see `internal/sync/CLAUDE.md` for why
  `NoResume=true` is forced for IMAP.
- `GetMessageRaw` / `GetMessagesRawBatch` → `UID FETCH (UID ENVELOPE
  INTERNALDATE RFC822.SIZE BODY.PEEK[])` chunked at
  `fetchChunkSize = 50`.
- `ListHistory` → returns an error: IMAP has no history endpoint.
  Callers (`cmd/.../sync.go`) detect IMAP sources up front and route
  them through full sync instead.
- `TrashMessage` → `UID MOVE` to the discovered trash folder (via
  `\Trash` attr or fallback name list).
- `DeleteMessage` → requires `UIDPLUS`: `UID STORE +FLAGS \Deleted` +
  `UID EXPUNGE`. Plain `EXPUNGE` would expunge every `\Deleted`
  message in the mailbox.
- `BatchDeleteMessages` → returns an error. The deletion executor
  detects this and falls back to per-message `DeleteMessage` (avoids
  the double-retry problem if we partially succeeded here and then
  the caller retried the whole batch).

## Composite message IDs

IMAP has no stable global message ID, so the client builds
`mailbox|uid` strings (`compositeID`). These are what flows through
`store.Message.SourceMessageID`. When messages move between mailboxes
across syncs (e.g. INBOX → Trash) the composite changes; sync detects
the duplicate via RFC822 Message-ID and rewrites the row in place
(`internal/sync/sync.go:errDuplicateRFC822`).

## All Mail and label dedup

If a `\All` mailbox exists (Gmail's All Mail or RFC 6154-compliant
servers), the client treats it as the canonical source and:

- For Gmail (`[Gmail]/`): enumerates only `\All` + Trash + Junk, since
  Gmail's All Mail is a superset minus those two folders.
- For other servers: enumerates every selectable mailbox and dedupes
  by RFC822 Message-ID (`seenRFC822IDs`) so overlapping virtual
  folders don't double-import.

A label map (`msgIDToLabels`) is built by reading just `ENVELOPE`
headers from the non-`\All` mailboxes. When a message is fetched from
`\All`, its label set is augmented with every other mailbox it
appears in, so Gmail-style multi-label semantics survive the round-trip.

## Connection lifecycle

`withConn` (`client.go:162`) holds the mutex, lazily connects, and
drops the connection on network errors so the next call reconnects
clean. `reconnect` preserves per-sync caches (mailbox list, label
map, dedup set) — only TCP-level state is cleared. Several methods
implement single-shot retry-after-reconnect for `enumerateMailbox`,
`fetchMailboxMessageIDs`, and `GetMessagesRawBatch` chunks.

`isNetworkError` (`client.go:517`) is string-matching on common Go
network errors. TODO(verify): consider replacing with `net.OpError` /
`os.IsTimeout` checks if false negatives appear.

## Authentication paths

- **Password** (`add-imap`) — `LOGIN` over a TLS / STARTTLS / plain
  connection. `Config.AuthMethod` is empty or `password`. Credentials
  loaded via `imap.LoadCredentials`.
- **XOAUTH2** (`add-o365`) — `Config.AuthMethod = "xoauth2"`,
  `password = ""`, and `WithTokenSource(fn)` supplies the bearer token.
  The token callback is invoked once per `connect()`; reconnects mint
  a fresh token. See `xoauth2Client.Next` for the auth-failure
  handling: returning an empty challenge response lets the server's
  diagnostic NO message surface to the user.

## When editing

- Don't add server-side mutations without checking the capability
  set: `UIDPLUS` for expunge-by-UID, `MOVE` for trash. Falling back
  silently on missing capabilities is worse than refusing the operation.
- Per-sync caches (`messageListCache`, `msgIDToLabels`,
  `seenRFC822IDs`) live on the `*Client` for the duration of the sync
  and must not leak between accounts. Clients are constructed fresh
  per sync run in `cmd/.../syncfull.go`.
- Keep `BODY.PEEK[]` (not `BODY[]`): mark-as-read on every sync would
  surprise users.
