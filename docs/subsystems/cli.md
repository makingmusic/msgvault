# CLI subsystem

`cmd/msgvault/cmd/` implements every Cobra command exposed by the
`msgvault` binary. The package is flat: one `.go` file per command (a
few group commands hold their subcommands in the same file), plus
shared helpers in `root.go`, `store_resolver.go`, `account_scope.go`,
`account_identity.go`, `confirm.go`, and `output.go`.

This doc enumerates every registered command in the tree. For the
package-level conventions and the patterns used by each command, see
[`cmd/msgvault/cmd/CLAUDE.md`](../../cmd/msgvault/cmd/CLAUDE.md).

## Persistent flags (every command inherits these)

Defined in `root.go:454-470`:

| Flag | Description |
|---|---|
| `--config` | Path to `config.toml` (default `~/.msgvault/config.toml`). |
| `--home` | Override `MSGVAULT_HOME` for this invocation. |
| `--verbose, -v` | Force `--log-level=debug`. |
| `--local` | Force local DB even when `[remote].url` is set. |
| `--log-file` | Override the on-disk log file path. |
| `--log-level` | `debug` / `info` / `warn` / `error`. |
| `--no-log-file` | Disable the log file for this run. |
| `--log-sql` | Log every SQL query at info level. |
| `--log-sql-slow-ms` | Threshold (ms) above which a query is logged as slow. |

## Command catalog

Counts: 49 top-level commands and 9 subcommands across two groups
(`identity`, `collection`), totalling 58 registered Cobra commands.
Numbers below cite `file:line` of the `var ...Cmd = &cobra.Command{`
declaration.

### Setup

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `init-db` | `initdb.go:10` | Create or upgrade the SQLite schema; print stats. | `internal/store` |
| `setup` | `setup.go:17` | Interactive first-run wizard: locate OAuth client secrets, write `config.toml`, optionally configure remote NAS. | filesystem only |
| `quickstart` | `quickstart.go:13` | Print embedded `quickstart.md` (designed to be piped into an AI agent). Runs without config. | none |
| `add-account <email>` | `addaccount.go:21` | Authorize a Gmail account via OAuth. Flags: `--headless`, `--force`, `--display-name`, `--oauth-app`, `--no-default-identity`. | `internal/oauth`, `internal/store` |
| `add-imap` | `addimap.go:60` | Add an IMAP account with username/password. Flags: `--host`, `--port`, `--username`, `--no-tls`, `--starttls`, `--no-default-identity`. Reads password from stdin or `MSGVAULT_IMAP_PASSWORD`. | `internal/imap`, `internal/store` |
| `add-o365 <email>` | `addo365.go:18` | Add a Microsoft 365 account via OAuth + IMAP XOAUTH2. Flags: `--tenant`, `--no-default-identity`. Requires `[microsoft]` config block. | `internal/microsoft`, `internal/imap`, `internal/store` |
| `update-account <email>` | `update_account.go:12` | Update settings for an existing account. Flag: `--display-name`. | `internal/store` |
| `remove-account <email>` | `remove_account.go:18` (constructor in `newRemoveAccountCmd`) | Delete an account and all its data. Flags: `--yes`, `--type` (gmail / mbox / etc.). Local-only. | `internal/store`, attachments dir, parquet cache |
| `list-accounts` | `list_accounts.go:17` | List configured accounts. | local or remote store |

### Sync

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `sync-full [email]` | `syncfull.go:31` | Full sync (Gmail + IMAP). Flags: `--query`, `--noresume`, `--before`, `--after`, `--limit`. Resumable. With no email: sync every configured account. | `internal/sync`, `internal/gmail`, `internal/imap` |
| `sync [email]` (alias `sync-incremental`) | `sync.go:22` | Incremental sync via Gmail History API; falls back to full sync for IMAP/non-Gmail. With no email: sync every account. | `internal/sync` |
| `verify <email>` | `verify.go:26` | Compare local count and integrity against Gmail; sample raw MIME. Flags: `--sample`, `--skip-db-check`. | `internal/store`, `internal/gmail` |

