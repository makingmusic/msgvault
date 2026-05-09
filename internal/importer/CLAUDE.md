# internal/importer

## Purpose

Shared ingest core for every email-shaped importer (MBOX, .emlx, PST). Provides `IngestRawMessage` — the single function that turns raw RFC 5322 bytes into `messages`, `message_bodies`, `message_raw`, `participants`, `message_recipients`, `attachments`, `message_labels`, and FTS rows. Per-format wrappers (`ImportMbox`, `ImportEmlxDir`, `ImportPst`) handle discovery, batching, dedup-existence checks, resume checkpointing, and source-specific raw bytes; the actual schema writes are uniform.

Chat-shaped importers (iMessage, WhatsApp, Messenger, Google Voice) bypass this core and call `internal/store` directly.

## Entry points

- `IngestRawMessage(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)` — `ingest.go:30`
- `ImportMbox(ctx, st, mboxPath, MboxImportOptions)` — `mbox_import.go:83`
- `ImportEmlxDir(ctx, st, rootDir, EmlxImportOptions)` — `emlx_import.go:86`
- `ImportPst(ctx, st, pstPath, PstImportOptions)` — `pst_import.go:91`

Subpackage `internal/importer/mboxzip` is invoked from `cmd/msgvault/cmd/import_mbox.go` to unpack `.zip` exports before handing each `.mbox` to `ImportMbox`.

## Output path (where data goes)

`IngestRawMessage` calls into `internal/mime.Parse` for the MIME tree, then `store.EnsureParticipantsBatch`, `store.EnsureConversation`, `store.PersistMessage` (atomic transaction over messages + body + raw + recipients + labels), `store.UpsertAttachment` (best-effort, post-transaction), and `store.UpsertFTS`. Attachment files are written via `internal/export.StoreAttachmentFile` (content-addressed `<dir>/<sha256[:2]>/<sha256>`).

## Key invariants

- **Sender identity**: `senderID` resolved from `parsed.From[0].Email` against `participantMap`. `IsFromMe = strings.EqualFold(parsed.From[0].Email, identifier)`.
- **Threading**: `threadKey` = first `References` → `In-Reply-To` → `Message-ID` → `rawHash` (`ingest.go:220`).
- **Dedup key (`source_message_id`)** is set per-format:
  - MBOX: `mbox-<sha256(raw)>-<seq>` (`mbox_import.go:456`) — the per-mailbox sequence number lets the same raw bytes appearing twice in a file dedupe to two distinct rows.
  - EMLX: `emlx-<sha256(raw)>` (`emlx_import.go:470`) — pure content hash so the same message in multiple mailboxes merges and accumulates labels (one label per mailbox).
  - PST: `pst-<entry.EntryID>` (`pst_import.go:506`) — uses MAPI node identifier rather than rawHash because `BuildRFC5322` produces non-deterministic multipart boundaries.
- **MIME parse failure** is recovered: `subject="(MIME parse error)"`, body explains the failure, raw bytes are still persisted (`ingest.go:38-47`).
- **Attachment failures** are logged but never abort ingest; once attachments are stored the actual count is back-corrected via `UPDATE messages SET attachment_count = ?` (`ingest.go:175-194`).

## Resume / checkpoint format

All three formats persist a JSON blob in `sync_runs.cursor_before` and update via `store.UpdateSyncCheckpoint`:

- MBOX: `{file, offset, seq}` — byte offset into file plus per-message sequence so resume keeps `source_message_id` stable.
- EMLX: `{root_dir, mailbox_index, mailbox_path, last_file}` — within-mailbox progress is filename string compare (`filePath <= startAfter`).
- PST: `{file, folder_index, folder_path, msg_index}` — folder-path validation guards against PST reordering between runs.

A resume against a different file/dir refuses to start unless `--no-resume` is passed.

## Edge cases / gotchas

- **`checkpointBlocked`** is set when an ingest error occurs mid-batch; it freezes the checkpoint cursor so a hard error doesn't advance past unimported messages. The PST flush resets it after each batch (`pst_import.go:403`); MBOX/EMLX leave it sticky for the run.
- **Existence check is two-track**: `MessageExistsWithRawBatch` (full message) versus `MessageExistsBatch` (any row); a message present without raw counts as `MessagesUpdated`, not `MessagesAdded`.
- **EMLX intra-batch dedup**: `pendingIdx` map merges duplicate content from multiple mailboxes into a single pending entry whose `LabelIDs` is the union (`emlx_import.go:477-494`). Without this, a second mailbox would skip the message and never apply its label.
- **Address sanitization** is in-place on `parsed.From/To/Cc/Bcc` so `participantMap` keys, `senderID` lookup, and `buildRecipientSet` all see identical UTF-8-clean strings (`ingest.go:55-63`).
- **PST attachment cap** uses a `limitWriter` so a corrupted PST reporting `size==0` cannot exhaust memory (`pst/reader.go:228-235`).
- **Batch sizes**: 200 messages or 32 MiB pending bytes triggers a flush across all three importers.
