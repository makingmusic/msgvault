# internal/fbmessenger

## Purpose

Imports Facebook Messenger conversations from a "Download Your Information" (DYI) export. Squeezes a chat-shaped data model (no email addresses, just display names) into msgvault's email-shaped schema by synthesizing `<slug>@facebook.messenger` addresses. Supports three on-disk shapes: per-thread JSON folders (the canonical DYI format), per-thread HTML folders (legacy / "Download Information" alt format), and the post-2024 E2EE flat-export single-JSON-per-thread format.

## Files

- `discover.go` — walks DYI roots, classifies thread directories, probes E2EE shapes
- `json_parser.go` — `ParseJSONThread` for per-thread `message_<N>.json` files
- `html_parser.go` — `ParseHTMLThread` for per-thread `message_<N>.html`
- `e2ee_parser.go` — `ParseE2EEJSONFile` for single-file E2EE threads
- `slug.go` — display-name → slug → synthetic `mime.Address` conversion
- `types.go` — `Thread`, `Message`, `Participant`, `Attachment`, `Reaction`, errors
- `importer.go` — `ImportDYI` orchestrator: discover → parse → write to store
- `testdata/` — fixtures: `json_simple`, `json_multifile`, `json_group`, `json_with_media`, `json_nontext`, `html_*`, `e2ee_simple`, `mixed`, `corrupt`

## Input format

DYI exports have shifted over time. `messagesRootCandidates` (`discover.go:42`) tries:

1. `<root>/your_activity_across_facebook/messages/` (post-2024)
2. `<root>/your_facebook_activity/messages/` (2025+)
3. `<root>/messages/` (pre-2024)

Inside that, `<section>/<thread_dir>/message_<N>.json` (or `.html`). Sections: `inbox`, `archived_threads`, `filtered_threads`, `message_requests`, `e2ee_cutover`. E2EE flat exports drop section dirs entirely and put `<Name>_<N>.json` files at the messages root.

## Identity model (the unusual part)

Messenger has no email addresses. `slug.go:54` synthesizes one per display name:

- Slug: NFKD-fold + strip combining marks + collapse non-alphanum to `.` + lowercase + trim dots.
- Address: `<slug>@facebook.messenger`. If slug is empty, falls back to `user.<8-hex-sha1(name)>@facebook.messenger` so every participant has a usable local-part.
- Two display names that produce identical slugs collide and merge into the same participant — a deliberate tradeoff documented in the import command help. The user passes `--me <slug>@facebook.messenger` to identify themselves.

## Threading

- One DB conversation per `<section>/<thread_dir>` (qualified so `inbox/foo` and `archived_threads/foo` don't collide; `importer.go:470`).
- `ConvType = "direct_chat"` if ≤2 participants else `"group_chat"`.
- `source_message_id = <section>/<thread_dir>__<prefix><index>` where prefix is `""` for JSON, `"html_"` when `--format both` imports both copies into one conversation.

## Mojibake handling

Facebook DYI JSON encodes UTF-8 bytes as Latin-1 code points (`café` → JSON `cafÃ©`). `DecodeMojibake` (`slug.go:74`) reverses this by re-interpreting Latin-1 code points as raw bytes; if the result isn't valid UTF-8 the original is returned. Applied to titles, sender names, content, share text, and reactor names.

## Output

`ImportDYI` writes directly via `internal/store` — does not use `internal/importer.IngestRawMessage` because messages aren't MIME. Per message:

- `messages` row with `MessageType = "fbmessenger"`, sender resolved from synthetic address.
- `message_bodies.body_text` — raw text or rendered placeholder (`[photo]`, `[sticker]`, `[call: 1m 23s]`, `[shared link] ...`).
- `message_raw` only on the *first* message per thread to avoid bloat (`importer.go:637`); format tag `fbmessenger_<json|html|e2ee_json>` and bytes are the concatenated JSON files (or HTML).
- `attachments` rows for photos/videos/audio/files/gifs/stickers; binaries copied to `<attachmentsDir>/<sha256[:2]>/<sha256>` via `handleAttachment` (`importer.go:801`), which rejects symlinks and non-regular files (a malicious DYI could plant a symlink to `~/.ssh/id_rsa`).
- `reactions` rows + the body has `\n\n[reacted: emoji (Actor), ...]` appended.
- Labels: parent `Messenger` plus per-section (`Messenger / Inbox`, `... / Archived`, etc.).

## Edge cases

- **`bad_siblings`**: files matching `message_*.json` but not the strict `^message_(\d+)\.json$` regex are skipped without aborting the thread; counted in `summary.FilesSkipped` (`json_parser.go:81-104`).
- **E2EE shape probe**: `discover.go:196` streams JSON tokens to detect threads (object with both `participants` and `messages`) without decoding the whole file. Files matching neither key are silently skipped; files with one of the two go to the parser to raise `ErrCorruptJSON`.
- **Sender re-resolution on re-import**: if a re-import can't resolve the sender (e.g. the user changed display name), the importer reads the prior `sender_id` from the DB rather than nullifying it (`importer.go:562-603`). It also rehydrates the display name from `message_recipients` for self-authored rows (the `--me` participant has empty display_name).
- **Path-escape rejection**: `resolveAttachmentURI` (`json_parser.go:358`) refuses URIs that resolve outside the export root; missing-but-inside-root paths are still recorded so the user has a trace.
- **`isKnownMetadataFile`** (`discover.go:245`) hard-codes the names of DYI metadata JSON files (settings, autofill, etc.) so they don't enter the thread index.

## Gotchas

- The slug collision behavior is a known fidelity loss; no "merge with warning" is logged at parse time, only at the schema level (same participant ID).
- HTML exports have no timezone information; timestamps are stored as UTC (decision D6, `html_parser.go:21`).
- JSON timestamps are millisecond-precision via `time.UnixMilli`.
- A thread with both JSON and HTML defaults to JSON (higher fidelity) under `--format auto`; pass `--format both` to import both into a single conversation with `html_`-prefixed source_message_id values.
- The thread checkpoint is at thread granularity; mid-thread interrupts re-process the whole thread (idempotent via `source_message_id` upsert).
