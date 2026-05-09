# internal/sync

Sync orchestration. Drives `gmail.API` to walk a remote mailbox, parse MIME,
and persist to `store.Store`. Two entry points on `*Syncer`:

- `Full(ctx, email)` — `sync.go:257`. Lists every message the query matches,
  resumable via SQLite-backed checkpoint.
- `Incremental(ctx, source)` — `incremental.go:21`. Replays Gmail History API
  records since `source.SyncCursor`. Falls back to `ErrHistoryExpired` on 404
  so the caller can trigger a full sync (Gmail keeps ~7 days of history).

Both share `parseToModel` / `persistMessage` / `ingestMessage`
(`sync.go:457`+) for the actual ingest.

## Sync model

- `gmail.API` is the only thing the syncer talks to remotely. Both `gmail.Client`
  and `imap.Client` implement it, so the same syncer drives Gmail or IMAP
  with `Options.SourceType` selecting the small behavioral differences.
- Full sync: `client.ListMessages` paginates IDs, `MessageExistsWithRawBatch`
  filters out already-stored IDs, `GetMessagesRawBatch` downloads the raw
  MIME for the rest, and `processBatch` feeds them through ingest. The
  `processed = listed`, `added = newly inserted`, `skipped = listed - added`
  invariant is what `summary` reports.
- Incremental: pulls `messagesAdded` / `messagesDeleted` / `labelsAdded` /
  `labelsRemoved` records from `client.ListHistory`. Label diffs on existing
  messages apply directly via `store.AddMessageLabels` /
  `RemoveMessageLabels` — no API fetch — see `handleLabelChange`
  (`incremental.go:274`).

## Checkpoints and resumability

- `store.StartSync` opens a `sync_runs` row; `UpdateSyncCheckpoint`
  persists `(page_token, processed, added, updated, errors)` after every
  page. `initSyncState` (`sync.go:116`) reuses an active row when
  `Options.NoResume` is false, picking up at the saved page token.
- After a successful run, `UpdateSourceSyncCursor` advances the per-source
  history_id and `CompleteSync` closes the run. The cursor advances even
  when individual messages errored — `errors_count > 0` is logged but
  doesn't block the cursor (`incremental.go:215`, `sync.go:380`).
- Don't break this: any change that bypasses `UpdateSyncCheckpoint` per
  page, or that fails to advance the source cursor on partial success,
  silently re-fetches everything on the next run.
- IMAP page tokens are numeric offsets into a session-built mailbox list
  (`internal/imap/client.go:614`). They are not stable across sessions, so
  the CLI forces `NoResume=true` for IMAP (`cmd/.../syncfull.go:365`).
  Already-imported messages are still skipped via `MessageExistsWithRawBatch`.

## MIME parsing path

`parseToModel` calls `internal/mime.Parse` on `RawMessage.Raw`. On parse
failure (`sync.go:475`), it stores a placeholder body so the raw blob is
preserved for later re-parsing — never let a parse error prevent
persistence. All string fields are run through `textutil.EnsureUTF8`
before insert; never relax that without tracing every reader.

For IMAP sources the `ThreadID` from the API is meaningless (composite
mailbox|uid). `deriveThreadKey` (`sync.go:809`) extracts a thread root
from `References` / `In-Reply-To` / `Message-ID` headers per RFC 2822.

## Rate limiting integration

The syncer never touches the rate limiter directly. `gmail.Client.request`
acquires tokens (`internal/gmail/client.go:95`) before each HTTP call and
calls `Throttle()` on 429/403-quota responses. The CLI builds the
`*RateLimiter` from `cfg.Sync.RateLimitQPS` and injects it via
`gmail.WithRateLimiter` (`cmd/.../syncfull.go:266`).

## Error handling and partial-failure semantics

- Per-message ingest errors are logged and counted in
  `checkpoint.ErrorsCount`; the loop continues. `GetMessagesRawBatch`
  itself returns `nil` entries for individual fetch failures (404s
  during incremental are expected: messages get auto-deleted between
  history scan and fetch).
- `errDuplicateRFC822` (`sync.go:687`) is an internal sentinel for
  IMAP cross-mailbox dedup: when the same RFC822 Message-ID is already
  stored under a different `mailbox|uid`, `UpdateMessageOnDedup` rewrites
  the composite ID and labels in place rather than re-downloading on
  every sync.
- A panic anywhere in the run is recovered (`sync.go:280`,
  `incremental.go:46`) and recorded via `FailSync` so the run row
  isn't left half-open. The summary returned in that case is `nil`.
- `ErrHistoryExpired` is the only typed error callers must distinguish
  from `Incremental` — see `cmd/.../sync.go` handling.

## Optional hooks

- `EmbedEnqueuer` (`sync.go:27`) — vector-search enqueue. Set via
  `SetEmbedEnqueuer`. Failures are warned, not fatal: missed IDs are
  recovered by `embed --full-rebuild`.
- `gmail.SyncProgress` and the optional `SyncProgressWithDate` extension
  drive the CLI / TUI progress bars.

## When editing

- Keep checkpoint resumability: every page must end with
  `UpdateSyncCheckpoint` and source cursor advances must survive
  per-message errors.
- Avoid N+1: batch via `GetMessagesRawBatch`,
  `MessageExistsWithRawBatch`, `EnsureLabelsBatch`,
  `EnsureParticipantsBatch`. Don't loop single-item store calls inside
  the page loop.
- Treat `RawMessage.Raw == nil` (non-nil stub) as a dedup skip, not an
  error — see `processBatch` at `sync.go:211`.
