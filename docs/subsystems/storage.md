# Storage subsystem

The storage cluster is `internal/store` (the system of record) plus
`internal/dedup` (a deduplication engine that sits above it). SQLite is
the default and only production-ready backend; PostgreSQL is scaffolded
behind a `Dialect` interface but not yet functional end-to-end. See
`docs/PG_STATUS.md` for the blocker list — this document does not
duplicate that content.

## Boundaries

- `internal/store` owns every SQL string in the system of record. No
  caller — including `internal/dedup`, `internal/sync`, the TUI, the
  HTTP API, or the MCP server — issues raw SQL against the message
  database.
- `internal/dedup` reads candidate metadata via store APIs and writes
  merges via `store.MergeDuplicates`. It contains no SQL.
- The DuckDB-over-Parquet analytics path (`internal/query`,
  `internal/parquet`) is a separate read-only layer that consumes the
  SQLite file via `sqlite_scan` (see `Store.DB()` for the unwrapped
  handle).

## The data model

Schema is defined in three files:

- `internal/store/schema.sql` — canonical DDL, loaded by both dialects.
  Today it is written in SQLite syntax (`INTEGER PRIMARY KEY`,
  `DATETIME`, `BLOB`); a PostgreSQL-native rewrite is the first
  blocker in `docs/PG_STATUS.md`.
- `internal/store/schema_sqlite.sql` — the FTS5 virtual table.
- `internal/store/schema_pg.sql` — `messages.search_fts TSVECTOR`
  column + GIN index.

### Tables

#### Sources & identity

- `sources` — one row per ingest account (Gmail OAuth, IMAP server,
  mbox import, etc.). Carries sync state (`last_sync_at`,
  `sync_cursor`, `sync_config` JSON, `oauth_app`). Unique on
  `(source_type, identifier)` and on `google_user_id`. Cascade-deletes
  conversations, messages, labels, attachments, sync state.
- `participants` — unified contacts. Keyed by `email_address` (partial
  unique index) or `phone_number` (E.164). `canonical_id` is the
  cross-platform dedup key.
- `participant_identifiers` — many-to-one identifiers per participant
  (`identifier_type`: `email`, `phone`, `apple_id`, `whatsapp`).
- `account_identities` — confirmed "me" addresses for sent-message
  detection. Per-source: an address confirmed in account A is not "me"
  in account B. `source_signal` is a sorted comma-separated set of
  evidence labels (e.g. `account-identifier,manual`).

#### Conversations & messages

- `conversations` — thread/chat container. `conversation_type` is one
  of `email_thread`, `group_chat`, `direct_chat`, `channel`. Has
  denormalized `message_count` / `participant_count` /
  `last_message_at` / `last_message_preview` (recomputed lazily by
  `Store.RecomputeConversationStats`).
- `messages` — the central table. The B-tree is intentionally narrow:
  body content lives in a separate table (see below). Carries
  `source_message_id` (platform native ID), `rfc822_message_id`
  (header-derived, used for cross-mailbox dedup), `sender_id`,
  multiple timestamp columns (`sent_at`, `received_at`, `internal_date`,
  `delivered_at`, `read_at`), soft-delete columns (`deleted_at`,
  `deleted_from_source_at`, `delete_batch_id`), and `message_type`
  (`email`, `imessage`, `sms`, `whatsapp`, `fbmessenger`, ...).
- `message_recipients` — to/cc/bcc/mention rows pointing at
  participants.
- `message_bodies` — `body_text` and `body_html`, keyed by
  `message_id`. **Separated to keep the messages B-tree small.** Read
  only via direct PK lookup in single-message detail views; never
  joined in list/aggregate/search queries (see SQL Guidelines in the
  root `CLAUDE.md`).
- `message_raw` — original RFC822/RCS/iMessage blob, zlib-compressed
  by default. Has an `encryption_version` column reserved for at-rest
  encryption (currently always `0`; no read/write code path).
- `attachments` — content-hash-addressed metadata; on-disk blobs live
  under `~/.msgvault/attachments/{2-char-prefix}/{hash}` and are
  shared across messages. Same `encryption_version` placeholder.
