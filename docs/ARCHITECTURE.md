# msgvault Architecture

This is the architectural map for msgvault. Read it once, then dive into the
per-subsystem docs in `docs/subsystems/` and the per-package `CLAUDE.md` files
in `internal/<pkg>/`.

If something here disagrees with code or per-subsystem docs, the per-subsystem
docs win — this file is synthesised from them and may drift.

> **Read-only edition.** This is a fork of upstream msgvault that is
> structurally incapable of mutating any remote mailbox. The
> `internal/deletion/` subsystem, the deletion CLI commands
> (`delete-staged`, `list-deletions`, `show-deletion`,
> `cancel-deletion`), the MCP `stage_deletion` tool, the Gmail/IMAP
> trash/delete client methods, and the `MessageDeleter` interface have
> all been removed. Sections of this document that describe
> "Mutation: staged deletion" and the deletions directory describe
> upstream behaviour and do not apply to this fork. See
> `plans/readonly-conversion.md` and `SECURITY.md` for the full list
> of removed surfaces and the regression tests that lock the property
> in place.

---

## What msgvault is

A single Go binary (CGO required) that:

1. **Ingests** messages from many sources (Gmail API, IMAP, Microsoft O365 via
   IMAP, MBOX, Apple Mail .emlx, Outlook PST, plus chat formats: iMessage,
   WhatsApp, Facebook Messenger DYI, Google Voice).
2. **Stores** them in a local SQLite database with raw RFC822 preserved
   (zlib-compressed) and a content-addressed attachment store on disk.
3. **Indexes** them with FTS5, optionally sqlite-vec for embeddings, and
   optionally a denormalised Parquet cache for fast aggregate analytics via
   DuckDB.
4. **Serves** them through three surfaces: an interactive TUI, a CLI, and a
   long-running daemon (`msgvault serve`) that exposes both an HTTP API and an
   MCP server.

The whole system is offline by default. Network access is needed only for
sync, embedding, and the optional update mechanism.

---

## The three faces

```
                 ┌────────────────────────────┐
                 │  TUI (interactive)         │
                 │  Bubble Tea + lipgloss     │──┐
                 └────────────────────────────┘  │
                                                 │
                 ┌────────────────────────────┐  │   query.Engine interface
                 │  CLI (Cobra, ~58 cmds)     │──┼─►  DuckDB+Parquet (analytics)
                 │  cmd/msgvault              │  │   SQLite FTS5      (text)
                 └────────────────────────────┘  │   sqlite-vec       (vector)
                                                 │   Postgres scaffold (stubs)
                 ┌────────────────────────────┐  │   remote.Engine    (HTTPS client)
                 │  Daemon: msgvault serve    │  │
                 │  • chi HTTP API            │──┘
                 │  • mark3labs MCP server    │
                 │  • robfig/cron scheduler   │
                 │  • optional embed jobs     │
                 └────────────────────────────┘
```

The TUI can run **local** (opens the SQLite store directly) or **remote**
(talks to a daemon over HTTPS via `internal/remote.Engine`). It chooses based
on `[remote] url` in `config.toml` plus the `--local` override.

The MCP CLI deliberately opens the store **read-only** so multiple Claude Code
sessions don't fight for the SQLite write lock.

---

## Layered package map

