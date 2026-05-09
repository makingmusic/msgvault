# Utilities and shared subsystems

This document covers the cross-cutting Go packages that don't belong
to a single feature: MIME parsing, encoding/text helpers, file
permissions, configuration, logging, deletion staging, self-update,
export, and the test-helper package. Each section focuses on the
public API and how the rest of the codebase consumes it.

For deeper invariants and editing notes, see the per-package
`CLAUDE.md` files referenced in each section.

---

## internal/mime — MIME message parsing

Wraps `github.com/jhillyerd/enmime` into a small `Message` type with
parsed addresses, body text/HTML, attachments, and a date normalized
to UTC.

### Public API

```go
mime.Parse(raw []byte) (*Message, error)
(*Message).GetBodyText() string
(*Message).GetFirstFrom() Address
mime.StripHTML(rawHTML string) string
```

`Message` fields: `Subject`, `Date`, `From/To/Cc/Bcc/ReplyTo []Address`,
`MessageID`, `InReplyTo`, `References []string`, `BodyText`, `BodyHTML`,
`Attachments []Attachment`, `Errors []string` (non-fatal parse
warnings).

`Attachment` carries `Filename`, `ContentType`, `ContentID`, `Size`,
`ContentHash` (SHA-256 hex), `Content []byte`, `IsInline`.

### enmime gotchas

- **Body parts vs attachments.** enmime's `Envelope.Attachments` and
  `Envelope.Inlines` include `text/plain`/`text/html` parts. The
  filter `isBodyPart` (`parse.go:134`) drops parts where the content
  type is `text/plain`/`text/html`, the part has no `FileName`, AND
  the disposition is not explicitly `attachment`. This matches the
  Python implementation and keeps the body out of attachments.
- **Address parsing.** Use `env.AddressList(header)` (via
  `parseAddressList`), not `env.GetHeader` + manual parsing. enmime
  handles RFC 2047 encoded-word, quoted display names, and group
  syntax that hand-rolled parsers miss.
- **Date headers.** `parseDate` (`parse.go:253`) tries 19 formats
  including ISO 8601, SQL-like, and the various RFC 822/1123 dialects
  with single- and double-digit days. Named timezones (EST, PST, MST)
  are intentionally treated as offset 0 because Go's `time.Parse`
  resolves them against the local system zone — platform-dependent
  and a known footgun in the Go time package.
- **Encoding.** enmime decodes `Content-Transfer-Encoding`
  (base64, quoted-printable) but only does limited charset
  conversion. Body text emerging from a misdeclared charset can
  still be invalid UTF-8 — this is what `internal/textutil` and the
  `repair-encoding` command are for.

### Repair-encoding flow

`cmd/msgvault/cmd/repair_encoding.go` (~717 lines) walks all
message-text columns (`subject`, `body_text`, `body_html`, `snippet`,
participant `display_name`/`email_address`/`domain`, conversation
`title`, label `name`, attachment `filename`). For each invalid
field:

1. Re-fetch the raw MIME from `message_raw` (zlib-compressed),
   `mime.Parse` it, and use the freshly parsed value if valid.
2. If still invalid, run `textutil.EnsureUTF8` (chardet → fallback
   encoding list → replacement chars).
3. Re-enqueue the message for re-embedding so vector search reflects
   the corrected text.

Used after a sync that produced invalid UTF-8 because the source
declared an incorrect charset. See `internal/mime/CLAUDE.md` and
`internal/textutil/CLAUDE.md`.

---

## internal/textutil — encoding helpers and terminal sanitization

Three responsibilities, one file (`encoding.go`):

### EnsureUTF8

```go
textutil.EnsureUTF8(s string) string
```

Best-effort conversion to valid UTF-8:

1. Fast path: `utf8.ValidString` → return as-is.
2. `chardet.NewTextDetector().DetectBest`. Confidence threshold 30 for
   short strings (≤50 bytes), 50 for longer ones — chardet is less
   reliable on short samples but we still try.
3. Fallback list, in order of likelihood for email content:
   `Windows1252` → `ISO8859_1` → `ISO8859_15` → `ShiftJIS` → `EUCJP`
   → `EUCKR` → `GBK` → `Big5`. First decoder that produces valid
   UTF-8 wins.
4. Last resort: `SanitizeUTF8` (replace invalid bytes with U+FFFD).

