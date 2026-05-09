# Query and search subsystem

This is the architectural overview of msgvault's read-side: how messages
are retrieved, aggregated, and searched once they're in the local
archive. The write side (Gmail sync, MIME parsing, store schema) is
covered separately.

Three packages collaborate:

- `internal/query` — the `Engine` interface and its three backends
  (DuckDB-over-Parquet, SQLite, PostgreSQL scaffold). Aggregates,
  list/detail, FTS5 search, raw SQL.
- `internal/search` — Gmail-syntax query parser. Converts a string into
  a structured `*search.Query`.
- `internal/vector` — embeddings, vector index (sqlite-vec), and the
  hybrid (BM25 + ANN) search engine. Build-tag gated.

A fourth piece — `cmd/msgvault/cmd/build_cache.go` — is the ETL that
turns the SQLite system-of-record into the Parquet analytics cache
DuckDB queries.

See package-level docs for implementation detail:

- `internal/query/CLAUDE.md`
- `internal/query/DESIGN.md` (original design rationale)
- `internal/search/CLAUDE.md`
- `internal/vector/CLAUDE.md`

## Storage layout

```
~/.msgvault/
├── msgvault.db              SQLite — system of record
│   • messages, message_bodies (split off the messages B-tree),
│     message_raw (zlib MIME), participants, labels, attachments,
│     conversations, sources, sync_runs, sync_checkpoints
│   • messages_fts — FTS5 virtual table
├── analytics/               Parquet — denormalized analytics cache
│   ├── messages/year=*/*.parquet      (hive-partitioned)
│   ├── participants/*.parquet
│   ├── message_recipients/*.parquet
│   ├── labels/*.parquet
│   ├── message_labels/*.parquet
│   ├── attachments/*.parquet
│   ├── conversations/*.parquet
│   ├── sources/*.parquet
│   └── _last_sync.json                (incremental ETL state)
├── vectors.db               SQLite — embeddings (sqlite-vec)
│   • index_generations, embeddings, pending_embeddings, embed_runs
│   • vectors_vec_d<dim>     vec0 virtual table, partitioned by generation_id
└── attachments/             Content-addressed blob store
```

## Engine selection

The `Engine` interface (`internal/query/engine.go:14`) is implemented by
three backends:

| Backend           | Used by                                    | When |
| ----------------- | ------------------------------------------ | ---- |
| `SQLiteEngine`    | always-available fallback                  | small archives, when Parquet cache is missing/stale, when `--force-sql`/`MCP_FORCE_SQL` is set |
| `DuckDBEngine`    | TUI, MCP, `serve` HTTP API                 | default when `query.HasCompleteParquetData` returns true |
| `PostgreSQLEngine`| scaffold; most methods `ErrNotImplemented` | future, see `docs/PG_STATUS.md` |
| `remote.Engine`   | TUI when `[remote].url` is configured      | Talks HTTP to a remote `msgvault serve`. Disables deletion + attachment export. (Lives outside `internal/query`.) |

Selection is duplicated across CLI commands (one each in `tui.go:124`,
`mcp.go:72`, `serve.go:124`):

```go
if !forceSQL && query.HasCompleteParquetData(analyticsDir) {
    engine = NewDuckDBEngine(...)
} else {
    engine = NewSQLiteEngine(s.DB())
}
```

`HasCompleteParquetData` (`duckdb.go:1818`) checks every entry in
`RequiredParquetDirs` — DuckDB unconditionally reads all tables and
fails at runtime if any are missing.

`DuckDBEngine` itself is hybrid: aggregates run over Parquet, but
message detail and FTS search route to SQLite, either through DuckDB's
`sqlite_scanner` extension (Linux/macOS) or through a separately-held
`*SQLiteEngine` (Windows, where sqlite_scanner has no MinGW build, or
when `DuckDBOptions.DisableSQLiteScanner` is set for tests).

## Aggregate analytics flow

```
Gmail API ─sync→ msgvault.db ─build_cache→ analytics/*.parquet ─DuckDB→ TUI/MCP/HTTP
                              (incremental, partitioned)
```

