# internal/query — Query engine

Backend-agnostic query layer for msgvault. Powers TUI aggregates, MCP, the
HTTP API, and CLI search/list. See `DESIGN.md` for the original rationale
(Engine interface, Parquet schema, hybrid SQLite/DuckDB approach) — this
file documents the current shape and gotchas.

## Engine interface

`Engine` (`engine.go:14`) is the consumer-facing contract. It exposes:

- Aggregates: `Aggregate`, `SubAggregate` (drill-down)
- Lists/details: `ListMessages`, `GetMessage`, `GetMessageBySourceID`,
  `GetAttachment`, `GetMessageRaw`, `GetMessageSummariesByIDs` (bulk
  hydration to avoid the per-hit N+1 from `GetMessage`)
- Search: `Search` (FTS5 over body), `SearchFast` (metadata-only),
  `SearchFastCount`, `SearchFastWithStats` (single materialized scan
  reused for paginated rows + count + stats)
- Misc: `GetGmailIDsByFilter`, `SearchByDomains`, `ListAccounts`,
  `GetTotalStats`

`TextEngine` (`text_engine.go`) is a parallel, narrower interface for the
SMS/iMessage/WhatsApp Texts mode; both `DuckDBEngine` and `SQLiteEngine`
implement it. Kept separate so remote/MCP/mock layers don't need to grow
text-mode methods.

## Backends

| Backend           | File                  | Purpose                                                                   |
| ----------------- | --------------------- | ------------------------------------------------------------------------- |
| `SQLiteEngine`    | `sqlite.go` (1.6k LOC)| Always-available fallback. Handles FTS5, full message detail, body fetch. |
| `DuckDBEngine`    | `duckdb.go` (2.5k LOC)| Default for TUI/MCP/serve when Parquet cache exists. Hybrid (see below).  |
| `PostgreSQLEngine`| `postgres.go`         | Scaffold only — most methods return `ErrNotImplemented`. See `docs/PG_STATUS.md`. |

Selection happens in callers, not in this package: `cmd/msgvault/cmd/tui.go:124`,
`mcp.go:72`, `serve.go:124` all do
`if !forceSQL && query.HasCompleteParquetData(analyticsDir) { NewDuckDBEngine(...) }
else { NewSQLiteEngine(...) }`.

`DESIGN.md` mentions a `RemoteEngine` — that lives in `internal/remote`,
not here, and is wired in `tui.go:73` via `remote.NewEngine(...)`.

### DuckDB hybrid approach

`DuckDBEngine` runs Parquet for fast aggregates and routes message-detail
and FTS work to SQLite. Two SQLite paths exist (`duckdb.go:21-29`):