The order is deliberate: Windows-1252 is the most common silent
producer of invalid bytes in Western emails (smart quotes, em/en
dashes, trademark, bullet, Euro sign, all in 0x80-0x9F where Latin-1
has nothing).

`textutil.GetEncodingByName(name)` is a charset-name → encoding map
keyed by IANA names (case variants accepted).

### SanitizeUTF8

`SanitizeUTF8(s)` walks `s` rune by rune; bytes that don't decode
become U+FFFD. Used as the EnsureUTF8 last resort and directly in
places that must guarantee valid UTF-8 without trying to recover the
original text.

### Truncation helpers

```go
TruncateRunes(s, maxRunes) string  // UTF-8 safe, adds "..." if truncated
FirstLine(s) string                // First line, capped at 200 runes
```

`FirstLine` is used for error messages — enmime in particular includes
malformed MIME content in error strings, and you don't want a 100KB
body in a log line.

### SanitizeTerminal

```go
textutil.SanitizeTerminal(s string) string
```

Strips ANSI escape sequences and C0/C1 control characters before
printing untrusted text to a TTY. Used for any user-supplied string
that hits the TUI or stderr: WhatsApp chat names, message snippets,
progress output.

Critical detail: C1 control chars (U+0080-U+009F) are checked on the
**decoded rune**, not the raw leading byte. UTF-8-encoded CSI
(0xC2 0x9B) would otherwise sneak through. Replaces `\r` and `\n`
with space (single-line callers only).

---

## internal/fileutil — secure file ops

Cross-platform helpers for writing files that should not be readable
by other users on the system (OAuth tokens, attachments, configs).

### Public API

```go
fileutil.SecureWriteFile(path, data, perm)
fileutil.SecureMkdirAll(path, perm)
fileutil.SecureChmod(path, perm)
fileutil.SecureOpenFile(path, flag, perm) (*os.File, error)
```

Same signatures as the matching `os` functions.

### Unix vs Windows

- `secure_unix.go` (build tag `!windows`) — thin wrappers; the Unix
  permission bits (mode `0600`/`0700`) already enforce owner-only.
- `secure_windows.go` (build tag `windows`) — when `perm & 0077 == 0`
  the helper additionally builds and applies a DACL granting
  `GENERIC_ALL` to the current user only, with
  `PROTECTED_DACL_SECURITY_INFORMATION` to block inherited ACEs. For
  directories, `CONTAINER_INHERIT_ACE | OBJECT_INHERIT_ACE` so
  children inherit the restriction.

DACL failures are logged at WARN and **not propagated** — the file
already has the requested Unix mode, and DACL application can fail
on network filesystems where it's not the right tool anyway.

### Where this matters

- OAuth tokens (`internal/oauth`) — 0600. A token file readable by
  another local user is an account compromise.
- Attachment files (`internal/export`) — 0600. They're email
  attachments, often containing PII.
- Config file (`internal/config`) — 0600. Contains API keys,
  client secrets paths.
- Manifest files (`internal/deletion`) — 0600.
- Update cache (`internal/update`) — 0600.

The `0700` directory mode for `~/.msgvault/` is enforced by
`Config.EnsureHomeDir`, which calls `SecureMkdirAll`.

---

## internal/config — config.toml loading

Loads `~/.msgvault/config.toml` (or path from `--config`), computes
derived paths, and provides the project's temp-directory helper.

### Resolution order

1. `MSGVAULT_HOME` env var (with `~` expansion in the value), or
2. `--home` flag (same expansion), or
3. `~/.msgvault/`.

`Config.HomeDir` is set; `Data.DataDir` defaults to it. Subpaths:

- `AttachmentsDir() = <DataDir>/attachments`
- `TokensDir()      = <DataDir>/tokens`
- `AnalyticsDir()   = <DataDir>/analytics`
- `LogsDir()        = Log.Dir or <DataDir>/logs`
- `DatabaseDSN()    = Data.DatabaseURL or <DataDir>/msgvault.db`

When `--config` points to a file outside the home dir, `HomeDir` is
silently set to that file's parent so all derived paths follow.

### Atomic Save

`Config.Save()` (`config.go:444`):

1. Resolve symlinks so atomic rename replaces the symlink target,
   not the symlink itself.
2. `os.CreateTemp` next to the destination.
3. Chmod 0600, encode TOML, fsync, close.
4. `os.Rename` over the destination.

Any failure path removes the temp file via deferred cleanup.

### MkTempDir