- `reactions` — tapbacks/emoji reactions for chat platforms.
- `labels` / `message_labels` — Gmail labels (system + user) plus the
  many-to-many.
- `conversation_participants` — group-chat membership with role and
  joined/left timestamps.

#### Sync state

- `sync_runs` — one row per sync attempt; surface for resumability and
  TUI status.
- `sync_checkpoints` — keyed by `(source_id, checkpoint_type)` for
  resumable imports.

#### Collections & migrations

- `collections` / `collection_sources` — named groupings of sources.
  The `"All"` collection is auto-managed by `EnsureDefaultCollection`
  and rejected by all explicit mutators (`ErrCollectionImmutable`).
- `applied_migrations` — one-time DATA migration ledger. Schema DDL
  itself is `IF NOT EXISTS` and re-runnable.

### Indexes

Hot read paths to keep in mind:

- `idx_messages_conversation (conversation_id, sent_at DESC)` — list/
  thread reads.
- `idx_messages_sent_at` — timeline-style aggregations.
- `idx_messages_deleted (source_id, deleted_from_source_at)` — gating
  by `LiveMessagesWhere`.
- `idx_messages_source_message_id` — Gmail-ID lookups for deletion.
- `idx_message_recipients_participant (participant_id, recipient_type)`
  — `from:`/`to:` filters in `SearchMessagesQuery`.
- `idx_attachments_hash` — content dedup.

## Read paths vs write paths

### Write paths

- Sync (`internal/sync` + `cmd/msgvault/cmd/sync*.go`) is the dominant
  writer. Each Gmail message becomes one
  `Store.PersistMessage(MessagePersistData)` call: upserts
  `messages`, then `message_bodies`, then `message_raw`, then four
  `replaceMessageRecipientsTx` calls, then `replaceMessageLabelsTx` —
  all in a single transaction (`messages.go:317`).
- Import paths (mbox, EMLX, Messenger DYI, PST) build the same shape.
- Dedup `MergeDuplicates` issues `UNION INTO` for labels, conditional
  raw-MIME backfill, and `UPDATE messages SET deleted_at, delete_batch_id`
  per loser, all in one transaction (`store/dedup.go:178`).

Write concurrency is bounded: SQLite gets `MaxOpenConns=4` (single
writer + multiple readers), PostgreSQL gets 25/5/5min.
`WithExclusiveLock` (`store.go:320`) wraps `BEGIN EXCLUSIVE` for
maintenance ops that must not race with sync workers (e.g. attachment
file deletion).

### Read paths

- Listing: `Store.ListMessages` — paginated, with `LiveMessagesWhere`,
  followed by `batchPopulate` to load recipients and labels via two
  IN-clause queries. **Never JOINs `message_bodies`.**
- Single message: `Store.GetMessage` — fetches the row, recipients
  per-type, labels, body via direct `message_bodies` PK, and
  attachments. The only place `message_bodies` is touched in API code.
- Search: `Store.SearchMessages` (legacy text query) and
  `SearchMessagesQuery` (parsed Gmail-style operators). Both consult
  `dialect.FTSSearchClause()` for the FTS join/where/order fragments.
  Falls back to `searchMessagesLike` (subject/snippet `LIKE`) when
  FTS5 is unavailable. `SearchMessagesQuery` further uses EXISTS
  subqueries for `from:`/`to:`/`cc:`/`bcc:`/`label:` to avoid
  DISTINCT+JOIN duplication.
- `GetMessagesSummariesByIDs` — designated hydration path for
  vector/hybrid search hits. Five SQL round-trips regardless of result
  count vs. seven per hit if `GetMessage` were called in a loop.
