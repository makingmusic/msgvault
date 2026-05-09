# Sync and Auth Subsystem

End-to-end view of how msgvault authorizes against a remote mailbox and
walks it into the local SQLite store. User-facing setup steps live at
msgvault.io; this document is for engineers changing or debugging the
internals.

## Architecture at a glance

```
                         ┌──────────────────────────────┐
                         │  cmd/msgvault/cmd            │
                         │  add-account / add-imap /    │
                         │  add-o365 / sync-full / sync │
                         └──────────────┬───────────────┘
                                        │
                ┌───────────────────────┼───────────────────────┐
                │                       │                       │
        ┌───────▼─────────┐    ┌────────▼────────┐    ┌─────────▼──────────┐
        │ internal/oauth  │    │internal/microsoft│    │ internal/imap      │
        │ Google OAuth    │    │ Azure AD OAuth   │    │ password creds     │
        │ + service accts │    │ + ID-token verify│    │                    │
        └───────┬─────────┘    └────────┬────────┘    └─────────┬──────────┘
                │                       │ access tokens         │ password
                ▼                       ▼                       ▼
           ~/.msgvault/tokens/  (0700 dir, 0600 files, atomic temp+rename)

                                        │
                                        ▼
        ┌──────────────────────────────────────────────────────┐
        │  internal/gmail.API                                  │
        │  ┌──────────────────────┐  ┌───────────────────────┐ │
        │  │ gmail.Client         │  │ imap.Client           │ │
        │  │ HTTPS Gmail v1       │  │ go-imap/v2 + XOAUTH2  │ │
        │  └──────────┬───────────┘  └───────────┬───────────┘ │
        └─────────────┼──────────────────────────┼─────────────┘
                      │                          │
                      └──────────┬───────────────┘
                                 ▼
                       ┌─────────────────────┐
                       │ internal/sync       │
                       │  Full / Incremental │
                       └──────────┬──────────┘
                                  ▼
                            internal/store
                            (SQLite + FTS5)
```

The `gmail.API` interface (`internal/gmail/api.go`) is the seam: the
syncer programs against it, and both Gmail HTTPS and IMAP implement it.
`MockAPI` and `DeletionMockAPI` round out the interface for tests.

## Source types

The `sources` table is the unit of truth for every remote account.
Relevant columns:

- `source_type` — `gmail` or `imap`. Other types (`mbox`, `apple_mail`,
  `pst`, `messenger`, ...) exist for importers but cannot be synced.
- `identifier` — the canonical address. For Gmail this is the email; for
  IMAP it's `imap[s|+starttls]://user@host:port` (`imap.Config.Identifier`).
- `oauth_app` — name of the OAuth client to use, or empty for the global
  default. Set when `add-account --oauth-app NAME` was used.
- `sync_config` — per-source JSON config. For IMAP this stores the
  `imap.Config` (host/port/auth method).
- `sync_cursor` — the Gmail history_id (decimal string). For IMAP it's
  unused.

`cmd/.../sync*.go` reads sources from this table and dispatches by
`source_type`. The path through `buildAPIClient`
(`cmd/msgvault/cmd/syncfull.go:234`) resolves credentials and constructs
the right `gmail.API` client.

## Account lifecycle

### `add-account` — Gmail OAuth (browser or service account)

1. Resolve `--oauth-app` against `cfg.OAuth` → either client-secrets path
   or service account key path.
2. **Service account branch:** mint a token via `JWTConfigFromJSON` with
   `Subject = email`, call Gmail profile API to confirm access. No token
   file is written.
3. **OAuth branch:** if a usable token already exists (right client_id,
   right scopes) we just register the source and exit. Otherwise:
   `oauth.Manager.Authorize` opens the browser, runs the local callback
   server on `localhost:8089`, exchanges the auth code for a token, and
   calls Gmail profile to verify the authorized account matches.
4. Persist the token to `~/.msgvault/tokens/<email>.json` (atomic temp +
   rename, 0600), persist the source row, and update the `oauth_app`
   binding.

A `*TokenMismatchError` thrown from step 3 surfaces a CLI hint to re-run
with the canonical address (`addaccount.go:251`).

### `add-imap` — username/password IMAP

1. Build `imap.Config`, prompt for password (huh masked input or piped
   stdin or `MSGVAULT_IMAP_PASSWORD`).
