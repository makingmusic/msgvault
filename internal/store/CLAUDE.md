# internal/store — scoped context

System of record for msgvault. SQLite is the production backend; PostgreSQL
is scaffolded behind a `Dialect` interface but is not functionally usable
end-to-end yet (see `docs/PG_STATUS.md`).

## Key types

- `Store` — concrete struct, no interface. ~129 methods bound on
  `*Store`. (`store.go:31`)
- `Dialect` — abstracts dialect-specific SQL: placeholder rebinding,
  `Now()`, `InsertOrIgnore`, FTS upsert/search, error-class predicates,
  schema files, WAL checkpoint. (`dialect.go:18`)
- `SQLiteDialect` (`dialect_sqlite.go:12`) and `PostgreSQLDialect`
  (`dialect_pg.go:14`).
- `loggedDB` / `loggedTx` / `loggedRows` — wrap `*sql.DB`/`*sql.Tx` with
  per-statement slog logging AND own the dialect's Rebind step.
  Embedding `*sql.DB` means existing call sites compile unchanged.
  (`db_logger.go:75`)
- `Message`, `MessagePersistData`, `Source`, `Label`, `Collection`,
  `AccountIdentity`, `MessageInspection`, `Stats`, `APIMessage`,
  `DuplicateGroupKey`, `DuplicateMessageRow`, `MergeResult`,
  `ContentHashCandidate`.
- `chunkInsert` + `insertInChunks` / `queryInChunks` / `execInChunks` —
  chunk helpers that respect SQLite's 999-param ceiling. (`store.go:443`)
- `identifierMatch` — encapsulates the email-vs-synthetic comparison rule
  used by `account_identities`. (`identifier_match.go:65`)

## Public API surface (categories)

- Lifecycle: `Open`, `OpenReadOnly`, `Close`, `WithExclusiveLock`,
  `CheckpointWAL`, `InitSchema`, `SchemaStale`, `DB`, `Rebind`,
  `FTS5Available`, `IsBusyError`. Postgres URLs (`postgres://` /
  `postgresql://`) route through `openPostgres`.
- Sources & sync: `GetOrCreateSource`, `ListSources`, `GetSourceByID`,
  `RemoveSource`, `RemoveSourceSerialized`, sync-run lifecycle in
  `sync.go`.
- Messages & content: `UpsertMessage`/`PersistMessage`,
  `UpsertMessageBody`, `UpsertMessageRaw`, `UpsertAttachment`,
  `EnsureConversation[WithType]`, `EnsureParticipant[ByPhone|sBatch]`,
  `EnsureLabel[sBatch]`, `ReplaceMessageRecipients`,
  `ReplaceMessageLabels`/`AddMessageLabels`/`RemoveMessageLabels`,
  `MarkMessageDeleted*`, `RecomputeConversationStats`.
- Search: `ListMessages`, `GetMessage`, `GetMessagesSummariesByIDs`,
  `SearchMessages`, `SearchMessagesQuery` (Gmail-style operators), and
  `messages_fts` upsert/backfill (`UpsertFTS`, `BackfillFTS`,
  `RebuildFTS`).
- Dedup support: `FindDuplicatesByRFC822ID`,
  `GetDuplicateGroupMessages`, `GetAllRawMIMECandidates`,
  `MergeDuplicates`, `UndoDedup`, `DeleteDedupedBatch`,
  `DeleteAllDeduped`, `BackfillRFC822IDs`, `StreamMessageRaw`.
- Collections: `EnsureDefaultCollection`, `CreateCollection`, list/get/
  add/remove. The auto-managed `"All"` collection is immutable to
  callers.
- Identities: `AddAccountIdentity`, `RemoveAccountIdentity`,
  `ListAccountIdentities`, `GetIdentitiesForScope`. Identifier matching
  goes through `identifierMatch`/`looksLikeEmail`.
- Migrations: `IsMigrationApplied`/`MarkMigrationApplied` (one-time data
  migrations only). DDL migrations are `IF NOT EXISTS` + idempotent
  `ALTER TABLE ADD COLUMN` blocks in `InitSchema` (`store.go:546`).
- Inspection (test helpers): `InspectMessage`, `InspectRecipientCount`,
  etc. (`inspect.go`)
- Subset export: `CopySubset` (SQLite-only, see below).

## Invariants

- **`message_bodies` is separated from `messages`** to keep the
  messages B-tree small. NEVER JOIN or scan it in list/aggregate/search
  queries — only direct PK lookup for single-message detail views.
  Hot-path queries use `subject`/`snippet` for non-FTS fallback.
  (`schema.sql:266`, `api.go:155`)
- **FTS5 is a virtual table joined by `rowid = m.id`** on SQLite;
  PostgreSQL stores `tsvector` on `messages.search_fts` directly, so its
  search join is empty. Always go through
  `dialect.FTSSearchClause()` — never write `messages_fts MATCH ?`
  inline. The dialect also returns `orderArgCount` so callers know how
  many extra binds the ORDER BY needs (SQLite: 0, Postgres: 1 for
  `ts_rank`).
