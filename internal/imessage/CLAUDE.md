# internal/imessage

## Purpose

Imports macOS iMessage / SMS history by reading `~/Library/Messages/chat.db` directly. iMessage is unique among the importers in that the source is itself a SQLite database; this package opens it read-only, pages through `message` rows, and writes into msgvault's store. Requires Full Disk Access permission on macOS.

## Files

- `client.go` — `Client` opens chat.db, paginates messages, ensures conversations/participants
- `parser.go` — Apple-epoch timestamp conversion, handle classification, attributedBody decoding
- `models.go` — `messageRow` struct mirroring the chat.db schema, `ImportSummary`
- `parser_test.go`

## Input format

`chat.db` is a SQLite database with tables (relevant subset):

- `message` — one row per message; key columns: `ROWID`, `guid`, `text`, `attributedBody` (BLOB), `date`, `is_from_me`, `service` (`"iMessage"` or `"SMS"`), `cache_has_attachments`, `handle_id`.
- `handle` — phone numbers and email addresses (`handle.id`).
- `chat` — conversation; `chat.guid` is `"any;-;<contact>"` for 1:1 or `"any;+;<groupguid>"` for groups.
- `chat_message_join` and `chat_handle_join` — many-to-many bridge tables.

Apple format reference: https://github.com/ReagentX/imessage-exporter has good schema notes; no official spec.

## Identity model

iMessage handles are phone numbers (E.164-normalized via `textimport.NormalizePhone`) or email addresses. `resolveHandle` (`parser.go:46`) classifies each. The device owner is identified two ways:

- `--me <phone-or-email>` flag: that handle becomes the owner participant.
- No flag: a synthetic `me@imessage.local` participant with `display_name = "Me"` is created so outbound messages don't show as "Unknown" (`client.go:182-191`).

`is_from_me=1` rows have a NULL `handle_id` in chat.db; sender resolves to the owner participant.

## Threading

- One conversation per `chat.guid`; if a message has no chat row, a synthetic `"no-chat-<rowid>"` is used (`client.go:339`).
- `convType` derived from chat GUID format: `";+;"` → `group_chat`, otherwise `direct_chat` (`client.go:619`).
- Group chat title falls back to participant names + `"+N more"` if `chat.display_name` is empty (`buildGroupTitle`, `client.go:674`).

## Key types and entry points

- `Client` — `client.go:22`; opens `chat.db` with `mode=ro&_journal_mode=WAL`.
- `NewClient(dbPath, opts...)` — `client.go:63`; calls `detectTimestampFormat` to pick seconds vs nanoseconds.
- `(*Client).Import(ctx, store, sourceID) (*ImportSummary, error)` — `client.go:146`; paginates by `ROWID > lastROWID`.
- `(*Client).fetchPage(ctx, afterROWID)` — `client.go:257`; left-joins `handle`, `chat_message_join`, `chat`.
- `appleTimestampToTime(ts)` / `timeToAppleTimestamp(t, useNano)` — `parser.go:20-42`; epoch offset 978307200 (2001-01-01 UTC).
- `extractAttributedBodyText(data)` — `parser.go:71`; handles two serialization formats.

## attributedBody decoding (the surprising part)

macOS Ventura+ / iOS 16+ stopped populating the plain-text `text` column for most iMessages. The body lives only in `attributedBody`, which is one of:

- **NSArchiver "streamtyped"** (most common modern format): starts with `\x04\x0bstreamtyped`. Text is embedded as an NSString. `extractStreamtypedText` (`parser.go:100`) scans for the `\x84\x01+` marker, decodes a single-byte (`< 0x80`) or multi-byte (`0x81 <len> [framing]`) length prefix, then extracts UTF-8 bytes terminated by a control byte. The framing skip (`parser.go:135-143`) accepts nulls and high bytes (`0x80-0xBF`) until valid text starts.
- **NSKeyedArchiver bplist**: starts with `bplist`. Decoded via `howett.net/plist` to `archive.Top["root"]`, then `Objects[rootUID]["NS.string"]` UID dereference (`parser.go:184`).

## Output

Direct `store` calls — no MIME synthesis. Per message:

- `messages` row with `MessageType` ∈ `{imessage, sms}` based on the `service` column.
- `message_bodies.body_text` only (no HTML).
- `message_raw` is a JSON envelope of the chat.db row + extracted body, with format `imessage_json` (`client.go:564`).
- `message_recipients` — `from`/`to` based on `is_from_me`; for outbound messages `to` lists every chat member from `chat_handle_join`.
- Labels: `iMessage` or `SMS`.

## Edge cases

- **Timestamp format detection**: `detectTimestampFormat` queries `MAX(date)`; values > 1e12 are nanoseconds, smaller are seconds (`client.go:106`).
- **Attachments are NOT extracted** — the importer logs a debug line but does not read from `~/Library/Messages/Attachments/`; `has_attachments` is set but no `attachments` rows are written. TODO(verify): this is a known gap noted in the message body comment at `client.go:447-452`.
- **Handle resolution**: phone-number-shaped handles take precedence over email-shaped (`resolveHandle`, `parser.go:46`); short codes / system identifiers fall through to `displayName` only.
- **No resume**: pagination by `ROWID` is naturally idempotent — re-running starts from row 0 again, but `source_message_id = ROWID` upserts cleanly. Date filters narrow the work.

## Gotchas

- `extractAttributedBodyText` is a heuristic byte-level decoder, not a complete archiver. Edge cases (very long messages, embedded objects) may return truncated or empty strings — the test suite covers common shapes but not exhaustive.
- `useNanoseconds` is per-Client; mixing dbs from different macOS versions in one run isn't supported.
- The query in `linkChatParticipants` runs per conversation; the cache prevents quadratic blowup but a chat with thousands of members will still issue one query for that chat.
- `me@imessage.local` is a magic synthetic email; `buildGroupTitle` excludes it explicitly (`client.go:679-684`).
