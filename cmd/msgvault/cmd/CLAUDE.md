# cmd/msgvault/cmd — CLI surface

Cobra-based command tree for the `msgvault` binary. One `.go` file per
top-level command (with a few groups that bundle subcommands), plus a
small set of shared helpers. The full per-command catalog lives in
[`docs/subsystems/cli.md`](../../../docs/subsystems/cli.md).

## Layout pattern

- `root.go` declares `rootCmd`, persistent flags, and the
  `PersistentPreRunE` that loads config, builds the logger, and emits
  the `msgvault startup` log line. Every command is registered from its
  own `init()` via `rootCmd.AddCommand(...)`.
- Each command lives in its own file (e.g. `syncfull.go`, `tui.go`,
  `mcp.go`, `serve.go`). Sibling files implement the same command's
  helpers — keep the package flat rather than introducing subpackages.
- Subcommand groups (`identity ...`, `collection ...`) declare a parent
  in the same file as their children and register them as a tree:
  `parentCmd.AddCommand(childCmd)`.
- Build-tag-gated commands (vector search) ship as paired files: a
  real implementation under `//go:build sqlite_vec` and a stub under
  `//go:build !sqlite_vec` that returns a clear error. See
  `embed_vector.go` / `embed_vector_stub.go`,
  `search_vector.go` / `search_vector_stub.go`,
  `serve_vector.go` / `serve_vector_stub.go`.

## Config and logging bootstrap (`root.go`)

`PersistentPreRunE` runs for every command except a small allowlist
(`version`, `update`, `quickstart`, `completion`, shell-completion
helpers). It:

1. Calls `config.Load(cfgFile, homeDir)` and stores the result in the
   package-level `cfg` (defaults to `~/.msgvault/config.toml`).
2. Calls `cfg.EnsureHomeDir()` to create the data directory.
3. Builds the logger via `logging.BuildHandler(...)` and assigns it to
   the package-level `logger` and `slog.Default`.
4. Configures store SQL logging through `store.ConfigureSQLLogging`.
5. Emits a single structured `msgvault startup` line with `command`,
   `argc`, version metadata, config path, and data dir. Argv is
   redacted via `sanitizeArgs` (`--password`, `--token`, etc.) before
   it reaches debug logs.

Persistent flags defined here: `--config`, `--home`, `--verbose`,
`--local`, `--log-file`, `--log-level`, `--no-log-file`, `--log-sql`,
`--log-sql-slow-ms`. Use these from any subcommand.

`Execute` / `ExecuteContext` install a deferred panic recovery
(`recoverAndLogPanic`) and a deferred `logResult.Close()`. Defer
ordering is load-bearing — see the comment in `ExecuteContext`.

OAuth helpers also live in `root.go`: `oauthSetupHint`,
`errOAuthNotConfigured`, `tryFindClientSecrets`, `wrapOAuthError`,
`getTokenSourceWithReauth`, `oauthManagerCache` (per-app `oauth.Manager`
cache used by `serve` for concurrent scheduled syncs).

## Shared helpers

- `store_resolver.go` — `OpenStore`, `OpenRemoteStore`,
  `openLocalStore`, `openLocalStoreAndInit`, `IsRemoteMode`,
  `MustBeLocal`. The `MessageStore` / `RemoteStore` interfaces let
  list/show/search commands work against either a local SQLite store
  or an HTTP `remote.Store` driver. Resolution order:
  `--local` flag → local; otherwise `[remote].url` in config → remote;
  otherwise local. `MustBeLocal(name)` is the gate for local-only
  commands (e.g. `import-*`, `delete-deduped`, `create-subset`).
  This file also owns the legacy-identity startup migration helpers
  (`runStartupMigrations`, `runPostSourceCreateMigrations`); ingest
  commands re-run migrations after creating their first source.
- `account_scope.go` — `ResolveAccountFlag` and `ResolveCollectionFlag`
  resolve `--account` / `--collection` against the store and return a
  `Scope` that exposes `SourceIDs()` and `DisplayName()`. Each
  resolver rejects the wrong kind of input with a hint to use the
  other flag (`"foo" is a collection, not an account; use --collection`).
  Use `Scope` whenever a command needs to filter messages by source.
- `account_identity.go` — `confirmDefaultIdentity`. All ingest commands
  (`add-account`, `add-imap`, `add-o365`, `import-*`) call this on
  every invocation to write the account's own identifier into
  `account_identities` exactly once. The function MUST be called
  before `runPostSourceCreateMigrations`; reversing the order
  re-introduces a known regression (see the long comment in the file).
- `confirm.go` — `confirmDestructive(r, w, mode)` for destructive
  prompts. Three modes: `ConfirmModePermanent` (must type literal
  `delete`), `ConfirmModeAllHidden` (y/N, EOF errors), and
  `ConfirmModeYesNo` (y/N, EOF cancels cleanly). Reader/writer split
  is for unit tests.
- `output.go` — shared `--limit` / `--after` / `--before` / `--json`
  flags, `parseCommonFlags`, `addCommonAggregateFlags`,
  `outputAggregateTable`, `outputAggregateJSON`, `formatSize`,
  `printJSON`. Used by `list-senders`, `list-domains`, `list-labels`.
- `cliprogress_test.go` — `rateWindow` ring buffer for ETA reporting
  (despite the `_test.go` suffix this is production code; only the
  tests live here, but the type is consumed by sync printers).

## Adding a new command

1. Create `cmd/msgvault/cmd/<verb>.go` with a `var <verb>Cmd =
   &cobra.Command{Use: "<verb>", ...}`.
2. Register it in `init()` with `rootCmd.AddCommand(<verb>Cmd)`. Define
   flags on the same command in the same `init()`.
3. If it reads/writes the local DB: call `openLocalStoreAndInit()` (or
   `OpenStore()` if it can run against a remote server).
4. If it must run locally: gate with `MustBeLocal("<verb>")`.
5. If it ingests messages: call `confirmDefaultIdentity` then
   `runPostSourceCreateMigrations` after creating a source (see
   `addaccount.go` for the canonical order).
6. If it accepts `--account` / `--collection`: use `ResolveAccountFlag`
   / `ResolveCollectionFlag` and act on `Scope.SourceIDs()`.
7. Add a sibling `<verb>_test.go`. Most existing tests build the
   command via its constructor (`newRemoveAccountCmd`) or invoke the
   command's `RunE` directly with a temp store; see
   `remove_account_test.go`, `deduplicate_test.go`, `serve_test.go`.
8. If the feature requires sqlite-vec, ship a `<verb>_vector_stub.go`
   alongside `<verb>_vector.go` so non-tagged builds give a clean
   error instead of failing to link.