### Import

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `import-mbox <identifier> <file>` | `import_mbox.go:33` | Import a `.mbox` file or `.zip` of mboxes. Flags: `--source-type`, `--label`, plus checkpoint/limit options. Local-only (gated via `MustBeLocal`). | `internal/mboximport` |
| `import-emlx [mail-dir]` | `import_emlx.go:30` | Import Apple Mail `.emlx` trees. Auto-discovers accounts via `~/Library/Accounts/Accounts4.sqlite`. Flags: `--account` (repeatable), `--identifier` (manual fallback), `--no-default-identity`. | `internal/emlximport` |
| `import-pst <identifier> <pst-file>` | `import_pst.go:23` | Import an Outlook PST. Resumable. Flags: `--no-resume`, `--skip-folder`, `--no-attachments`. | `internal/pstimport` |
| `import-messenger <dyi-export-dir>` | `import_messenger.go:24` | Import Facebook Messenger DYI export (JSON or HTML). Flags: `--me` (required, `<slug>@facebook.messenger`), `--format` (json/html/both), `--limit`, `--no-resume`, `--checkpoint-every`. Local-only. | `internal/messengerimport` |
| `import-imessage` | `import_imessage.go:26` | Import macOS Messages from `chat.db` (requires Full Disk Access). Flags: `--db-path`, `--after`, `--before`, `--limit`. | `internal/imessageimport` |
| `import-gvoice <takeout-voice-dir>` | `import_gvoice.go:23` | Import Google Voice texts/calls/voicemails from a Takeout `Voice/` folder. Flags: `--after`, `--limit`, `--no-default-identity`. | `internal/gvoiceimport` |
| `import-whatsapp <msgstore.db>` | `import.go:27` | Import a decrypted WhatsApp `msgstore.db`. Required: `--phone` (E.164). Flags: `--media-dir`, `--contacts`, `--limit`, `--display-name`, `--no-default-identity`. Local-only. | `internal/whatsappimport` |
| `import [path]` | `import.go:238` | **Deprecated** alias forwarding `import --type whatsapp` to `import-whatsapp`. Hidden in help; kept for one release cycle. | — |

### Search and inspection

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `search <query>` | `search.go:26` | Gmail-syntax search (`from:`, `to:`, `subject:`, `label:`, `has:`, `before:`, `after:`, `older_than:`, `newer_than:`, `larger:`, `smaller:`, plus FTS terms). Flags: `--limit`, `--offset`, `--json`, `--account`, `--collection`, `--mode` (`fts` / `vector` / `hybrid`). Vector / hybrid require sqlite-vec build tag. Routes to remote when `[remote].url` is set. | `internal/search`, `internal/query`, `remote.Store`, `internal/vector/*` |
| `show-message <id>` | `show_message.go:20` | View a single message. Flag: `--json`. Works against local or remote. | local store or `remote.Store` |
| `query [sql]` | `query.go:20` | Run arbitrary SQL against the Parquet analytics views. Flag: `--format` (`json` / `csv` / `table`). Auto-builds cache if stale. | `internal/query` (DuckDB) |
| `list-senders` | `list_senders.go:11` | Top senders by count/size. Common aggregate flags (`--limit`, `--after`, `--before`, `--json`). | `internal/query` |
| `list-domains` | `list_domains.go:11` | Top sender domains. Same flag set. | `internal/query` |
| `list-labels` | `list_labels.go:11` | Label distribution. Same flag set. | `internal/query` |
| `stats` | `stats.go:15` | Print archive stats (counts, size). | local or remote store |
| `tui` | `tui.go:24` | Bubble Tea TUI. Flags: `--account`, `--local`, `--force-sql`. Auto-builds Parquet cache when stale. | `internal/tui`, `internal/query` |

