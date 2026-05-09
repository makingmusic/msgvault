# internal/fileutil

Cross-platform helpers for writing sensitive files (OAuth tokens,
attachments, configs) with owner-only permissions.

## File layout

- `secure_unix.go` (build tag `!windows`) — thin wrappers over `os.*`.
- `secure_windows.go` (build tag `windows`) — same API, but for
  owner-only modes additionally applies a DACL via
  `windows.SetNamedSecurityInfo`.

## Public API

```go
SecureWriteFile(path string, data []byte, perm os.FileMode) error
SecureMkdirAll(path string, perm os.FileMode) error
SecureChmod(path string, perm os.FileMode) error
SecureOpenFile(path string, flag int, perm os.FileMode) (*os.File, error)
```

Same signatures as `os.WriteFile`, `os.MkdirAll`, `os.Chmod`,
`os.OpenFile`. On Unix, behavior is identical. On Windows, when
`perm & 0077 == 0` (no group/other bits), the helper additionally:

1. Resolves the current user SID via the process token.
2. Builds an ACL that grants `GENERIC_ALL` to that SID only.
3. Sets `DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION`
   so inherited ACEs are blocked.
4. For directories, enables `CONTAINER_INHERIT_ACE | OBJECT_INHERIT_ACE`
   so children inherit the restriction.

## Failure semantics

DACL failures are **logged as warnings, not returned**
(`secure_windows.go:86`). The file/dir was already created with the
requested Unix mode, and the OS may not be able to apply a DACL on
network filesystems. Treat the DACL as defense-in-depth.

`SecureMkdirAll` walks up the path before creating, applies the DACL
to every directory it newly created. Pre-existing directories are not
touched.

## Threat model and TOCTOU

Windows `SetNamedSecurityInfo` operates by path after the file is
already open, so there is a brief TOCTOU window between create and
DACL application. Acceptable because exploitation requires local
access already, and the file mode (the kernel-enforced Unix bits) is
correct from the start.

## Callers

- `internal/oauth` — token files (0600).
- `internal/config` — config.toml (0600), home dir (0700).
- `internal/deletion` — manifest JSON (0600).
- `internal/export` — attachment files (0600), output files (0600).
- `internal/update` — update cache (0600).

## When editing

- Don't drop the `isOwnerOnly` guard. Applying a restrictive DACL on a
  world-readable file is wrong: the user expects mode 0644 to be world
  readable.
- If a new caller wants permissive modes (e.g. 0644 attachments), use
  `os.WriteFile` directly. Don't extend `SecureWriteFile` with a
  "non-secure" branch — the name carries meaning.
- Keep the Unix and Windows files in sync: any new function added to
  one must exist in the other with the same signature, or the package
  won't build on the missing platform.
