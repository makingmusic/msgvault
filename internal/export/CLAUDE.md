# internal/export

Two related responsibilities:

1. **Storing attachments** in content-addressed local storage during
   sync (`store_attachment.go`).
2. **Exporting** messages and attachments out of msgvault on demand
   (`attachments.go`) — to a zip, to a directory, or to stdout.

## File layout

- `store_attachment.go` — `StoreAttachmentFile` (sync-time write to
  `<attachmentsDir>/<hash[:2]>/<hash>`), atomic write,
  validation/dedup, in-memory mod-time cache.
- `attachments.go` — `Attachments` (zip), `AttachmentsToDir`
  (individual files), `ValidateContentHash`, `ValidateOutputPath`,
  `SanitizeFilename`, `StoragePath`, `CreateExclusiveFile`.
- `open_nofollow_unix.go` (build tag `unix`) — `O_NOFOLLOW`
  reads.
- `open_nofollow_windows.go` (build tag `windows`) — falls back to
  plain `O_RDONLY`; relies on size+hash validation.
- `open_nofollow_other.go` (build tag `!windows && !unix`) — same
  fallback as Windows.

## Content-addressed storage

Layout: `<attachmentsDir>/<hash[:2]>/<hash>` where `hash` is SHA-256
hex of the raw decoded bytes. Two-character prefix avoids 100K+ files
in one directory (filesystem perf).

`StoreAttachmentFile`:

1. Computes SHA-256, validates against any provided hash (mismatch →
   error).
2. Resolves `attachmentsDir` to its target via `EvalSymlinks` and
   chmod 0700.
3. Creates `<hash[:2]>/` if missing, refuses if it's a symlink.
4. If the final path already exists → validates the existing file
   (size + content hash) and returns the storage path; nothing
   written.
5. Otherwise: writes to a temp file in the same directory, fsyncs
   nothing (relies on rename being atomic on same fs), renames into
   place. On Windows, rename can fail if another process raced; the
   code falls back to validating the existing file.

`validatedAttachmentFiles` (`store_attachment.go:21`) is a process-
local `sync.Map` cache keyed by `(path, size, expectedHash)` →
`modTime`. Skips re-hashing files that have already been validated
this run. Important during repair-encoding or re-sync where the same
attachments may pass through repeatedly.

## Export modes

| Function | Output | Use case |
| --- | --- | --- |
| `Attachments(zipFilename, attDir, []AttachmentInfo)` | Zip file in CWD | TUI bulk export |
| `AttachmentsToDir(outDir, attDir, []AttachmentInfo)` | Individual files in outDir | `export-attachments <id>` CLI |
| `CreateExclusiveFile(path, perm)` | Open *os.File | Single-file CLI like `export-eml -o foo.eml` (via wrapper) |

`Attachments` (zip) returns `ExportStats` with the zip path, count,
size, and any errors. **Important:** if `Count == 0` or any
`writeError` was set, the zip file is removed before returning.
`FormatExportResult` then renders a user-facing message.

`AttachmentsToDir` returns `DirExportResult` (per-file
`ExportedFile{Path, Size}` plus errors). Files are 0600. Conflicts
get `_1, _2, ...` suffixes via `CreateExclusiveFile`.

## Filename and path safety

- `ValidateContentHash` — exactly 64 lowercase hex chars. Prevents
  path traversal via crafted hashes.
- `ValidateOutputPath` — rejects absolute, Windows drive-relative
  (`C:foo`), UNC (`\\server\share`), rooted (leading `/` or `\`),
  and `..` traversal. Used on `--output` flags.
- `SanitizeFilename` — replaces `/ \ : * ? " < > | \n \r \t` with
  `_`. Used to derive zip entry names from email-supplied
  filenames.
- `resolveUniqueFilename` — appends `_<n>` when a sanitized name
  collides within the export. Falls back to the content hash if
  the original filename sanitizes to empty/`.`.

## Symlink handling

`openNoFollow` is the platform-shimmed read-with-no-symlink-traversal.
On Unix it uses `unix.O_NOFOLLOW`. On Windows there's no equivalent;
the size+hash validation in `validateExistingAttachmentFile` catches
contents-don't-match cases but cannot prevent reading
through a reparse point. Acceptable threat model: the attacker
already has local write access to the attachments dir.

## When editing

- The hash-prefix layout is load-bearing for filesystem perf. Don't
  flatten it without measuring.
- `validatedAttachmentFiles` is process-scoped; clearing it requires
  restart. Don't add a TTL — the typical run never sees the same
  file twice.
- New export targets: build on top of `StoragePath` and
  `CreateExclusiveFile`, never construct paths by hand. The
  validation order (validate hash → derive path) is the security
  boundary.
- Always 0600 on output files. CLI users can chmod after; we never
  write something world-readable for them by default.
