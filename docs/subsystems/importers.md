# Importers Subsystem

This document describes how msgvault ingests data from heterogeneous source formats into a unified schema. There are ten importer-related packages and seven user-facing CLI commands. They split cleanly into two groups:

- **Email-shaped importers** (MBOX, .emlx, PST) share `internal/importer.IngestRawMessage` and operate on raw RFC 5322 bytes.
- **Chat-shaped importers** (iMessage, WhatsApp, Messenger, Google Voice) bypass the MIME core and write directly via `internal/store` because their input has no email envelope.

The shared schema is described in `internal/store/schema.sql` (and CLAUDE.md at the repo root). The interesting surface is how each format gets squeezed into it.

---

## 1. The importer pattern

### 1.1 What's shared (`internal/importer`)

`IngestRawMessage(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)` (`internal/importer/ingest.go:30`) is the single function that persists one parsed email. It:

1. Calls `internal/mime.Parse(raw)`; on parse failure synthesizes a placeholder body so the raw bytes are still preserved.
2. Sanitizes UTF-8 in From/To/Cc/Bcc address fields *in place* so participant keys, sender lookup, and recipient sets all see identical strings.
3. Calls `store.EnsureParticipantsBatch` for every address, then resolves the sender from `parsed.From[0].Email`.
4. Computes `is_from_me = strings.EqualFold(parsed.From[0].Email, identifier)`.
5. Picks a thread key: first `References` → `In-Reply-To` → `Message-ID` → `rawHash` (`ingest.go:220`).
6. Calls `store.PersistMessage` (atomic transaction over messages + body + raw + recipients + labels).
7. Writes attachments outside the transaction (best-effort; failures logged, never fatal).
8. Indexes FTS via `store.UpsertFTS`.

The three email-format wrappers (`ImportMbox`, `ImportEmlxDir`, `ImportPst`) all share the same pattern around this:

- Discover what to import (one file? one tree of files? a folder hierarchy?).
- Resume from a JSON checkpoint stored in `sync_runs.cursor_before` (validated against the input path).
- Batch up to 200 messages or 32 MiB pending bytes.
- Run a two-track existence check (`MessageExistsWithRawBatch` for full presence, `MessageExistsBatch` for "any row") to classify each pending item as skip / update / add.
- Call `IngestRawMessage` for each new message.
- Flush, checkpoint, repeat.
- On hard ingest error: set `checkpointBlocked = true` so progress doesn't advance past failed messages.

### 1.2 What's per-format

Each format owns:

- **Discovery**: how to enumerate input units (one per format).
- **Raw bytes**: for MBOX and EMLX the raw is what's on disk; for PST the bytes are *synthesized* from MAPI properties (see §2.3).
- **`source_message_id` formula**: the dedup key. Choosing this badly is how you accidentally re-import the same message a hundred times.
- **Fallback date**: `From ` line for MBOX, plist `date-sent` for EMLX, `ClientSubmitTime`/`MessageDeliveryTime`/`CreationTime` for PST.
- **Label model**: nothing (MBOX), one per mailbox dir (EMLX), one per folder path (PST).

Chat formats own their entire pipeline — discovery, parsing, identity model, threading, store writes — without touching `IngestRawMessage` at all.

---

## 2. Email-shaped importers

### 2.1 MBOX (`internal/mbox` + `internal/importer/mbox_import.go`)

**Input shape**: a single Unix mbox-family file (or a `.zip` containing one or more, unpacked by `internal/importer/mboxzip` from the CLI). Messages separated by `From ` lines with ctime-style dates. Body lines starting `From ` are escaped as `>From ` (mboxo) or `>+From ` (mboxrd).

**Parsing**: `internal/mbox.NewReader(io.Reader)` returns a streaming `Reader` that yields one `Message{FromLine, Raw}` at a time. It handles seekable underlying readers (so resume offsets work) and unescapes mboxrd lines by default. Separator detection requires both the `From ` prefix *and* a parseable date (`reader.go:223`) to avoid misclassifying body lines that look like separators.

**Normalization**: `mbox.ParseFromSeparatorDateStrict(fromLine)` produces a UTC `time.Time` used as `fallbackDate` when MIME headers lack `Date:`. Strict mode allowlists known TZ abbreviations to avoid Go's permissive default of mapping unknowns to UTC.

