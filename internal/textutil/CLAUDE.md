# internal/textutil

Encoding repair, UTF-8 sanitization, and terminal-output sanitization.
Three concerns, one file.

## File layout

- `encoding.go`:
  - `EnsureUTF8(s string) string` — best-effort UTF-8 conversion.
  - `SanitizeUTF8(s string) string` — replace invalid bytes with
    U+FFFD.
  - `GetEncodingByName(name string) encoding.Encoding` — IANA charset
    name lookup (Windows-1252, ISO-8859-*, Shift-JIS, EUC-JP/KR, GBK,
    Big5, KOI8).
  - `TruncateRunes`, `FirstLine` — UTF-8 safe truncation.
  - `SanitizeTerminal(s string) string` — strip ANSI escape sequences
    and C0/C1 control characters before printing untrusted text.

## EnsureUTF8 strategy

If `s` is already valid UTF-8, returned as-is. Otherwise:

1. `chardet.NewTextDetector().DetectBest`. Confidence threshold is
   `30` for short strings (<=50 bytes) and `50` for longer ones
   (`encoding.go:31`). If detection succeeds and `GetEncodingByName`
   recognizes the charset, decode with that.
2. Fallback: try `Windows1252` → `ISO8859_1` → `ISO8859_15` → `ShiftJIS`
   → `EUCJP` → `EUCKR` → `GBK` → `Big5`, in that order
   (`encoding.go:50`). First decoder that produces valid UTF-8 wins.
3. Last resort: `SanitizeUTF8` (replacement-character substitution).

The order matters: Windows-1252 is the most common silent producer of
invalid UTF-8 bytes in Western emails (smart quotes, em/en dashes,
trademark, bullet, Euro). Latin-1 is a superset most of the time but
misses Win1252's 0x80-0x9F range.

## SanitizeTerminal

Used for any user-supplied string written to a TTY: WhatsApp chat
names, message snippets in TUI rows, progress output. Strips:

- ESC-initiated CSI sequences (`ESC [ ... <0x40-0x7E>`)
- ESC-initiated OSC sequences (`ESC ] ... BEL` or `ESC ] ... ESC \`)
- Other 2-byte ESC sequences
- C0 control bytes (< 0x20) except `\t`
- C1 control bytes (0x80-0x9F), checked on the **decoded rune**, not
  the raw leading byte, so UTF-8-encoded C1 like 0xC2 0x9B (CSI) is
  caught.
- `\r` and `\n` are replaced with space — single-line callers don't
  want line breaks.

## When editing

- `EnsureUTF8` is hot in repair-encoding and during sync. Don't add
  per-call allocations on the happy path (`utf8.ValidString` short
  return is critical).
- The encoding fallback list trades correctness for the most common
  case. Don't add an encoding to the list without thinking about what
  it might falsely "successfully decode" (e.g., GB18030 will decode
  almost anything).
- Two encodings missing from `GetEncodingByName`: macroman and
  windows-1251 (Cyrillic). Add only when a real bug requires it.