```go
config.MkTempDir(pattern string, preferredDirs ...string) (string, error)
```

The standard temp-directory helper for the whole project. Tries (in
order):

1. Each `preferredDirs` value (skipping empty strings).
2. `os.TempDir()`.
3. `<MSGVAULT_HOME>/tmp/`.

Exists because Windows `%TEMP%` is regularly inaccessible due to
group policy or antivirus. Also applies 0700 + DACL to whatever
directory was created. Use this everywhere; never call
`os.MkdirTemp("", ...)` directly.

### ServerConfig safety

`ServerConfig.ValidateSecure()` refuses to start when bind address is
non-loopback, no API key is set, and `AllowInsecure` is false. Called
from `cmd/msgvault/cmd/serve.go` before binding.

---

## internal/logging — slog handler with file rotation

Single entry point: `BuildHandler(opts) (*Result, error)`. Returns a
fan-out `slog.Handler` that writes:

- **Stderr** as human-readable text (always on).
- **`<LogsDir>/msgvault-YYYY-MM-DD.log`** as JSON (opt-in via
  `Log.Enabled = true`, `Log.Dir = ...`, or the `--log-file` CLI
  flag).

Every record carries a process-wide `run_id` (6 random hex bytes) so
multiple concurrent runs sharing a log file can be told apart with
`jq 'select(.run_id == "abc123")'`.

### Rotation

When the daily file exceeds `MaxFileBytes` (default 50 MiB) at open
time, it's rotated to `.1`, existing `.1` to `.2`, etc., up to
`KeepRotated` (default 5). Files beyond the keep window are deleted.

### TUI integration

The TUI swaps `slog.Default()` to `Result.FileOnlyLogger()` while the
alternate screen is active. That logger discards records when file
logging is disabled, otherwise writes JSON to the daily file (still
with `run_id`). Stderr text would otherwise paint over the TUI.

### Logging conventions (PII rules)

Per the global CLAUDE.md, every call site is responsible for not
leaking sensitive data:

- **Never log user content or PII at INFO.** No body text, no
  attachment names, no email addresses except as small fields like
  `account=` for the user's own account.
- INFO is for: intent classification, counts, durations, error
  types, sync IDs.
- DEBUG is for full context. Operators turn this on with
  `--verbose` or `Log.Level = "debug"` when something is wrong.
- Don't compute for logging. If a stat is needed only for a log
  line, get it from the work the call already did.

This package doesn't enforce these rules — it can't see the
arguments. The rules are the code-review checklist.

---

## internal/deletion — staged Gmail deletion

Highest-stakes feature in the project: this is the only code that
actually deletes user data from Gmail. It's split into two steps —
**stage** (build a manifest) and **execute** (apply it) — with the
manifest as a durable, reviewable artifact between them.

### The staging-manifest pattern

A `Manifest` is a JSON file under `<DataDir>/deletions/<status>/`:

```
deletions/
  pending/      # staged, not yet executed
  in_progress/  # executor running (or interrupted)
  completed/    # finished; succeeded count + failed IDs preserved
  failed/       # all messages failed
  cancelled/    # cancelled before completion
```

Status is authoritative by **directory**, not by the inline `Status`
field. If a crash leaves them inconsistent, the directory wins and
the field self-heals on next save.

Filename pattern: `YYYYMMDD-HHMMSS-<sanitized-description>.json`. The
description sanitizer (`sanitizeForFilename`) keeps `[A-Za-z0-9-_]`,
maps space and `.` to `-`, drops everything else, capped at 20 chars.

### Manifest format

```json
{
  "version": 1,
  "id": "20240315-103200-monthly-cleanup",
  "created_at": "2024-03-15T10:32:00Z",
  "created_by": "tui",
  "description": "Monthly cleanup",
  "filters": {
    "senders": ["spam@bad.example"],
    "labels": ["Promotions"],
    "before": "2023-01-01"
  },
  "summary": {
    "message_count": 421,
    "total_size_bytes": 18372913,
    "date_range": ["2020-01-01", "2022-12-30"],
    "top_senders": [{"sender": "spam@bad.example", "count": 421}]
  },
  "gmail_ids": ["18f0abc...", "..."],
  "status": "pending",
  "execution": null
}
```

`Filters` is metadata only. The executor uses `gmail_ids` directly —
filters are not re-evaluated. This is intentional: the IDs are
captured at staging time so what the user reviews is exactly what
gets deleted.

