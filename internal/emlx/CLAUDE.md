# internal/emlx

## Purpose

Parser and discovery for Apple Mail's `.emlx` on-disk format. Splits each `.emlx` file into its raw RFC 5322 bytes plus optional XML plist metadata, and walks `~/Library/Mail` (or any mail directory) to enumerate every mailbox containing `.emlx` files. The actual ingest path lives in `internal/importer.ImportEmlxDir`; this package only does parsing and filesystem layout.

## Input format

`.emlx` is Apple Mail's file-per-message format. Layout:

1. Line 1: ASCII decimal byte count of the MIME body, terminated by `\n`.
2. Next N bytes: raw RFC 5322 MIME message.
3. Remainder (optional): Apple's XML plist with `date-sent`, `flags`, `original-mailbox`.

Apple Mail's `date-sent` plist value is a `<real>` (or `<integer>`) holding seconds since Apple's 2001-01-01 epoch. `reader.go:133` adds 978307200 to convert to Unix time.

There is no public spec; format reverse-engineered. See https://en.wikipedia.org/wiki/Email#Filename_extensions for context.

## Key types and entry points

- `Message{Raw []byte, PlistDate, Flags, OrigMailbox}` — `reader.go:20`
- `Parse(data []byte) (*Message, error)` — `reader.go:36`
- `ParseFile(path) (*Message, error)` — `reader.go:79`
- `Mailbox{Path, MsgDir, Label, Files []string}` — `discover.go:12`
- `DiscoverMailboxes(rootDir) ([]Mailbox, error)` — `discover.go:36`
- `LabelFromPath(rootDir, mailboxPath) string` — `discover.go:117`
- `IsUUID(s) bool` — `discover.go:266` (also used by `internal/applemail`)

## Output

This package does not write to the database. `ImportEmlxDir` calls `ParseFile` per file and forwards `msg.Raw` to `IngestRawMessage`, with `msg.PlistDate` as the fallback date when MIME's Date header is absent.

## Layout handling (the part that's surprising)

Apple has shipped at least three on-disk layouts. `findMessagesDir` (`discover.go:170`) tries each:

- **Legacy**: `<mailbox>.mbox/Messages/*.emlx`
- **V10 modern**: `<mailbox>.mbox/<UUID>/Data/Messages/*.emlx`
- **V10 partitioned**: `<mailbox>.mbox/<UUID>/Data/{0..9}/{0..9}/.../Messages/*.emlx` — files sharded into numeric (0–9) subdirectories. `collectPartitionFiles` (`discover.go:362`) recurses through these.

The discoverer auto-detects: if the input path is itself a `.mbox`/`.imapmbox`, only that mailbox is returned; otherwise it walks every `.mbox`/`.imapmbox` under the root.

`LabelFromPath` strips containers like `Mailboxes/`, `IMAP-*/`, `POP-*/`, account UUIDs, and `.mbox`/`.imapmbox` suffixes to produce a clean folder-path label (e.g. `INBOX/Archive/2023`).

## Edge cases

- `.partial.emlx` (download in progress) is excluded by `isEmlxFile` (`discover.go:292`).
- A bad/missing plist is silent — `parsePlist` swallows all errors (`reader.go:88`).
- `Strict = false` on the XML decoder so non-strict Apple-emitted plists parse.
- `byteCount > available` returns an error rather than reading garbage (`reader.go:57`).
- `date-sent` may be `<real>` or `<integer>`; both branches handled.

## Gotchas

- The plist encoder used by Apple Mail does not always include `<?xml ...?>`; `parsePlist` falls back to scanning for `<plist`.
- File ordering within a mailbox is via `sort.Strings` (lexicographic on absolute paths). Resume relies on this exact ordering — if the OS returns directory entries in a different order across runs, partition collection still produces the same sorted result because every list is `sort.Strings`'d.
- `IsUUID` requires exact 36-char `8-4-4-4-12` hex format; Apple Mail's account GUIDs follow this. Used by `applemail.findV10GUIDs` to map directories to email addresses via `Accounts4.sqlite`.
