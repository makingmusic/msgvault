# internal/gvoice

## Purpose

Imports Google Voice history (SMS, MMS, calls, voicemail) from a Google Takeout export. The Takeout format is one HTML file per conversation slice — each text-conversation HTML embeds many SMS messages while each call HTML is a single record. This package parses the HTML, derives owner phone numbers from a sibling `Phones.vcf`, and writes directly to `internal/store` (not via `internal/importer.IngestRawMessage`).

## Files

- `parser.go` — filename classification, VCF parsing, HTML parsing, ID computation
- `client.go` — `Client` orchestrator: index → ensure labels → per-message import
- `models.go` — `fileType`, `indexEntry`, `textMessage`, `callRecord`, `ImportSummary`
- `parser_test.go`

## Input format

Takeout's "Voice" folder contains:

- `Phones.vcf` — vCard with the owner's Google Voice number and cell number. Items use `itemN.TEL:` paired with `itemN.X-ABLabel:Google Voice` (`parser.go:83`).
- `Calls/` directory with one HTML file per artifact, named one of:
  - `<Name> - Text - <ISO timestamp>.html` — SMS conversation (many messages)
  - `<Name> - Received|Placed|Missed - <ISO timestamp>.html` — call log
  - `<Name> - Voicemail - <ISO timestamp>.html`
  - `Group Conversation - <ISO timestamp>.html` — group SMS thread
  - `<Name> - <ISO timestamp>.html` — call without explicit type (parser reads `<title>` to classify)

HTML uses microformats / hCard classes: `.message`, `.tel`, `.fn`, `.dt`, `.haudio`, `.duration`, `.published`. No public spec; reverse-engineered.

## Identity model

There are no email addresses. Participants are keyed by E.164 phone number via `internal/textimport.NormalizePhone`. The owner has two phones: the Google Voice number (used as `source.identifier`) and the device cell number (only used for thread keying of group chats). Display names from the HTML's `<span class="fn">` populate `display_name` on the participant, with `"Me"` flipping `IsMe = true`.

## Threading

- 1:1 SMS: `threadID = <other-party-E.164>` (`parser.go:411`).
- Group SMS: `threadID = "group:" + sorted(all-participant-phones).join(",")` (`parser.go:402`).
- Calls: `threadID = "calls:" + <contact-phone>` so calls thread separately from texts even with the same contact.

## Key types and entry points

- `Client` — `client.go:21`; `NewClient(takeoutDir, opts...)` reads `Phones.vcf` upfront and stores `owner.GoogleVoice` as the identifier.
- `(*Client).Import(ctx, store, sourceID) (*ImportSummary, error)` — `client.go:124`.
- `(*Client).buildIndex()` — `client.go:724`; pre-walks Calls/, parses every HTML file, builds an `indexEntry` slice sorted by timestamp. Date filters apply at this stage.
- `parseTextHTML(r) ([]textMessage, []groupParticipants, error)` — `parser.go:136`.
- `parseCallHTML(r) (*callRecord, error)` — `parser.go:275`.
- `parseVCF(data) (ownerPhones, error)` — `parser.go:83`.
- `computeMessageID(parts...)` — `parser.go:391`; deterministic 16-char hex from sha256 prefix; used as `source_message_id`.

## Output

Direct `store` calls — no MIME synthesis. Per message:

- `messages` row with `MessageType` ∈ `{google_voice_text, google_voice_call, google_voice_voicemail}`.
- `message_bodies.body_text` — for texts the raw `<q>` content; for calls a synthesized line like `"Received call from <Name> (1m 23s)"`.
- `message_raw` — the entire source HTML file (format `gvoice_html`); same file referenced by every message extracted from it.
- `message_recipients` — `from`/`to` resolved per direction. For owner-sent messages, "to" is the *other* participant resolved by scanning the file for any non-Me sender (`resolveContactID`, `client.go:620`).
- Labels: `sms`, `mms` (when attachments present), `call_received`, `call_placed`, `call_missed`, `voicemail`.

## Edge cases

- **VCF format quirks**: items can appear in any order; `parseVCF` collects `itemN.TEL` and `itemN.X-ABLabel` lines first then matches them. `(0)` trunk prefixes are stripped (`textimport/phone.go:24`).
- **Attachments**: HTML references `<a class="video">` and `<img>`; the actual media files live alongside the HTML in `Calls/` but are *not* copied — only the `<a href>`/`<img src>` is recorded. TODO(verify): there is no attachment-file copy path; the storage_path stays empty.
- **LRU cache of one**: `lastFilePath` (`client.go:36`) avoids re-parsing the same HTML for consecutive messages. Index is timestamp-sorted, so consecutive messages from the same file are common but not guaranteed.
- **Bills.html** is hard-skipped by `classifyFile` (`parser.go:46`).
- **Multiple timestamp formats** for `<abbr class="dt"|.published>` — handles `.000-07:00`, `.000Z`, plain offset.
- **Outbound-only files**: a 1:1 text file with only Me-sent messages can't resolve the contact; `resolveContactID` returns 0 and there's no "to" recipient row.

## Gotchas

- HTML is parsed with `golang.org/x/net/html` then walked manually — no XPath / selectors. `walkNodes` returning `true` skips children; many handlers re-walk subtrees for nested classes.
- Group conversations don't have a "Me" entry in the participant list; `Me` is identified in messages only via `<span class="fn">Me</span>` (`parser.go:215`).
- `computeMessageID` uses sender phone + RFC3339Nano timestamp + body[:50]. Two messages with identical content sent at the same nanosecond from the same phone would collide — extremely unlikely but possible for replicated content.
- Resume is *not* implemented; re-running is idempotent because `source_message_id` upserts.