1. **Sync** writes to SQLite (`internal/sync/sync.go`). One row per
   message; bodies into `message_bodies`, raw MIME zlib-compressed into
   `message_raw`, attachments deduped by content-hash.
2. **build-cache** (`cmd/msgvault/cmd/build_cache.go`) opens DuckDB and
   ATTACHes the SQLite DB read-only via the sqlite extension. It runs
   `COPY (SELECT … FROM sqlite_db.<table>) TO 'analytics/.../...parquet'
   (FORMAT PARQUET, COMPRESSION 'zstd')` for each table. Messages are
   `PARTITION_BY (year)`; junction tables are sharded with unique
   filenames per incremental batch (`incr_<lastID>.parquet`) because
   Parquet files cannot be appended in place.
   - `cacheSchemaVersion` (currently 5) is bumped whenever columns are
     added/removed; a mismatch on read forces a full rebuild
     (`build_cache.go:131`).
   - The max-message-id check uses **direct SQLite**, not DuckDB's
     sqlite extension, because the sqlite extension does not use SQLite
     indexes (`build_cache.go:147`).
   - Concurrent calls are serialized by `buildCacheMu` so the scheduler
     can fire syncs for multiple accounts in parallel without
     corrupting `_last_sync.json` or partition directories.
   - On Windows, where the sqlite_scanner extension is unavailable,
     `setupSQLiteSource` falls back to CSV intermediates.
3. **Read path**: `DuckDBEngine` opens an in-memory DuckDB
   (`SetMaxOpenConns(1)` so session settings stick), `INSTALL/LOAD
   sqlite`, ATTACHes `msgvault.db AS sqlite_db` read-only, registers
   views over the Parquet glob (`views.go:RegisterViews`), and is
   ready. Aggregates are built on top of CTEs over the Parquet files;
   list and detail queries go through SQLite via `sqlite_db.<table>`.
   Results for a single search are cached in a temp table keyed on
   `(conditions, args)` so paginated calls don't re-scan Parquet
   (`duckdb.go:47-57`, `searchCacheTable`).

### Performance claim

`DESIGN.md` quotes a "~3000x faster" speedup for aggregates. That number
is from the original design doc, not benchmarked here. `benchmark_test.go`
is in the package but doesn't ship a current measurement. The
qualitative reason is real: Parquet aggregates avoid SQLite JOINs,
read only the columns they touch, and DuckDB parallelizes via
`SET threads = GOMAXPROCS(0)` (`duckdb.go:99`). TODO(verify): if a number
is needed for the docs, re-run the benchmark.

## Text search flow

Two CLI surfaces:

- `msgvault search <query>` (CLI) — `cmd/msgvault/cmd/search.go`.
- HTTP API + MCP server-side — `internal/api/handlers.go` and the MCP
  setup in `cmd/msgvault/cmd/mcp.go`.

Both use `search.Parse` to build a `*search.Query`, then dispatch by
`--mode`:

