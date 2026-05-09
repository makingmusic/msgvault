# CLAUDE.md

Scoped instructions for Claude Code working in this repo. Read this first.
For deep context, follow the pointers at the bottom.

## General workflow

When a task involves multiple steps (implement + commit + PR), complete the
full chain without stopping. If you create a branch, commit, and open a PR,
finish all of it.

Always commit after every turn. Don't ask "shall I commit?" — just commit.
Committing is the expected default after every change.

PR descriptions are concise and changelog-oriented: what changed, why, how to
use it. No test plans, no design rationale, no implementation details — those
belong in commit messages and specs.

## Project overview

msgvault is an offline message-archive tool. Single Go binary (CGO required),
SQLite system of record, optional Parquet+DuckDB analytics cache, optional
sqlite-vec vector search. Three faces: CLI (~58 Cobra commands), TUI
(Bubble Tea), and a `serve` daemon (chi HTTP + mark3labs MCP + cron
scheduler).

Ingests from many sources beyond Gmail: IMAP, Microsoft O365 (via IMAP),
MBOX, Apple Mail .emlx, Outlook PST, plus chat formats (iMessage, WhatsApp,
Facebook Messenger DYI, Google Voice).

Read **`docs/ARCHITECTURE.md`** for the full system map.

## Layout

```
cmd/msgvault/cmd/             Cobra CLI (~58 commands; see docs/subsystems/cli.md)
internal/
├── api/                      chi HTTP server (`msgvault serve`)
├── mcp/                      MCP server (mark3labs/mcp-go)
├── scheduler/                robfig/cron scheduled syncs + embed jobs
├── remote/                   HTTPS client implementing query.Engine
├── tui/                      Bubble Tea TUI
├── store/                    SQLite system of record + Dialect interface (PG scaffold)
├── dedup/                    Dedup algorithms (separate from store/dedup.go)
├── search/                   Search query parser
├── query/                    Multi-backend query engine (DuckDB-Parquet, SQLite, PG stub)
├── vector/                   sqlite-vec embeddings, generations, OpenAI-compatible backend
├── sync/                     Sync orchestration (full + incremental)
├── gmail/                    Gmail API client + ratelimit + mocks
├── imap/                     IMAP client (also implements gmail.API)
├── oauth/                    OAuth2 (browser + device + service account)
├── microsoft/                Microsoft Graph / O365 OAuth → IMAP token
├── importer/                 Shared email-format ingest core
├── emlx/                     Apple Mail .emlx parser + discover
├── mbox/                     MBOX parser
├── pst/                      Outlook PST parser (mooijtech/go-pst)
├── applemail/                Apple Mail metadata helpers
├── fbmessenger/              Facebook Messenger DYI
├── gvoice/                   Google Voice export
├── imessage/                 iMessage (chat.db; attributedBody parsing)
├── whatsapp/                 WhatsApp export
├── textimport/               Generic text-message helpers (phone normalisation)
├── deletion/                 Staged-deletion manifest store + executor
├── mime/                     enmime + chardet wrappers
├── textutil/                 Charset / encoding helpers
├── fileutil/                 Cross-OS secure file modes (chmod 600)
├── logging/                  slog conventions
├── config/                   TOML config loading; MkTempDir
├── update/                   Self-update (checksum required)
├── export/                   .eml / attachment export
└── testutil/                 Shared test helpers (builders, fs, store, archive)
```

Each `internal/<pkg>/` has a `CLAUDE.md` scoped to that package. Read it
before editing in that directory.

## Quick commands

```bash
make build              # debug build
make build-release      # optimised, stripped
make install            # install to ~/.local/bin or GOPATH/bin
make test               # default tags: fts5 sqlite_vec
make lint               # golangci-lint --fix
make lint-ci            # CI lint, no auto-fix
make bench              # query engine benchmarks
make install-hooks      # prek pre-commit hook
make test-pg            # PostgreSQL scaffold; expected to fail (see PG_STATUS.md)
```

