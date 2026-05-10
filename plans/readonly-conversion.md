# Read-Only Conversion Plan

**Goal:** Convert msgvault into a product that is *structurally incapable* of
modifying any remote email server. The tool becomes a one-way email backup:
read from the server, write to the local archive, never the reverse.

**Repo positioning:** This repo is a fork of upstream msgvault. The fork's
single purpose is to be the read-only edition. Users who want write
capability (trash, delete, etc.) should use upstream msgvault directly.
Because the repos are split, this fork has no `writeable` build to ship and
no need for build-tag gating — there is only one build, and it is read-only.

**Why "structurally incapable" and not just "off by default":** A backup tool
that holds delete-capable credentials is an attractive blast radius. Even if
deletion is gated behind a flag, a bug, supply-chain compromise, or
malicious flag-flip can reach the user's mailbox. The right answer is to
remove the capability so the credentials it holds *cannot* delete, and the
binary it ships *contains no code* that calls a write API.

---

## 1. Threat model & success criteria

A user installing msgvault should be able to truthfully say:

1. **No write scope is ever requested.** Re-consenting after this change
   downgrades the OAuth grant to `gmail.readonly`. Microsoft Graph requests
   only `Mail.Read` (or IMAP read scopes).
2. **The binary works end-to-end against a Google Cloud OAuth client
   whose configured scope list contains *only* `gmail.readonly`.** If a
   user locks their GCP project to read-only at the cloud-console level,
   msgvault must complete OAuth, sync, and all queries with no
   `invalid_scope` error and no missing-feature gap. This is the
   user-facing acceptance test for the whole conversion: the GCP project
   is the source of truth, and msgvault must be a clean subset of it.
3. **No code path in the binary issues a Gmail trash/delete or IMAP
   STORE/EXPUNGE/MOVE.** Even if an attacker gained the OAuth token, this
   binary cannot use it to mutate.
4. **The `internal/deletion/` subsystem does not exist** in the shipped
   binary. No staged manifests, no executor, no CLI to run them.
5. **A grep for known mutation verbs returns no production code** — only
   tests asserting absence (see §6).
6. **CI fails the build** if any of the above invariants regress.

Non-goals:
- Continuing to support deletion behind a flag. The product premise is
  that the tool *cannot* delete; flags don't satisfy that.
- Migrating existing pending deletion manifests "into the new world." On
  upgrade, those go away; users who staged deletions must run them with the
  old binary or accept that they're discarded.

---

## 2. Mutation surface inventory (from audit)

| # | Location | Mutation | Disposition |
|---|---|---|---|
| 1 | `internal/oauth/oauth.go:28-37` — `Scopes`, `ScopesDeletion` | Requests `gmail.modify` and `https://mail.google.com/` | Replace with `gmail.readonly` only; delete `ScopesDeletion` |
| 2 | `internal/gmail/api.go:32-41` — `MessageDeleter` interface | Declares `TrashMessage`, `DeleteMessage`, `BatchDeleteMessages` | Delete interface and all methods |
| 3 | `internal/gmail/client.go:541-574` | Implementations of the three Gmail write calls | Delete; remove rate-limit op codes |
| 4 | `internal/gmail/ratelimit.go:22-37` | `OpMessagesTrash/Delete/BatchDelete` cost constants | Delete |
| 5 | `internal/imap/client.go:888-940` | `TrashMessage` (MOVE), `DeleteMessage` (STORE+EXPUNGE), `BatchDeleteMessages` | Delete |
| 6 | `internal/deletion/` (whole package) | Manifest manager + executor | Delete the package |
| 7 | `cmd/msgvault/cmd/deletions.go` | `list-deletions`, `show-deletion`, `delete-staged` Cobra commands + scope-escalation flow | Delete file |
| 8 | `cmd/msgvault/cmd/delete_deduped.go` | `delete-deduped` (local DB only — see §4) | Keep, or rename and reframe; not remote mutation |
| 9 | `internal/tui/` — `stageForDeletion`, `confirmDeletion`, `d`/`D` keybindings | Stages messages into `pending/` deletion manifests | Delete deletion staging UI; rebind `d`/`D` to no-op or another use |
| 10 | `internal/tui/actions.go` — `ActionController.StageForDeletion` | Bridge from TUI to deletion manifests | Delete method |
| 11 | `internal/microsoft/oauth.go:647-651` — `DeleteToken` | Revokes the user's *own* refresh token at Microsoft | Keep — not a message mutation; user-initiated logout (§4) |
| 12 | `~/.msgvault/deletions/` directory | Disk-side staged manifests from earlier runs | Migration cleanup on first new-binary launch (§7) |
| 13 | `internal/api/` and `internal/mcp/` | (Audited as read-only; no deletion endpoints/tools) | No change needed; add a regression test |

