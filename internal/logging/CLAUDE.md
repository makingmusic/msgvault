# internal/logging

Builds the process-wide `slog.Handler` used by every CLI invocation.
Fans records out to stderr (text) and to a daily rotating file (JSON),
attaches a process-wide `run_id` to every record.

## File layout

- `logging.go` — `BuildHandler(opts) (*Result, error)` plus the
  internal `multiHandler`, daily rotation logic, and `discardHandler`.

## Public API

`BuildHandler(opts Options) (*Result, error)` — call once at CLI
startup. Returns:

- `Result.Handler` — the slog handler to install via
  `slog.SetDefault(slog.New(handler))`.
- `Result.FileHandler` — the JSON-to-disk handler in isolation. The
  TUI command swaps `slog.Default()` to this so log records don't
  corrupt the alternate-screen rendering.
- `Result.RunID` — 6-byte hex correlation token, automatically
  attached to every log record.
- `Result.FilePath` — the resolved log file path, empty when file
  logging is disabled or fell back to stderr-only.
- `Result.Close()` — flush/close file handles.

## Defaults

- `MaxFileBytes` = 50 MiB, `KeepRotated` = 5 (i.e., `.1` through `.5`
  rotated siblings, then deletion).
- File name: `msgvault-YYYY-MM-DD.log` (UTC).
- File mode: `0o600`.
- Level: from `LevelString` (config) or `LevelOverride`. Accepted
  strings: `debug`, `info`, `warn`/`warning`, `error`. Empty/unknown →
  `info`.

## Conventions enforced by callers (NOT this package)

This package does not parse PII out of records — that's the
responsibility of every call site. Per the global CLAUDE.md:

- **Never log user/financial PII at INFO.** Email content, account
  balances, holdings, attachment names containing real filenames.
  Use `Debug` for context the operator needs only when explicitly
  enabled.
- At `Info`: intent classification, counts, durations, error types.
- Don't compute log-only data. If you need a stat for a log line,
  compute it from data already on the hot path.

The text vs JSON split exists because operators want both: greppable
for-humans on stderr, mechanically parseable on disk. Don't add a
single-format mode.

## File-only logger for TUI

`Result.FileOnlyLogger()` (`logging.go:109`) returns a logger that
silently drops to discard when file logging is disabled. The TUI's
`alternate screen` mode replaces `slog.Default()` with this so any
slog calls inside the renderer don't paint over the screen, but the
records still land in the daily file under the same `run_id`.

## When editing

- The `multiHandler` clones records before forwarding (`Handle`,
  `logging.go:373`) — don't optimize that away. slog records carry
  attribute slices that handlers may mutate.
- Rotation order is from `keep` down to `1` (`logging.go:298`); doing
  it the other way leaks data when two slots collide.
- Don't add cross-day rotation logic. The current design is one file
  per UTC day, no exceptions; cross-day means the timestamp is the
  primary key.
- If logging file open fails, BuildHandler degrades to stderr-only and
  prints a one-line warning. Do not hard-fail; logging must never
  break the CLI.
