# internal/update

GitHub-release-based self-update. Polls
`api.github.com/repos/wesm/msgvault/releases/latest`, downloads the
platform-specific archive, verifies SHA-256, and atomically replaces
the running binary.

## File layout

- `update.go` — `CheckForUpdate`, `PerformUpdate`, archive
  extractors, version comparison, install logic.

## Public surface

- `CheckForUpdate(currentVersion, forceCheck bool) (*UpdateInfo, error)`
- `PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error`
- `InstallFromArchive(archivePath, expectedChecksum string) error` —
  test/integration entry point that bypasses HTTP.
- `FormatSize(bytes int64) string`

## Caching

`update_check.json` in `<MSGVAULT_HOME>/`:

- Release builds: 1-hour cache.
- Dev builds: 15-minute cache.

Cache stores last-checked-at + release tag. `forceCheck=true` bypasses
it. `checkCache` (`update.go:606`) returns `(info, done=true)` when a
valid cache result is usable, `(nil, false)` when fresh data is
needed.

## Asset selection

Asset name pattern (`update.go:106`):
`msgvault_<version>_<GOOS>_<GOARCH>.{tar.gz|zip}` (zip on Windows,
tar.gz elsewhere).

Checksum lookup, in order:

1. `SHA256SUMS` or `checksums.txt` asset on the release (preferred —
   single line per file, parsed by `extractChecksum`).
2. Hex-pattern match in the release `Body` (markdown release notes).

If no checksum can be located, `PerformUpdate` returns an error
**before** downloading. The package refuses to install unverified
binaries.

## Version comparison

- `extractBaseSemver` strips `v` prefix and prerelease tags
  (`-rc1`, `-5-gabcdef`, etc.) to a `MAJOR.MINOR.PATCH` core.
- `isDevBuildVersion` recognizes either non-semver-prefixed strings
  or git-describe suffixes (`-N-gHASH[-dirty]`).
- `normalizeSemver` turns `rc10` into `rc.10` so `golang.org/x/mod/semver`
  compares the digits numerically (otherwise rc10 < rc2).
- Dev builds are NEVER auto-replaced. The CLI requires `--force` to
  install the latest official release over a dev build.

## Install steps

`installBinaryTo` (`update.go:260`) handles the Windows-can't-delete-
running-exe case:

1. Remove stale `<dst>.old` (best-effort; may fail on Windows if a
   previous run is still active — cleaned up on next update).
2. Rename current binary to `<dst>.old` (works on Windows even for
   the running process — that's the only operation that does).
3. Copy new binary to original path.
4. Chmod 0755.
5. Try to remove `.old` (silent failure on Windows).

On copy failure, attempts to restore from backup. Returns the install
error.

## Archive extraction safety

Both `extractTarGz` and `extractZip` route entries through
`sanitizeTarPath` (`update.go:441`):

- Reject absolute paths (leading `/`).
- Reject Windows volume names.
- Reject `..` traversal.
- Verify the resolved abs path is under the destination dir.

Symlinks and hardlinks (`tar.TypeSymlink`, `tar.TypeLink`) are
silently skipped, not extracted. Don't change this — a symlink
pointing to `/usr/local/bin/git` extracted into the install dir would
clobber unrelated binaries.

## When editing

- Never skip checksum verification. The `expectedChecksum == ""`
  branches all return errors; keep them that way.
- The `nolint:gosec` on `http.Get` for the download URL
  (`update.go:327`) exists because the URL comes from the
  GitHub API response. The URL is constrained to be a release-asset
  URL on github.com; if you change the source, re-evaluate.
- Don't hardcode `wesm/msgvault` in more places — it lives in
  `githubAPIURL` (`update.go:26`).
- Cache file is owner-only via `fileutil.SecureWriteFile`
  (`update.go:651`); don't downgrade the mode.