2. Connect, `STATUS INBOX` round-trip to validate credentials.
3. Save password to `<tokens_dir>/imap_<sha256-prefix>.json` (atomic +
   0600).
4. Persist source with `sync_config` JSON and the
   `imap[s|+starttls]://...` identifier.

### `add-o365` — Microsoft 365 / Outlook.com via OAuth + IMAP

1. Build a `microsoft.Manager` from `cfg.Microsoft.ClientID` and
   tenant.
2. `Authorize` runs the PKCE browser flow on `localhost:8089/callback/microsoft`.
3. Verify the ID token (signature, issuer, audience, expiry, nonce). On
   `tid` mismatch with the initially-guessed scope (personal vs org),
   **rerun** the browser flow with the correct IMAP scope — silent refresh
   cannot acquire consent for a different resource.
4. Determine the IMAP host from the persisted scope (`outlook.office.com`
   for personal, `outlook.office365.com` for org).
5. Build an XOAUTH2 `imap.Config`, persist source. If a Microsoft IMAP
   source for the same email already exists (e.g. a previous host), update
   in place rather than creating a duplicate.

The XOAUTH2 scopes are `IMAP.AccessAsUser.All`, `offline_access`, `openid`,
`email`. There is no Microsoft Graph use today: mail flows through IMAP.

## First sync, incremental, re-auth

`sync-full <email>`:

1. Resolve sources by identifier-or-display-name and filter to
   syncable types.
2. Build the API client per source: for Gmail, `getTokenSourceWithReauth`
   wraps `oauth.Manager.TokenSource`. If the saved refresh token has
   expired or been revoked, the helper invokes `AuthorizeManual` to
   prompt re-auth without launching a browser (works on TTYs by virtue
   of `isatty`).
3. Pass options through `sync.Options` (`Query`, `NoResume`, `Limit`,
   `BatchSize`, `AttachmentsDir`). For IMAP, `NoResume` is forced true
   because IMAP page tokens are session-local offsets.
4. `Syncer.Full(ctx, email)` walks every page, calling
   `MessageExistsWithRawBatch` to skip already-stored IDs and
   `GetMessagesRawBatch` to pull new MIME. After every page the
   checkpoint is persisted.
5. On completion, `UpdateSourceSyncCursor` writes the final
   `profile.HistoryID`. This is what enables the next incremental sync.

`sync` (incremental):

1. Same source resolution. Sources without `sync_cursor` are skipped
   with a hint to run full sync first. IMAP sources are silently
   redirected to full sync.
2. `Syncer.Incremental(ctx, source)` calls `client.ListHistory(start =
   sync_cursor)` and walks the records. Per page:
   - Collect every message ID from `messagesAdded` / `labelsAdded` /
     `labelsRemoved` and run a single `MessageExistsBatch` to classify
     known vs unknown.
   - Batch-fetch unknown new messages via `GetMessagesRawBatch`.
   - Apply label diffs to known messages directly via
     `AddMessageLabels` / `RemoveMessageLabels` — no API call.
   - Batch-mark deletions via `MarkMessagesDeletedBatch`.
3. On 404 from `ListHistory`, return `ErrHistoryExpired` (Gmail keeps
   ~7 days of history). The CLI prints a "run sync-full to catch up"
   hint and exits 0.
4. The cursor advances on every successful run, even when individual
   messages errored — preventing one stuck message from blocking all
   future syncs.

## Gmail History API model

`historyTypes` requested: `messageAdded`, `messageDeleted`, `labelAdded`,
`labelRemoved` (`internal/gmail/client.go:477`). Things explicitly **not**
requested or used: `messageMoved` (Gmail expresses moves as label
add/remove), `draftCreated/Updated/Deleted`, draft mutations, send events.

A History record IS NOT a single change: each record can carry
multiple `messagesAdded` / `messagesDeleted` / `labelsAdded` /
`labelsRemoved` for many message IDs. `incremental.go` collects across
the whole page, batches existence checks once, and only then walks the
records to dispatch label changes. This keeps the database write set
linear in changes-per-page, not changes-per-record.

The cursor is monotonic and represents the highest history_id observed.
`GetProfile.HistoryID` is what we persist after each sync — not the
last-seen `history.id` — because the profile value is what the next
`startHistoryId` must be relative to.