- `fts` (default) → `engine.Search` over FTS5 (or LIKE fallback when
  FTS5 isn't compiled in).
- `vector` → pure ANN against the active generation.
- `hybrid` → fused BM25 + ANN via the sqlite-vec backend's single-CTE
  query.

### FTS5 path

`SQLiteEngine.Search` (and `DuckDBEngine.Search`, which falls through to
the embedded `*SQLiteEngine` for FTS) builds an FTS5 `MATCH` query from
`Query.TextTerms` and folds in operator filters as additional WHERE
predicates (date bounds, sender, label, etc).

The FTS5 index is the `messages_fts` virtual table; `hasFTSTable`
caches presence after first check (`sqlite.go:35-60`). When FTS5 is
absent (e.g. `mattn/go-sqlite3` built without the `fts5` tag) the path
falls back to subject + sender LIKE.

### Vector + hybrid path

Activated by `--mode=vector|hybrid` and only available in
`-tags sqlite_vec` builds. The CLI:

1. `search.Parse(queryStr)` → `*search.Query`.
2. `hybrid.BuildFilter(ctx, mainDB, q)` — resolves
   address/label tokens to participant_id/label_id at the Go layer
   (substring `LIKE` against `participants.email_address`, exact match
   on `labels.name`).
3. Open `sqlitevec.Backend`, ATTACH `vectors.db` to a fresh main-DB
   connection, resolve the active generation
   (`vector.ResolveActiveForFingerprint`).
4. Embed the free-text via `embed.Client` (OpenAI-compatible
   endpoint).
5. `vector` mode: backend.Search(ANN k-NN). `hybrid` mode: type-assert
   to `vector.FusingBackend` and call `FusedSearch`.
6. Hydrate results from main DB (`hydrateHybridResults`); RRF order is
   preserved.

The fused CTE (`internal/vector/sqlitevec/fused.go:128`) materializes a
`filtered` set first, then runs BM25 and ANN over it, then full-outer-
joins on message_id with RRF score
`1/(k+rnk_bm25) + 1/(k+rnk_ann)`. Subject-boost is applied later in
hybrid.Engine via `Fuse` only in the non-fused fallback path; when
`FusingBackend` is used, the boost is computed in Go after the SQL
returns (TODO(verify): the fused CTE itself doesn't apply boost — it's
the `hybrid/rrf.go:Fuse` reference path that does).

Pool saturation: each per-signal CTE pulls `KPerSignal+1` and reports
the count via subqueries; if either side filled its pool the caller
gets `saturated=true` so users see `pool_saturated` in JSON output.

## Search query syntax

Parsed by `internal/search/parser.go:Parse`. See
`internal/search/CLAUDE.md` for full operator list. Examples:

```
from:alice@example.com has:attachment
subject:meeting after:2024-01-01
project report newer_than:30d
"exact phrase" label:INBOX
from:@example.com larger:5M before:2024-06
```

Notable behaviors:

- Bare domain shorthand: `from:example.com` is rewritten to
  `from:@example.com` only when the suffix matches a known TLD
  (`parser.go:75-110`). Unrecognized TLDs need explicit `@`
  (`from:@brand.pizza`).
- Unknown operators fall through to free text — `foo:bar` becomes a
  text term, not silently dropped.
- `Parser.Now` is injectable for deterministic tests.
- Caller injects `AccountIDs` (from `--account` / `--collection`) and
  `HideDeleted` after parsing — the parser doesn't know about scope.

## Vector search lifecycle

### Backfill (full rebuild)

```
msgvault build-embeddings --full-rebuild
```

`pickEmbedGeneration` (`embed_vector.go:168`) resolves which generation
to drain:

- `--full-rebuild`: confirm prompt, then `CreateGeneration(model, dim)`
  — inserts a `building` row, seeds `pending_embeddings` with every
  embeddable message ID, stamps `seeded_at`.
- Default mode with a building generation matching the configured
  fingerprint: resume it. `EnsureSeeded` re-runs the seed if the
  previous attempt crashed before stamping `seeded_at`.
- Default mode with no building, but an active generation matching the
  fingerprint: top up the active index.
- Mismatched in-flight build with a different fingerprint:
  `ErrBuildingInProgress` — user must retire or activate it first.

The worker (`embed/worker.go:RunOnce`) loops:

1. `Queue.Claim` — atomic `UPDATE pending_embeddings SET claimed_at, claim_token`.
2. `loadMessageText` → `Preprocess` (subject prefix, optional quote/sig
   strip, char-cap truncation).
3. `Client.Embed` — POST to OpenAI-compatible endpoint with batched
   inputs.
4. `Backend.Upsert` — write to `vectors_vec_d<dim>` and `embeddings`,
   bump `message_count`.
5. `Queue.Complete` — delete claimed rows.
6. Periodic `Progress` callback for stderr output.

Stale claims (from a crashed worker) are reclaimed by
`Queue.ReclaimStale` using a threshold derived from
`EmbedTimeout × EmbedMaxRetries` with a 10-minute floor.

When the queue drains to zero AND we were targeting a building
generation, the CLI calls `Backend.ActivateGeneration` — atomic state
swap from `building` to `active`.

### Generation rotation

Sync drives `Enqueuer.EnqueueMessages` (`embed/enqueue.go:30`), which
inserts each new message into **every non-retired generation**. While a
rebuild is in flight, both active + building stay current. When the
rebuild activates, the operator manually retires the old generation
(or it's left as an inactive sibling).

### Missing-embedding handling

Search and hydration tolerate races:

- Hydration of hybrid hits (`search_vector.go:hydrateHybridResults`)
  drops messages that were soft-deleted between rank and hydrate, with
  a warning log. The result list may be shorter than the ranked hits.
- The fused CTE itself filters `LiveMessagesWhere("m", true)` so dedup
  losers and source-deleted rows never reach BM25/ANN scoring.
- Worker drops messages that have no embeddable text (empty body after
  preprocess) by `Queue.Complete` without `Upsert`, advancing the queue.

## Failure modes

| Symptom                                                    | Cause                                                                | Mitigation                                                |
| ---------------------------------------------------------- | -------------------------------------------------------------------- | --------------------------------------------------------- |
| TUI/MCP boot logs "No cache data available"                | `analytics/` dirs absent or incomplete                              | `msgvault build-cache`                                    |
| TUI logs "Cache schema version mismatch"                   | `cacheSchemaVersion` bumped in a release                            | Auto-triggers full rebuild on next `build-cache`         |
| DuckDB error "Invalid string encoding found in Parquet"    | corrupt UTF-8 in source data                                        | `query.HintRepairEncoding` wraps with hint to run `msgvault repair-encoding` (`encoding_hint.go`) |
| `sqlite_scanner extension unavailable`                     | Windows or DisableSQLiteScanner option                              | Logged warning, falls through to direct SQLite path; results identical |
| Search returns "vector search not enabled"                 | `[vector].enabled = false` or no `[vector.embeddings]` in config    | Configure or fall back to `--mode=fts`                    |
| `ErrNotEnabled` from hybrid engine                         | No generation in `index_generations`                                | Run `msgvault build-embeddings --full-rebuild`            |
| `ErrIndexBuilding`                                         | Building exists but no active yet (first build)                     | Wait for build to drain; then auto-activates              |
| `ErrIndexStale`                                            | Configured model:dim differs from active generation's fingerprint   | Either revert config or `--full-rebuild` to swap generations |
| `ErrEmbeddingTimeout`                                      | Embed endpoint slower than request context                          | Mapped to 503 by HTTP/MCP; client retries                 |
| `ErrPaginationUnsupported`                                 | `--offset > 0` with `--mode=vector|hybrid`                          | CLI rejects upfront; UI hides pagination in those modes   |
| `ErrBuildingInProgress`                                    | Two builds with different fingerprints requested                    | Retire/activate the existing building generation first    |
| Hybrid result count < ranked hits                          | Soft-delete race between ranking and hydration                      | Logged at warn level; not user-actionable                 |

## Test surface

- `internal/query`:
  - `sqlite_aggregate_test.go`, `sqlite_crud_test.go`,
    `sqlite_search_test.go`, `sqlite_injection_test.go`
  - `duckdb_test.go` (3.5k LOC), `views_test.go`,
    `text_search_live_test.go`
  - `benchmark_test.go` — micro-benchmarks (no live perf assertions)
  - `source_filter_test.go`, `encoding_hint_test.go`
  - `testfixtures_test.go` — shared sample data
  - `postgres_test.go` — scaffolded; expected to fail until PG_STATUS
    blockers are resolved
- `internal/search`:
  - `parser_test.go` — operator parsing, tokenizer edge cases,
    relative-date math via injected `Now`.
- `internal/vector`:
  - `config_test.go`, `generations_test.go`, `stats_test.go`
  - `embed/{queue,enqueue,worker,client,preprocess}_test.go`
  - `hybrid/{engine,filter,rrf}_test.go`
  - `sqlitevec/{backend,fused,migrate,ext}_test.go` — gated by
    `-tags sqlite_vec`.
- Cache build integration:
  `cmd/msgvault/cmd/build_cache_test.go`,
  `build_cache_messenger_test.go`.
