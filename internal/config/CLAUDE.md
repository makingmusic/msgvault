# internal/config

Loads and persists `~/.msgvault/config.toml`, computes derived paths
(`AttachmentsDir`, `TokensDir`, `AnalyticsDir`, `LogsDir`,
`DatabasePath`), and provides `MkTempDir` (the Windows-friendly temp
directory helper used everywhere else).

## File layout

- `config.go` — all of it. ~620 lines but mostly TOML struct
  declarations and accessors.

## Top-level shape

```go
type Config struct {
    Data      DataConfig
    Log       LogConfig
    OAuth     OAuthConfig
    Microsoft MicrosoftConfig
    Sync      SyncConfig
    Chat      ChatConfig
    Server    ServerConfig
    Remote    RemoteConfig
    Vector    vector.Config
    Identity  IdentityConfig
    Accounts  []AccountSchedule
    HomeDir   string  // computed, not in file
}
```

## Path resolution rules

- `DefaultHome()` checks `MSGVAULT_HOME` env var first, then
  `~/.msgvault/`. Expands `~` in the env var value
  (`config.go:221`).
- `Load(path, homeDir)` accepts an optional explicit config path and
  optional override home directory:
  - Both empty → load `~/.msgvault/config.toml`, optional (missing →
    defaults).
  - `homeDir` set → override HomeDir; load
    `<homeDir>/config.toml`.
  - `path` set → must exist; HomeDir defaults to that file's parent
    directory so all data lives next to the config.
- After decode, **all path fields go through `expandPath`** (handles
  `~` and Windows-style quoted paths). When `--config` is explicit,
  relative paths are then resolved against `HomeDir` via
  `resolveRelative`.

## Save (atomic)

`Config.Save()` (`config.go:444`) writes the config back atomically:

1. Resolves symlinks so atomic rename replaces the target, not the
   symlink.
2. Creates a temp file in the same dir.
3. Chmod 0600, encode TOML, sync, close.
4. Rename into place.

## Derived paths

| Method | Returns |
| --- | --- |
| `DatabaseDSN()` | `Data.DatabaseURL` if set, else `<DataDir>/msgvault.db` |
| `DatabasePath()` | Filesystem path; decodes `file:` URI percent-encoding; errors on `postgres://` etc. |
| `AttachmentsDir()` | `<DataDir>/attachments` |
| `TokensDir()` | `<DataDir>/tokens` |
| `AnalyticsDir()` | `<DataDir>/analytics` |
| `LogsDir()` | `Log.Dir` if set, else `<DataDir>/logs` |
| `ConfigFilePath()` | The actual path used (when --config), else `<HomeDir>/config.toml` |

## OAuth lookups

`OAuthConfig.ClientSecretsFor(name)`:
- `name == ""` → returns `OAuth.ClientSecrets`.
- Otherwise → `OAuth.Apps[name].ClientSecrets`.
- Returns a guidance error string when missing.

`HasAnyConfig()` is the "is OAuth set up at all?" predicate used by
add-account and the quickstart wizard.

## ServerConfig safety

`ServerConfig.ValidateSecure()` (`config.go:50`) refuses to start
when bind address is non-loopback, no API key is set, and
`AllowInsecure` is false. The `serve` command calls this before
binding.

## MkTempDir

`config.MkTempDir(pattern, preferredDirs...)` (`config.go:539`) is
**the** temp-dir helper for the project. It tries (in order):

1. Each `preferredDirs` value.
2. `os.TempDir()` (system default).
3. `<MSGVAULT_HOME>/tmp/`.

This exists because Windows `%TEMP%` is sometimes unreadable due to
group policy / antivirus. Use this everywhere instead of
`os.MkdirTemp("", ...)`.

## When editing

- New TOML fields: add the struct field with a `toml:"..."` tag, set
  defaults in `NewDefaultConfig` if non-zero, and (if it's a path)
  add it to the `expandPath` and `resolveRelative` blocks in
  `Load`.
- Don't break `Save` atomicity. The temp-file-then-rename pattern is
  load-bearing: a crash mid-write must not leave a half-written
  config.
- Adding a new derived path? Match the `<Name>Dir()` method
  convention. Other commands rely on consistent helpers.
- `MkTempDir` is the only correct way to make a temp directory in
  this project. New callers go through it.