## IMAP "incremental" via UID

There is no IMAP equivalent of the History API. msgvault makes no
attempt to track UID-VALIDITY or use CONDSTORE: every "incremental"
on IMAP is a full sync that relies on the `MessageExistsWithRawBatch`
fast path to skip messages already in the store. New messages arrive
because their composite IDs aren't yet in the database. Deletions are
**not** detected — a message removed on the server will linger locally
until the next full sync that doesn't list it... and even then, the
sync only inserts, doesn't reconcile. TODO(verify): confirm whether
deletion detection lives elsewhere (e.g. the deletion executor) or is a
known gap for IMAP sources.

Cross-mailbox dedup (same RFC822 Message-ID seen under multiple
`mailbox|uid`s) is handled by `errDuplicateRFC822` →
`UpdateMessageOnDedup` which rewrites the composite ID in place. This
is the mechanism that keeps an INBOX → Trash move from creating a
duplicate row.

## Rate-limiting strategy

Per-account, in-process. Each `gmail.Client` owns its own
`*RateLimiter`. The CLI builds it from `cfg.Sync.RateLimitQPS` (default
5). At the default the bucket is 250 capacity, 250 units/sec refill —
the per-user Gmail quota. Operation costs match Gmail's documented
units (`messages.get` = 5, `batchDelete` = 50, etc.).

Adaptive backoff:

- 429 → `Throttle(30s)`
- 403 with `rateLimitExceeded` body → `Throttle(60s)`
- Throttling drains tokens to zero, halves the refill rate, and refuses
  to shorten an existing throttle window
- Throttle window expiration auto-restores the base refill rate

Multiple syncs on the same account in parallel will exceed the budget
because the limiter is per-process per-client; msgvault's CLI and
daemon paths sync sources sequentially today, so this isn't currently
a problem.

## Credential storage layout

```
~/.msgvault/tokens/
├── 0700 dir, owner only
├── alice@gmail.com.json                  # Google OAuth
├── microsoft_alice@outlook.com.json      # Microsoft OAuth
└── imap_<sha256-prefix>.json             # IMAP password
```

Every file is `0600` and written via temp-file + rename for atomicity.
`fileutil.SecureMkdirAll` / `SecureChmod` apply Windows DACLs where
relevant.

Google token JSON: oauth2.Token + `scopes[]` + `client_id`. Microsoft
token JSON: oauth2.Token + `scopes[]` + `tenant_id`. IMAP creds JSON:
just `{"password": "..."}`.

## Multi-OAuth-app feature

`config.toml` can declare named OAuth apps:

```toml
[oauth.apps.acme]
client_secrets = "/path/to/acme_secret.json"
```

`add-account you@acme.com --oauth-app acme` resolves the secrets file
through `cfg.OAuth.ClientSecretsFor("acme")` and stores `acme` in
`sources.oauth_app`. Subsequent syncs use that binding automatically.
`Manager.TokenMatchesClient(email)` lets the CLI detect and force-reauth
when the user re-runs `add-account` with a different `--oauth-app`.

## Service account + domain-wide delegation

For Workspace orgs that prefer key-based auth over interactive OAuth,
configure a service account key path in
`config.toml [oauth.apps.<name>]` (or as the global default). The CLI
detects this via `cfg.OAuth.ServiceAccountKeyFor(appName)` and routes
through `oauth.NewServiceAccountManager` instead of `oauth.NewManager`.

The key file is permission-checked (must be `0600` on Unix, mode bits
`0o077` rejected). Each `TokenSource(ctx, email)` call clones the
JWTConfig with `Subject = email` set — the magic that makes
domain-wide delegation work. There is no per-user token file: tokens
are signed JWTs, refreshed on demand, cached only inside the oauth2
library.

`oauth.ValidateTokenEmail` runs the same Gmail-profile sanity check
that browser-flow tokens get. `add-account` with a service account
exits without writing any token file.

## Failure modes