**What's already absent (good news):** Gmail Send/Insert/Import/Modify,
draft creation, label CRUD, IMAP APPEND/COPY, flag setting (read/unread),
calendar/contacts mutations. The codebase never had these; we don't have
to remove them, but we do need a regression test (§6) to prove they stay
absent.

---

## 3. Defense in depth

We apply mutation-prevention at multiple layers so a single mistake
cannot reintroduce write capability.

**Layer A — OAuth scopes (the credential itself).**
- `internal/oauth/oauth.go`: `Scopes = []string{gmail.GmailReadonlyScope}`.
- Delete `ScopesDeletion` entirely. Delete any code that consults it.
- The requested scope set must be a strict subset of what's configured on
  the OAuth client in Google Cloud Console. Concretely: a user who
  configures their GCP OAuth consent screen with **only**
  `https://www.googleapis.com/auth/gmail.readonly` listed must be able
  to use this binary without hitting `Error 400: invalid_scope` on the
  consent screen. After this change, requesting any other Gmail scope
  is impossible — there is no code path that asks for one.
- Microsoft Graph: confirm we only request `Mail.Read`,
  `Mail.ReadBasic`, or IMAP `https://outlook.office.com/IMAP.AccessAsUser.All`
  for read. Audit `internal/microsoft/oauth.go` scope strings.
- IMAP: protocol has no scope concept; protected by Layer B.

**Layer B — Code (no write call sites in the binary).**
- Delete the deletion subsystem and the Gmail/IMAP write methods (rows
  2–10 above). The mutation verbs cease to exist as Go symbols.

**Layer C — Runtime assertion.**
- On startup, `internal/oauth` warns loudly if any token on disk has a
  non-readonly scope, with a one-line remediation: "Re-authorize:
  `msgvault add-account <email>`. This fork no longer uses write
  scopes." This catches stale tokens from the prior version. (We warn
  rather than panic so the binary still works for read; see §5.)

(There is no build-tag layer or symbol-table CI check in this fork.
Upstream msgvault remains the writeable edition, so this fork ships
exactly one build and the deletion of the code is itself the guarantee.
The compiler enforces it: any reintroduced call site fails to build.
Two narrow tests in §6 cover the two regressions the compiler can't
see — silent scope widening, and IMAP `BODY[` reintroduction.)

---

## 4. Edge cases and judgment calls

**`delete-deduped` (CLI) and local SQLite row deletion.**
This command deletes rows from the *local* archive, not from the remote
server. It's part of the dedup workflow (`internal/dedup/`). Keep it, but:
- Rename to `prune-local` (or similar) so the verb "delete" never appears
  on a command in this product.
- Update help text: "Removes locally-deduplicated rows from the msgvault
  archive. Does not affect any remote mailbox."
- Verify there is no path from this command into Gmail/IMAP write calls
  (audit confirms there isn't, but re-verify after deletion subsystem is
  removed).