### Deduplication and deletion

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `deduplicate` (alias `dedup`, `dedupe`) | `deduplicate.go:20` | Find and merge duplicate messages by Message-ID (and optionally content hash). Flags: `--account`, `--collection`, `--dry-run`, `--content-hash`, `--undo <batch-id>` (repeatable), `--delete-dups-from-source-server`. Reversible. | `internal/dedup`, `internal/store` |
| `delete-deduped` | `delete_deduped.go:11` | Permanently hard-delete dedup-hidden rows. Flags: `--batch` (repeatable), `--all-hidden`, `--no-backup`, `--yes`. Local-only; not reversible. | `internal/store` |
| `list-deletions` | `deletions.go:22` | List deletion batches across all statuses. | `internal/deletion` |
| `show-deletion <batch-id>` | `deletions.go:93` | Show a single deletion manifest. | `internal/deletion` |
| `cancel-deletion [batch-id]` | `deletions.go:118` | Cancel pending/in-progress batches. Flag: `--all`. | `internal/deletion` |
| `delete-staged [batch-id]` | `deletions.go:236` | Execute staged deletions against Gmail. Flags: `--permanent`, `--yes`, `--dry-run`, `--list`, `--account`. Gated by `MSGVAULT_ENABLE_REMOTE_DELETE=1` for the v1 release; `--list` and `--dry-run` work without the gate. `--permanent` and `--yes` are mutually exclusive. | `internal/deletion`, `internal/gmail` |

### Service / integration

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `serve` | `serve.go:28` | Long-running daemon: HTTP API + scheduled syncs (cron in `[[accounts]]` config). Reads vector features when `[vector]` enabled with sqlite-vec build. | `internal/api`, `internal/scheduler`, `internal/sync`, vector backends |
| `mcp` | `mcp.go:22` | MCP (Model Context Protocol) server over stdio for Claude Desktop. Flags: `--force-sql`, `--no-sqlite-scanner`, `--http`, `--http-allow-insecure`. Read-only access to the store. | `internal/mcp`, `internal/query`, vector backends |

### Cache / analytics

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `build-cache` (alias `build-parquet`) | `build_cache.go:45` | Export SQLite to partitioned Parquet via DuckDB. Flag: `--full-rebuild`. Incremental by default. | `internal/query` (DuckDB ETL) |
| `cache-stats` (alias `parquet-stats`) | `build_cache.go:524` | Show row counts and file sizes for the analytics cache. | filesystem |

### Vector / embeddings

These commands and their helpers are gated by the `sqlite_vec` build
tag. `make build` sets `-tags "fts5 sqlite_vec"`. Without the tag, the
stub files compile in instead and return a clear error.

| Command | File:line | Stub | Real impl | Notes |
|---|---|---|---|---|
| `build-embeddings` | `embed.go:14` | `embed_vector_stub.go:14` (`runEmbed`) | `embed_vector.go:23` | Build/update vector index. Flags: `--full-rebuild`, `--yes`. Requires `[vector]` enabled in config. |
| `search --mode=vector` / `search --mode=hybrid` | `search.go:26` | `search_vector_stub.go:15` (`runHybridSearch`) | `search_vector.go:30` | Vector or hybrid retrieval. Stub error mentions exact rebuild command. |
| (helper) `setupVectorFeatures` | — | `serve_vector_stub.go:15` | `serve_vector.go:24` | Wires backend / hybrid engine / embed worker into `serve` and `mcp`. The struct itself lives in `vector_features.go:20` (`vectorFeatures`) and is build-tag-neutral. |

`embed_progress.go` is build-tag-neutral and contains `rateWindow`, the
batch-rate ring buffer used by the embed worker's progress printer.

### Maintenance

