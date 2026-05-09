# internal/remote

HTTP client to a remote `msgvault serve` daemon. Used by the TUI when
`[remote] url = "..."` is configured (and `--local` is not passed) so a
laptop client can browse a NAS-hosted archive without copying the
database. Mirror image of `internal/api`.

## Files

- `engine.go` — `Engine` (engine.go:24): implements `query.Engine` over
  HTTP. Compile-time guard at engine.go:29
  (`var _ query.Engine = (*Engine)(nil)`). Also defines the JSON DTOs
  that mirror `api/handlers.go` response shapes
  (`aggregateResponse`, `filteredMessagesResponse`,
  `messageSummaryJSON`, `searchFastResponse`, `deepSearchResponse`),
  the `buildAggregateQuery`/`buildFilterQuery`/`buildStatsQuery`
  request encoders, and `buildSearchQueryString` which reconstructs a
  Gmail-style query string from a `*search.Query`.
- `engine_test.go` — exercises Engine round-trips against a stub
  http.Server.
- `store.go` — `Store` (store.go:19): the lower-level HTTP client for
  the small `MessageStore` surface used by `internal/api`. `Config`
  (store.go:26): `URL`, `APIKey`, `AllowInsecure`, `Timeout` (default
  30s). DTOs match the API: `statsResponse`, `messageResponse`,
  `messageDetailResponse`, `attachmentResponse`,
  `listMessagesResponse`, `searchResponse`, `accountsResponse`,
  `AccountInfo`. `handleErrorResponse` decodes `{error,message}`
  envelopes into a Go error.
- `store_test.go` — exercises Store round-trips.

## Engine interface implementation

`Engine` proxies to `Store.doRequestWithContext` for every method.
Endpoints used:

| `query.Engine` method        | HTTP call                                          |
| ---------------------------- | -------------------------------------------------- |
| `Aggregate`                  | `GET /api/v1/aggregates`                           |
| `SubAggregate`               | `GET /api/v1/aggregates/sub`                       |
| `ListMessages`               | `GET /api/v1/messages/filter`                      |
| `GetMessage`                 | `GET /api/v1/messages/{id}` (via `Store.GetMessage`) |
| `GetTotalStats`              | `GET /api/v1/stats/total`                          |
| `Search` (deep/FTS)          | `GET /api/v1/search/deep`                          |
| `SearchFast`                 | `GET /api/v1/search/fast`                          |
| `SearchFastCount`            | `GET /api/v1/search/fast?limit=0`                  |
| `SearchFastWithStats`        | `GET /api/v1/search/fast`                          |
| `ListAccounts`               | `GET /api/v1/accounts` (via `Store.ListAccounts`)  |

`GetMessageSummariesByIDs` (engine.go:532) loops `GetMessage` per id —
explicit TODO in the comment to add a bulk endpoint when remote MCP
hydration grows. Other methods that the remote API does not expose
return `ErrNotSupported` (engine.go:21): `GetMessageBySourceID`,
`GetMessageRaw`, `GetAttachment`, `GetGmailIDsByFilter`,
`SearchByDomains`. The local API maps that sentinel via
`api.isEngineUnsupported` to a 501.

`Search` (engine.go:585) refuses multi-account scope
(`len(q.AccountIDs) > 1`) — the deep-search HTTP endpoint accepts a
single `source_id` query parameter only.

## Auth / configuration

`Store.New` (store.go:34) requires:
- `URL` non-empty, scheme `http` or `https` with a host.
- HTTPS is enforced when scheme is `http` and `AllowInsecure=false` —
  returns the multi-line error at store.go:46-49 with remediation
  steps. Trusted networks (LAN/Tailscale) opt in with
  `[remote] allow_insecure = true`.

Auth header: `X-API-Key` (store.go:94). Set when `cfg.APIKey != ""`. The
server-side `authMiddleware` accepts both `X-API-Key` and `Authorization:
Bearer ...` (api/server.go:271), but this client only sends the former.

`Accept: application/json` is set on every request. Default timeout is
30s applied at the `http.Client.Timeout` level (store.go:60-69), so a
stalled server tears the entire request down rather than hanging the
TUI.

## Where it's wired

- `cmd/msgvault/cmd/tui.go:66-79` — TUI picks remote engine when
  `cfg.Remote.URL != ""` and `--local` not passed. In remote mode the
  TUI banner prints `Connected to remote: ...`; deletion and export
  paths are disabled.
- `cmd/msgvault/cmd/export_token.go` — `[[accounts]]` registration also
  uses `Store` to push tokens to a daemon via
  `POST /api/v1/auth/token/{email}` and `POST /api/v1/accounts`.

## Editing recipes

When adding a new HTTP endpoint to `internal/api`, mirror it here:

1. Add a DTO struct in `engine.go` (or `store.go` for the
   `MessageStore` surface) matching the JSON tags in `api/handlers.go`.
2. Add a `buildXQuery` helper if the endpoint takes structured params.
3. Add the method on `Engine` (or `Store`) that calls
   `doRequestWithContext`, decodes the body, and converts to the
   `query` types.
4. Add the matching engine_test.go / store_test.go round-trip.
