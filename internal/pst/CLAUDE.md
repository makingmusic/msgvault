# internal/pst

## Purpose

Reads Microsoft Outlook PST archives (32-bit and 64-bit ANSI/Unicode) and reconstructs RFC 5322 / MIME bytes that the rest of msgvault can consume via `IngestRawMessage`. Built on `github.com/mooijtech/go-pst/v6`. The actual import driver lives in `internal/importer.ImportPst`; this package handles per-message extraction and MIME synthesis.

## Files

- `reader.go` — opening PST files, walking folders, extracting MAPI message properties, reading attachments
- `mime.go` — converting a `MessageEntry + AttachmentEntry[]` into RFC 5322 bytes
- `mime_test.go`, `reader_test.go`
- `testdata/32-bit.pst`, `testdata/support.pst` — public test fixtures

## Input format

PST (Personal Storage Table) is Microsoft's binary format. Spec: https://learn.microsoft.com/en-us/openspecs/office_file_formats/ms-pst/.

go-pst exposes:

- folder tree (root → subfolders, with `MessageCount`)
- per-folder message iterator (each yields a `pstlib.Message`)
- MAPI properties (subject, body text/HTML, sender, recipients, dates, headers)
- attachment iterator with streaming `WriteTo`

`reader.go:18` registers extended charsets so go-pst can decode non-UTF-8 body parts.

## Key types and entry points

- `File` — wraps `pstlib.File` with `Open(path)` / `Close()` (`reader.go:39-62`).
- `FolderEntry{Name, Path, MsgCount}` — `reader.go:65`; `Path` is slash-joined.
- `MessageEntry` — extracted MAPI fields (`reader.go:71`); see `EntryID`, `TransportHeaders`, `BodyText`, `BodyHTML`, `SenderName/Email/AddressType`, `DisplayTo/Cc/Bcc`, `MessageID`, `InReplyTo`, `References`, `SentAt`, `ReceivedAt`, `CreationTime`.
- `AttachmentEntry{Filename, MIMEType, ContentID, Size, Content}` — `reader.go:113`.
- `(*File).WalkFolders(WalkFolderFunc) error` — `reader.go:129`; recursive, builds slash paths.
- `ExtractMessage(msg, folderPath) *MessageEntry` — `reader.go:172`; returns nil for non-email items (calendar, contact, task).
- `ReadAttachments(msg, maxBytes) ([]AttachmentEntry, error)` — `reader.go:240`; uses a `limitWriter` (`reader.go:223`) so a malicious PST reporting `size==0` cannot exhaust memory.
- `BuildRFC5322(msg, attachments) ([]byte, error)` — `mime.go:26`.

## Output

`internal/importer.ImportPst` walks folders, calls `ExtractMessage` + `ReadAttachments`, hands the result to `BuildRFC5322`, then forwards the synthesized bytes to `IngestRawMessage`. Folder paths become labels via `store.EnsureLabel`. Source dedup key is `pst-<entry.EntryID>` (the MAPI node identifier), not the rawHash, because MIME synthesis is non-deterministic (random multipart boundaries) — two runs over the same PST would otherwise duplicate everything.

## MIME synthesis strategy (`mime.go:26`)

1. If `TransportMessageHeaders` is non-empty (typical for SMTP-delivered mail), use those headers verbatim, stripping any pre-existing `MIME-Version`/`Content-Type`/`Content-Transfer-Encoding`.
2. Otherwise synthesize headers from MAPI properties: `From`, `To/Cc/Bcc` (display names only — see Gotchas), `Date`, `Subject`, `Message-Id`, `In-Reply-To`, `References`, plus `X-Msgvault-Source: pst` and `X-Msgvault-Synthesized: true` markers.
3. Body chosen by content presence:
   - text + html → `multipart/alternative`
   - html only → `text/html; quoted-printable`
   - text only or empty → `text/plain; quoted-printable`
4. Attachments wrap the body in `multipart/mixed`; inline parts (with `Content-Id`) get `Content-Disposition: inline`, others `attachment`.

## Edge cases

- **Exchange Distinguished Names** (`/O=...`): `senderEmail` is replaced with the SMTP address from `GetSmtpAddress()` if available, otherwise the last `CN=` component is extracted (`reader.go:188-195`, `307-322`).
- **Windows FILETIME** (100-ns ticks since 1601-01-01) is converted in `windowsFiletimeToTime` (`reader.go:27`); the constant `11644473600` is the epoch difference in seconds.
- **Sub-folder read failures** are silently swallowed (`reader.go:158-161`) — some PST variants can fail to read sub-folder metadata.
- **DisplayTo/Cc/Bcc are display names only** when synthesized — PST stores recipient names in the message but addresses live in a separate recipient table that go-pst does not expose. This is a fidelity loss; importers compensate by trusting `TransportMessageHeaders` whenever present.
- **Subject fallbacks**: `GetSubject()` → `GetNormalizedSubject()` → `GetInternetSubject()` (`reader.go:179-185`).

## Gotchas

- Calling `BuildRFC5322` twice on the same `MessageEntry` produces *different* bytes (multipart boundaries are randomized by `multipart.NewWriter`). This is why dedup uses `EntryID`, not `sha256(raw)`.
- Calendar items, contacts, tasks and notes appear in the message iterator and must be filtered out by `ExtractMessage` returning nil — `ImportPst` continues silently in that case.
- `extractCN` walks DN components from the back; the last `CN=` in the chain wins. PSTs with unusual DN structures may produce surprising names.
- Charset registration in `init()` modifies global state in `emersion/go-message/charset` — safe in this repo because no other package calls `RegisterEncoding`.
