# internal/gmail

Gmail REST v1 client and the `gmail.API` interface that the rest of the
codebase programs against. Two production implementations live in this
repo: `*Client` here and `*imap.Client` (which implements the same
interface for IMAP servers).

## File layout

- `api.go` — interface definitions and domain types (`Profile`, `Label`,
  `RawMessage`, `MessageListResponse`, `HistoryRecord`, ...). Pure types,
  no I/O. The interface is composed of `AccountReader` + `MessageReader` +
  `MessageDeleter` so callers can take only what they need.
- `client.go` — concrete HTTP implementation. Owns retry/backoff,
  base64url decode, JSON-to-domain mapping. The unexported
  `*Response` JSON structs live here; domain types live in `api.go`.
- `ratelimit.go` — token-bucket limiter with adaptive throttle.
- `sync_types.go` — `SyncProgress` interface and `SyncSummary` struct
  used by the syncer; lives here (not in `internal/sync`) so the
  interface is implemented against the I/O layer's vocabulary.
- `mock.go` — `MockAPI`, full read-side mock for sync tests.
- `deletion_mock.go` — `DeletionMockAPI`, focused mock for deletion
  workflows (per-message error injection, transient failures, rate-limit
  simulation). The non-deletion methods panic — pick the right mock.

## Client vs API split

`gmail.API` is the seam. Sync, deletion, and TUI test paths all take
`gmail.API`, never `*Client`. Don't widen the interface without thinking
about the IMAP and mock implementations.

`Client` is constructed via `NewClient(tokenSource, opts...)`. The
`oauth2.TokenSource` is wrapped by `oauth2.NewClient` which auto-refreshes
on each request — the client never sees raw tokens. Defaults: `userID =
"me"`, `concurrency = 10`, 30s timeout, `qps = 5` (via
`NewRateLimiter(5.0)`).

## Retry and error class handling

`Client.request` (`client.go:93`) is the single retry surface. Up to 12
attempts with exponential-full-jitter backoff capped at 600s (`maxRetries
= 12`, `maxBackoff = 600`, `calculateBackoff`):

| Status | Behavior |
| --- | --- |
| 2xx | Return body |
| 429 | `Throttle(30s)` on the limiter, retry |
| 403 + `rateLimitExceeded` body marker | `Throttle(60s)`, retry. Detected by `isRateLimitError` (`client.go:414`) |
| 403 (other) | Return immediately — actual permission error |
| 5xx | Retry |
| 401 | Return immediately — `oauth2` should have refreshed; if it didn't, the token is bad |
| 404 | Return `*NotFoundError{Path}` (typed) — sync uses `errors.As` to demote 404s to `Debug` log level |
| Other 4xx | Return immediately |

Network-level errors (no response) also retry. The retry loop is the
only place where logs about rate limiting appear; sync code stays quiet.

## Rate limit token bucket

`RateLimiter` (`ratelimit.go`) models Gmail's per-user quota:

- Capacity `DefaultCapacity = 250`, refill `DefaultRefillRate = 250.0`
  units/sec at the maximum. `NewRateLimiter(qps)` scales the refill rate
  down by `qps / 5.0` (clamped to 1.0). At the default `qps=5` you get
  full 250/s; `qps=1` gives 50/s.
- Operation costs (`Operation.Cost`): `messages.get/list/trash` = 5,
  `delete` = 10, `batchDelete` = 50, `history.list` = 2, `labels.list`
  and `profile` = 1. Match Gmail's published quota units.
- `Acquire(ctx, op)` blocks until enough tokens; respects ctx cancellation.
  `TryAcquire` is non-blocking.
- `Throttle(d)`: hard pause until `now+d`, drains tokens, halves refill
  rate (`throttleRecoveryFactor = 0.5`) until next call to `RecoverRate`
  (currently no caller, refill rate is restored automatically when the
  throttle window passes — see `refill()` at `ratelimit.go:185`). Throttle
  windows do not shorten: a 60s 403 throttle won't be cut short by a
  later 30s 429.

The limiter takes a `Clock` interface so tests can drive virtual time.

## Mocks

- `MockAPI`: fixture-style mock. Set `Messages`, `MessagePages`,
  `HistoryRecords`, etc., then call methods. Errors injected via
  `*Error` fields. Tracks call counts. Implements `API` end-to-end.
- `DeletionMockAPI`: only implements the `MessageDeleter` methods;
  the rest panic. Designed for the deletion executor: per-message
  permanent errors (`TrashErrors`, `DeleteErrors`), transient
  failures-then-success counters, before-call hooks, batch error
  injection, simulated rate-limiting (`RateLimitAfterCalls`).

Both ensure they implement `API` via blank `var _ API = ...` at the
bottom of the file — keep these for compile-time checks.

## When editing

- Don't add a new HTTP call without routing it through `Client.request`
  — the retry/throttle/auth wiring lives there.
- Operation costs in `Operation.Cost` track the published Gmail quota.
  When adding a new op, check the docs and add the case there too.
- The `error` returned for 404 is `*NotFoundError`, not a wrapped
  `errors.New`. Callers check it with `errors.As`. Don't change the
  type without auditing every `errors.As(err, &nfe)` site.