### Trash vs Permanent

`Method` controls the action:

- `MethodTrash` → `gmail.API.TrashMessage`. Reversible for 30 days
  via Gmail UI/API. **The default.**
- `MethodDelete` → `gmail.API.DeleteMessage` (per-message) or
  `BatchDeleteMessages` (up to 1000 IDs at once). **Permanent.**

### Execute vs ExecuteBatch

Two execution modes (`executor.go`):

- `Execute(ctx, manifestID, opts)` — per-message. Loops through
  `GmailIDs`, calls `TrashMessage` or `DeleteMessage` one at a time,
  checkpoints every `BatchSize` messages (default 100).
- `ExecuteBatch(ctx, manifestID)` — uses Gmail's
  `users.messages.batchDelete` (up to 1000 IDs per call). Permanent
  deletion only. Falls back to per-message on a batch failure.
  Retries previously-failed IDs from the manifest's `FailedIDs`
  before resuming the main run.

### Idempotency and 404 handling

`isNotFoundError` (`executor.go:18`) treats Gmail 404 as success on
the assumption the message was already deleted — possibly by a
previous partial run. The local DB is still marked deleted via
`MarkMessageDeletedByGmailID` so the local view matches reality.

### Insufficient-scope detection

`isInsufficientScopeError` (`executor.go:23`) recognizes Google's
scope errors (substrings: `ACCESS_TOKEN_SCOPE_INSUFFICIENT`,
`insufficient authentication scopes`, `Insufficient Permission`).
When detected, the executor returns `resultFatal` — saves a
checkpoint and halts. The user must re-auth with a deletion-capable
scope before continuing.

### Resume

Both executors honor `Execution.LastProcessedIndex` and resume from
there when `ExecuteOptions.Resume` is true (default). `ExecuteBatch`
additionally retries the previous run's `FailedIDs` first; successes
move them out of the failure set, remaining failures form the new
`FailedIDs`.

### Cancellation order (the subtle part)

`Manager.CancelManifest` (`manifest.go:386`) renames the manifest
file **before** rewriting the inline status:

1. `os.Rename(pending/<id>.json, cancelled/<id>.json)` — atomic on
   same filesystem.
2. Reload from new path, set `Status = StatusCancelled`, save.

A crash between step 1 and step 2 leaves a manifest in `cancelled/`
with `Status: pending`. Acceptable — the directory is authoritative
and the field self-heals on next save. The reverse order would
leave a manifest in `pending/` with `Status: cancelled`, which
contradicts the dir and breaks list-deletions output.

### Safety mechanisms summary

1. Two-step staging: stage to `pending/`, executor moves to
   `in_progress/` only when explicitly executed.
2. Default method is trash (recoverable for 30 days).
3. 404 treated as success (resume safety).
4. Scope errors halt immediately; partial progress is preserved.
5. Per-message and per-batch checkpoints; resume picks up where it
   left off.
6. Failed IDs are retried on resume (batch mode).
7. Manifest files are 0600 via `fileutil.SecureWriteFile`.
8. The directory, not the inline status field, is authoritative.

### CLI

`cmd/msgvault/cmd/deletions.go` (~838 lines) provides:

- `list-deletions` — prints manifests grouped by status.
- `show-deletion <id>` — `Manifest.FormatSummary`.
- `cancel-deletion [id]` or `--all` — `Manager.CancelManifest`.
- `execute-deletion <id> [--permanent] [--batch]`.

The TUI is the primary staging UI; CLI is for review and execution.

See `internal/deletion/CLAUDE.md` for editing notes.

---

## internal/update — self-update from GitHub releases

Polls `api.github.com/repos/wesm/msgvault/releases/latest`, downloads
the platform-specific archive, verifies SHA-256, and replaces the
running binary atomically.

### Public API

```go
update.CheckForUpdate(currentVersion string, forceCheck bool) (*UpdateInfo, error)
update.PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error
update.InstallFromArchive(archivePath, expectedChecksum string) error
update.FormatSize(bytes int64) string
```

`UpdateInfo` carries `CurrentVersion`, `LatestVersion`, `DownloadURL`,
`AssetName`, `Size`, `Checksum`, `IsDevBuild`.

### Caching

`update_check.json` in `<MSGVAULT_HOME>/`. 1-hour cache for releases,
15-minute for dev builds. `forceCheck=true` bypasses.

### Asset naming

