# internal/vector — Semantic + hybrid search

Vector and hybrid (BM25 + ANN) search over the email corpus. Embeddings
come from an external OpenAI-compatible endpoint; vectors live in their
own SQLite database (`~/.msgvault/vectors.db`) using the
[sqlite-vec](https://github.com/asg017/sqlite-vec) extension. Spec:
`docs/superpowers/specs/2026-04-19-vector-search-design.md`.

## Layout

```
internal/vector/
├── backend.go       Backend, FusingBackend, Filter, Hit, FusedHit, FusedRequest
├── config.go        TOML [vector] config + ApplyDefaults + Validate
├── generations.go   ResolveActive / ResolveActiveForFingerprint
├── stats.go         CollectStats — JSON view for /api/v1/stats and MCP
├── env.go           lookupEnv var (so tests can stub os.Getenv)
├── errors.go        Sentinel errors (ErrIndexStale, ErrIndexBuilding, ...)
│
├── embed/           Embedding pipeline
│   ├── client.go        HTTP client to OpenAI-compatible endpoint
│   ├── preprocess.go    Subject prefix, quote/signature stripping, char-cap truncation
│   ├── queue.go         pending_embeddings claim/complete/release/reclaim
│   ├── enqueue.go       Sync hook — inserts new message IDs into every non-retired generation
│   └── worker.go        RunOnce — pulls batches, embeds, upserts, reports progress
│
├── hybrid/          Search-side orchestration
│   ├── engine.go        Mode dispatch (vector|hybrid), embed query, call FusingBackend
│   ├── filter.go        BuildFilter: parse search.Query → vector.Filter (resolves IDs)
│   └── rrf.go           Fuse: RRF + subject-boost (used in non-fused fallback path)
│
└── sqlitevec/       Backend implementation (build tag: sqlite_vec)
    ├── ext.go / ext_stub.go    RegisterExtension (sqlite-vec auto-extension)
    ├── migrate.go              Apply schema.sql + EnsureVectorTable (vec0 virtual table)
    ├── schema.sql              index_generations, embeddings, pending_embeddings, embed_runs
    ├── backend.go              CreateGeneration, Activate, Retire, Upsert, Search, Stats
    └── fused.go                FusedSearch — single-CTE BM25+ANN+filters
```

## Build tag

`sqlitevec` is gated by `//go:build sqlite_vec`. Without the tag,
`ext_stub.go` reports `ErrNotBuilt` and `Available()=false`. The release
Makefile sets `-tags "fts5 sqlite_vec"`. CLI commands that need the
extension have `_stub.go` companions (`embed_vector_stub.go`,
`search_vector_stub.go`) that fail with a clear "rebuild with -tags"
message.

## Generations and lifecycle

`schema.sql` defines `index_generations` with at most one row each in
states `building` and `active` (enforced by partial unique indexes).
Lifecycle (matching the spec):

1. `CreateGeneration(model, dim)` — inserts a `building` row, then runs
   `seedPending` to enqueue every currently-embeddable message into
   `pending_embeddings`. The seed phase is committed *after* the row
   insert so a concurrent `Enqueuer` driven by sync immediately sees the
   new generation and dual-enqueues newly-synced messages.
   `seeded_at` is stamped at the end of the seed pass; if a crash
   happens before that, `EnsureSeeded` re-runs the (idempotent
   `INSERT OR IGNORE`) seed on the resume path so we never activate an
   empty index (`backend.go:80-100`, `embed_vector.go:200-228`).
2. Worker drains the queue (`embed/worker.go:RunOnce`) — claim batch →
   load message text → preprocess → call `Client.Embed` → `Upsert` into
   the dim-specific `vectors_vec_d<N>` table → `Queue.Complete`.
3. `ActivateGeneration` flips the row to `active` (atomic swap).
4. `RetireGeneration` marks an old generation `retired` so the
   `Enqueuer` stops dual-enqueuing into it.

`Enqueuer.EnqueueMessages` (sync hook) inserts each new message ID into
every non-retired generation (`enqueue.go:30-91`). When a rebuild is
in flight, that means active + building both stay current. Bulk-insert
uses a single `INSERT OR IGNORE … FROM json_each(?)` per generation to
avoid starving concurrent claims with vectors.db lock contention.

## Backend abstraction

`Backend` (`backend.go:114`) is the minimum contract. `FusingBackend`
(`backend.go:161`) is an optional capability — the hybrid engine type-
asserts and rejects hybrid mode if the backend is non-fusing
(`hybrid/engine.go:144`). The MVP only ships the sqlite-vec backend; a
"lance" backend is reserved in `Config.Backend` validation
(`config.go:117`).

## Vector storage

`vectors_vec_d<dim>` is a sqlite-vec `vec0` virtual table partitioned by
`generation_id` (`migrate.go:62-66`). One vec0 table per dimension means
a re-embedding to a different model+dim creates a new generation in a
distinct table; old vectors stay valid until retired.

## Distance metric

The `embedding MATCH` operator returns `distance` (sqlite-vec default is
**L2** unless the column was declared otherwise; `EnsureVectorTable`
declares `FLOAT[N]` with no metric override, so it's L2 distance). The
fused query converts to a similarity score with `1.0 - v.vec_dist`
(`fused.go:181`). TODO(verify): with default L2, `1 - dist` is not a
true cosine similarity; it's only meaningful for ranking purposes within
one query. The score is exposed in `--explain` output but never used for
fusion (RRF uses ranks only).

## RRF fusion

`hybrid/rrf.go:Fuse` is the standalone reference implementation
(retained for tests / non-fused fallback). The shipping path is
`sqlitevec.FusedSearch` (`fused.go`), which runs BM25 (FTS5) + ANN +
filters in a single CTE and computes
`1/(k+r_bm25) + 1/(k+r_ann)` server-side.

Subject boost: when the message's subject contains any of the lowercased
query terms, RRF score is multiplied by `SubjectBoost` (default 2.0,
`config.go:166`). Configurable; pass 1.0 to disable.

Pool saturation: each side over-fetches `KPerSignal+1` rows; if either
hits the cap, `saturated=true` is surfaced via `ResultMeta.PoolSaturated`
and as `pool_saturated` in the JSON response.

## Filter semantics

`vector.Filter` (`backend.go:69-81`) is pre-resolved at the Go layer
(addresses → participant IDs, labels → label IDs) so the SQL deals only
in integers. Repeated same-field operators are AND-of-OR groups: each
inner slice is one search-token resolution; backends emit one EXISTS
clause per group, AND'd together. `from:alice from:bob` requires both
tokens to match different (or the same) `from` rows on a single
message. See `hybrid/filter.go:BuildFilter` and `vector/backend.go:48-66`.

## Errors and error mapping

`errors.go` defines the sentinel set. Notable mapping rules:

- `ErrEmbeddingTimeout` — wraps `context.DeadlineExceeded` from the
  embed call so HTTP/MCP can return 503 instead of 500
  (`hybrid/engine.go:120`).
- `ErrIndexStale` — config fingerprint differs from the active
  generation's fingerprint. User must rebuild or revert config.
- `ErrIndexBuilding` — first-ever build is in progress; no active yet.
- `ErrNotEnabled` — no generation exists at all.
- `ErrPaginationUnsupported` — vector/hybrid modes only support
  single-page results; the CLI rejects `--offset` upfront
  (`search.go:85`).

## When editing

- Filter additions must be plumbed through `BuildFilter` (Go-side
  resolution) AND the four `*GroupClauses` helpers in `sqlitevec/fused.go`
  AND the corresponding CTE filter conditions. Silent dropping is hard
  to spot.
- Schema changes: edit `schema.sql` AND add an idempotent ALTER in
  `migrate.go` (the `seeded_at` precedent shows the pattern).
- New sentinel errors go in `errors.go` with documentation of how
  callers should map them.
