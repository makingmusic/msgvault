# Service layer: HTTP API, MCP, scheduler, remote

## Three faces of msgvault

The same single binary plays three roles depending on the command invoked:

1. **CLI** — one-shot commands (`init-db`, `add-account`, `sync-full`,
   `import-emlx`, `build-cache`, `export-token`, etc.). Direct
   read/write access to `msgvault.db` and the attachments + analytics
   directories.
2. **Daemon** (`msgvault serve`) — long-running process that exposes the
   archive over an authenticated HTTP API and runs scheduled syncs +
   embedding work in the background. Optional MCP server on a separate
   command (`msgvault mcp`).
3. **TUI client** (`msgvault tui`) — interactive Bubble Tea front-end.
   Picks between a *local* `query.Engine` (DuckDB-over-Parquet or
   SQLite) and a *remote* `query.Engine` (HTTP client) based on
   configuration.

The HTTP API is the shared contract: the daemon serves it, the remote
TUI consumes it via `internal/remote`, `export-token` POSTs to it
(`/api/v1/auth/token/{email}`, `/api/v1/accounts`), and any third-party
client can use the same surface with an API key.

## What runs in `msgvault serve`

`cmd/msgvault/cmd/serve.go runServe` (serve.go:59) is the entry point.
A single daemon process hosts:

- **HTTP API** (`internal/api.Server`) — bound to
  `cfg.Server.BindAddr:cfg.Server.APIPort` (defaults `127.0.0.1:8080`).
  Started in a goroutine (serve.go:217-222).
- **Scheduler** (`internal/scheduler.Scheduler`) — cron jobs for each
  enabled account in `cfg.Accounts`, plus an optional
  `EmbedJob` when vector search is enabled. Started before the API
  goroutine (serve.go:195).
- **Vector features** (optional, `-tags sqlite_vec`) — `vectors.db` is
  opened by `setupVectorFeatures` (serve_vector.go:24) and the
  resulting `Backend`, `HybridEngine`, `Worker`, `Enqueuer` flow into
  both the API server (`hybridEngine`, `backend`, `vectorCfg`) and the
  scheduler (`EmbedJob`). The same `vf.Enqueuer` is wired into each
  scheduled `Syncer` so newly ingested message IDs get queued for
  embedding (serve.go:397-399).
- **Query engine** — DuckDB over Parquet when
  `query.HasCompleteParquetData(analyticsDir)` and the cache is fresh,
  otherwise SQLite (serve.go:122-143). A single `query.Engine` is
  shared with the API server and (transitively) every API handler.

**MCP is not started by `serve`.** It is its own command
(`cmd/msgvault/cmd/mcp.go`). MCP shares the same on-disk database and
analytics directory, but runs in a separate OS process — typically
spawned per Claude Desktop session — and opens the SQLite store
read-only (mcp.go:48) so multiple concurrent MCP processes do not
contend on the write lock.

Shutdown order on SIGINT/SIGTERM (serve.go:243-271): graceful API
shutdown (10s budget) → `sched.Stop()` → wait up to 30s for in-flight
syncs and post-sync embed passes to drain.

## HTTP API surface

Mounted under `/api/v1` by `Server.setupRouter` (api/server.go:118).
Detailed handler list lives in `internal/api/CLAUDE.md`. Summary:

| Method | Path                              | Purpose                                             |
| ------ | --------------------------------- | --------------------------------------------------- |
| GET    | `/health`                         | No-auth liveness ping                               |
| GET    | `/api/v1/stats`                   | Archive totals + vector stats                       |
| GET    | `/api/v1/messages`                | Paginated list (page/page_size)                     |
| GET    | `/api/v1/messages/{id}`           | Detail (engine path returns body_html)              |
| GET    | `/api/v1/messages/{id}/inline`    | Serves CID-referenced inline image bytes            |
| GET    | `/api/v1/search?q=…&mode=…`       | `mode=fts` (default) / `vector` / `hybrid`          |
| POST   | `/api/v1/query`                   | Raw SQL against DuckDB views (engine must support)  |
| GET    | `/api/v1/aggregates`              | Top-N senders/domains/labels/time                   |
| GET    | `/api/v1/aggregates/sub`          | Sub-aggregate after drill-down                      |
| GET    | `/api/v1/messages/filter`         | Filtered list (sender/recipient/label/dates/etc.)   |
| GET    | `/api/v1/stats/total`             | Filtered totals + group_by                          |
| GET    | `/api/v1/search/fast`             | Metadata-only search + stats                        |
| GET    | `/api/v1/search/deep`             | FTS5 body search                                    |
| GET    | `/api/v1/accounts`                | Lists configured accounts + scheduler info          |
| POST   | `/api/v1/accounts`                | Adds a new `[[accounts]]` entry, registers cron     |
| POST   | `/api/v1/sync/{account}`          | Triggers an out-of-band sync                        |
| GET    | `/api/v1/scheduler/status`        | Scheduler running flag + per-account status         |
| POST   | `/api/v1/auth/token/{email}`      | Uploads OAuth token JSON to `~/.msgvault/tokens/`   |