CGO is required (mattn/go-sqlite3, marcboeker/go-duckdb, asg017/sqlite-vec).
Default build tags: **`fts5 sqlite_vec`**.

## Build-tag-gated features

`sqlite_vec` gates vector search. Three command pairs ship a real + stub
file (`embed_vector.go` / `embed_vector_stub.go`, etc.). Without the tag,
binary still builds; vector commands return clean errors.

## Storage layout

All under `~/.msgvault/` (override with `MSGVAULT_HOME`):

- `msgvault.db` — SQLite (WAL)
- `attachments/<2>/<sha256>` — content-addressed binaries
- `tokens/<email>.json` — OAuth tokens, chmod 600
- `analytics/year=YYYY/` — Parquet, partitioned by year
- `deletions/{pending,completed,failed,cancelled}/` — manifest staging
- `tmp/` — fallback temp dir (use `config.MkTempDir`, never `os.MkdirTemp("")`)
- `config.toml` — optional config

## Working in this repo — invariants and rules

### Always

- Run `go fmt ./...` and `go vet ./...` before committing. Stage all resulting changes including formatting-only files.
- Stage **all** modified files. Run `git diff` and `git status` before committing.
- Use `error` returns wrapped with `fmt.Errorf("...: %w", err)`.
- Table-driven tests.
- Route DB ops through `*store.Store`. Don't open `*sql.DB` ad-hoc.
- Use `config.MkTempDir` for temp dirs. `os.MkdirTemp("", ...)` breaks on Windows under group policy.
- Use `internal/testutil`. The right helper depends on what layer you're testing — see `internal/testutil/CLAUDE.md` for the catalog (three message builders for three layers; `dbtest` vs `storetest` separation exists to break import cycles).

### Never

- **Never JOIN or scan `message_bodies` in list/aggregate/search queries.** It's separated from `messages` so the messages B-tree stays small. Only access via direct PK lookup for single-message detail views. For text search use FTS5 (`messages_fts`).
- **Never use SELECT DISTINCT with JOINs.** Use EXISTS sub-queries instead — semi-join is faster and avoids duplicates at the source.
- **Never log user content / email addresses / message bodies at INFO.** PII rule from global CLAUDE.md. Use DEBUG for full context. INFO carries intent classification, counts, durations, error types.
- **Never call an expensive function solely to log its output.** Reuse pipeline results.
- **Never write code that loops single-row queries to "fan out".** Rewrite as JOIN, WHERE IN, or batch fetch. When converting a single-item method to a multi-item variant, rewrite the query — don't wrap.
- **Every outbound HTTP call must have a timeout.** Especially on fan-out paths (api server, scheduler, embed backend). Use `AbortSignal.timeout()`-equivalent (`context.WithTimeout`).
- Never commit test fixtures with real PII. Use `alice`, `bob`, `user@example.com`. The `internal/testutil/security_data.go` helpers exist for this.

### When changing data flow

Trace forward after any fix. If you change how data is written, audit every
reader. Removed dead guards your fix made unreachable. The write-side fix is
half the job; the read-side impact is the other half.

### Pre-PR self-review

Always re-read every changed file as a reviewer, not the author, before
opening a PR. Check: reconnect/retry, PII in logs, dead code from your own
fixes, duplicate computation, falsy-vs-None coercion. One self-review pass
prevents multi-round churn. Don't skip it for "small" changes.

### PR review

Post review feedback as `gh pr comment`, not `gh pr review --request-changes`
(GitHub blocks self-review-changes). The branch agent picks up fixes
independently.

## Database backends

SQLite is the default and only functional backend. PostgreSQL is **scaffolded
behind a `Dialect` interface** (`internal/store/dialect.go`); see
`docs/PG_STATUS.md` for blockers. `internal/query/postgres.go` returns
`ErrNotImplemented` for almost every method.

The `loggedDB` wrapper silently calls `Dialect.Rebind` on every query, so most
store call sites can keep using `?` placeholders portably. Only callers that
bypass `loggedDB` (e.g. `subset.go`, raw `*sql.Conn` paths) need explicit
rebind. `subset.go` is intentionally SQLite-only.