| Command | File:line | Purpose & key flags | Touches |
|---|---|---|---|
| `repair-encoding` | `repair_encoding.go:18` | Fix invalid UTF-8 across subjects, bodies, snippets, participant fields, conversation titles, label names, attachment filenames. Re-parses raw MIME with charset detection. | `internal/textutil`, `internal/store` |
| `rebuild-fts` | `rebuild_fts.go:12` | Rebuild the FTS5 virtual table. | `internal/store` |
| `export-eml <id>` | `export_eml.go:24` | Export a single message to a `.eml` file. | `internal/store` |
| `export-attachment <content-hash>` | `export_attachment.go:22` | Export a single attachment by content hash. | attachments dir |
| `export-attachments <message-id>` | `export_attachments.go:17` | Export every attachment of a message. | attachments dir |
| `export-token <email>` | `export_token.go:24` | Export an OAuth token (used to seed a remote NAS). | `internal/oauth` |
| `create-subset` | `create_subset.go:13` | Copy the most recent N messages to a fresh `msgvault.db`. Flags: `--output` (required), `--rows` (required). Local-only. | `internal/store.CopySubset` |
| `logs` | `logs.go:28` | View / tail structured log files in `<data dir>/logs`. Flags: `-n`, `--follow`, `--run-id`, `--level`, `--grep`, `--all`, `--path`. | filesystem |

### Identity (`identity ...`)

Parent: `identity.go:25`. Manage the confirmed "me" identifiers per
account. Used by dedup's sent-copy detection.

| Command | File:line |
|---|---|
| `identity list` | `identity.go:38` |
| `identity show <account>` | `identity.go:224` |
| `identity add <account> <identifier>` | `identity.go:263` |
| `identity remove <account> <identifier>` | `identity.go:328` |

### Collections (`collection ...`)

Parent: `collection.go:15`. Named groupings of accounts; the only way
to dedup across sources. A default `All` collection always exists.

| Command | File:line |
|---|---|
| `collection create <name>` | `collection.go:25` |
| `collection list` | `collection.go:32` |
| `collection show <name>` | `collection.go:38` |
| `collection add <name>` | `collection.go:45` |
| `collection remove <name>` | `collection.go:52` |
| `collection delete <name>` | `collection.go:59` |

The `add`, `remove`, and `create` subcommands all take
`--accounts <email1,email2,...>`.

### Misc

| Command | File:line | Purpose |
|---|---|---|
| `version` | `version.go:17` | Print version, commit, build date, Go version, OS/arch. Skips config load. |
| `update` | `update.go:13` | Self-update binary. Flags: `--check`, `--yes`, `--force`. Skips config load. |
| `completion [shell]` | `completion.go:9` | Emit shell completion (bash / zsh / fish / powershell). Skips config load. |

## Common flag conventions

- `--account <name>` — restrict to one source (single-select). Resolved
  via `ResolveAccountFlag` (`account_scope.go:52`); rejects collection
  names with a hint to use `--collection`.
- `--collection <name>` — expand to every member account. Resolved via
  `ResolveCollectionFlag` (`account_scope.go:104`).
- `--limit, -n` — bound result rows. Used by aggregate commands via
  `addCommonAggregateFlags` (`output.go:51`) and ad-hoc by sync /
  search / import.
- `--after`, `--before` — `YYYY-MM-DD` date filters; aggregate commands
  parse these in `parseCommonFlags` (`output.go:23`).
- `--json` — emit machine output. Aggregate commands route through
  `outputAggregateJSON` (`output.go:96`); search / show-message route
  through their own `printJSON` callers.
- `--yes, -y` — skip a destructive confirmation. See `confirm.go` for
  the three confirmation modes.
- `--local` — persistent flag defined on `rootCmd`; force local DB
  even when `[remote].url` is set.
- `--no-default-identity` — used by every ingest command
  (`add-account`, `add-imap`, `add-o365`, `import-emlx`,
  `import-gvoice`, `import-whatsapp`). Help text in
  `account_identity.go:13`.

## How commands open the right store

`store_resolver.go` centralises the open path:

- `OpenStore() MessageStore` — local SQLite when `--local` is set OR
  no `[remote].url` is configured; otherwise an HTTP `remote.Store`.
- `OpenRemoteStore() RemoteStore` — always remote; errors with a
  `[remote]` config example if not configured.
- `openLocalStoreAndInit()` — opens the local DB, runs `InitSchema`,
  and runs `runStartupMigrations` (legacy `[identity]` migration).
  Most commands that mutate local data call this.