**`internal/microsoft/oauth.go:DeleteToken` (decided).**
This revokes the user's *own* refresh token at the Microsoft endpoint and
deletes the local token file. It's "logout," not "modify mailbox." **Keep
it; rename the public method to `RevokeOwnToken`** to make the intent
unambiguous to future readers and auditors. Update all call sites and
docstrings accordingly.

**Deletion manifests on disk from earlier installs.**
On startup, if `~/.msgvault/deletions/pending/` or `in_progress/` is
non-empty, log a one-time WARN: "Found N staged deletions from a prior
version of msgvault. msgvault no longer performs deletions; these have
been moved to ~/.msgvault/deletions.archived/ and will be ignored.
Delete the directory to dismiss." Move (don't delete) so the user can
inspect. (See §7.)

**TUI keybindings `d` and `D` (decided).**
Currently stage selected / stage all matching for deletion. **Repurpose
both keys to surface a transient banner (toast/popup) explaining that
this is the read-only edition of msgvault and deletion is disabled.**
The keys remain bound — pressing them is the user's discovery moment for
the product framing, not a silent no-op.

Banner text (draft, refine in implementation):
> "This is the read-only edition of msgvault. Deletion is disabled by
> design — this build cannot trash or delete email on any remote server.
> If you need deletion, use upstream msgvault."

Implementation notes:
- Both `d` and `D` show the same banner (no need to differentiate
  selected vs all-matching — neither does anything).
- Banner auto-dismisses after a few seconds or on any keypress; should
  not block input.
- Help screen (`?`) lists `d`/`D` as "show read-only notice" so users
  who scan help discover the framing without having to press a key.

**MCP server.**
Audit `internal/mcp/` tool list. If any tool name suggests mutation
(`delete_*`, `trash_*`, `archive_*`, `mark_*`), confirm it's read-only or
remove it. Add an MCP tool-list test that asserts every registered tool
has a read-only contract.

**HTTP API (`internal/api/`).**
Add a regression test that enumerates every chi route and asserts none
use `POST/PUT/PATCH/DELETE` for mutations *of remote state*. (Local
queries can still POST for search bodies; the test should be specific to
remote state.)

---

## 5. User-facing changes

**README and `docs/ARCHITECTURE.md` rewrite.**
- Top-of-README banner: "msgvault is a read-only email backup tool. It
  cannot send, modify, label, trash, or delete email on any remote
  server. It only reads."
- Replace any mention of trash/delete/staging in user-facing docs.
- Document the OAuth scopes explicitly: "We request only
  `https://www.googleapis.com/auth/gmail.readonly`. We will never request
  write scopes; the binary contains no code that calls write APIs."

**SECURITY.md update.**
- Add a "What msgvault cannot do" section listing every non-capability
  with the corresponding code-level guarantee (build tag, missing symbol,
  scope, …).
- Add a section on what to do if you previously granted msgvault write
  scopes: revoke at https://myaccount.google.com/permissions and re-add
  the account.

**CLI help and `--help` output.**
- Verify no command help text references trashing/deletion/staging.
- Removed commands: `list-deletions`, `show-deletion`, `delete-staged`.
  These should print a helpful migration message if invoked (Cobra's
  unknown-command default is fine; or register stub commands that print
  one line and exit nonzero).

**TUI help screen.**
- Update `?` help to remove `d`/`D` deletion entries.

**Re-consent flow.**
- First launch on the new binary, after detecting any token with a
  non-readonly scope, prints: "Your existing OAuth tokens grant write
  access. msgvault no longer uses write scopes. Re-authorize with
  `msgvault add-account <email>`; the new grant will be read-only.
  Existing tokens remain valid until you revoke them at
  myaccount.google.com/permissions." Do not auto-delete the existing
  token — the user may have other tools using it. (Unlikely for `tokens/`
  in `~/.msgvault/`, but principle: do not silently revoke things.)

---

## 6. Verification strategy