- Stats: `GetStats` / `GetStatsForScope` — five COUNT queries with
  unscoped/global vs. scoped IN-clause variants. `DatabaseSize` is
  always the global file size; on PostgreSQL `os.Stat(s.dbPath)` is a
  URL and silently reports `0` (PG_STATUS issue #9).

### The `LiveMessagesWhere` predicate

Defined in `live_messages.go`. Returns
`<alias>.deleted_at IS NULL` always plus
`AND <alias>.deleted_from_source_at IS NULL` when
`hideDeletedFromSource` is true. The `("", "m") × bool` combinations
are pre-computed as constants to avoid `fmt.Sprintf` allocation on
hot paths. Every read query that should hide dedup losers and source-
deleted rows MUST use this; manual `deleted_at IS NULL` should not
appear in new code.

## The Dialect interface

Defined in `internal/store/dialect.go`. Centralises everything that
varies between SQLite and PostgreSQL:

- Placeholder rebinding (`?` → `$N`).
- Timestamp expression (`datetime('now')` vs `NOW()`).
- Conflict handling: `InsertOrIgnore` for complete statements,
  `InsertOrIgnorePrefix`/`InsertOrIgnoreSuffix` for chunked builders.
- FTS surface: `FTSUpsert`, `FTSSearchClause` (returns join, where,
  orderBy, and `orderArgCount` so callers know how many extra `?`
  placeholders the order-by introduces), `FTSDeleteSQL`,
  `FTSBackfillBatchSQL`, `FTSAvailable`, `FTSNeedsBackfill`,
  `FTSClearSQL`, `FTSRebuildSchema`, `SchemaFTS`.
- Connection lifecycle: `InitConn`, `SchemaFiles`, `CheckpointWAL`.
- Schema migration probe: `SchemaStaleCheck`,
  `IsDuplicateColumnError`.
- Error classifiers: `IsConflictError`, `IsNoSuchTableError`,
  `IsNoSuchModuleError`, `IsReturningError`, `IsBusyError`.

`SQLiteDialect` (`dialect_sqlite.go:12`) is the production
implementation. `PostgreSQLDialect` (`dialect_pg.go:14`) implements
every method, but the surrounding store code still has gaps that keep
PostgreSQL from working end-to-end:

- `LastInsertId` is not supported by pgx, but `EnsureConversation`,
  `EnsureParticipant`, `StartSync`, `GetOrCreateSource` etc. still call
  it. Only `upsertMessageWith` has been converted to `RETURNING`
  (`messages.go:214`).
- `subset.go` is fully SQLite-specific (PRAGMAs, ATTACH DATABASE,
  string substitution into `ATTACH '%s'`).
- `schema.sql` itself uses SQLite types and SQLite autoinc.
- `PostgreSQLEngine` (in `internal/query`) returns `ErrNotImplemented`
  for most aggregate methods.

The two dialects produce subtly different FTS behaviour that already
exists in code:

- PostgreSQL applies `setweight('A')` to subject and `'B'` to sender;
  SQLite FTS5 has no weighting. Ranking will diverge.
- PostgreSQL's `FTSBackfillBatchSQL` uses INNER JOIN on
  `message_bodies`; SQLite uses LEFT JOIN. Messages with no body row
  are not indexed on PG (PG_STATUS issue #6).

## FTS5 mechanics

- `messages_fts` is a **standalone** FTS5 virtual table (not
  contentless) with `unicode61 remove_diacritics 1` tokenizer.
  Columns: `message_id UNINDEXED`, `subject`, `body`, `from_addr`,
  `to_addr`, `cc_addr`. (`schema_sqlite.sql`)
- Every write goes through `dialect.FTSUpsert` (the dialect owns the
  rowid duplication). `INSERT OR REPLACE` is used so re-syncs idempotently
  refresh the row (`dialect_sqlite.go:34`).
- Joins use `JOIN messages_fts fts ON fts.rowid = m.id`. **Never
  inline this** — go through `dialect.FTSSearchClause()`.
- Backfill: `BackfillFTS` clears (`DELETE`) and refills in 5,000-row
  ID-range batches with progress callbacks
  (`messages.go:978`/`backfillFTSRange`). Each batch is a separate
  `Exec` so partial progress survives interruption.
- Rebuild path: `RebuildFTS` calls
  `SQLiteDialect.FTSRebuildSchema`, which `DROP TABLE IF EXISTS
  messages_fts` and recreates it from `schema_sqlite.sql`. This is the
  only reliable recovery from malformed FTS5 shadow tables — the
  `rebuild` PRAGMA reads from the (corrupt) shadows; `delete-all` is
  rejected on contentful tables. PostgreSQL's
  `FTSRebuildSchema` is a `TODO` stub.
- Build tag: the binary needs `-tags fts5` (set as default in the
  Makefile alongside `sqlite_vec`). Without it, `messages_fts`
  creation fails with "no such module: fts5" and the dialect's
  `IsNoSuchModuleError` lets `InitSchema` continue with FTS disabled
  — `SearchMessages` then falls back to `searchMessagesLike`.
- `FTSDeleteSQL` is **explicit** (called by `RemoveSource`) because
  virtual tables are not subject to FK cascades.

## Migrations

Two layers:

1. **DDL migrations**: hard-coded `ALTER TABLE ADD COLUMN ...` block
   inside `InitSchema` (`store.go:560`). Each entry is run
   unconditionally; "duplicate column" is swallowed via
   `dialect.IsDuplicateColumnError`. The list is **append-only**:
   reordering or removing rows skips columns on existing databases.
   `SchemaStale` (`store.go:532`) checks for the most recently added
   column (`conversations.conversation_type`) so the CLI can warn the
   user before opening a stale DB.
2. **Data migrations**: one-time, recorded in `applied_migrations`
   via `IsMigrationApplied`/`MarkMigrationApplied` (`migrations.go`).
   The only one today is
   `legacy_identity_to_per_account` (`migrate_legacy_identity.go`),
   which copies legacy global identity addresses into per-source
   `account_identities` rows. Empty-input still marks the migration
   applied so a later config change does not re-run.

## subset.go

`CopySubset(srcDBPath, dstDir, rowCount)` produces a smaller, self-
consistent SQLite copy of the latest `rowCount` messages with all
referenced sources/conversations/participants/labels/attachments. Two
phases:

1. Open destination via `Store.Open` + `InitSchema` so all schema and
   the default collection exist.
2. Reopen the destination raw with `_foreign_keys=OFF`, `ATTACH
   DATABASE 'src'`, then run `INSERT INTO ... SELECT ...` per-table
   in dependency order. Orphan `reply_to_message_id` references are
   nulled out post-copy. After detach, `PRAGMA foreign_key_check`
   verifies integrity, denormalized conversation counts are
   recomputed, and `populateFTS` rebuilds `messages_fts` directly.

This is **SQLite-only by design** (PRAGMAs, ATTACH, sqlite_master
fallback queries) and is intentionally deferred from the PostgreSQL
work in `docs/msgvault_integration.md`. There is also a graceful
degradation when the source DB lacks an `oauth_app` column: it
falls back to inserting `NULL` (`subset.go:258`).

## Encryption at rest

Currently a placeholder. `encryption_version INTEGER DEFAULT 0` columns
exist on `attachments` (`schema.sql:231`) and `message_raw`
(`schema.sql:280`), but no read or write code in the package consults
or sets them — every code path treats raw_data as plain (zlib-
compressed) bytes. The root `CLAUDE.md` lists "App-level encryption"
under "Not Yet Implemented." Future work would version-gate
encrypt/decrypt at these column boundaries.

## Performance properties

- **Hot path: list / aggregate.** Bounded by index-only scans on
  `messages` and `message_recipients` because `message_bodies` is a
  separate table. The B-tree separation is a load-bearing optimization,
  not stylistic — see SQL Guidelines.
- **Hot path: search.** `messages_fts MATCH ?` joined to `messages`
  by rowid. Postgres uses a GIN tsvector index instead, but the same
  join pattern collapses to a single-table predicate
  (`m.search_fts @@ plainto_tsquery(...)`).
- **`GetRandomMessageIDs`** uses `ORDER BY RANDOM()` only when total
  count < 10,000 or limit ≥ total; otherwise it does Go-side reservoir
  sampling with random offsets (O(limit) instead of O(n)). This is
  dialect-portable.
- **Chunk helpers** (`queryInChunks`, `insertInChunks`, `execInChunks`)
  cap at 500 IN-clause rows or 900/`valuesPerRow` for INSERTs to stay
  under SQLite's 999 parameter limit.
- **WAL checkpoint on Close** (TRUNCATE mode) prevents WAL accumulation
  across sessions. Read-only opens skip it.
- **Slow-query log** at 100ms by default (`db_logger.go`) — every
  Query/Exec is timed via the `loggedDB`/`loggedTx`/`loggedRows`
  wrappers, with the slow threshold and full-trace toggle in
  process-wide atomics.
- **Compression**: `message_raw.raw_data` is zlib by default
  (`UpsertMessageRaw`); `GetMessageRaw` decompresses transparently when
  `compression='zlib'`.

## Test surface

- `store_test.go` (1725 LOC) — exercises the end-to-end persist /
  retrieve / mark-deleted lifecycle.
- `dedup_test.go` + `dedup_delete_test.go` — store-side dedup
  primitives; soft-delete, batch ID semantics, undo, hard-delete.
- `subset_test.go` (1285 LOC) — most extensive single test file:
  selection ordering, FK consistency post-copy, oauth_app fallback,
  conversation count recomputation, FTS population.
- `dialect_pg_test.go` — dialect-string unit tests (Rebind quote
  safety, Now, InsertOrIgnore variants, FTSSearchClause shape) without
  requiring a live Postgres.
- `postgres_internal_test.go` — `postgresColumnExistsSQL` schema
  scoping, `postgresConnConfig` runtime params, store close cleanup.
- `rebuild_fts_test.go` — happy path, availability-flag bypass, post-
  drop recovery, progress reporting.
- `migrations_test.go` — `applied_migrations` idempotence.
- `migrate_legacy_identity_test.go` — empty-input still marks applied;
  per-source duplication; deferred-when-no-sources branch.
- `account_identities_test.go`, `identifier_match_test.go` — case
  rules for email vs synthetic identifiers.
- `collection_test.go`, `sources_test.go`, `sync_test.go`,
  `messages_test.go`, `api_test.go`, `inspect_test.go`,
  `live_messages_test.go`, `db_logger_test.go`, `sqlite_error_test.go`.
- `internal/dedup/dedup_test.go` (744 LOC) — engine-level scenarios
  including sent-copy override, content-hash MID-survivor protection,
  manifest staging.
- Postgres dual-backend test harness via `MSGVAULT_TEST_DB` is wired
  but expected to fail before schema init completes (PG_STATUS).

## Known issues / smells / TODOs

- **`schema.sql` is not portable.** Loaded for both dialects today;
  PostgreSQL will reject `INTEGER PRIMARY KEY` autoinc, `DATETIME`,
  `BLOB`. PG_STATUS blocker #1.
- **`LastInsertId` is still on the hot path** for several upserts.
  Each is a separate PR-sized fix (`RETURNING id` rewrite).
- **Mixed placeholder styles in `SearchMessages`.** `FTSSearchClause`
  yields `$1` on Postgres, but the `LIMIT ? OFFSET ?` suffix is still
  in `?`-form (`api.go:271`). Both run through `loggedDB` rebind, so
  on SQLite it is consistent; on Postgres the dialect-supplied `$N`
  fragments would collide with rebind numbering. PG_STATUS blocker #5.
- **`GetStats.DatabaseSize`** silently reports `0` for PostgreSQL
  (`os.Stat` on a URL).
- **`PostgreSQLEngine.FTSRebuildSchema`** returns "not yet implemented".
- **`subset.go` bypasses the dialect entirely** and reaches into
  `loggedDB.DB` for raw SQLite features. Documented as deferred.
- **`PostgreSQLEngine` is never constructed.** The TUI/MCP/HTTP API
  still build `SQLiteEngine` unconditionally — the PG path can connect
  but cannot serve aggregates or search. PG_STATUS issue #12.
- **Encryption-at-rest columns** are dead schema today; no migration
  path is sketched in code.
- **`AttachmentPathsUniqueToSource` runs before `RemoveSource`'s FK
  cascade**; callers must order operations correctly. The race window
  between collecting candidates and deleting files is closed by
  `IsAttachmentPathReferenced` immediately before each unlink, with
  `WithExclusiveLock` for the surrounding maintenance op.