- `MustBeLocal(name)` — gate inside `RunE` for commands that have no
  remote-server equivalent (`import-*`, `delete-deduped`,
  `create-subset`, `import` deprecated alias).

The `MessageStore` interface is intentionally narrow:

```go
GetStats() (*store.Stats, error)
ListMessages(offset, limit int) ([]store.APIMessage, int64, error)
GetMessage(id int64) (*store.APIMessage, error)
SearchMessages(q string, offset, limit int) ([]store.APIMessage, int64, error)
Close() error
```

Both `*store.Store` (local SQLite) and `*remote.Store` (HTTP client)
satisfy it. `RemoteStore` extends with `ListAccounts()`.

## How commands handle multi-account scope

The `Scope` type returned by the resolvers (`account_scope.go:11`)
exposes:

- `IsEmpty()` — no `--account` / `--collection` was supplied.
- `IsCollection()` — distinguish single-source vs. multi-source.
- `SourceIDs()` — the list of source IDs to filter on. For collections
  this is every member; for a single account it's a one-element slice.
- `DisplayName()` — the user-facing label for log lines and prompts.

Dedupe, search, identity-list, and the deletion staging path all use
`Scope.SourceIDs()` to drive the SQL filter rather than re-implementing
"is this an account or a collection" logic.

## Tests

36 `_test.go` files in the package. Coverage breakdown:

- Account / identity / collection lifecycle:
  `addaccount_test.go`, `addimap_test.go`, `addo365_test.go`,
  `remove_account_test.go`, `update_account_test.go` (none currently),
  `account_identity_test.go`, `account_scope_test.go`,
  `identity_test.go`, `collection_test.go`.
- Imports: `import_mbox_test.go`, `import_mbox_e2e_test.go`,
  `import_messenger_e2e_test.go`, `import_imessage_test.go`,
  `build_cache_messenger_test.go`.
- Search and queries: `search_test.go`, `search_vector_test.go`,
  `query_test.go`, `stats_test.go`.
- Vector: `embed_vector_test.go`, `embed_progress_test.go`,
  `serve_vector_stub_test.go`, `search_vector_test.go`.
- Maintenance: `repair_encoding_test.go`, `verify_test.go`,
  `export_attachments_test.go`, `export_attachment_test.go`,
  `export_token_test.go`.
- Deletion path: `deletions_test.go`, `deduplicate_test.go`,
  `delete_deduped_test.go`.
- Service: `serve_test.go`, `mcp_test.go`.
- Sync: `sync_test.go`.
- Setup: `setup_test.go`, `root_test.go`, `confirm_test.go`,
  `cliprogress_test.go`.

Most tests build a temp `MSGVAULT_HOME`, run schema init, and exercise
the command's `RunE` (or a private helper) directly. Cobra's flag
plumbing is bypassed by setting the package-level flag variables
explicitly. Where a command is constructed by a factory
(`newRemoveAccountCmd`), the test invokes the factory and uses
`SetArgs` / `Execute` for full Cobra-path coverage.

## Notes on observed gaps and cruft

- `import` (`import.go:238`) is `Hidden` and `Deprecated` — kept only
  for one release cycle as an alias for `import-whatsapp`.
- `update_account_test.go` does not exist; `update-account`'s only
  current capability is `--display-name`, so coverage is thin.
- The README command table at `README.md:79-100` lists only ~20 of
  the 49 top-level commands. Many useful surfaces — `query`, `mcp`,
  `deduplicate`, `delete-staged`, `list-deletions`, `cache-stats`,
  `add-imap`, `add-o365`, `import-pst`, `import-messenger`,
  `import-imessage`, `import-gvoice`, `import-whatsapp`,
  `build-embeddings`, `create-subset`, `export-eml`, `export-token`,
  `export-attachment(s)`, `rebuild-fts`, `verify`, `logs`,
  `identity *`, `collection *` — are not advertised there. They are
  fully wired and tested.
- The `cliprogress_test.go` filename is misleading: it contains the
  `rateWindow` test, but the type itself is a sibling of
  `embed_progress.go` and is consumed by production code.