Search-mode quirks:

- `mode=vector|hybrid` requires at least one free-text term
  (api/handlers.go:510). Filter-only queries must use `mode=fts`.
- Vector/hybrid responses do not paginate beyond `page=1`
  (api/handlers.go:421). Page size clamped to
  `vectorCfg.Search.MaxPageSizeHybridClamp()`.
- Vector errors translate to 503 with codes `vector_not_enabled`,
  `index_stale`, `index_building`, `embedding_timeout`
  (api/handlers.go:539-555).

## MCP surface

Tool catalog (registered in `internal/mcp/server.go newMCPServer`,
server.go:90). All names are passed verbatim to MCP clients:

| Tool name                  | Arguments                                                                 | Returns                                                       |
| -------------------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------- |
| `search_messages`          | `query` (req), `mode={fts,vector,hybrid}`, `account`, `limit`, `offset`, `explain` | `[]MessageSummary` (FTS) or `hybridSearchResponse` (vector/hybrid) |
| `get_message`              | `id` (req)                                                                | `MessageDetail` (body, recipients, attachments)               |
| `get_attachment`           | `attachment_id` (req)                                                     | Metadata text + base64 `BlobResourceContents` (≤ 50 MB)       |
| `export_attachment`        | `attachment_id` (req), `destination`                                      | `{path, filename, size}`                                      |
| `list_messages`            | `account`, `from`, `to`, `label`, `after`, `before`, `has_attachment`, `limit`, `offset` | `[]MessageSummary`                                            |
| `get_stats`                | none                                                                      | `{stats, accounts, vector_search?}`                           |
| `aggregate`                | `group_by={sender,recipient,domain,label,time}` (req), `account`, `limit`, `after`, `before` | `[]AggregateRow`                                              |
| `stage_deletion`           | EITHER `query` OR (`from`, `domain`, `label`, `after`, `before`, `has_attachment`) — never both, plus `account` | `{batch_id, message_count, status, next_step}` (manifest only) |
| `search_by_domains`        | `domains` (req, comma-sep), `limit`, `offset`, `after`, `before`          | `[]MessageSummary`                                            |
| `find_similar_messages`    | `message_id` (req), `limit`, `account`, `after`, `before`, `has_attachment` | `similarMessagesResponse` (only registered when `Backend != nil`) |

No MCP resources or prompts are exposed. Errors are returned as MCP
"tool error" results (`mcp.NewToolResultError`) with stable code
prefixes (`vector_not_enabled:`, `index_stale:`, `pagination_unsupported:`,
`missing_free_text:`, …) — see `translateVectorErr` (mcp/handlers.go:46).

`stage_deletion` only writes a manifest under `{dataDir}/deletions`
(`manifest.CreatedBy = "mcp"`). Actual deletion requires the operator
to run `MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged`
(mcp/handlers.go:1011). MCP cannot delete from Gmail directly.

## Auth model

| Where         | Mechanism                               | Notes                                                                                  |
| ------------- | --------------------------------------- | -------------------------------------------------------------------------------------- |
| HTTP API      | `cfg.Server.APIKey` via `X-API-Key` or `Authorization: Bearer <key>` | Constant-time compare (api/server.go:291). Empty `APIKey` disables auth (WARN logged). |
| Remote client | Sends `X-API-Key` only (remote/store.go:94) | Pulls from `cfg.Remote.APIKey`.                                                        |
| MCP (stdio)   | None — trust boundary is the local user | One process per Claude Desktop session.                                                |
| MCP (HTTP)    | None — `mcp --http` is loopback-only by default; `--http-allow-insecure` required for non-loopback (cmd/mcp.go:163) | Reverse proxy in front for any real exposure. |

`cfg.Server.ValidateSecure()` (config/config.go:50) refuses to start
the daemon when `BindAddr` is non-loopback, `APIKey` is empty, and
`allow_insecure` is false. Called from both `serveCmd.RunE`
(serve.go:61) and `Server.Start` (api/server.go:198).