## Sync invariants

- **history_id advances even on partial failure** (sync.go:380, incremental.go:215). One bad message can't block all future syncs.
- **IMAP fakes `gmail.API`** so the same syncer handles both. ListHistory returns an error for IMAP. ThreadID is a synthetic `mailbox|uid` overridden by the syncer using References/In-Reply-To headers.
- **`errDuplicateRFC822` rewrites composite IDs in place** when IMAP messages move between mailboxes (INBOX → Trash) — the syncer detects the duplicate by RFC822 Message-ID and updates rather than re-downloading.
- **IMAP forces `NoResume=true`**; resume is cheap because `MessageExistsWithRawBatch` skips already-imported messages.

## Deletion safety

- The **directory** under `~/.msgvault/deletions/` is authoritative — it wins over the inline `Status` field. `CancelManifest` renames before rewriting the field; a crash between the two leaves the file in `cancelled/` with `Status: pending` and the directory wins.
- `Execute` (per-message) lands in `failed/` only when **all** messages failed.
- `ExecuteBatch` always lands in `completed/` even with partial failures, because batch semantics expect partial progress. On resume it retries previous failures before continuing.
- Permanent delete (vs Trash) requires the `https://mail.google.com/` Gmail scope or DWD equivalent.

## TUI invariants

- Stale-response filtering uses **per-domain request ID counters**. Snapshot the counter into the `tea.Cmd` closure; drop messages whose ID doesn't match.
- `transitionBuffer` caches the pre-load frame. `View()` returns it verbatim until the matching `handle*Loaded` clears it. Don't bypass.
- `cacheNeedsBuild` deliberately does not short-circuit — it collects every staleness signal so the log line names every cause.
- The TUI auto-builds the Parquet cache on launch.

## Configuration

```toml
# ~/.msgvault/config.toml

[oauth]
client_secrets = "/path/to/client_secret.json"   # default Gmail OAuth app

[oauth.apps.acme]                                # named OAuth app for a Workspace org
client_secrets = "/path/to/acme_secret.json"
# Or service account with domain-wide delegation:
# service_account_key = "/secure/path/sa.json"

[sync]
rate_limit_qps = 5

[[accounts]]                                     # daemon-mode scheduled sync
email = "you@gmail.com"
schedule = "0 2 * * *"
enabled = true

[server]
api_port = 8080
bind_addr = "0.0.0.0"
api_key = "your-secret-key"                      # empty disables auth, logs WARN

[remote]                                         # TUI client
url = "https://msgvault.example.com:8080"
api_key = "your-secret-key"
# allow_insecure = true                          # only set for plain HTTP
```

## TUI keybindings

`j/k` `↑/↓` navigate · `Enter` drill in · `Esc`/`Backspace` back ·
`Tab` cycle views · `s` cycle sort · `r` reverse sort · `t` time view ·
`a` filter by account · `f` filter by attachments · `Space` toggle select ·
`A` select-all-visible · `x` clear selection · `d` stage selected ·
`D` stage all matching · `/` search · `?` help · `q` quit

## Test data hygiene

Never use real names / addresses / identifiers in fixtures. Use `alice`,
`bob`, `Test User`, `user@example.com`. `internal/testutil/security_data.go`
has helpers. Verify before commit.

## Where to read next

- `docs/ARCHITECTURE.md` — full system map (start here)
- `docs/TESTING.md` — test taxonomy + prioritised gap list
- `docs/subsystems/<name>.md` — per-area deep dive
- `internal/<pkg>/CLAUDE.md` — scoped context when editing in that package
- `docs/PG_STATUS.md` — Postgres scaffold blockers
- `docs/accounts-identities-collections-dedup/` — identity model design
- `docs/recovery.md` — recovery procedures
- `internal/query/DESIGN.md` — query engine design (note: stale on RemoteEngine)
- `SECURITY.md` — security model
