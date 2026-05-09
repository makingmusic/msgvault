# internal/mbox

## Purpose

Streaming reader for Unix MBOX files (mboxo + mboxrd variants). Parses `From ` separator lines, returns one `Message` (separator + raw RFC 5322 bytes) at a time, and tracks logical byte offsets so callers can checkpoint and resume mid-file. Used by `internal/importer.ImportMbox` for HEY.com-style provider exports and Gmail Takeout `.mbox` files.

## Files

- `reader.go` — streaming reader, separator detection, mboxrd unescape, validate
- `from_separator_date.go` — ctime-style date parser used to *recognize* a `From ` line (since the separator itself has no fixed delimiter beyond the date shape)
- `reader_test.go`, `from_separator_date_test.go`

## Input format

MBOX is the original Unix mbox-family format. Each message is preceded by a line of the shape:

```
From <sender> <ctime-style date>
```

Two body-escaping conventions:

- **mboxo**: only originally-`From `-prefixed body lines are escaped to `>From `.
- **mboxrd**: any body line matching `^>+From ` gets an extra `>` prepended.

`Reader.unescapeFrom` (default `true`) strips one leading `>` from any `^>+From ` line, which correctly unwinds both mboxrd and mboxo. Disable with `SetUnescapeFrom(false)` if a literal `>From ` body line must be preserved.

There is no canonical spec. See https://en.wikipedia.org/wiki/Mbox.

## Key types and entry points

- `Message{FromLine, Raw}` — `reader.go:25`
- `NewReader(io.Reader) *Reader` — `reader.go:63`; honors `io.Seeker.Seek` so resume offsets are absolute (`reader.go:67-71`).
- `NewReaderWithMaxMessageBytes(r, max)` — `reader.go:81`; importer uses 128 MiB.
- `(*Reader).Next() (*Message, error)` — `reader.go:112`; returns `io.EOF` when done.
- `(*Reader).Offset()` and `(*Reader).NextFromOffset()` — used as checkpoint cursors.
- `Validate(r, maxBytes) error` — `reader.go:254`; sniffs the first ~8 MiB for any `From ` separator before commitment.
- `ParseFromSeparatorDate(line)` / `ParseFromSeparatorDateStrict(line)` — used both for validation and for fallback-date extraction (the strict variant is what the importer trusts as a date hint).

## Output

This package only parses bytes. `internal/importer.ImportMbox` consumes `Message.Raw` (forwarded to `IngestRawMessage`) and uses `Message.FromLine`'s parsed date as the `fallbackDate` when the MIME headers lack a Date.

## Edge cases

- `^From ` separator detection requires both the `From ` prefix *and* a parseable ctime-like date (`isFromSeparatorLine`, `reader.go:223`); a literal body line `From hello world` with no date won't be misclassified.
- Lines longer than 32 MiB (`maxLineBytes`) are rejected — defense against a missing newline that would otherwise OOM.
- `bufio.ErrBufferFull` is handled by accumulating across multiple buffer reads (`readLineBytes`, `reader.go:192`).
- `maxMessageBytes` is enforced cumulatively per message; once exceeded, remaining bytes are still consumed (so the reader stays aligned to the next separator) but the return value is `ErrMessageTooLarge`.
- Empty-bodied messages are returned (rather than skipped or errored).

## Gotchas

- The strict date parser only accepts a small allowlist of TZ abbreviations (`fromSeparatorTZAbbrevOffsets`, `from_separator_date.go:72`). Permissive parsing falls through Go's default behavior, which silently maps unknown abbreviations to UTC — the importer uses strict to avoid that footgun.
- `Reader` is *not* safe to use with `bufio.NewReader` wrapped externally — wrap your `io.Reader` directly with `mbox.NewReader`. The reader maintains its own buffer.
- `FromLine` is the *next* message's separator after `Next()` returns. The reader buffers the first separator on first call.