```
┌─ internal/store ───────────────────────────────────────────────────┐
│  SQLite system of record. Dialect interface (Postgres scaffold).   │
│  loggedDB wrapper does silent Rebind for `?` → `$N`.               │
│  86 public methods. FTS5 helpers. Migrations. subset.go (SQLite).  │
│  Sub-packages: dedup (semantics), search (parser).                 │
└─────────────────────▲──────────────────────────────────────────────┘
                      │
┌─ internal/sync ─ orchestrator ─────────────────────────────────────┐
│  Drives gmail.API (also implemented by imap.Client).               │
│  Full + incremental + history_id checkpointing.                    │
│  Routes MIME parse → store, advances cursor on partial failure.    │
└──────▲──────▲──────────────────────────▲──────────▲────────────────┘
       │      │                          │          │
   ┌───┴──┐ ┌─┴────┐                ┌────┴────┐ ┌───┴────────┐
   │gmail │ │imap  │                │importer │ │ deletion   │
   │      │ │      │                │ ingest  │ │ executor   │
   │+oauth│ │ /sasl│                │ ────    │ │ ────       │
   │+ms-  │ │      │                │ emlx    │ │ Manifests  │
   │ graph│ │      │                │ mbox    │ │ on disk;   │
   └──────┘ └──────┘                │ pst     │ │ Trash vs   │
                                    │ fbmsgr  │ │ permanent. │
                                    │ gvoice  │ └────────────┘
                                    │ imessage│
                                    │ whatsapp│
                                    │ apple   │
                                    │ textmpt │
                                    └─────────┘

┌─ internal/query  ── multi-backend query engine ────────────────────┐
│  Engine interface. Implementations: DuckDB-Parquet, SQLite,        │
│  Postgres (mostly stubs). text_engine: FTS5 + optional vector.     │
│  Used by: TUI, MCP, HTTP API, CLI.                                 │
└────────────────────────────────────────────────────────────────────┘

┌─ internal/vector ── sqlite-vec embeddings (build tag) ─────────────┐
│  Generations (rotation), enqueuer dual-writes during rebuild.      │
│  Backend: any OpenAI-compatible /v1/embeddings endpoint.           │
└────────────────────────────────────────────────────────────────────┘

┌─ internal/api  ─ chi HTTP    │  ┌─ internal/mcp ─ MCP tools ──────┐
│   /api/v1/...                │  │  search_messages,                │
│   • api_key auth             │  │  find_similar_messages, …        │
│   • CORS                     │  │  conditional registration on     │
│   • Gentle timeout 503s      │  │  vector backend presence         │
└──────────────────────────────┘  └─────────────────────────────────┘

┌─ internal/scheduler ─ cron (per-account drop-not-queue locking) ───┐
└────────────────────────────────────────────────────────────────────┘

┌─ internal/remote ─ HTTPS client implementing query.Engine ─────────┐
└────────────────────────────────────────────────────────────────────┘

┌─ internal/tui ─ Bubble Tea ────────────────────────────────────────┐
│  Stale-response filtering via per-domain request IDs.              │
│  transitionBuffer prevents flicker during async loads.             │
└────────────────────────────────────────────────────────────────────┘

┌─ Foundation ───────────────────────────────────────────────────────┐
│  mime  (enmime + chardet)         logging  (slog, PII rules)       │
│  textutil (charset/encoding)      config   (TOML, secret loading)  │
│  fileutil (cross-OS chmod 600)    update   (self-update; SHA req'd)│
│  testutil (builders, fixtures)    export   (.eml, attachments)     │
└────────────────────────────────────────────────────────────────────┘
```

---

## Data model

The system of record is SQLite. Core tables (full detail in
`docs/subsystems/storage.md`):

| Table                        | Purpose                                                        |
|------------------------------|----------------------------------------------------------------|
| `sources`                    | Per-account state: history_id, OAuth app, sync cursor          |
| `accounts` / `identities` / `collections` | Identity model. Email addresses cluster into identities; collections group identities. The `"All"` collection is auto-created and immutable to callers (see `docs/accounts-identities-collections-dedup/`). |
| `conversations`              | Thread abstraction (Gmail thread, RFC References for IMAP)     |
| `messages`                   | Lean header row — kept narrow for fast B-tree scans            |
| `message_bodies`             | Plaintext / HTML body — separated by design. **Never JOIN/scan in list/aggregate queries**; only PK lookup |
| `message_raw`                | Original RFC822, zlib-compressed                               |
| `participants`, `message_recipients` | From/To/Cc/Bcc with content_hash for dedup            |
| `labels`, `message_labels`   | Gmail-style labels (m:n)                                       |
| `attachments`                | Metadata; binaries live at `~/.msgvault/attachments/<2>/<sha256>` |
| `messages_fts`               | FTS5 virtual table (rowid → messages.id)                       |
| `vec_embeddings_*`           | sqlite-vec virtual tables, one per active generation           |
| `deletion_*` / `dedup_*`     | Staging + execution metadata for mutations                     |
| `sync_runs`, `sync_checkpoints` | Resumability                                               |

