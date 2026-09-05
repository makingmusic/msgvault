# Plan: nightly sync fix + read-only jubei/openclaw access to msgvault

**Status:** Planned, not yet executed. User will say when to proceed.

**Update (05 Sep 2026), decisions from live discussion with the user:**

- Section A (the nightly sync-gap fix on jarvis) was independently re-verified
  live and confirmed still accurate — same gap, same fix. The user wants
  jarvis's own backup+sync story fixed and made properly responsible for
  itself **first**, as a standalone step, independent of jubei access timing.
  Execute Section A on its own; don't gate it on B/C.
- Sections B and C (SSH key on jarvis for jubei, MCP wiring on jubei) are
  **deferred** — not declined, just intentionally sequenced after Section A,
  pending a separate go-ahead. The user was initially unclear on why jubei
  needs a key on jarvis at all given no data is copied to jubei; clarified:
  the key authorizes jubei to open an SSH-stdio session that runs
  `msgvault-ro mcp` **on jarvis** — jubei is a remote client asking questions
  of a server process that never leaves jarvis, not a second copy of the
  vault. This is unrelated to backups/sync and doesn't need to happen at the
  same time as Section A.
- Notifications for jarvis-side failures (sync fail, backup fail, watchdog
  staleness) should land in **both** places: jarvis's existing path
  (`msgvault_notify` — syslog + Discord webhook, unchanged) **and** jubei,
  since the user talks to jubei regardless of which channel. Live check
  (05 Sep 2026): jubei has **no configured chat channels** at all
  (`openclaw channels status` → "no configured chat channels"; heartbeat is
  idle "waiting for delivery route"), so an active push (`openclaw agent
  --deliver` / `openclaw message send`) has nowhere to land today. jubei's own
  `AGENTS.md` says it auto-loads `memory/YYYY-MM-DD.md` (today's daily notes)
  and `MEMORY.md` at the start of every session with the user — that's the
  real, already-working integration point. Proposed mechanism: extend
  `scripts/lib.sh`'s `msgvault_notify()` to also append the alert to jubei's
  `~/.openclaw/workspace/memory/$(date +%Y-%m-%d).md` via
  `incus exec jubei -- su - jarvis -c '...'` (jarvis-the-host already has
  this access — no new credential needed, unlike Section B/C). **Not yet
  implemented** — deferred while the get_stats status-query idea below was
  explored instead; revisit once B/C timing is decided.
- New capability added to scope: **querying sync/backup status via MCP**
  (see below) — the user's own idea, in place of (for now) an on-demand sync
  trigger from jubei, which stays explicitly out of scope (see that section).

## Execution context

This plan touches two machines: the **host** (jarvis) and the **jubei** container. Only the host has the filesystem/cron access Sections A and B need, and jubei has no path to the host yet (that's the access gap this plan closes) — so it's a **jarvis-host plan**, not a jubei-side or atsu-side one:

- **jarvis-running agent** (default execution site): run Sections A and B directly on the host. For Section C (jubei's `~/.codex/config.toml`), use `incus exec jubei -- <command>` rather than SSH-ing in — no shell hop needed.
- **atsu-running agent**: atsu is a sibling container with no built-in reach into the host or into jubei. If this plan is handed to an atsu-based agent, it should hand Sections A/B back to a jarvis-running agent rather than attempt them itself; there's no benefit to routing this plan through atsu at all.

**Backup strategy — must be addressed before execution:** before starting, ask the user whether the existing backup coverage (nightly rsync of `msgvault.db` + `attachments/` to the Synology NAS, plus the watchdog staleness check) is sufficient once jubei's SSH key is authorized, and confirm what backs up/rotates the new `authorized_keys` restriction and jubei-side MCP config. Resolve this before treating the plan as final.

## Pre-execution verification (04 Sep 2026)

No blockers found. Confirmed against live state:
- Sync gap is real and current: `list-accounts` still shows `LAST SYNC 2026-08-20` on both accounts — 15 days stale as of today.
- Host crontab has exactly the two entries the plan describes (`backup-rsync.sh` 2:30am, `backup-watchdog.sh` 9am) and nothing for `sync-daily.sh`/`run-daily.sh` — confirms the gap, no surprise entries to account for.
- Host `~/.ssh/authorized_keys` has exactly one key (`jarvis@jarvis`) — appending jubei's key is additive, won't disturb existing access.
- Host `sshd_config`'s `PubkeyAuthentication`/`AuthorizedKeysFile` lines are commented out, meaning OpenSSH defaults apply (pubkey auth on, default authorized_keys path) — no config change needed for step B to work.
- `/home/jarvis/.local/bin/msgvault-ro` exists and runs on the host.
- jubei's `jarvis` user already has its SSH keypair (`/home/jarvis/.ssh/id_ed25519` + `.pub`) — ready to be pasted into the host's `authorized_keys` in step B1.
- jubei's `~/.codex/config.toml` exists and has no `[mcp_servers]` section yet — step C1's addition is a clean append, not a merge/conflict.
- Host disk: 752GB free — irrelevant either way since this plan copies no data, just adds access.
- Unrelated but worth knowing: jubei already has `git clone`s of `msgvault`/`corpus`/`agentworkspace`/`mylibrary` under `~/githubbed/` for reference — those are code/doc mirrors only, no bearing on this plan.

One thing to double-check at execution time, not a blocker: the host also runs `atsu`/`jipjip` nightly backup crons at `0 3 * * *`, right after the new `run-daily.sh` 2am slot — unrelated systems (different containers, different scripts), so no expected resource conflict, but worth a glance at execution time if `run-daily.sh` ever runs long.

## Context / findings

- `MSGVAULT_HOME` = `/home/jarvis/msgvault` on the host (jarvis machine). `~/.msgvault`
  is a stale, unrelated 250KB leftover — not touched by this plan.
- Two accounts archived: `harinder@gmail.com` (285K msgs) and `harinder@paytm.com`
  (1.75M msgs). Total footprint: **548GB** (292GB `msgvault.db` SQLite + 252GB
  `attachments/`, content-addressed + ~200MB Parquet analytics cache), living on
  the host's main disk (`/dev/nvme0n1p2`, 752GB free).
- Software is a **read-only fork** (`msgvault-ro`, source at
  `/home/jarvis/code/tooling/msgvault`): OAuth scope is `gmail.readonly` only,
  no code path can write/delete/label on Gmail, and this fork has additionally
  removed the local deletion-staging subsystem entirely (`stage_deletion` MCP
  tool is unregistered, `delete-staged`/`list-deletions` CLI commands deleted).
- Built-in MCP server (`msgvault mcp`) exposes exactly the read/query surface
  we need: `search_messages`, `get_message`, `get_attachment`,
  `export_attachment`, `list_messages`, `get_stats`, `aggregate`,
  `search_by_domains`, `find_similar_messages`. No custom wrapper needed.
- **Gap found:** nightly cron currently runs `backup-rsync.sh` (2:30am, rsyncs
  the whole vault to a Synology NAS) and `backup-watchdog.sh` (9am staleness
  check) — both healthy. But the actual Gmail **sync** step
  (`scripts/sync-daily.sh`, pulls new mail) is **not scheduled anywhere**. A
  `run-daily.sh` wrapper exists that chains sync → backup but nothing calls
  it. Last successful sync log is **20 Aug 2026** — 15+ days of new mail
  missing as of this writing.
- jubei (incus container, 4 vCPU / 8GB RAM) runs **openclaw** (AI agent
  gateway, codex backend) entirely as its `jarvis` user, **uid 1001** inside
  the container (`ubuntu` squats uid 1000). jubei's own container rootfs
  lives on a *separate physical disk* from the host's msgvault data (a
  dedicated 953GB zfs pool shared with atsu/jipjip/talkingsocks-dev, 857GB
  free) — copying the 548GB vault in there would eat that shared pool and
  create a second, drifting copy. **Decision: do not duplicate data.**
- jubei's `jarvis` user does **not yet have SSH access to the host** — it has
  a keypair (`~/.ssh/id_ed25519` in jubei) but the host's
  `/home/jarvis/.ssh/authorized_keys` does not include jubei's public key yet
  (host currently only trusts one key, comment `jarvis@jarvis`).

## Decisions (confirmed with user)

1. **Access method: SSH-stdio MCP.** openclaw's codex launches
   `msgvault-ro mcp` on the host over SSH, using MCP's stdio transport
   through the SSH session. No network port opened on the host, no uid
   renumbering needed, no filesystem mount into jubei at all — host stays
   the single copy of the data.
2. **Scope: read/query only from jubei.** openclaw gets search/read/aggregate
   via the MCP tools above. All writes (new-mail ingestion) remain a
   host-only nightly job. No on-demand sync trigger from jubei in this pass.
3. **Fix the nightly sync gap** on the host as part of this work.
4. Short maintenance windows on jubei are acceptable if ever needed (not
   expected to be needed for the SSH-stdio approach specifically, since it
   touches no jubei users/processes).
5. **Sync/backup status becomes queryable read-only via MCP** (added 05 Sep
   2026, user's idea): once jubei has MCP access (Section B/C), it can ask
   `get_stats` directly instead of relying only on pushed notifications. See
   "E. Sync/backup status on get_stats" below for the implementation, which
   proceeds independently of B/C's timing — it's a msgvault-side capability
   available to any MCP client. **On-demand sync trigger from jubei was
   considered and explicitly declined** for this pass — see "Explicitly out
   of scope" below for the reasons (lock contention with the nightly cron,
   Gmail API quota exposure, turning a read-only credential write-capable).

## Plan of execution (when approved)

### A. Host: fix the nightly sync gap
1. Add a cron entry that runs `run-daily.sh` (sync → backup, already chains
   correctly and shares the existing lock file) in place of the current
   standalone `backup-rsync.sh` line, e.g.:
   ```
   0 2 * * * /home/jarvis/msgvault/scripts/run-daily.sh >/dev/null 2>&1
   ```
   at 2:00am — 30 minutes ahead of the current 2:30am slot — and remove the
   now-redundant standalone `backup-rsync.sh` cron line (run-daily.sh already
   calls it). Keep `backup-watchdog.sh` at 9am as-is.
2. Verify the next morning: check `scripts/logs/run-daily-*.log`, confirm
   `list-accounts` shows a `LAST SYNC` timestamp from that night, and confirm
   `~/.local/state/msgvault-backup/last_success` still updates.
3. No changes to `sync-daily.sh` / `backup-rsync.sh` internals — both are
   already correct and idempotent (lock file, failure trap, `--after`/log
   rotation not needed here).

### B. Host: authorize jubei's SSH key
1. Read jubei's public key (`incus exec jubei -- cat /home/jarvis/.ssh/id_ed25519.pub`).
2. Append it to `/home/jarvis/.ssh/authorized_keys` on the host (idempotent
   append, not overwrite — check it's not already present first).
   Optionally restrict it with a `command=` / `restrict` prefix scoped to
   only the msgvault MCP invocation, so this key can't be used for a general
   shell login — worth doing given it's a fairly wide-reaching credential
   (access to two decades of two mailboxes). Recommend:
   ```
   command="MSGVAULT_HOME=/home/jarvis/msgvault /home/jarvis/.local/bin/msgvault-ro mcp",restrict ssh-ed25519 AAAA... jubei-openclaw-msgvault-mcp
   ```
   (`restrict` disables port/agent/X11 forwarding and PTY allocation — plain
   stdio only, which is all MCP needs.)
3. Test from jubei: `ssh jarvis@192.168.50.79` should now run the MCP server
   directly (or, if not using `command=` pinning, run the explicit command)
   non-interactively, no password prompt.

### C. jubei: wire the MCP server into openclaw/codex
1. Add an MCP server entry to jubei's `~/.codex/config.toml`:
   ```toml
   [mcp_servers.msgvault]
   command = "ssh"
   args = ["-o", "BatchMode=yes", "jarvis@192.168.50.79"]
   ```
   (omit the remote command in `args` if using the `command=`-pinned
   authorized_keys entry from step B2 — the key itself forces what runs.)
2. Confirm openclaw's `codex` plugin picks up MCP servers from this file
   (it already reads `~/.codex/config.toml` for `[projects]` trust and other
   settings — same file, additive `[mcp_servers.*]` block).
3. Sanity-check via openclaw/codex directly: ask it to run `get_stats` or a
   small `search_messages` query and confirm it returns real data without
   any local copy of the vault existing in jubei.
4. Leave `mcporter` skill as-is (currently disabled) unless testing shows
   codex's native `mcp_servers` config isn't picked up by openclaw's agent
   runtime, in which case fall back to wiring it through `mcporter` instead.
5. **Leave an onboarding doc for jubei-openclaw** so it knows this capability
   exists and how to use it — see "jubei-openclaw onboarding" below for the
   exact location and content.

### jubei-openclaw onboarding

jubei's openclaw agent keeps its continuity files at
`/home/jarvis/.openclaw/workspace/` (confirmed 04 Sep 2026): `AGENTS.md`
(operating manual), `USER.md` (durable directives), `MEMORY.md` (durable
facts/decisions, loaded every main session per its own `AGENTS.md`),
`memory/YYYY-MM-DD.md` (daily logs), and two purpose-built but currently
empty folders — `reference/` and `plans/`. `reference/` is exactly the right
home for "a new capability exists, here's how to use it":

1. Write `/home/jarvis/.openclaw/workspace/reference/msgvault-mcp-access.md`
   covering: what changed (read-only MCP access to two Gmail archives via
   SSH-stdio, no local copy), the tool list (`search_messages`, `get_message`,
   `get_attachment`, `export_attachment`, `list_messages`, `get_stats`,
   `aggregate`, `search_by_domains`, `find_similar_messages`), the scope
   limits (read/query only — no write/delete/label, ever, by design), and a
   couple of example asks ("what did X email me about Y", "how many emails
   from domain Z").
2. Add a one-line pointer in `MEMORY.md` (durable facts/decisions, loaded
   every main session): something like "msgvault MCP access added <date> —
   read-only query tools for harinder@gmail.com + harinder@paytm.com via
   `mcp_servers.msgvault`. See reference/msgvault-mcp-access.md." This is
   what actually makes the agent *notice* the change on its next session,
   per its own `AGENTS.md` memory rules — the reference doc alone won't be
   read proactively.
3. Write both only after Section C2/C3 are actually done and verified — the
   doc should describe what's real, not what's planned.

### E. Sync/backup status on `get_stats` (msgvault code change, independent of B/C timing)

Added 05 Sep 2026. Extends the existing `get_stats` MCP tool
(`internal/mcp/handlers.go`) with two read-only additions, both best-effort
(never fail the tool call), mirroring the existing `vector.CollectStats`
side-channel pattern already used for vector-search stats:

1. **Per-account last-sync timestamp** — `AccountInfo.LastSyncAt`, sourced
   from `sources.last_sync_at` (previously read only by the CLI's
   `list-accounts`, never exposed through `query.Engine`).
2. **Backup freshness** — a new `backup` field: `{last_success_at, age_hours,
   stale, error?}`, read from the *mtime* of
   `~/.local/state/msgvault-backup/last_success` (same signal
   `scripts/backup-watchdog.sh` already uses — not the ISO8601 string written
   inside the file). Configurable via a new `[backup]` config.toml section
   (`state_dir`, `max_age_hours`, default 36 to match the watchdog script).
   Deliberately host-ops metadata bolted onto `get_stats` as a side channel,
   not part of the core query engine — same shape as the vector-stats
   precedent.

This does **not** require jubei's SSH key to exist yet — it's available to
any MCP client (including a local `msgvault-ro mcp` invocation on jarvis
itself) as soon as it's built. jubei only gets to *use* it once Section B/C
are executed.

### D. Validation checklist
- [ ] Host: `run-daily.sh` runs nightly at 2am, sync completes, cache
      rebuilds, backup still completes at ~2:30.
- [ ] Host: `backup-watchdog.sh` continues to report "within threshold".
- [ ] Host: `authorized_keys` addition doesn't affect the existing key/login.
- [ ] jubei: SSH to host succeeds non-interactively and only runs the pinned
      MCP command (verify the `restrict`/`command=` actually blocks a plain
      interactive shell if that hardening is applied).
- [ ] jubei/openclaw: a real query (e.g. `search_messages` for a known
      sender) returns correct results end-to-end.
- [ ] No new copy of `msgvault.db` or `attachments/` exists anywhere on
      jubei's own storage.

## Explicitly out of scope for this pass
- Postgres backend (checked: not functionally usable yet per
  `docs/PG_STATUS.md` — schema/query layer incomplete).
- **On-demand sync trigger from jubei** (write path stays host-only for now).
  Reconsidered and explicitly declined again on 05 Sep 2026 after the user
  raised it directly: `sync-daily.sh`/`run-daily.sh` already coordinate
  through one flock (`msgvault-maintenance.lock`) that a remote trigger would
  need to respect or risk a race; Gmail's API is quota-limited
  (`rate_limit_qps=30`) and a remotely-triggerable sync is a lever a bug or a
  compromised key could pull repeatedly; and it would turn a pure read-only
  credential into a write-capable one reachable from another container. If
  revisited later, design it as its own narrowly-scoped, rate-limited trigger
  — not a generic tool call alongside the read/query surface.
- Renumbering jubei's `jarvis` uid to 1000 (only needed for the bind-mount
  approach, which was not chosen).
- Cleaning up the stale `~/.msgvault` directory.
