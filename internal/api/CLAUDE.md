# internal/api

HTTP API server for `msgvault serve`. Mounted by `cmd/msgvault/cmd/serve.go`
into the daemon process; also driven directly in tests via `Server.Router()`.

## Composition

- `server.go` — `Server`, `ServerOptions`, `NewServer`, `NewServerWithOptions`,
  `Start`, `Shutdown`, `setupRouter` (server.go:118), `authMiddleware`
  (server.go:271), `loggerMiddleware`, `handleHealth`.
- `handlers.go` — every `/api/v1/*` handler plus the response DTOs.
- `middleware.go` — `CORSMiddleware`, `RateLimiter`/`RateLimitMiddleware`
  (per-IP token bucket, 10 rps / burst 20, 10-min TTL eviction).

## Router (chi/v5)

Middleware stack, applied in `setupRouter` (server.go:118):

1. `chimw.RequestID`
2. `loggerMiddleware` (server.go:250) — slog `http request` line per call
3. `chimw.Recoverer`
4. `chimw.Timeout(s.requestTimeout)` — gentle (deferred) timeout; default 60s.
   The `http.Server.WriteTimeout` is intentionally `requestTimeout + 5s` so the
   chi timeout fires first and the structured 503 reaches the client
   (server.go:213-225). `TestHandleSearch_HybridEmbeddingTimeoutFiresChi`
   locks this contract.
5. `CORSMiddleware` — driven by `cfg.Server.CORSOrigins`; if no origins are
   configured the middleware is a no-op (handlers/preflight return without
   CORS headers). `MaxAge` defaults to 86400.
6. `RateLimitMiddleware` (per-IP, returns 429 + JSON `rate_limit_exceeded`).

Routes (no auth):
- `GET  /health`, `HEAD /health` → `handleHealth` (server.go:305).

Routes under `/api/v1` (auth via `authMiddleware`):

| Method | Path                              | Handler                | Source                 |
| ------ | --------------------------------- | ---------------------- | ---------------------- |
| GET    | `/stats`                          | `handleStats`          | handlers.go:269        |
| GET    | `/messages`                       | `handleListMessages`   | handlers.go:303        |
| GET    | `/messages/{id}`                  | `handleGetMessage`     | handlers.go:343        |
| GET    | `/messages/{id}/inline?cid=...`   | `handleMessageInline`  | handlers.go:1629       |
| GET    | `/search?q=&mode=fts\|vector\|hybrid` | `handleSearch`     | handlers.go:401        |
| POST   | `/query` `{sql}`                  | `handleQuery`          | handlers.go:953        |
| GET    | `/aggregates`                     | `handleAggregates`     | handlers.go:1307       |
| GET    | `/aggregates/sub`                 | `handleSubAggregates`  | handlers.go:1346       |
| GET    | `/messages/filter`                | `handleFilteredMessages` | handlers.go:1387     |
| GET    | `/stats/total`                    | `handleTotalStats`     | handlers.go:1436       |
| GET    | `/search/fast`                    | `handleFastSearch`     | handlers.go:1476       |
| GET    | `/search/deep`                    | `handleDeepSearch`     | handlers.go:1547       |
| GET    | `/accounts`                       | `handleListAccounts`   | handlers.go:625        |
| POST   | `/accounts`                       | `handleAddAccount`     | handlers.go:865        |
| POST   | `/sync/{account}`                 | `handleTriggerSync`    | handlers.go:683        |
| GET    | `/scheduler/status`               | `handleSchedulerStatus`| handlers.go:715        |
| POST   | `/auth/token/{email}`             | `handleUploadToken`    | handlers.go:742        |

## Auth model

`authMiddleware` (server.go:271) checks `Authorization` first, falling back to
`X-API-Key`. `Bearer ` prefix stripped. Comparison via
`subtle.ConstantTimeCompare` against `cfg.Server.APIKey`. **Empty `APIKey`
disables auth entirely** — the daemon logs a WARN at startup
(server.go:209) but does not refuse. To refuse insecure binds,
`cfg.Server.ValidateSecure()` is called in `Start` (server.go:198): a
non-loopback `BindAddr` with no `APIKey` and `allow_insecure=false` returns an
error before the listener opens.

## Request/response shapes

DTOs live alongside handlers in `handlers.go`:
- `StatsResponse` (handlers.go:35), `MessageSummary` (handlers.go:76),
  `MessageDetail` (handlers.go:93), `SearchResult` (handlers.go:108),
  `hybridSearchResponse` (handlers.go:119) for vector/hybrid mode.
- `APIMessage = store.APIMessage` and `AccountStatus = scheduler.AccountStatus`
  are aliases — single source of truth (server.go:36, server.go:48).
- Error envelope: `ErrorResponse{error,message}` via `writeError`
  (handlers.go:170).

The `/messages/{id}` handler prefers `s.engine.GetMessage` (rich `body_html`),
falls back to `s.store.GetMessage` when the engine returns
`query.ErrNotImplemented` (handlers.go:351-368, see `isEngineUnsupported`
handlers.go:1621).

`handleHybridSearch` (handlers.go:491) bulk-hydrates hits via
`GetMessagesSummariesByIDs` to avoid the per-hit N+1, and translates vector
sentinels (`vector.ErrNotEnabled`, `ErrIndexStale`, `ErrIndexBuilding`,
`ErrEmbeddingTimeout`) to 503 with stable error codes.

## Editing recipes

**Add a new endpoint:** register the route in `setupRouter` (server.go:157),
write the handler in `handlers.go`, declare its response struct near the
other DTOs, return JSON via `writeJSON`/`writeError`. Mirror the route in
`internal/remote/engine.go` if remote TUI needs it.

**Expose a new query.Engine method:** add the JSON DTO + handler here, add
the corresponding method to `query.Engine` (and SQLite/DuckDB
implementations), then add the HTTP client call to
`internal/remote/engine.go`. Engines that cannot satisfy the call should
return `query.ErrNotImplemented`; `isEngineUnsupported` (handlers.go:1621)
maps both that and `remote.ErrNotSupported` into 501/fallback paths.

**Hardening a handler:** every handler should null-check `s.store`/`s.engine`
and return 503 (`store_unavailable`/`engine_unavailable`). Filter-only
endpoints reject `mode=vector|hybrid` queries with no free text
(handlers.go:510). Pagination limit cap is `maxPageSize=500` (handlers.go:32).
