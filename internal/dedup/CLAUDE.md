# internal/dedup — scoped context

Duplicate detection and merging engine. Sits **above** the store: it reads
candidate rows via `store.FindDuplicatesByRFC822ID` /
`GetDuplicateGroupMessages` / `GetAllRawMIMECandidates` /
`StreamMessageRaw`, and writes merges via `store.MergeDuplicates` /
`UndoDedup`. The actual SQL lives in `internal/store/dedup.go`; this
package contains no SQL.

## Relationship to `internal/store/dedup.go`

- `internal/store/dedup.go` — store-side primitives: find groups, merge
  on a survivor (union labels, backfill raw MIME, soft-delete losers
  with a batch ID), restore by batch ID, hard-delete batches.
- `internal/dedup/dedup.go` — engine: orchestrates a scan, picks
  survivors with a policy, formats human-readable reports. Has no DB
  knowledge beyond calling `store`.

This is the **read-only edition**. Dedup affects only the local
SQLite archive — there is no remote-deletion staging surface here.
`Engine.Execute` soft-hides losers locally; `Engine.Undo` restores
them. Permanent local removal is a separate command (`prune-local`).

## Key types

- `Engine` (`dedup.go`) — owns `*store.Store`, `Config`, logger.
  Constructed via `NewEngine`.
- `Config` — `SourcePreference`, `DryRun`, `ContentHashFallback`,
  `AccountSourceIDs`, `Account`, `ScopeIsCollection`,
  `IdentityAddressesBySource`. (No `DeleteDupsFromSourceServer` /
  `DeletionsDir` in this fork — those existed in upstream's
  remote-deletion staging path, which is removed here.)
- `DuplicateGroup` / `DuplicateMessage` — the in-memory representation
  of a candidate group plus survivor index. `DuplicateMessage.IsSentCopy`
  OR-combines three signals: Gmail SENT label, `messages.is_from_me`,
  identity-address match.
- `Report`, `ExecutionSummary` — public output shapes.

## Public API

- `NewEngine(*store.Store, Config, *slog.Logger) *Engine`
- `Engine.Scan(ctx) (*Report, error)` — primary pass groups by RFC822
  Message-ID; optional secondary pass groups by normalized raw-MIME
  hash (`scanNormalizedHashGroups`). Backfills missing
  `rfc822_message_id` from stored MIME first (skipped in dry-run).
- `Engine.Execute(ctx, *Report, batchID)` — merges every group and
  soft-deletes losers locally. Returns the count and batch ID; no
  remote-staging side effect.
- `Engine.Undo(batchID) (int64, error)` — clears `deleted_at` /
  `delete_batch_id` for the given batch. Returns the restored row count.
- `Engine.FormatReport`, `Engine.FormatMethodology` — humans-only
  output.
- `SanitizeFilenameComponent`, `DefaultSourcePreference` — exposed
  helpers.

## Invariants and load-bearing rules

- **`AccountSourceIDs` MUST be non-empty.** `Scan` rejects empty input
  with an error; the CLI iterates one source at a time when no
  `--account` is given. This is what keeps Sent-folder safety: dedup
  never crosses account boundaries unless `ScopeIsCollection` is true.
- **Sent-copy filter overrides survivor selection.** When any message
  in a group looks like a sent copy, only sent copies are eligible
  survivors (`selectSurvivor`).
- **Content-hash pass cannot demote a Message-ID-pass survivor.** If a
  content-hash group contains exactly one MID survivor that lost the
  content-hash selection, the engine forces that survivor to win — the
  alternative would silently destroy labels already merged into it.
  Two MID survivors in one content-hash group skips the group; one MID
  survivor + a sent-copy orphan also skips.

## Common gotchas

- `normalizeRawMIME` strips a fixed list of transport headers
  (`Received`, `Delivered-To`, DKIM/ARC, `X-Gmail-*`, etc.) and
  re-emits the kept headers sorted, before hashing. Body bytes are
  hashed verbatim — CRLF vs LF line endings produce different hashes.
- The content-hash worker pool is `min(NumCPU, 16, len(ids))` and
  caps decompression-failure warnings at 5 (counter is atomic).
- `BackfillRFC822IDs` is called inside `Scan` (non-dry-run) BEFORE
  grouping — the report otherwise undercounts. Dry-run reports the
  count as **negative** to signal "would backfill."
- Survivor-selection tiebreakers: source-type priority → has raw MIME
  → label count (more wins) → earlier `archived_at` → lower id.
  (`isBetter` at `dedup.go:803`)
- Identity-address comparison goes through
  `store.NormalizeIdentifierForCompare` — email-shaped identifiers are
  case-insensitive, synthetic ones (Matrix MXIDs, chat handles) are
  case-sensitive. Per-source keying is enforced.

## When editing here

- Do not introduce SQL here. Add a new method on `*store.Store` and
  call it.
- Do not introduce a remote-deletion pathway. This fork is the
  read-only edition: dedup must never propose, stage, or execute
  deletions against any remote server. Local soft-delete (via
  `MergeDuplicates`) is the only mutation surface.
- Do not change the order of passes. The Message-ID pass must run
  first; the content-hash pass relies on `messageIDSurvivors` /
  `excludeIDs`.
