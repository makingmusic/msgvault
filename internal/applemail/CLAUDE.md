# internal/applemail

## Purpose

Helpers that resolve Apple Mail's V10 directory layout to email addresses by reading macOS's `~/Library/Accounts/Accounts4.sqlite`. The `.emlx` parser (`internal/emlx`) finds mailboxes on disk; this package answers the separate question "which email account does this UUID directory belong to?". Used by the `import-emlx` CLI to auto-discover accounts when the user doesn't pass `--identifier` explicitly.

## Files

- `accounts.go` — UUID discovery, V10 dir resolution, Accounts4.sqlite query
- `accounts_test.go`

## Input format

Two distinct inputs:

1. **Apple Mail's V-versioned directory tree** under `~/Library/Mail/`:
   - `V*/` (V2, V9, V10, ...) — version-tagged container
   - `V*/<UUID>/` — one per account; UUID matches `Accounts4.sqlite.ZACCOUNT.ZIDENTIFIER`
   - `V*/<UUID>/<MAILBOX>.mbox/` — actual mailboxes (handed off to `internal/emlx`)

2. **`Accounts4.sqlite`** — Apple's system account database. Schema (relevant subset):
   - `ZACCOUNT` table: `Z_PK`, `ZIDENTIFIER` (UUID), `ZUSERNAME` (often the email), `ZACCOUNTDESCRIPTION`, `ZPARENTACCOUNT` (FK to parent row)
   - Apple stores email accounts as a child row whose parent holds the description.

There is no documented schema; reverse-engineered. Format may change with macOS releases.

## Key types and entry points

- `AccountInfo{GUID, Email, Description}` — `accounts.go:17`
- `(AccountInfo).Identifier() string` — `accounts.go:32`; returns email if present, else description.
- `DefaultAccountsDBPath() string` — `accounts.go:41`
- `ResolveAccounts(dbPath, []guids) (map[guid]AccountInfo, error)` — `accounts.go:52`; opens DB read-only and runs the `ZACCOUNT child JOIN parent` query.
- `DiscoverV10Accounts(mailDir, accountsDBPath, logger) ([]AccountInfo, error)` — `accounts.go:109`
- `V10AccountDir(mailDir, guid) (string, error)` — `accounts.go:177`; finds the right V* directory for a given GUID, preferring directories that actually contain mailboxes.
- `findV10GUIDs(mailDir)` — `accounts.go:148`; scans V* dirs newest-first and returns deduplicated GUIDs.

## Output

This package writes nothing to msgvault. The `import-emlx` command uses it to map each discovered V10 GUID directory to a real email address, then calls `internal/importer.ImportEmlxDir` once per account with `Identifier = AccountInfo.Email`.

## Edge cases

- **Multiple V* directories**: macOS upgrades layer V2 → V9 → V10 over time without removing old data. `sortedVDirs` returns newest-first; `findV10GUIDs` deduplicates across them. `V10AccountDir` prefers a directory with mailboxes, falling back to the newest existing.
- **GUID not in Accounts4.sqlite**: logged as a warning and skipped (`accounts.go:133`). Stale on-disk data is common after account removal.
- **"On My Mac" / local accounts**: have no `ZUSERNAME`, so `Email` is empty and `Identifier()` returns the description string. The importer still runs but the source identifier won't be a real email.
- **Read-only open**: `dbPath+"?mode=ro"` (`accounts.go:57`) — Apple Mail might be running and holding write locks; opening RO sidesteps `database is locked` errors.

## Gotchas

- The query `COALESCE(NULLIF(child.ZUSERNAME, ''), NULLIF(parent.ZUSERNAME, ''), '')` (`accounts.go:74`) walks one level up the parent chain; deeper hierarchies (rare) won't be resolved.
- `versionGreater` (`accounts.go:241`) compares `V` versions by trailing-digit length first, then string compare. `V10 > V9 > V2`. If Apple ever ships `V100` this still works (longer string sorts higher); but `V010` would also beat `V9`, so don't expect strict numeric semantics.
- `IsUUID` is provided by `internal/emlx` (`emlx/discover.go:266`), not this package.
- `Accounts4.sqlite` is macOS-only; `DefaultAccountsDBPath` returns a Unix path that won't exist on Linux/Windows. Callers must guard.