- **`LiveMessagesWhere(alias, hideDeletedFromSource)`** is the canonical
  predicate for "visible messages." Always call it instead of writing
  `deleted_at IS NULL` by hand. Pre-allocated constants for `("",
  "m") × bool` keep hot paths allocation-free. (`live_messages.go`)
- **EXISTS over DISTINCT+JOIN.** SQL guideline already in CLAUDE.md;
  search/dedup queries follow this strictly.
- **`upsertMessageWith` uses RETURNING** with a SQLite < 3.35 fallback
  (`Exec`+`SELECT`) gated by `dialect.IsReturningError`.
- **WAL checkpoint on Close** unless `readOnly`. Read-only SQLite uses
  `_query_only=true` (NOT `mode=ro`) so WAL sidecars can still be
  managed. (`store.go:174`)
- **SQLite `MaxOpenConns=4`** for normal mode, `1` for `:memory:`
  (per-connection databases). Postgres pool is 25/5/5min.
  (`store.go:104`)
- **The default `"All"` collection is auto-managed** by
  `EnsureDefaultCollection` on every `InitSchema`; explicit mutations
  are rejected via `ErrCollectionImmutable`.
- **Encryption-at-rest is a placeholder.** `encryption_version INTEGER
  DEFAULT 0` columns exist on `attachments` and `message_raw`
  (`schema.sql:231`, `:280`) but no read/write code references them.
  Raw MIME is zlib-compressed only.

## Common gotchas

- `SQLiteDialect.Rebind` is a no-op; `PostgreSQLDialect.Rebind` walks
  the SQL char-by-char and is quote-safe. Never wrap `?` in
  `Sprintf("'?'")` etc.
- `LastInsertId()` is **not** supported by pgx — the
  `EnsureConversation`/`EnsureParticipant`/`StartSync`/etc. paths still
  use it. This is one of the listed PG blockers; the only place that
  has been converted to `RETURNING` is `upsertMessageWith`.
- 11 PRAGMAs are set via DSN parameters (`defaultSQLiteParams`), not
  `db.Exec`. Postgres equivalents go through `pgx` `RuntimeParams` so
  every pooled connection inherits them on the startup packet, not just
  the first one.
- `subset.go` reopens with `_foreign_keys=OFF` for bulk copy, runs
  `PRAGMA foreign_key_check` after, and bypasses the dialect entirely.
  It is **SQLite-only** by design.
- FTS5 build tag: `fts5` is in default build tags (Makefile). A binary
  built without it will fail with "no such module: fts5" on FTS init;
  `IsNoSuchModuleError` is what lets `InitSchema` continue gracefully.
- DDL migrations run as `ALTER TABLE ADD COLUMN ...` blocks, swallowing
  `IsDuplicateColumnError` per dialect. Append-only — do not reorder or
  remove rows in the migration list, or pre-existing DBs may skip a
  column. (`store.go:560`)
- Most `Store` methods still pass raw `?` to `s.db.Exec` — but
  `loggedDB.Exec` runs the dialect Rebind. The PG blocker list says
  "thread Rebind through" — that's only true for code paths that bypass
  `loggedDB`, mostly `subset.go` and a few helpers (see
  `docs/PG_STATUS.md`).
- `RemoveSource` does NOT GC orphan participants. Attachments cascade
  via FK; on-disk attachment files are content-addressed and outlive
  the row (cleanup is the caller's job — see
  `AttachmentPathsUniqueToSource` + `IsAttachmentPathReferenced`).

## When editing here

- Do: route every new query through the dialect for placeholders,
  timestamps, and conflict handling. Use
  `s.dialect.Now()`/`InsertOrIgnore[Prefix|Suffix]`/`FTSSearchClause`.
- Do: use `LiveMessagesWhere`. Do: use `queryInChunks`/`execInChunks`/
  `insertInChunks` for IN-list and multi-row inserts.
- Don't: write `datetime('now')` or SQLite-specific syntax in new
  queries unless the function is documented as SQLite-only (`subset.go`).
- Don't: touch `message_bodies` outside the single-message
  detail/inspection paths.
- Don't: add a SELECT-then-INSERT race for "create if missing" — use
  `dialect.InsertOrIgnore` + a follow-up SELECT (see
  `EnsureDefaultCollection`).
- Don't: rely on `LastInsertId` if you intend the code to ever run on
  Postgres; use `RETURNING id` like `upsertMessageWith`.

## Schema files

- `schema.sql` — canonical DDL. Targeted at SQLite syntax today
  (`INTEGER PRIMARY KEY` autoinc, `DATETIME`, `BLOB`). Both dialects
  load this via `SchemaFiles()`. PostgreSQL execution will require type
  translation (PG_STATUS blocker #1).
- `schema_sqlite.sql` — FTS5 virtual table only. SQLite-only. Loaded by
  `SchemaFTS()`.
- `schema_pg.sql` — `ALTER TABLE messages ADD COLUMN search_fts
  TSVECTOR` + GIN index. Loaded as `SchemaFTS()` for PostgreSQL.
- One-time DATA migrations live in the `applied_migrations` table
  (`migrations.go`). DDL is `CREATE/ALTER ... IF NOT EXISTS` so
  re-running `InitSchema` is safe.