**Schema files**: `internal/store/schema.sql` (portable), `schema_sqlite.sql`
(FTS5 triggers), `schema_pg.sql` (tsvector + GIN; scaffold only — see
`docs/PG_STATUS.md`).

**Encryption-at-rest is dead schema**: `encryption_version` columns exist on
`attachments` and `message_raw` but no Go code reads or writes them.

---

## Storage on disk

All under `~/.msgvault/` (override with `MSGVAULT_HOME`):

```
~/.msgvault/
├── msgvault.db              SQLite system of record (WAL mode)
├── msgvault.db-wal/-shm     SQLite WAL/shared memory
├── attachments/<2>/<sha256> Content-addressed binaries
├── tokens/<email>.json      Per-account OAuth tokens, chmod 600
├── analytics/year=YYYY/     Parquet, partitioned by message year
├── analytics/_last_sync.json Incremental cache state
├── deletions/{pending,completed,failed,cancelled}/  Manifest staging
├── tmp/                     Project temp dir (config.MkTempDir fallback)
└── config.toml              Optional configuration
```

The deletion **directory is authoritative** — a manifest's location wins over
its inline `Status` field. Cancel renames first, then rewrites the field.

---

## End-to-end dataflow

### Gmail / IMAP / O365 sync

```
add-account → oauth.Authorize ─► tokens/<email>.json (chmod 600)
       │                              │
       ▼                              ▼
sync-full / sync (incremental)   gmail.Client / imap.Client (api.go shim)
       │
       ▼
internal/sync.Run
   ├── List page → ratelimit token bucket
   ├── For each msg: Get → mime.Parse → store.IngestMessage
   │     • dedup by RFC822 Message-ID; on collision UpdateMessageOnDedup
   │     • errDuplicateRFC822 rewrites composite IDs in-place
   ├── Append to history; advance history_id even on partial failure
   └── Checkpoint after each page (resumable)
```

IMAP fakes `gmail.API`. ListHistory errors out for IMAP; deletion falls back
from Batch to per-message. Microsoft O365 OAuth lives in `internal/microsoft`
and produces an IMAP token; the IMAP client takes it from there.

### File-format import

Email-shaped formats (MBOX, EMLX, PST) flow through
`internal/importer.IngestRawMessage`. PST builds RFC5322 from a binary tree
which the importer immediately re-parses with the standard MIME path —
wasteful but uniform.

Chat formats (iMessage, WhatsApp, Facebook Messenger DYI, Google Voice)
**bypass the shared importer**. Each one synthesizes identities and threads in
its own way and writes to the store directly.

### Build cache (Parquet)

`build-cache` (`cmd/msgvault/cmd/build_cache.go`) opens both DuckDB and a
direct SQLite connection — DuckDB's `sqlite_scanner` bypasses SQLite indexes,
so the max-id check is done via the direct connection. Output is denormalised
Parquet partitioned by year under `~/.msgvault/analytics/year=YYYY/`.

The TUI launcher auto-runs this when `cacheNeedsBuild` detects new messages,
dedup hides, or source-deletes. It collects all causes before returning so the
log line names every reason.

### Query

```
TUI / MCP / HTTP / CLI search
        │
        ▼
query.Engine                         ◄─ single interface, multiple impls
   ├── DuckDBEngine    (Parquet aggregates — the fast path)
   ├── SQLiteEngine    (text/FTS5; aggregates fallback when no Parquet)
   ├── PostgresEngine  (mostly ErrNotImplemented — see PG_STATUS.md)
   └── remote.Engine   (HTTPS client to a `serve` daemon)

text path: text_engine.go → FTS5 OR sqlite_vec OR hybrid (RRF)
```

Hybrid uses Reciprocal Rank Fusion of BM25 + vector. Two implementations: a
Go-side `hybrid/rrf.Fuse` (subject boost applied in Go) and a SQL CTE
`sqlitevec.FusedSearch` (subject boost applied — TODO(verify)).

### Mutation: staged deletion