`config.IsLoopback()` (config/config.go:39) treats `""`, `"localhost"`,
and any address resolving to `IsLoopback()` as loopback — covers the
full `127.0.0.0/8` range and IPv6 `::1`.

`POST /api/v1/auth/token/{email}` writes tokens with mode 0600 via
`fileutil.SecureChmod` plus an atomic temp-file rename
(api/handlers.go:795-824). Email is path-sanitized; if the cleaned
path escapes `tokensDir`, a sha256 fallback name is used
(api/handlers.go:834).

## Local-vs-remote TUI selection

`cmd/msgvault/cmd/tui.go:62-80` — the TUI uses a `remote.Engine` when:

- `cfg.Remote.URL != ""`, **and**
- `--local` flag is not passed.

Otherwise it opens the local SQLite store, runs schema migrations,
optionally builds the Parquet cache (unless `--skip-cache-build`), and
selects DuckDB or SQLite as the local engine.

Remote mode disables features that require local storage: deletion
execution, attachment export, and any path that calls `GetMessageRaw` /
`GetAttachment` / `SearchByDomains` (remote engine returns
`remote.ErrNotSupported` for those — engine.go:574, 580, 706, 707).

## Threading / concurrency

- HTTP server runs `chi` handlers concurrently. Per-request middleware
  stack: RequestID → logger → Recoverer → chi `Timeout` (gentle, 60s
  default) → CORS → per-IP rate limit (10 rps, burst 20). The chi
  timeout fires before the underlying `http.Server.WriteTimeout`
  (`requestTimeout + 5s`, api/server.go:213-225) so callers get a
  structured 503 instead of a torn TCP connection.
- Scheduler: per-account `running` flag prevents overlap; cross-account
  syncs run concurrently in cron's worker pool. Embed job uses
  `sync.Mutex.TryLock` — drop-not-queue. See
  `internal/scheduler/CLAUDE.md`.
- API token-upload endpoint takes `cfgMu` (sync.RWMutex) when adding
  accounts (api/server.go:64, api/handlers.go:895-926). Reads also
  copy the `cfg.Accounts` slice under the read lock to avoid racing
  with concurrent adds.
- MCP: each tool call runs synchronously on the goroutine the
  transport hands it. The server has no global mutexes; concurrency is
  bounded by the transport (stdio is single-threaded; StreamableHTTP
  runs goroutines per request).

## Security considerations

- Default `BindAddr=127.0.0.1` (config/config.go:249). Explicit opt-in
  required to expose the daemon on a LAN.
- CORS off by default — configure `cors_origins` to enable. `*` is
  accepted as a wildcard (middleware.go:42).
- Rate limit is per-source-IP only; behind a proxy you'll need to add
  `X-Forwarded-For` handling (currently uses
  `r.RemoteAddr`, middleware.go:144).
- OAuth tokens uploaded via the API land at 0600 inside a 0700
  tokens dir (`fileutil.SecureMkdirAll`, api/handlers.go:779).
- `mcp --http` has no built-in auth; loopback bind is enforced unless
  `--http-allow-insecure` is set (cmd/msgvault/cmd/mcp.go:163-197).
- See `SECURITY.md` for the broader threat model — notably, the
  database is **not** encrypted at rest.

TODO(verify): the API rate-limit threshold (10 rps / burst 20) is
hard-coded in `setupRouter` (api/server.go:149); not exposed via
config today.

## Test surface

- `internal/api/server_test.go` and `handlers_test.go` exercise the
  routing, auth, CORS, rate limit, and every handler against an
  in-memory store + a stub `query.Engine`. The test that locks the
  chi-timeout-fires-first contract is
  `TestHandleSearch_HybridEmbeddingTimeoutFiresChi`.
- `internal/api/middleware_test.go` covers CORS preflight, the per-IP
  rate-limiter, and TTL eviction.
- `internal/mcp/server_test.go` exercises every tool with a fake
  engine + fake vector backend.
- `internal/scheduler/scheduler_test.go` covers add/remove,
  start/stop, per-account locking, `TriggerSync` contention, and
  `EmbedJob` activation gating.
- `internal/remote/engine_test.go` and `store_test.go` round-trip
  every method against a `httptest.NewServer` that emits the matching
  JSON shape.
- `cmd/msgvault/cmd/serve_test.go` and `mcp_test.go` cover CLI
  bootstrapping, including `normalizeMCPHTTPAddr` loopback
  enforcement.