**Storage**: `IngestRawMessage` with `sourceMsgID = "mbox-<sha256(raw)>-<seq>"`. The `seq` is per-mailbox and persists in the checkpoint; identical raw bytes appearing twice in the same file dedupe to two distinct rows because the seqs differ. This is intentional — duplicate messages in mbox exports are rare but real (e.g. a message to multiple folders concatenated into one mbox).

**Checkpoint**: `{file, offset, seq}` JSON in `sync_runs.cursor_before`. Resume seeks to `offset` and validates that the file is the same (by absolute path or `os.SameFile` on inode).

### 2.2 .emlx (`internal/emlx` + `internal/importer/emlx_import.go`)

**Input shape**: Apple Mail's per-message format. One file per message, each with a decimal byte count on line 1, the raw MIME bytes, and an optional XML plist trailer.

**Parsing**: `emlx.Parse(data)` returns `Message{Raw, PlistDate, Flags, OrigMailbox}`. Plist failures are silent — the raw is what matters.

**Discovery**: `emlx.DiscoverMailboxes(rootDir)` walks the V10 directory layout (legacy `<mbox>/Messages/`, modern `<mbox>/<UUID>/Data/Messages/`, partition-only `<mbox>/<UUID>/Data/{0..9}/.../Messages/`) and returns one `Mailbox` per directory containing `.emlx` files. Filenames within each mailbox are sorted lexicographically; `.partial.emlx` is excluded.