The bulk of the conversion is *deletion of code*, and the compiler is the
test for deletion: if `MessageDeleter` doesn't exist, code that called it
doesn't build. Reflection tests asserting "method X is not present" or
CI greps over the binary's symbol table are checking the same thing the
build already enforces, so we don't add them.

Two automated tests earn their keep — they cover regressions the
compiler genuinely cannot see:

1. **`internal/oauth/scopes_test.go`** (~5 lines). Asserts `Scopes`
   contains exactly one entry, `gmail.GmailReadonlyScope`. Defends
   against a future "let me add label support" PR silently widening the
   scope list. The compiler can't catch this because `Scopes` is
   `[]string` — anything is type-valid.
2. **`internal/imap/no_body_fetch_test.go`** (~10 lines). Source-grep
   over `internal/imap/*.go` asserting no `BODY[` occurrence outside an
   allowlisted comment — only `BODY.PEEK[` or the library equivalent
   (`Peek: true`) is permitted. Defends against a contributor copy-
   pasting a fetch that silently marks messages as read on the server.

**Existing deletion tests** in `internal/deletion/` are deleted with the
package; nothing to do there.

**What we deliberately *don't* test:**
- Reflection assertions that `TrashMessage` / `DeleteMessage` /
  `BatchDeleteMessages` are absent — redundant with the compiler.
- API/MCP route enumeration asserting "no mutation routes" — would also
  flag legitimate read-side POSTs (search bodies, sync triggers) and
  produce false-positive noise. Any new write route would be obvious in
  the PR diff.
- A `go tool nm` symbol-table CI check — overkill once the code is
  deleted, and fragile across Go versions because library symbols change.

**Manual smoke tests before release:**
- Run against a Gmail test account. Confirm OAuth consent screen shows
  only "Read your email." Confirm no command in `--help` mentions
  deletion.
- **Restricted-GCP-client test (gating release).** Create a Google
  Cloud project whose OAuth consent screen lists *only*
  `https://www.googleapis.com/auth/gmail.readonly` under "scopes for
  Google APIs." Generate an OAuth client (desktop or web) under that
  project. Drop its `client_secret.json` into a fresh msgvault home and
  run `msgvault add-account`. The consent flow must complete without
  `invalid_scope`, and a subsequent `msgvault sync` must fetch messages
  successfully. This is the contract for the whole conversion — if it
  fails, the binary still asks for a scope the user-configured GCP
  project doesn't grant, and the release is not done.

---

## 7. Migration / one-time cleanup on upgrade

First launch on the new binary:

1. Detect `~/.msgvault/deletions/{pending,in_progress}/` non-empty.
   Move the entire `~/.msgvault/deletions/` tree to
   `~/.msgvault/deletions.archived-<timestamp>/`. Log one WARN with the
   path. Do not delete; the user may want to inspect.
2. Detect tokens with non-readonly scope (Gmail OAuth tokens carry the
   granted scope). Print a one-time notice (see §5) and continue. Do not
   auto-revoke. Mark the token file with a `.legacy-scope` sibling marker
   so we don't re-warn on every launch.
3. The `deletions` directory itself is not used by the new binary, so
   subsequent launches are silent.

No DB schema change is needed. The `messages.deleted_at` column remains
because `delete-deduped` / `prune-local` still uses it for local
soft-delete semantics; nothing about that table touches the network.

---

## 8. Implementation order (single PR)

Ship as one PR. The "phases" below are an ordering for the
implementation work — do them in this order so the tree compiles after
every step and so a partial review can read the diff narrative — but
they all land together. Rationale: this is a coherent product
repositioning, not an incremental feature; reviewers benefit from seeing
the whole picture, and we don't ship a half-converted binary.

Suggested working order inside the PR:

1. **Scope reduction.** OAuth scope downgrade in `internal/oauth/oauth.go`;
   delete `ScopesDeletion`; add `internal/oauth/scopes_test.go`.