```
TUI selects → stage → ~/.msgvault/deletions/pending/<id>.json (manifest)
                              │
                              ▼
delete-staged                 (review + execute)
   ├── Execute (per-msg)  → fail dir only if all messages failed
   └── ExecuteBatch       → completed dir always; retries previous failures
                            on resume; success can decrement failure count
```

Trash vs permanent delete is per-manifest. Permanent delete needs the
`https://mail.google.com/` scope (or domain-wide-delegation equivalent).

---

## Concurrency story

| Surface         | Concurrency model                                                      |
|-----------------|------------------------------------------------------------------------|
| CLI commands    | One process per invocation. SQLite WAL allows reads during writes.     |
| Sync            | Sequential within an account; multiple accounts can run in parallel via the scheduler. Cron ticks during in-flight sync skip silently (`scheduler.running[email]`). |
| Embedding       | `sync.Mutex.TryLock` — drop, don't queue. Auto-activation on `pendingCount==0` is non-atomic by design (see `internal/scheduler/CLAUDE.md`). |
| HTTP API        | chi gentle timeout: 503 reaches client *before* TCP teardown, locked in by a test. WriteTimeout = requestTimeout + 5s. |
| MCP server      | Read-only store (CLI flag); no writers expected.                       |
| TUI             | Bubble Tea single-threaded Update loop. Async loads carry per-domain request IDs; stale responses are dropped. transitionBuffer prevents flicker. |

---

## Build tags

Default tags: `fts5 sqlite_vec`. Three command pairs are stub-gated:

| Real file                  | Stub file                       |
|----------------------------|---------------------------------|
| `embed_vector.go`          | `embed_vector_stub.go`          |
| `search_vector.go`         | `search_vector_stub.go`         |
| `serve_vector.go`          | `serve_vector_stub.go`          |

Without `sqlite_vec`, the binary still builds; vector commands return clean
errors.

---

## Security boundaries

(See also `SECURITY.md`.)

- **Token files**: `~/.msgvault/tokens/<email>.json`, chmod 600. `internal/fileutil` enforces cross-OS.
- **Service accounts**: `chmod 600` reminded in README; not enforced by msgvault.
- **API auth**: empty `[server] api_key` *disables* auth and only logs WARN; `cfg.Server.ValidateSecure()` refuses to start non-loopback.
- **MCP `--http`**: rejects non-loopback hosts unless `--http-allow-insecure`. Empty host (`[]:port`) is treated as non-loopback (Go binds all interfaces).
- **Remote client**: refuses plain HTTP unless `[remote] allow_insecure = true`.
- **Update**: refuses to install without checksum; tar/zip extraction sanitises paths and skips symlinks.
- **PII in logs**: per global CLAUDE.md, never log message content / addresses at INFO. Use DEBUG.

---

## Where to look next

| Doc                                                | When you need                              |
|----------------------------------------------------|--------------------------------------------|
| `docs/subsystems/storage.md`                       | Schema, dialect, FTS5, dedup invariants     |
| `docs/subsystems/query-and-search.md`              | Engine layering, FTS, vector, hybrid        |
| `docs/subsystems/sync-and-auth.md`                 | Gmail/IMAP/O365 + OAuth flows               |
| `docs/subsystems/importers.md`                     | Each ingest format end-to-end               |
| `docs/subsystems/tui.md`                           | TUI state machine, navigation, search       |
| `docs/subsystems/service-layer.md`                 | HTTP API, MCP, scheduler, remote            |
| `docs/subsystems/utilities.md`                     | Mime, deletion, testutil, config, etc.      |
| `docs/subsystems/cli.md`                           | Full command catalog (58 commands)          |
| `docs/TESTING.md`                                  | Test taxonomy + prioritised gap list        |
| `docs/PG_STATUS.md`                                | Postgres scaffold blockers                  |
| `docs/accounts-identities-collections-dedup/`      | Identity model design                       |
| `internal/<pkg>/CLAUDE.md`                         | Scoped context when editing in that package |
| `internal/query/DESIGN.md`                         | Query engine design (note: stale on RemoteEngine) |