`msgvault_<version>_<GOOS>_<GOARCH>.{tar.gz|zip}` (zip on Windows).

### Checksum

The package **refuses to install without a checksum**. Sources
checked in order:

1. `SHA256SUMS` or `checksums.txt` asset on the release.
2. Hex pattern in the release `Body` (markdown notes).

Computed during download (sha256 streamed alongside the file write)
to avoid re-reading the archive.

### Install flow on Windows

The running .exe can't be deleted but can be renamed. Pattern:

1. Remove stale `<dst>.old` (best-effort).
2. Rename `<dst>` to `<dst>.old`.
3. Copy new binary to `<dst>`.
4. Chmod 0755.
5. Try to remove `<dst>.old` (silent failure on Windows; cleaned up
   next update).

### Archive extraction safety

`sanitizeTarPath` (`update.go:441`) is the path validator for both
tar and zip extraction. Rejects:

- Absolute paths (leading `/`).
- Windows volume names (`C:`).
- `..` traversal.
- Resolved paths that escape the destination dir.

Symlink and hardlink tar entries are skipped, not extracted.

### Dev build handling

Dev builds (git-describe-format versions like `0.16.1-2-gabcdef[-dirty]`)
are detected by `isDevBuildVersion`. They're never auto-replaced; the
CLI requires `--force` to install the latest official release over a
dev build.

### CLI

`cmd/msgvault/cmd/update.go` (~114 lines) drives the user flow: prints
current/latest, download URL, size, SHA256, install location, prompts
for confirmation (skippable with `--yes`), runs `PerformUpdate` with a
`%`-progress callback.

---

## internal/export — message and attachment export

Two responsibilities:

1. **Storage** during sync — `StoreAttachmentFile` writes attachments
   to content-addressed local storage (`store_attachment.go`).
2. **Export** — extract data out of msgvault (`attachments.go`):
   zip, individual files, EML, single attachment.

### Content-addressed storage

Layout: `<attachmentsDir>/<hash[:2]>/<hash>` where `hash` is SHA-256
hex of the raw decoded attachment bytes. Two-character prefix avoids
filesystem-degrading single-directory file counts.

`StoreAttachmentFile(attachmentsDir, *mime.Attachment)`:

1. Computes SHA-256, validates against any provided hash.
2. Resolves `attachmentsDir` (EvalSymlinks), chmods 0700.
3. Creates the `<hash[:2]>/` subdir, refusing if it's a symlink.
4. If final path exists → validate (size + content hash) and return.
5. Otherwise: write to a temp file in same dir, rename atomically.
6. On Windows rename collision (concurrent writer), validate the
   existing file instead of failing.

In-memory `validatedAttachmentFiles` cache (`sync.Map` keyed by
`(path, size, expectedHash)` → `modTime`) skips redundant re-hashing
during repair-encoding and re-sync runs.

### Zip export

`export.Attachments(zipFilename, attDir, []query.AttachmentInfo) ExportStats`
— writes a zip in CWD. Returns `ExportStats{Count, Size, Errors,
ZipPath, WriteError}`.

If `Count == 0` or `WriteError == true`, the zip is removed before
returning. `FormatExportResult(stats)` renders the human-facing
message.

Filename collisions: `resolveUniqueFilename` appends `_1, _2, ...`
to the sanitized base name. Falls back to the content hash if the
original sanitizes to empty.

### Per-file export

`export.AttachmentsToDir(outDir, attDir, []query.AttachmentInfo) DirExportResult`
— writes one file per attachment to `outDir`. Files are 0600. Uses
`CreateExclusiveFile` (O_CREATE|O_EXCL with `_<n>` suffix on
collision) — no overwriting.

`DirExportResult.TotalSize()` sums exported bytes.

### EML export

`cmd/msgvault/cmd/export_eml.go` exports a single message as a
standard `.eml` file:

- Resolves the message reference (`resolveMessage`): tries the input
  as a numeric internal ID first, falls back to source message ID
  (Gmail message ID, IMAP UID, etc.).
