# Read-Only Conversion Plan

**Goal:** Convert msgvault into a product that is *structurally incapable* of
modifying any remote email server. The tool becomes a one-way email backup:
read from the server, write to the local archive, never the reverse.

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

**Layer C — Build-tag enforcement.**
- Add a build tag `readonly` that becomes the **default** (and for v1, the
  *only* supported) build. The Makefile sets it. There is no `writeable`
  tag; we don't ship the alternative.
- This is belt-and-braces: even if someone copies code back from git
  history, the default build won't compile it.

**Layer D — Runtime assertion.**
- On startup, `internal/oauth` panics if any token on disk has a
  non-readonly scope, with a one-line remediation: "Re-authorize:
  `msgvault add-account <email>`. msgvault no longer accepts write
  scopes." This catches stale tokens from the prior version.

**Layer E — Static check in CI.**
- A `go test ./...` test that greps the compiled binary's symbol table
  for forbidden function names (`TrashMessage`, `DeleteMessage`, …) using
  `go tool nm`. Fails the build if any appear. (Cheap, fast, catches a
  whole class of regression.)

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

**`internal/microsoft/oauth.go:DeleteToken`.**
This revokes the user's *own* refresh token at the Microsoft endpoint and
deletes the local token file. It's "logout," not "modify mailbox." Keep
it. Rename the public method to `RevokeOwnToken` to make the intent
unambiguous.

**Deletion manifests on disk from earlier installs.**
On startup, if `~/.msgvault/deletions/pending/` or `in_progress/` is
non-empty, log a one-time WARN: "Found N staged deletions from a prior
version of msgvault. msgvault no longer performs deletions; these have
been moved to ~/.msgvault/deletions.archived/ and will be ignored.
Delete the directory to dismiss." Move (don't delete) so the user can
inspect. (See §7.)

**TUI keybindings `d` and `D`.**
Currently stage selected / stage all matching for deletion. Three
options: (a) remove the bindings, (b) repurpose for "remove from local
view" with no remote effect, (c) repurpose for "export to .eml." Default
to (a) — the simplest is fewest-keys.

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

**Unit/integration:**
- Delete tests for the deletion subsystem (they go with the package).
- Add `internal/oauth/scopes_test.go`: asserts `Scopes` contains exactly
  one entry, `gmail.GmailReadonlyScope`.
- Add `internal/gmail/readonly_test.go`: asserts `MessageDeleter` is not
  a defined type and `*Client` has no method named `TrashMessage`,
  `DeleteMessage`, `BatchDeleteMessages`. (Reflection-based; runs fast.)
- Add `internal/imap/readonly_test.go`: same shape as the Gmail one.

**Binary symbol check:**
- New CI step `make verify-readonly`: compiles a release binary, runs
  `go tool nm` plus `grep -E 'Trash|BatchDelete|StoreFlags'`, and fails
  the build if anything matches an allowlist-curated forbidden set.

**HTTP/MCP route check:**
- `internal/api/readonly_test.go`: enumerates routes; asserts none match
  forbidden patterns.
- `internal/mcp/readonly_test.go`: enumerates registered tools; asserts
  every tool's declared capability is read-only.

**Manual smoke test before release:**
- Run against a Gmail test account. Confirm OAuth consent screen shows
  only "Read your email." Confirm no command in `--help` mentions
  deletion. Confirm a binary diff against the current release
  shows the deletion symbols are gone (`go tool nm` before/after).
- **Restricted-GCP-client test (gating release).** Create a Google
  Cloud project whose OAuth consent screen lists *only*
  `https://www.googleapis.com/auth/gmail.readonly` under "scopes for
  Google APIs." Generate an OAuth client (desktop or web) under that
  project. Drop its `client_secret.json` into a fresh msgvault home and
  run `msgvault add-account`. The consent flow must complete without
  `invalid_scope`, and a subsequent `msgvault sync` must fetch messages
  successfully. This test is the contract for the whole conversion — if
  it fails, the binary still asks for a scope the user-configured GCP
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

## 8. Implementation phases

Sequenced so each phase leaves the tree compiling and tests passing.

**Phase 1 — Scope reduction.** OAuth scope downgrade, `Scopes` and
`ScopesDeletion` cleanup, scope-test added. Deletion code still present
but unreachable for new tokens. *Smallest reversible step; ship and
verify in isolation if desired.*

**Phase 2 — TUI and CLI removal.** Delete `cmd/msgvault/cmd/deletions.go`,
TUI staging code, `d`/`D` keybindings. Rename `delete-deduped` →
`prune-local`. Update `--help` and TUI `?`. Update tests.

**Phase 3 — Subsystem and client method removal.** Delete
`internal/deletion/` entirely. Delete `MessageDeleter` interface and
`Trash/Delete/BatchDelete` methods on Gmail and IMAP clients. Delete
their rate-limit op constants. Add the readonly_test.go files.

**Phase 4 — Build tag, symbol check, runtime assertion.** Add `readonly`
build tag (default). Add `make verify-readonly`. Add startup
legacy-scope detection and migration of the `deletions/` directory.

**Phase 5 — Docs and product framing.** README rewrite, SECURITY.md
update, ARCHITECTURE.md update, release notes explaining the breaking
change.

Each phase is its own PR. After Phase 3, the *capability* is gone; Phases
4–5 are about making that durable and visible.

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

**Open question — IMAP "read receipt" side effects.** Some IMAP servers
mutate state when a client issues `FETCH BODY[]` (vs `FETCH BODY.PEEK[]`)
by setting the `\Seen` flag. Audit `internal/imap/` to confirm we
exclusively use `BODY.PEEK[]` (or equivalent in the chosen IMAP library)
for fetches. If we don't, fix it; reading should not flip read/unread on
the server.

**Open question — Microsoft Graph DeleteToken.** We keep this as
"revoke own credential," but it's a remote-mutation in a literal sense.
Decision recommended: keep it, rename to `RevokeOwnToken`, document it
as logout. Surface it only via an explicit `msgvault remove-account`
command. Confirm this is acceptable to the product framing or remove it
and let users revoke at the Microsoft account page.

**Open question — what to do with the `d`/`D` keybindings.** Default
recommendation: unbind. Alternatives: rebind to "export selected to
.eml" (useful and read-only) or "hide from local view." Pick before
Phase 2.

---

## 10. Done definition

- [ ] OAuth scopes reduced to read-only; old `ScopesDeletion` removed.
- [ ] `internal/deletion/` removed; no callers remain.
- [ ] Gmail and IMAP clients have no `Trash`/`Delete`/`BatchDelete`
      methods.
- [ ] CLI: `delete-staged`, `list-deletions`, `show-deletion` removed;
      `delete-deduped` renamed to `prune-local`.
- [ ] TUI: deletion staging UI and `d`/`D` keybindings removed (or
      rebound to a read-only action).
- [ ] `make verify-readonly` passes; runs in CI.
- [ ] Startup legacy-scope warning + `deletions/` directory migration in
      place.
- [ ] README, SECURITY.md, ARCHITECTURE.md updated.
- [ ] Release notes call out the breaking change and the upgrade path.
- [ ] Smoke test: fresh OAuth consent screen shows read-only scope only;
      `go tool nm` on the release binary contains none of the forbidden
      symbols.
- [ ] Restricted-GCP-client test passes: a Google Cloud OAuth client
      configured with only `gmail.readonly` in its scope list completes
      OAuth + sync + query end-to-end with no `invalid_scope` error.
