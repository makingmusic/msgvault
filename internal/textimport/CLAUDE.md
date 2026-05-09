# internal/textimport

## Purpose

Tiny shared utility package for phone-number-keyed importers (Google Voice, iMessage). Exposes one function: `NormalizePhone`, which converts free-form phone-number input into E.164 (`+<country><subscriber>`) format. Used wherever an importer needs a stable participant key that isn't an email address.

## Files

- `phone.go` — `NormalizePhone`
- `phone_test.go`, `integration_test.go`

## Input format

Free-form strings. Examples accepted:

- `+1 (650) 555-1234` → `+16505551234`
- `(650) 555-1234` → `+16505551234` (10 digits assumed US)
- `1-650-555-1234` → `+16505551234`
- `0044 7700 900000` → `+447700900000` (00-prefix → +)
- `+44 (0)7700 900000` → `+447700900000` (UK trunk-prefix `(0)` stripped)
- `tel:+44 7700 900000` → `+447700900000` (non-digit non-`+` chars are dropped)

Rejected:

- Email-shaped (`foo@bar`) → error.
- Embedded `+` (`1+555...`) → error.
- Empty / no digits → error.
- < 7 digits or > 15 digits after normalization → error (E.164 spec max 15).

## Key entry point

- `NormalizePhone(raw string) (string, error)` — `phone.go:12`

## Output

Returns the normalized E.164 string with leading `+`. Callers (`gvoice/parser.go:109`, `imessage/parser.go:51`, `whatsapp/mapping.go:134` for the WhatsApp-specific variant) use this to key participants.

## Edge cases / rules

- A leading `+` is preserved; an embedded `+` is rejected (defensive against malformed input like `1+5551234567`).
- `(0)` is stripped *before* digit collection so `+44 (0)7700` becomes `+44 7700`.
- `00` international prefix → replaced with `+`.
- 10 digits with no leading `+` or `00` is assumed US (`+1` prefix); 11 digits starting with `1` is also assumed US.
- 7–15 digits otherwise are treated as already-internationally-formatted and just get a `+` prefix.

## Gotchas

- The 10-digit US assumption is a US-centric heuristic. A 10-digit number from another country with no country code will be mis-prefixed `+1`. WhatsApp avoids this by using `normalizePhone` in `internal/whatsapp/mapping.go` which is *stricter* — it rejects anything not already digit-only and trusts the JID's country code.
- The `4..15`-digit range is wider than strict E.164; 4-6 digit short codes (e.g. SMS shortcodes `40404`) will pass. Callers that need to distinguish must check separately.
- `unicode.IsDigit` accepts any Unicode digit class — Devanagari, Arabic-Indic, etc. — and converts them to ASCII via `WriteRune`. TODO(verify): the resulting string may contain non-ASCII digit characters since `WriteRune` writes the rune as-is; this is unlikely to be hit in practice but worth confirming if it matters.
- No package supports parsing or formatting — this is the simplest possible normalizer. For full phone-number parsing, a library like `nyaruka/phonenumbers` would be needed; the project chose minimal dependencies.