**Normalization**: `IngestRawMessage` with `fallbackDate = msg.PlistDate` (Apple's 2001-01-01 epoch shifted to Unix). The mailbox's path becomes the label name via `LabelFromPath` (strips `Mailboxes/`, `IMAP-*/`, `POP-*/`, account UUIDs, `.mbox`/`.imapmbox` suffixes).

**Storage**: `IngestRawMessage` with `sourceMsgID = "emlx-<sha256(raw)>"` — pure content hash. This is the deliberate divergence from MBOX: the same message appearing in two mailboxes (e.g. INBOX and All Mail) produces the same source_message_id, which lets the importer attach *both* mailbox labels to one row. The intra-batch `pendingIdx` map (`emlx_import.go:477-494`) handles this in-memory before flush; cross-batch hits are handled by `MessageExistsWithRawBatch` returning the existing message ID and the importer calling `AddMessageLabels`.

**Checkpoint**: `{root_dir, mailbox_index, mailbox_path, last_file}` JSON. Mid-mailbox resume skips files lexicographically `<= last_file`.

**Auto-discovery**: the CLI (`cmd/msgvault/cmd/import_emlx.go`) calls `internal/applemail.DiscoverV10Accounts` first, which reads `~/Library/Accounts/Accounts4.sqlite` to map V10 UUID directories to email addresses.

### 2.3 PST (`internal/pst` + `internal/importer/pst_import.go`)

**Input shape**: Microsoft Outlook's monolithic binary format. Built on `github.com/mooijtech/go-pst/v6`. A single `.pst` file holds a folder tree, message bodies, attachments, MAPI property streams.

**Parsing**: `pst.Open(path)` wraps the go-pst File; `WalkFolders` returns one entry per folder with a slash-joined path (`Personal Folders/Inbox/Archive`); `ExtractMessage(msg, folderPath)` reads MAPI properties; `ReadAttachments(msg, maxBytes)` streams binaries through a bounded `limitWriter` that defends against PSTs reporting `size==0` for huge attachments.

**Normalization is the crux**: PST stores messages as MAPI property bags, not RFC 5322 bytes. `pst/mime.go:BuildRFC5322` synthesizes valid MIME:

1. If `TransportMessageHeaders` is non-empty, use those headers verbatim (stripping any pre-existing MIME content headers).
2. Otherwise emit From/To/Cc/Bcc/Date/Subject/Message-Id/In-Reply-To/References from MAPI fields, plus markers `X-Msgvault-Source: pst` and `X-Msgvault-Synthesized: true`.
3. Body: text + html → `multipart/alternative`; html only → quoted-printable html; text only or empty → quoted-printable text.
4. Attachments wrap the body in `multipart/mixed`; `Content-Id`-bearing attachments get `inline` disposition.

Exchange Distinguished Names (`/O=...`) for senders are resolved via `GetSmtpAddress()` first, falling back to the last `CN=` component. Dates are converted from Windows FILETIME (100-ns ticks since 1601-01-01).

**Storage**: `IngestRawMessage` with `sourceMsgID = "pst-<entry.EntryID>"`. Note this is *not* `sha256(raw)` — `BuildRFC5322` uses random multipart boundaries from `multipart.NewWriter`, so two runs over the same PST produce different bytes for the same logical message. The MAPI EntryID is stable across runs.

**Checkpoint**: `{file, folder_index, folder_path, msg_index}` JSON. Folder-path validation guards against folder reordering between runs.

---

## 3. Chat-shaped importers

These don't fit the email model. There are no SMTP envelopes, no Message-IDs, often no email addresses. The strategy in every case is to synthesize whatever the email schema requires from whatever the source provides.

### 3.1 iMessage (`internal/imessage`)

**Input shape**: macOS's `~/Library/Messages/chat.db` SQLite database. Read-only open; requires Full Disk Access on macOS. Schema includes `message`, `handle`, `chat`, `chat_message_join`, `chat_handle_join`.

**Parsing**: `Client.fetchPage` paginates by `ROWID` with date filters. `extractAttributedBodyText` decodes the `attributedBody` BLOB, which is one of two Apple serialization formats:

- **NSArchiver streamtyped** (modern; macOS Ventura+ stopped populating the plain `text` column for most messages): byte-level scan for the `\x84\x01+` NSString marker, decode a single- or multi-byte length prefix, skip framing bytes, extract UTF-8.
- **NSKeyedArchiver bplist**: `howett.net/plist` decode, follow `$top.root` UID to the `NS.string` value.

**Identity**: handles are phones (E.164-normalized via `internal/textimport.NormalizePhone`) or emails. The owner is identified by `--me <phone-or-email>`; without that flag, a synthetic `me@imessage.local` participant with display name "Me" is created so outbound messages don't show as "Unknown".

**Threading**: one conversation per `chat.guid`. `";+;"` in the GUID means group chat, `";-;"` means direct. Group titles synthesized from participant names + "+N more" when `chat.display_name` is empty.

**Storage**: direct store calls. `MessageType` is `imessage` or `sms` based on the `service` column; `source_message_id` is the chat.db `ROWID` as a string; `message_raw` is a JSON envelope of the row plus extracted body.

**Attachments are not yet extracted**. `has_attachments` is set but no `attachments` rows are written; the binaries in `~/Library/Messages/Attachments/` aren't read. TODO(verify): documented gap.

**Timestamps**: Apple epoch (2001-01-01). `detectTimestampFormat` queries `MAX(date)`; values > 1e12 are nanoseconds (Sierra+), smaller are seconds.

### 3.2 WhatsApp (`internal/whatsapp`)

**Input shape**: a *decrypted* `msgstore.db` SQLite backup. User decrypts with an external tool; the importer takes the path to the resulting `.db`.

**Parsing**: `Importer.Import` opens read-only, runs schema-version probes via `hasColumn` (cached `PRAGMA table_info`), iterates chats via `fetchChats`, then per-chat runs paged `fetchMessages` plus batch `fetchMedia`/`fetchReactions`/`fetchQuotedMessages`.

**Identity** is the surprise: WhatsApp uses JIDs (`<user>@<server>`). `normalizePhone(user, server)` (`mapping.go:134`) accepts purely-numeric users of length 4–15 and prepends `+`. Non-phone JIDs (`lid:`, `broadcast`, `status`) are skipped. Post-2024 WhatsApp uses opaque "lid" JIDs in the message table that don't directly contain phone numbers; `fetchLidMap` builds a `lid_row_id → phone JID` translation table from the `jid_map` table, and `resolveLidSender` translates *before* calling `normalizePhone`. This is a hard requirement because lid user strings can be 15 digits and pass E.164 validation despite being garbage.

`--phone <E.164>` is required and becomes both the source identifier and the owner participant.

**Threading**: one conversation per `chat.raw_string` (the JID). Group detection: `group_type > 0 OR server == "g.us"` — Communities use `group_type = 0` despite being groups, so the server check is required (`mapping.go:17`). Reply threading uses an in-memory `keyIDToMsgID` cache cleared per chat (bounded memory) plus a SQL fallback for cross-chat or cross-run replies.

**Storage**: direct store calls. `MessageType = "whatsapp"`, `source_message_id = key_id` (WhatsApp's globally unique message ID), `message_raw` is the JSON-marshaled internal row. Media is copied to `<attachmentsDir>/<sha256[:2]>/<sha256>` only when `--media-dir` is provided; without that flag the importer clears `has_attachments` rather than leaving phantom rows. Path-traversal defense in `handleMediaFile` rejects absolute paths, `..`, and paths that escape `mediaDir` (with a base-filename fallback).

**Skipped types**: 7 (system), 9 (location), 10 (contact card), 11 (status), 15/64/66 (calls), 99 (poll). Calls and polls would need their own data model.

### 3.3 Facebook Messenger DYI (`internal/fbmessenger`)

**Input shape**: a "Download Your Information" export from Facebook. Three on-disk variants:

- **Per-thread JSON**: `<root>/<messages-root>/<section>/<thread-dir>/message_<N>.json` (numbered `1, 2, ...`). Multi-file threads concatenate.
- **Per-thread HTML**: same path but `.html`. Lower fidelity (no millisecond timestamps, no reactions).
- **E2EE flat**: `<root>/<messages-root>/<Name>_<N>.json` — single file per thread, no section dirs.

`messagesRootCandidates` (`discover.go:42`) tries `your_activity_across_facebook/messages/`, `your_facebook_activity/messages/`, and bare `messages/` to handle 2024/2025 layout shifts. Sections: `inbox`, `archived_threads`, `filtered_threads`, `message_requests`, `e2ee_cutover`.

**Parsing**:

- `ParseJSONThread` reads every numbered `message_<N>.json`, concatenates `messages` arrays, sorts by `timestamp_ms`, dedupes participants. Unrecognized siblings (`message_final.json`) are skipped and recorded in `BadSiblings`.
- `ParseHTMLThread` walks `golang.org/x/net/html` trees, treats UTC for all timestamps (HTML exports have no TZ).
- `ParseE2EEJSONFile` decodes a single `Name_N.json` after a streaming-token shape probe (`probeE2EEShape`, `discover.go:196`) classifies it as thread / non-thread / unknown without reading the whole file.

`DecodeMojibake` (`slug.go:74`) reverses Facebook's well-known Latin-1-over-UTF-8 encoding bug (`café` written as `cafÃ©`). Applied to titles, sender names, content, share text, reactor names.

**Identity** is the most contorted of all the importers: there are no email addresses, no stable IDs — only display names. `Slug(name)` (`slug.go:24`) NFKD-folds the name, strips combining marks, collapses non-alphanum to `.`, and lowercases. `Address(name)` returns `mime.Address{Name, Email: <slug>@facebook.messenger}`. Empty slugs fall back to `user.<8-hex-sha1(name)>`. Two display names that produce identical slugs *merge* into one participant — a deliberate fidelity tradeoff documented in the `--me` flag help. The user must pass `--me <slug>@facebook.messenger`.

**Threading**: one conversation per `<section>/<thread-dir>` (qualified to prevent inbox/archived collisions). `direct_chat` if ≤2 participants else `group_chat`. `source_message_id = <section>/<thread-dir>__<prefix><index>` where prefix is `""` for JSON or `"html_"` when `--format both` imports both into one conversation.

**Storage**: direct store calls. `MessageType = "fbmessenger"`. Body is rendered text or a placeholder (`[photo]`, `[sticker]`, `[shared link] ...`, `[call: 1m 23s]`). Reactions get appended to the body as `\n\n[reacted: emoji (Actor), ...]` *and* written as first-class `reactions` rows. `message_raw` is stored *only on the first message* per thread (format `fbmessenger_<json|html|e2ee_json>`) to avoid duplicating the entire thread file thousands of times.

**Attachments**: photos, videos, audio, files, gifs, stickers. Resolved via `resolveAttachmentURI` which guards against path-escape and copies into content-addressed storage. `handleAttachment` rejects symlinks and non-regular files (a malicious DYI export could plant a symlink to `~/.ssh/id_rsa`).

**Sender re-resolution**: on re-import, if the current run can't resolve a sender (e.g. user changed display name), the importer reads the prior `sender_id` from the DB rather than nullifying it (`importer.go:562-603`). It also rehydrates the display name from `message_recipients` for self-authored rows (the `--me` participant has empty display_name).

### 3.4 Google Voice (`internal/gvoice`)

**Input shape**: Google Takeout's "Voice" folder containing `Phones.vcf` and `Calls/` with one HTML file per artifact. Files use a small filename grammar (parser.go:19-33): `<Name> - Text - <ts>.html`, `<Name> - Received|Placed|Missed|Voicemail - <ts>.html`, `Group Conversation - <ts>.html`, `<Name> - <ts>.html` (call without explicit type — parser reads `<title>` to classify).

**Parsing**: `parseVCF` extracts the user's Google Voice number and cell number from `itemN.TEL` + `itemN.X-ABLabel:Google Voice` pairs. `parseTextHTML` walks microformat classes (`.message`, `.tel`, `.fn`, `.dt`); each text-conversation HTML may contain dozens of messages and every one becomes a separate `indexEntry`. `parseCallHTML` parses the `.haudio` block for one call record per file.

**Identity**: phone numbers (E.164 via `internal/textimport.NormalizePhone`). Owner identified by `Phones.vcf` (Google Voice number is the source identifier; cell number is also tracked because it appears in `tel:` hrefs as the owner's "from"). `Me` recognized from `<span class="fn">Me</span>` flips `IsMe`.

**Threading**: 1:1 SMS → `<other-party-E.164>`; group SMS → `"group:" + sorted(phones).join(",")`; calls → `"calls:" + <contact-phone>` so calls thread separately from texts with the same person.

**Storage**: direct store calls. `MessageType` ∈ `{google_voice_text, google_voice_call, google_voice_voicemail}`. `source_message_id = computeMessageID(senderPhone, RFC3339Nano(ts), body[:50])` — sha256 prefix as 16-char hex. `message_raw` is the entire source HTML (format `gvoice_html`); the same file is referenced by every message extracted from it. Body for calls is synthesized like `"Received call from Alice (1m 23s)"`.

**Caching**: `lastFilePath` is a one-entry LRU (`client.go:36`); since the index is timestamp-sorted, consecutive messages from the same file are common and re-parsing is avoided.

**Attachments are NOT copied** — the parser records `<a class="video">` and `<img>` references but the binaries remain unimported. TODO(verify): the storage_path stays empty.

---

## 4. Identity model summary

| Format | Owner identifier | Other participants | Key |
|---|---|---|---|
| MBOX | from `--identifier <email>` | from MIME headers | email |
| EMLX | from `--account` or auto-discovered via `Accounts4.sqlite` | from MIME headers | email |
| PST | from `--identifier <email>` | from `TransportHeaders` (preferred) or MAPI display names | email; Exchange DN → SMTP fallback |
| iMessage | from `--me <handle>` or synthetic `me@imessage.local` | from `handle` table | E.164 phone or email |
| WhatsApp | from `--phone <E.164>` (required) | from `jid` table; `lid` JIDs translated via `jid_map` | E.164 phone |
| Messenger | from `--me <slug>@facebook.messenger` (required) | synthesized from display name slugs | `<slug>@facebook.messenger` synthetic email |
| Google Voice | parsed from `Phones.vcf` | from `tel:` hrefs and `<span class="fn">` | E.164 phone |

The Messenger slug-collision behavior is the only place where two distinct human beings can deliberately merge into one participant, and the only place where the local-part of an email isn't a real address.

---

## 5. Threading summary

| Format | Conversation key | Conversation type heuristic |
|---|---|---|
| MBOX/EMLX/PST | `References[0]` → `In-Reply-To` → `Message-ID` → `sha256(raw)` | not used (`null` MessageType) |
| iMessage | `chat.guid` (or `"no-chat-<rowid>"`) | `";+;"` in GUID → group |
| WhatsApp | `chat.raw_string` (JID) | `group_type > 0 OR server == "g.us"` |
| Messenger | `<section>/<thread-dir>` | ≤2 participants → direct |
| Google Voice | `<other-phone>` / `"group:" + sorted(phones)` / `"calls:" + phone` | `Group Conversation -` filename → group |

---

## 6. Attachment handling

Three different patterns:

- **Email importers**: `internal/mime.Parse` extracts attachments from MIME parts; `internal/export.StoreAttachmentFile` writes content-addressed `<dir>/<sha256[:2]>/<sha256>`. PST goes the long way around: go-pst yields raw attachment streams which `BuildRFC5322` re-embeds as base64 MIME parts before MIME parsing splits them out again. Wasteful but keeps the storage path uniform.
- **WhatsApp / Messenger**: separate extraction. Read the source file from a user-provided directory (`--media-dir` for WhatsApp, the DYI tree for Messenger), sha256-hash it, copy to the same content-addressed layout, write an `attachments` row. Both reject symlinks / path-escape attempts.
- **iMessage / Google Voice**: not extracted. iMessage sets `has_attachments` but writes no `attachments` rows; Google Voice records media references in the body but doesn't copy files.

The `<sha256[:2]>/<sha256>` layout is the single shared convention — any importer that copies binaries goes there.

---

## 7. Dedup interplay

`source_message_id` is the universal upsert key (one row per `(source_id, source_message_id)`). What it contains varies:

- MBOX: `mbox-<sha256(raw)>-<seq>` — content + position so duplicate raw bytes in one file produce two rows.
- EMLX: `emlx-<sha256(raw)>` — pure content; same message in two mailboxes = one row + two labels.
- PST: `pst-<entry.EntryID>` — MAPI node ID; `BuildRFC5322` is non-deterministic so content hash wouldn't be stable.
- iMessage: chat.db `ROWID` as a string — unique within a Messages installation.
- WhatsApp: `key_id` from the message table — globally unique per message in WhatsApp's namespace.
- Messenger: `<section>/<thread-dir>__<prefix><index>` — section-qualified to avoid inbox vs archived collisions; prefix `html_` for the second copy under `--format both`.
- Google Voice: 16-char sha256 prefix of `senderPhone | RFC3339Nano timestamp | body[:50]`.

Email importers additionally have content-hash dedup via `MessageExistsWithRawBatch`. Chat importers rely on the upsert alone.

---

## 8. Failure modes per format

- **MBOX**: bad `From ` separators in the middle of a body can be misclassified as new messages; mboxrd unescape handles most cases. Lines > 32 MiB are fatal (defense against missing newlines). Resume validates inode-equality; an mbox file replaced mid-import is rejected.
- **EMLX**: `.partial.emlx` (download-in-progress) is excluded. Plist failures are silent. Files exceeding `MaxMessageBytes` (128 MiB default) are skipped with a warning; the importer continues.
- **PST**: corrupted PSTs reporting `size==0` for huge attachments are bounded by `limitWriter`. Sub-folder read errors are silent. Calendar/contact items are skipped (ExtractMessage returns nil).
- **iMessage**: missing Full Disk Access produces a clear error at `db.Ping`. `attributedBody` decode failures return empty string (the message is still imported, just bodyless). No resume — re-running re-walks but upserts are idempotent.
- **WhatsApp**: empty `key_id` → message skipped (can't dedup). Schema-version drift handled by `hasColumn`. `lid` JID with no `jid_map` entry → message skipped. No resume.
- **Messenger**: corrupt JSON → whole thread skipped (`ThreadsSkipped++`). Hard ingest error per thread does not advance the checkpoint, so retry will rescan that thread. `--limit` mid-thread is preserved by *not* checkpointing past the partial thread. Bad sibling files counted but not fatal.
- **Google Voice**: file classification failure → file skipped. `parseVCF` failing to find the Google Voice number is fatal (we have no source identifier).

All formats: `ctx.Err()` aborts cleanly and saves a checkpoint where supported.

---

## 9. Test fixtures

- `internal/fbmessenger/testdata/` — `json_simple`, `json_multifile`, `json_group`, `json_with_media`, `json_with_media_alt`, `json_nontext`, `html_simple`, `html_multi_media`, `html_with_media`, `html_timestamps`, `e2ee_simple`, `mixed`, `corrupt`. Adding a new fixture: create a directory matching the DYI on-disk shape, then add a sub-test that calls `Discover` + `ParseJSONThread`/`ParseHTMLThread`/`ParseE2EEJSONFile`. Use synthetic names per `CLAUDE.md` rules — no real user data.
- `internal/pst/testdata/` — `support.pst`, `32-bit.pst`. These are real PSTs from go-pst's test corpus.
- `internal/emlx/`, `internal/mbox/` — small inline `[]byte` fixtures in `_test.go` files (no testdata dirs).
- `internal/imessage/`, `internal/whatsapp/`, `internal/gvoice/` — fixture databases / VCFs are constructed inline in tests.
- E2E tests at `cmd/msgvault/cmd/import_*_test.go` (e.g. `import_mbox_e2e_test.go`, `import_messenger_e2e_test.go`) drive the full CLI path against constructed fixtures and assert on the resulting DB state.

To add a new chat format: add a new `internal/<format>/` package with discovery, parsing, and an `Import...` function that calls into `internal/store` directly. Add a CLI in `cmd/msgvault/cmd/import_<format>.go` following `import_messenger.go` as the closest template. The shared MIME core is *not* the right starting point unless your input format is already RFC 5322.
