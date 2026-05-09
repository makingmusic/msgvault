# internal/mime

Wraps [`jhillyerd/enmime`](https://github.com/jhillyerd/enmime) into a
small, opinionated `Message` model used by the sync pipeline, the
repair-encoding command, and the EML/show-message paths.

## File layout

- `parse.go` — the entire surface area:
  - `Parse(raw []byte) (*Message, error)` — main entry point. Builds
    a `Message` from a raw RFC 2822 byte slice.
  - `Message`, `Address`, `Attachment` — domain types.
  - `parseDate`, `dateFormats` — date header parser supporting 19+
    formats.
  - `StripHTML` — HTML-to-plain-text helper for body extraction.

## Key invariants

- **Email addresses are lowercased.** `parseAddressList`
  (`parse.go:115`) calls `strings.ToLower` on every `addr.Address`.
  Domain is also lowercased. Display names are kept as-is.
- **Dates are normalized to UTC.** Named timezones (EST, PST, MST...)
  are treated as offset 0 because Go's `time.Parse` resolves them
  against the local system zone, which is platform-dependent. Numeric
  offsets (`-0700`, `Z`) get proper conversion. See `toUTC`
  (`parse.go:241`) and `hasNumericOffset`.
- **`text/plain` and `text/html` parts without a filename and without
  `Content-Disposition: attachment` are body parts, not attachments.**
  This matches the Python parser's behavior. See `isBodyPart`
  (`parse.go:134`).
- **Attachment hashes are SHA-256 hex of the decoded content.** Stored
  on `Attachment.ContentHash`, also used as the content-addressed
  storage key in `internal/export.StoreAttachmentFile`.
- **`env.Errors` is preserved on `Message.Errors` as strings.** These
  are non-fatal parse warnings — sync still records the message.

## Body extraction

`Message.GetBodyText()` prefers `BodyText`, falls back to
`StripHTML(BodyHTML)`. `StripHTML` (`parse.go:306`) drops
`<script>/<style>/<head>` content entirely, converts block tags to
newlines, decodes entities, normalizes whitespace. Pre-formatted
content loses indent (acceptable for previews).

## Repair-encoding flow

`cmd/msgvault/cmd/repair_encoding.go` re-runs `mime.Parse` on stored
raw MIME blobs (zlib-compressed in `message_raw`) when text columns
fail `utf8.ValidString`. Re-parsed text typically has correct
charset metadata; if parsing still produces invalid bytes, the
command falls back to `textutil.EnsureUTF8`.

## When editing

- Don't add a parser dependency without checking that `enmime` already
  exposes the data — `Envelope.GetHeader` and `AddressList` already
  cover most needs.
- `parseDate` returns the zero `time.Time` on failure (no error). Many
  emails have unparseable dates; the caller decides whether to fall
  back to `internal_date` from Gmail metadata. Don't change to return
  an error without auditing `internal/sync`.
- `parseAddressList` returns `nil` (not empty slice) on parse failure
  — many call sites use `len(addrs) == 0` so this is fine, but don't
  rely on a non-nil return.
- New date formats go in `dateFormats` (`parse.go:201`); the order
  matters because the first match wins.