2. **CLI surface removal.** Delete `cmd/msgvault/cmd/deletions.go`. Rename
   `delete-deduped` → `prune-local` and update help text.
3. **TUI changes.** Replace `stageForDeletion`/`confirmDeletion` with the
   read-only-notice banner on `d`/`D`. Update `?` help.
4. **Subsystem removal.** Delete `internal/deletion/` entirely. Delete
   `MessageDeleter` interface and `Trash`/`Delete`/`BatchDelete` methods
   on Gmail and IMAP clients. Delete their rate-limit op constants.
   Rename Microsoft `DeleteToken` → `RevokeOwnToken`.
5. **Regression tests.** Add `internal/oauth/scopes_test.go` and
   `internal/imap/no_body_fetch_test.go` (the only two tests this PR
   needs — see §6).
6. **Migration.** Add startup legacy-scope detection and
   `~/.msgvault/deletions/` directory archive (§7).
7. **Docs.** README, SECURITY.md, ARCHITECTURE.md updates. Release note
   explaining this is the read-only fork.

After step 4, the *capability* is gone; steps 5–7 make that durable and
visible.

---

## 9. Risks and open questions

**Risk: existing users have staged deletions they expected to run.**
Mitigation: archive the directory rather than delete; document the old
binary's release tag in the upgrade notes so users can run pending
deletions with the prior version if they really want to.

**Risk: re-consent friction.** Users with existing tokens won't be forced
to re-consent (their old tokens still work for read calls), but the
warning may confuse them. Mitigation: clear one-time WARN; docs link
explaining "your old token has more permissions than this version uses,
which is harmless but you can revoke and re-grant for least privilege."

**Risk: future contributor adds a write call back in.** This is exactly
what the symbol-table CI check and readonly_test.go files defend against.
The product invariant is enforced in code, not by convention.

**Resolved — IMAP `\Seen` side effects.** Audited. Two fetch sites
exist in `internal/imap/client.go`: line 340 fetches metadata only
(UID + Envelope, no body), and line 703 uses `Peek: true` (`BODY.PEEK[]`)
with an inline comment explaining why. CLAUDE.md line 108 codifies it
as a guardrail. No fix needed; add a regression test (see §6) so a
future contributor can't reintroduce `BODY[` by accident.

**Resolved — Microsoft Graph DeleteToken.** Keep, rename to
`RevokeOwnToken`. Document as "logout / remove-account" — the remote
call only revokes the user's own credential, never any mailbox content.

**Resolved — TUI `d`/`D` keybindings.** Both keys show a transient
banner explaining this is the read-only edition and deletion is
disabled. See §4 for banner text and behavior.

---

## 10. Done definition

- [ ] OAuth scopes reduced to read-only; old `ScopesDeletion` removed.
- [ ] `internal/deletion/` removed; no callers remain.
- [ ] Gmail and IMAP clients have no `Trash`/`Delete`/`BatchDelete`
      methods.
- [ ] CLI: `delete-staged`, `list-deletions`, `show-deletion` removed;
      `delete-deduped` renamed to `prune-local`.
- [ ] TUI: deletion staging UI removed; `d`/`D` show the read-only-edition
      banner; `?` help updated.
- [ ] Microsoft `DeleteToken` renamed to `RevokeOwnToken`; call sites
      updated.
- [ ] `internal/oauth/scopes_test.go` and
      `internal/imap/no_body_fetch_test.go` added and passing.
- [ ] Startup legacy-scope warning + `deletions/` directory migration in
      place.
- [ ] README, SECURITY.md, ARCHITECTURE.md updated.
- [ ] Release notes call out the breaking change and the upgrade path.
- [ ] Smoke test: fresh OAuth consent screen shows read-only scope only;
      no command in `--help` mentions deletion.
- [ ] Restricted-GCP-client test passes: a Google Cloud OAuth client
      configured with only `gmail.readonly` in its scope list completes
      OAuth + sync + query end-to-end with no `invalid_scope` error.
