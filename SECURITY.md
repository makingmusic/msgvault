# Security Policy

## Reporting Vulnerabilities

If you discover a security vulnerability in msgvault, please report it responsibly:

1. **Do NOT open a public GitHub issue**
2. Email the maintainer directly or use GitHub's private vulnerability reporting feature
3. Include steps to reproduce, impact assessment, and any suggested fixes
4. Allow reasonable time for a fix before public disclosure

## What this fork cannot do

This fork of msgvault is the *read-only edition*. The product premise
is that the binary is structurally incapable of mutating any remote
mailbox. The non-capabilities below are enforced in code, not by
convention:

| Cannot | How that is enforced |
|---|---|
| Trash a Gmail message | The `MessageDeleter` interface and `TrashMessage` method are deleted. The compiler rejects any call site. |
| Permanently delete a Gmail message | `DeleteMessage` and `BatchDeleteMessages` are deleted. The `OpMessagesDelete` / `OpMessagesBatchDelete` rate-limit constants are deleted. |
| Modify Gmail labels, send mail, create drafts | The Gmail client never had these methods. There are no call sites in the binary. |
| Move, delete, or `\Deleted`-flag IMAP messages | The IMAP client's `TrashMessage` and `DeleteMessage` (`UID MOVE`, `UID STORE \Deleted`, `UID EXPUNGE`) are deleted. |
| Mark IMAP messages as read on fetch | The IMAP client uses `BODY.PEEK[]` exclusively. A regression test (`internal/imap/no_body_fetch_test.go`) source-greps for `BODY[` and fails the build if reintroduced. |
| Request anything beyond `gmail.readonly` | `internal/oauth/oauth.go` declares one scope. A regression test (`internal/oauth/scopes_test.go`) asserts the scope list is exactly `[gmail.readonly]`. |
| Stage messages for later deletion (CLI/TUI/MCP) | The `internal/deletion/` subsystem is deleted. The CLI commands (`delete-staged`, `list-deletions`, `show-deletion`, `cancel-deletion`) are deleted. The TUI's `d`/`D` keys show a banner explaining the build is read-only. The MCP `stage_deletion` tool is unregistered. |

The only remote call this fork makes that has *any* server-side effect
is `microsoft.Manager.RevokeOwnToken` (renamed from `DeleteToken`),
which revokes the user's own OAuth refresh token at Microsoft when
they run `msgvault remove-account`. It never touches mailbox content.

If you previously granted msgvault write scopes from the upstream
edition, your existing token still works for read calls but carries
more permission than this fork uses. To downgrade to least privilege:
revoke the grant at https://myaccount.google.com/permissions, then
re-run `msgvault add-account <email>`. The new grant will request only
`gmail.readonly`.

For the full design rationale see `plans/readonly-conversion.md`.

## Threat Model

### What msgvault protects

| Asset | Storage | Risk if compromised |
|-------|---------|-------------------|
| OAuth2 tokens | `~/.msgvault/tokens/` (per-account files) | Read-only Gmail API access to victim's account (no write capability — see "What this fork cannot do" above) |
| Email bodies | SQLite database (`~/.msgvault/msgvault.db`) | Exposure of 20+ years of personal email |
| Attachments | Content-addressed files (`~/.msgvault/attachments/`) | Exposure of personal documents |
| Contact metadata | SQLite (participants table) | Social graph exposure |
| Search indexes | FTS5 virtual table in SQLite | Keyword-level exposure of email content |
| Analytics cache | Parquet files (`~/.msgvault/analytics/`) | Aggregate email metadata exposure |

### Security controls in place

**File permissions:**
- OAuth token files created with 0600 permissions (owner read/write only)
- Config directory (`~/.msgvault/`) should be 0700
- Attachment storage directory (`~/.msgvault/attachments/`) is created with 0700; attachment files are 0600
- Cross-platform support including Windows DACL

**SQL injection prevention:**
- All SQLite queries use parameterized statements via `database/sql`
- DuckDB queries over Parquet files use parameterized queries
- No string concatenation for query building

**Command injection prevention:**
- OAuth browser launch uses validated, well-formed URLs only
- No user-controlled input passed to `exec.Command` or shell execution

**Path traversal prevention:**
- Attachment storage uses content-hash addressing (SHA-256)
- Config paths resolved relative to a fixed base directory
- No user-controlled path components in file operations

**Input validation:**
- MIME parsing with charset detection (gogs/chardet) and safe encoding conversion
- Email addresses validated before database insertion
- Gmail API message IDs validated as alphanumeric

### Known limitations

**No encryption at rest:**
- The SQLite database is not encrypted. Anyone with filesystem access to `~/.msgvault/` can read all archived emails.
- OAuth tokens are stored as plaintext JSON files (protected by file permissions only).
- Mitigation: Rely on OS-level full-disk encryption (FileVault, BitLocker, LUKS).

**CGO dependencies:**
- SQLite (mattn/go-sqlite3) and DuckDB (marcboeker/go-duckdb) use CGO, introducing native code that is harder to audit than pure Go.
- Mitigation: Pin dependency versions, use govulncheck in CI, review updates via Dependabot.

**Gmail API deletion:**
- msgvault can stage and execute deletions via the Gmail API (trash or permanent delete).
- Mitigation: Deletion requires explicit user action, manifests are generated before execution, and the operation is logged.

## Automated Security Review

External pull requests are automatically reviewed by a Claude-powered security bot that checks for:
- Hardcoded secrets and credential exposure
- Command/SQL injection vulnerabilities
- Path traversal and file permission issues
- OAuth token handling problems
- Dependency supply chain risks (go.mod/go.sum changes)
- Workflow tampering (.github/ directory changes)

See [`.github/SECURITY_BOT.md`](.github/SECURITY_BOT.md) for details.

## Supply Chain

- **Dependabot** monitors Go modules and GitHub Actions for updates
- **govulncheck** runs on every PR (call-graph aware, Go vulnerability database)
- **CODEOWNERS** requires maintainer approval for go.mod, go.sum, and .github/ changes
- **GitHub Actions pinned to commit SHAs** to prevent tag-based supply chain attacks