1. DuckDB's `sqlite_scanner` extension (Linux/macOS) — `ATTACH 'msgvault.db'
   AS sqlite_db (TYPE sqlite, READ_ONLY)`, queries use `sqlite_db.<table>`.
2. Direct SQLite via the embedded `*SQLiteEngine` field (`sqliteEngine`)
   — used on Windows (no MinGW build of sqlite_scanner) and when
   `DuckDBOptions.DisableSQLiteScanner` is set for testing.

`hasSQLiteScanner` records which mode is active. The shared helpers in
`shared.go` accept a `tablePrefix` arg ("" or `"sqlite_db."`) so the same
SQL serves both paths.

The engine also caches a single search result in a temp table
(`searchCacheTable`) so paginated calls for the same query don't re-scan
Parquet (`duckdb.go:47-57`). The cache is keyed on conditions+args.

### Parquet schema and views

Parquet files live under `~/.msgvault/analytics/`, with `messages/`
hive-partitioned by `year=`. Built by `cmd/msgvault/cmd/build_cache.go`
(see `docs/subsystems/query-and-search.md`). `cacheSchemaVersion` in
build_cache.go is bumped when columns change to force a full rebuild.

`views.go` registers DuckDB views over the Parquet glob:

- Base views: `messages`, `participants`, `message_recipients`, `labels`,
  `message_labels`, `attachments`, `conversations`, `sources`.
- Convenience views: `v_messages` (sender resolved via dual-path:
  `message_recipients` for email or `messages.sender_id` for chat),
  `v_senders`, `v_domains`, `v_labels`, `v_threads`.

`probeColumns` (`views.go:26`) reads a `DESCRIBE SELECT *` against each
Parquet table at startup so older caches missing newly-added columns
(`phone_number`, `attachment_count`, `sender_id`, `message_type`,
`title`, `conversation_type`, `source_type`) get sane defaults via
`COALESCE`/`NULL::BIGINT` instead of failing the query.

`SQLQuerier` (`views.go:19`) is satisfied only by `DuckDBEngine.QuerySQL`;
the API exposes it at `internal/api/handlers.go:954` for MCP raw-SQL
queries.

### Text-search vs aggregate separation

`text_engine.go` / `text_models.go` exist because the Texts mode has
different shapes (conversations as first-class, contacts instead of
domains, no labels in some sources). The structural code lives in
`sqlite_text.go` and `duckdb_text.go`; both are compile-time-asserted to
implement `TextEngine`.

### encoding_hint.go

DuckDB rejects Parquet files containing invalid UTF-8 with a specific
error message. `IsEncodingError` matches the substring;
`HintRepairEncoding` wraps the error with a hint to run
`msgvault repair-encoding`. Used at the engine boundary in MCP/TUI so
end-users get an actionable suggestion instead of an opaque DuckDB error.

### source_filter.go

Single helper that turns `SourceID *int64` and `SourceIDs []int64` into a
SQL fragment + args. `SourceIDs` (multi, used for collections) takes
precedence; an empty-but-non-nil slice produces `1=0` — a deliberate
"match-nothing" so a collection with zero members returns nothing rather
than everything.

## Gotchas

- **Never JOIN `message_bodies`** in list/aggregate/search queries. The
  table is split off precisely to keep the messages B-tree fast. Direct
  PK lookup only (see `shared.go:282`). Same rule from `CLAUDE.md`.
- **Never JOIN `message_raw`** in list queries — same reasoning, blob
  data. Direct PK lookup in `getMessageRawShared`.
- **Never `SELECT DISTINCT` with a JOIN.** Use `EXISTS` subqueries (also
  in `CLAUDE.md`). The vector path explicitly does this for repeated
  same-field operators (e.g. `from:alice from:bob`); see
  `vector/backend.go:48-66` for the AND-of-OR semantics.
- **Liveness filtering**: `store.LiveMessagesWhere("m", true)` excludes
  dedup losers (`deleted_at`) and source-deleted rows
  (`deleted_from_source_at`). Apply at every list/search entry; the raw
  body fetch in `shared.go:192` enforces it server-side too.
- **Email-only filter**: `emailOnlyFilterM` / `emailOnlyFilterMsg` in
  `shared.go:18-22` keep email aggregates from picking up SMS/chat rows;
  the Texts mode has its own filter (`textTypeFilter`).
- **DuckDB over `sqlite_scan` does NOT use SQLite indexes** — see
  `build_cache.go:147` (max-id check uses direct SQLite for that
  reason). Be wary of putting hot paths through `sqlite_db.<table>`.
- **`DuckDBEngine` uses `db.SetMaxOpenConns(1)`** because session
  settings (threads, ATTACH) don't propagate across pooled connections.

## When editing

- Adding a new `Engine` method: extend the interface in `engine.go`,
  implement in `sqlite.go` AND `duckdb.go`, return `ErrNotImplemented`
  in `postgres.go`, and add a `MockEngine` field in
  `internal/api/querytest`.
- Adding a Parquet column: bump `cacheSchemaVersion` in
  `build_cache.go`, add the column to the COPY query and to both view
  registration paths (`views.go` and `parquetCTEs` in `duckdb.go:273`).
- Avoid duplicating SQL — the helpers in `shared.go` exist precisely to
  share between the two SQLite paths.

## Never

- Do not call `GetMessage` in a loop over search/list results — use
  `GetMessageSummariesByIDs` (the contract is documented at
  `engine.go:36`).
- Do not add raw `SELECT DISTINCT … JOIN` aggregates.
- Do not introduce new public fields on `MessageFilter` without
  threading them through both `SQLiteEngine.buildFilterConditions` and
  `DuckDBEngine.buildFilterConditions` — silent skips become very hard
  to spot.