- Reads `message_raw` from the store (zlib-compressed during sync).
- Writes via `fileutil.SecureWriteFile` at 0600. Default filename is
  `<source_message_id>.eml` after `sanitizeEMLFilename` (replaces
  `/`, `\`, NUL with `_` and applies `filepath.Base` to defeat any
  IMAP mailbox names containing path separators).
- `--output -` writes to stdout (binary).

### Single-attachment export

`cmd/msgvault/cmd/export_attachment.go` exports by SHA-256 content
hash:

- Validates hash format via `export.ValidateContentHash` (exactly 64
  hex chars).
- Three output modes (mutually exclusive):
  - Binary (default): stdout or `--output <path>`.
  - `--base64`: streamed base64 to stdout.
  - `--json`: JSON envelope `{content_hash, size, data_base64}`.
- Reads from `<AttachmentsDir>/<hash[:2]>/<hash>`.

### Bulk attachment export

`cmd/msgvault/cmd/export_attachments.go` is the message-scoped
counterpart: takes a message ID, resolves attachments, calls
`export.AttachmentsToDir`. Verifies the output dir is writable up
front by creating and deleting a `.msgvault_write_test-*` temp file.

### Path safety

`export.ValidateOutputPath` and `export.ValidateContentHash` are the
two security boundaries:

- `ValidateContentHash` — exactly 64 hex chars. Prevents path
  traversal via crafted hashes.
- `ValidateOutputPath` — rejects absolute, Windows drive-relative
  (`C:foo`), UNC (`\\server\share`), rooted (`/foo`, `\foo`), and
  `..`-traversal. Used on `--output` flags so an
  email-supplied filename can't escape into `/etc/cron.d/`.

### Symlink handling

`openNoFollow` is the platform-shimmed read with no symlink
traversal:

- Unix (`open_nofollow_unix.go`): `unix.O_NOFOLLOW`.
- Windows (`open_nofollow_windows.go`): plain `O_RDONLY` —
  Windows has no equivalent. Size+hash validation in
  `validateExistingAttachmentFile` is the compensating control.
- Other (`open_nofollow_other.go`): same fallback as Windows.

---

## internal/testutil — shared test helpers

Catalog of reusable helpers for `*_test.go` files. **Search this
package before adding a new helper.**

### Layout

- `testutil.go` — package doc.
- `assert.go` — assertions, `MustNoErr`.
- `builders.go` — `query.MessageSummary` / `MessageDetail` builders.
- `fs_helpers.go` — `WriteFile` / `ReadFile` with traversal guards.
- `store_helpers.go` — `NewTestStore` (SQLite or per-test PG schema).
- `archive_helpers.go` — tar.gz / zip builders.
- `security_data.go` — `PathTraversalCases()` attack vectors.
- `encoding.go` — `EncodedSamples()` byte sequences.
- `dbtest/` — in-memory SQLite + builders, no `internal/store` import.
- `storetest/` — `Fixture` for tests that exercise the real Store.
- `email/` — RFC 2822 message builder.
- `ptr/` — generic pointer helpers.
- `tbmock/` — mock `testing.TB` for testing test helpers.

### Choosing the right store helper

| Want | Use |
| --- | --- |
| Real `*store.Store` (production code path) | `testutil.NewTestStore(t)` |
| Real Store + one source + one conversation + assertions | `storetest.New(t)` (returns `*Fixture`) |
| Direct SQL access without importing `internal/store` | `dbtest.NewTestDB(t, schemaPath)` |
| Standard seed data (alice/bob/carol) | `tdb.SeedStandardDataSet()` |

`NewTestStore` honors `MSGVAULT_TEST_DB=postgres://...` for per-test
schemas; without it, falls back to a SQLite tempfile. PG cleanup uses
`t.Cleanup` to drop the random schema regardless of test outcome.

`storetest.Fixture` is a wrapper with helpers: `CreateMessage`,
`CreateMessages`, `EnsureLabels`, `EnsureParticipant`, `StartSync`,
`AssertLabelCount`, `AssertMessageDeleted`, `AssertActiveSync`,
`AssertNoActiveSync`, plus a `MessageBuilder` that builds and
inserts `*store.Message`.

`dbtest.TestDB` exists to break import cycles: packages that the
store imports can't import store-using helpers. Builders include
`AddSource`, `AddConversation`, `AddLabel`, `AddMessage` (with
from/to/cc/bcc), `AddParticipant`, `AddMessageLabel`, `EnableFTS`,
`MarkDeletedByID`, `MarkDeletedBySourceID`. Internal counters keep
IDs unique across calls.

### Choosing the right message builder

Three exist, in three different packages:

| Builder | Type built | Use case |
| --- | --- | --- |
| `testutil.NewMessageSummary(id)` | `query.MessageSummary` | TUI / aggregate-list tests |
| `testutil.NewMessageDetail(id)` | `query.MessageDetail` | Show-message / detail-view tests |
| `storetest.NewMessage(srcID, convID)` | `*store.Message` | Store-layer tests; `.Create(t, st)` inserts |
| `storetest.Fixture.NewMessage()` | `*store.Message` | Same, with deterministic per-test counter |

The `query.*` builders set sensible defaults (`Test Subject`,
`sender@example.com`, `2024-01-01`). Each `WithX` returns the builder
for chaining. `Build()` returns the value, `BuildPtr()` returns
`*T`.

### Email/MIME builder

`testutil/email.NewMessage()` builds full RFC-2822 raw bytes with
attachments. Default headers (`From: sender@example.com`, etc.) plus
`Date: Mon, 01 Jan 2024 12:00:00 +0000`. Methods chain.

```go
raw := email.NewMessage().
    From("alice@example.com").
    Subject("Test").
    Body("Hello").
    WithAttachment("doc.pdf", "application/pdf", pdfBytes).
    CRLF().    // RFC 2822 line endings
    Bytes()
```

`email.MakeRaw(Options{...})` is a simpler one-shot for cases that
don't need attachments or arbitrary headers.

### Encoded sample data

`testutil.EncodedSamples()` returns a fresh `EncodedSamplesT` per
call, with byte sequences for testing charset detection and repair:

- Win1252: smart-quote-right, en/em dash, double quotes, trademark,
  bullet, Euro.
- Latin1: o-acute, c-cedilla, u-umlaut, n-tilde, registered,
  degree.
- Short Asian: ShiftJIS_Konnichiwa, GBK_Nihao, Big5_Nihao,
  EUCKR_Annyeong.
- Long Asian (long enough for `chardet` to identify
  confidently): ShiftJIS_Long, GBK_Long, Big5_Long, EUCKR_Long,
  each paired with `_UTF8` strings for assertions.

When adding a field, also extend the explicit `cloneBytes` list in
`EncodedSamples()` (a maintainer note explains why this is preferred
over reflection).

### Path-traversal vectors

`testutil.PathTraversalCases()` returns OS-appropriate attack vectors
for path-sanitizer tests:

- All platforms: rooted, `..` escape, nested `..` escape, just `..`.
- Windows: `C:\Windows\system32`, UNC `\\server\share\file.txt`,
  drive-relative `C:foo` and `D:subdir\file.txt`,
  forward-slash absolute `/abs/path`.
- Non-Windows: `/abs/path`.

Use this to drive table tests; don't reinvent the list inline.

### Pointer helpers

`testutil/ptr`: `Bool(v)`, `Int64(v)`, `String(v)`, `Time(v)`,
`Date(year, month, day)` (UTC midnight).

`dbtest.StrPtr` exists for the same reason — `dbtest` can't import
`ptr` without dragging `testing` into a non-test scope.

### Mock testing.TB

`testutil/tbmock.NewMockTB(t)` returns a `*MockTB` whose `Fatal*`,
`FailNow`, and `Skip*` methods panic with `FatalSentinel{Msg}` instead
of calling `runtime.Goexit`. `tbmock.ExpectFatal(mtb, fn)` recovers
the sentinel. **Use only when testing test helpers themselves**
(e.g., to verify that `MustNoErr` does fail on non-nil error). Not
appropriate for normal test code.

---

## Cross-package interactions

- `mime` produces `Attachment` values that `export.StoreAttachmentFile`
  writes to disk. The hash stored on `Attachment.ContentHash` is
  validated by `export` against the recomputed SHA-256 before any I/O.
- `textutil.EnsureUTF8` is the fallback after `mime.Parse` re-runs in
  `repair-encoding`. Both packages know about charset detection;
  `textutil` is the lowest layer.
- `fileutil.Secure*` is called by every package that writes
  user-sensitive data: `oauth`, `config`, `deletion` manifests,
  `export` attachments and outputs, `update` cache.
- `config.MkTempDir` is called by `update`, `export`, and other
  long-running operations to avoid the Windows `%TEMP%` failure
  modes.
- `logging.BuildHandler` runs once at CLI startup
  (`cmd/msgvault/cmd/root.go`); every package then uses
  `slog.Default()` or a derived logger.
- `testutil` is imported by every `*_test.go` that touches the store,
  builds messages, exercises encoding logic, or tests path
  sanitization. New shared helpers go here, never duplicated in
  feature-package test files.
