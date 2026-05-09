# internal/whatsapp

## Purpose

Imports messages from a *decrypted* WhatsApp Android `msgstore.db` SQLite backup. Like iMessage, the input is a SQLite DB rather than parseable files; this package opens it read-only, fetches chats and messages with batched JOINs, resolves senders via the JID system (including the post-2024 "lid" indirection), and writes directly to `internal/store`. Decryption is the user's responsibility — tools like `wa-crypt-tools` are typical.

## Files

- `types.go` — `waChat`, `waMessage`, `waMedia`, `waReaction`, `waGroupMember`, `waQuoted`, `waLidMapping`, `ImportOptions`, `ImportSummary`, `ImportProgress`
- `importer.go` — `Importer.Import` orchestrator (chats → messages → media/reactions/quotes)
- `mapping.go` — chat/message → store mapping, JID normalization, type/role enums
- `queries.go` — all the SQL: `fetchChats`, `fetchMessages`, `fetchMedia`, `fetchReactions`, `fetchGroupParticipants`, `fetchQuotedMessages`, `fetchLidMap`, `hasColumn` (PRAGMA cache)
- `contacts.go` — `ImportContacts` for vCard contact-name backfill (does not create participants)
- `mapping_test.go`, `queries_test.go`, `contacts_test.go`

## Input format

WhatsApp's `msgstore.db` schema (relevant subset, varies by version — guarded with `hasColumn`):

- `chat` — `_id`, `jid_row_id`, `subject`, `group_type`, `hidden`, `sort_timestamp`
- `jid` — `_id`, `raw_string`, `user`, `server` (`s.whatsapp.net`, `g.us`, `lid`, `broadcast`, etc.)
- `message` — `_id`, `chat_row_id`, `from_me`, `key_id`, `sender_jid_row_id`, `timestamp` (ms), `message_type`, `text_data`
- `message_media` — body for media messages (`mime_type`, `media_caption`, `file_path`, `file_size`, `width`, `height`, `media_duration`)
- `message_add_on` + `message_add_on_reaction` — reactions
- `message_quoted` — reply threading (`key_id` of the quoted message)
- `group_participants` — group memberships (TEXT JIDs, not row IDs!)
- `jid_map` — translates "lid" pseudo-JIDs to real phone JIDs

No public spec; reverse-engineered from open-source backup tools.

## Identity model

WhatsApp identifies users by JID: `<user>@<server>`. msgvault only stores phones, so:

- `normalizePhone(user, server)` (`mapping.go:134`) accepts purely-numeric `user` of length 4–15 and prepends `+` to produce E.164.
- Non-phone JIDs (`lid:`, `broadcast`, `status`) return empty string and are skipped.
- **lid resolution**: post-2024 WhatsApp uses opaque `lid` (linked-identifier) JIDs in the message table that don't directly contain phone numbers. `fetchLidMap` builds a `lid_row_id → phone JID` map from `jid_map`; `resolveLidSender` (`mapping.go:168`) translates before phone normalization. This is a hard requirement — `lid` user strings can be 15 digits and pass E.164 validation despite not being real phone numbers (`importer.go:277`).

`--phone <E.164>` is required and becomes both the source identifier and the owner participant.

## Threading

- One conversation per `chat.raw_string` (the JID, e.g. `447700900000@s.whatsapp.net` or `<groupid>@g.us`).
- `convType`: `group_chat` if `group_type > 0` *or* `server == "g.us"`. Communities use `group_type = 0` despite being groups, so the server check is required (`mapping.go:17`).
- Title: `chat.subject` if set, else empty (resolved by participant lookup at display time).
- Reply threading: `message_quoted.key_id` is mapped to a `messages.id` via the per-chat `keyIDToMsgID` cache; falls back to a SQL lookup against earlier imports if not in cache (`importer.go:421-426`).

## Key entry points

- `Importer{store, progress}` — `importer.go:21`; `NewImporter(store, progress)`.
- `(*Importer).Import(ctx, waDBPath, ImportOptions) (*ImportSummary, error)` — `importer.go:38`.
- `(*Importer).handleMediaFile(media, opts)` — `importer.go:530`; copies media to `<attachmentsDir>/<sha256[:2]>/<sha256>` with path-traversal checks.
- `ImportContacts(store, vcfPath) (matched, total, err)` — `contacts.go:23`; updates display names for *existing* participants only.

## Output

Direct `store` calls. Per message:

- `messages` row with `MessageType = "whatsapp"`, `source_message_id = key_id` (WhatsApp's globally unique message ID).
- `message_bodies.body_text` from `text_data`, with `media_caption` appended after `\n\n` if present.
- `message_raw` is the JSON-marshaled `waMessage` struct, format `whatsapp_json`.
- `message_recipients` — not explicitly set; sender_id and conversation participants carry the relationships.
- `attachments` — only when media was actually copied (storage_path or content_hash non-empty); without `--media-dir` the importer clears the `has_attachments` flag rather than writing phantom rows (`importer.go:399-409`).
- `reactions` — emoji + reactor + timestamp.
- Labels: not used.

## Skipped message types

`isSkippedType` (`mapping.go:107`) drops: 7 (system), 9 (location), 10 (contact card), 11 (status/story), 15 (call), 64 (missed call), 66 (group call), 99 (poll). Calls and polls would need their own data model.

## Edge cases

- **Empty `key_id`**: skipped — without a unique ID, upserts would collide (`importer.go:268`).
- **Schema version drift**: `hasColumn` (`queries.go:23`) caches `PRAGMA table_info` per `(*sql.DB, table)` so old DBs without `group_type` or `media_caption` fall back to `0` / `NULL`.
- **`group_participants.gjid/jid` are TEXT**, not row IDs — joined to `jid` via `raw_string` not `_id` (`queries.go:316`). Easy to get wrong.
- **Path-traversal defense for media**: `handleMediaFile` rejects absolute paths, `..`, and any computed path that escapes `mediaDir`; falls back to filename-only join if the relative path escapes (`importer.go:537-567`).
- **Streamed hashing**: `io.LimitReader` over `MaxMediaFileSize+1` so a single huge file can't OOM the importer.
- **Self-reactions**: when `sender_jid_row_id` is null (or the reactor is the device owner), reaction is attributed to `selfParticipantID` (`importer.go:461-464`).
- **Cross-chat reply lookup**: if a quoted message isn't in the in-memory cache (cleared per chat), a SQL fallback resolves it from a previous run (`importer.go:423`).

## Gotchas

- Resume is not implemented; the importer updates `sync_runs` counters but resuming a partial import re-walks from the start. Upserts make it idempotent.
- `keyIDToMsgID` is `clear()`'d per chat (`importer.go:127`) to bound memory; cross-chat replies (rare) won't thread on the first run unless the quoted message was imported first by chat order.
- `RecomputeConversationStats` runs at the end; deferred sync run completion is wrapped in a `defer` that calls `FailSync` on error or `CompleteSync` otherwise (`importer.go:80-86`).
- `ImportContacts` deliberately *cannot* create participants — so contact import only enriches the names of phones that have already exchanged messages.