| Failure | What happens |
| --- | --- |
| Network blip during list/fetch | `Client.request` retries up to 12 times with exponential-jitter backoff capped at 600s (`internal/gmail/client.go:102`). IMAP equivalents reconnect once per chunk. |
| Refresh token revoked at provider | `Manager.TokenSource` returns an error; `getTokenSourceWithReauth` falls back to `AuthorizeManual` if a TTY is present, otherwise propagates. The sync exits with the error captured in `syncErrors`. |
| OAuth app changed (different `client_id`) | `TokenMatchesClient` returns false; `add-account` forces re-auth. |
| Microsoft token has stale IMAP scope (post scope-correction) | `microsoft.Manager.TokenSource` rejects with a hint to run `add-o365` again (`oauth.go:241`). |
| `sync-full` interrupted (Ctrl-C, crash) | Checkpoint row in `sync_runs` survives; next `sync-full` resumes from `cursor_before`. Panics inside the loop are recovered and recorded as failed runs, not silent partial state. |
| Partial sync (some messages 4xx/parse-fail) | Counted in `errors_count`. Cursor still advances so a single bad message can't block the account forever. |
| Gmail history_id gap (>~7 days since last sync) | `ListHistory` returns 404 → `*NotFoundError` → `ErrHistoryExpired`. The CLI tells the user to run `sync-full`. |
| IMAP server resets connection mid-fetch | `withConn` clears the dead conn, next operation reconnects. Per-chunk single-shot retry inside `GetMessagesRawBatch`. |
| Token file corrupted (half-written JSON) | Atomic temp+rename writes prevent this; if a legacy / external corruption occurs, `loadTokenFile` errors and the user re-runs the appropriate add-account command. |
| MIME parse error | Message is stored with a placeholder body and a warning log; raw blob preserved in `message_raw` for later re-parsing. Never blocks the sync (`internal/sync/sync.go:475`). |
| Cross-mailbox IMAP duplicate | `errDuplicateRFC822` → in-place composite-ID rewrite. No double row, no re-download next time. |

## Test surface

- **`internal/gmail/MockAPI`** — full read-side mock. Set
  `MessagePages`, `Messages`, `HistoryRecords`, error-injection maps,
  then drive the syncer.
- **`internal/gmail/DeletionMockAPI`** — focused mock for the deletion
  executor: per-message permanent / transient errors, before-call
  hooks, simulated rate limiting (`RateLimitAfterCalls`,
  `RateLimitDuration`). The non-deletion methods panic — pick the
  right mock.
- **`internal/sync/fixtures_test.go` + `testenv_test.go`** — fixture
  builder for `RawMessage`s and a `*Store` harness that initializes
  the schema in a temp SQLite file. `sync_test.go` exercises both
  full and incremental paths against `MockAPI`.
- **`internal/oauth/oauth_test.go`** — uses an in-process
  `httptest.Server` to stand in for the Gmail profile endpoint and
  injects a fake `browserFlowFn` so tests don't open a real browser
  or bind a real port.
- **`internal/microsoft/oauth_test.go`** — uses `verifyIDTokenFn` to
  bypass real OIDC validation and `browserFlowFn` to fake the auth
  code exchange. Covers tenant-scope correction, ID-token replay
  protection, and refresh-failure paths.
- **`internal/imap/client_xoauth2_test.go`** etc. — exercises the
  XOAUTH2 SASL exchange against a fake server.
- **`internal/gmail/ratelimit_test.go`** — uses a fake `Clock` to
  test refill / throttle / recovery without sleeping.

When adding tests, prefer the existing mocks over building a new fake
that implements `gmail.API` from scratch — keeps the test surface
small and ensures behavior tracks the real implementations.

## When changing this subsystem

- Don't break checkpoint resumability. Every page must end with
  `UpdateSyncCheckpoint`. The source cursor must advance on partial
  success.
- Don't introduce N+1 in the page loop. Use the `*Batch` store
  helpers and `GetMessagesRawBatch`.
- Don't bypass the atomic-write pattern in token storage.
- Don't add a new HTTP call without going through `Client.request`
  (Gmail) — retries, rate limiting, and 4xx classification all live
  there.
- Don't add a new `gmail.API` method without checking that
  `imap.Client`, `MockAPI`, and `DeletionMockAPI` all implement it
  sensibly. Several of those mocks have `var _ API = (*X)(nil)`
  compile-time checks; keep them.
- New auth providers (e.g. Yahoo OAuth) should follow the
  `microsoft` package pattern: produce a `func(ctx)(string, error)`
  callback, plug into `imap.WithTokenSource`, persist tokens with the
  same atomic-write conventions and provider-prefixed filenames.
