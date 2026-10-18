# Nightly sync and NAS backup operations

This runbook documents the host-specific maintenance job for the MsgVault
archive at `/home/jarvis/msgvault`. It is operational configuration for the
`makingmusic/msgvault` fork, not part of the upstream MsgVault CLI.

## Repository and deployment ownership

The source checkout is `/home/jarvis/code/tooling/msgvault` with these remotes:

- `origin`: `https://github.com/makingmusic/msgvault.git` — the writable fork;
- `upstream`: `https://github.com/wesm/msgvault` — the upstream project.

The canonical backup script is tracked at
`scripts/rsync-nas/backup-rsync.sh`. Its deployed path is a symlink:

```text
/home/jarvis/msgvault/scripts/backup-rsync.sh
  -> /home/jarvis/code/tooling/msgvault/scripts/rsync-nas/backup-rsync.sh
```

This single-source arrangement is intentional. The deployed script previously
lived outside Git and could drift from the reviewed version. Do not replace the
symlink with a separate editable copy.

Runtime helpers such as `run-daily.sh`, `sync-daily.sh`, `lib.sh`,
`backup-watchdog.sh`, logs, and the lock file remain under
`/home/jarvis/msgvault/scripts`. They are not tracked in Git; the copies taken
before the 17–18 October 2026 changes are kept beside them as
`*.bak-20261017`.

## Schedule and execution flow

The `jarvis` user crontab contains:

```cron
# Monday–Saturday: personal Gmail only
0 2 * * 1-6 /home/jarvis/msgvault/scripts/run-daily.sh <personal-gmail> >/dev/null 2>&1
# Sunday: the Workspace mailbox (weekly) plus personal Gmail
0 2 * * 0   /home/jarvis/msgvault/scripts/run-daily.sh <workspace-mailbox> <personal-gmail> >/dev/null 2>&1
0 9 * * *   /home/jarvis/msgvault/scripts/backup-watchdog.sh >/dev/null 2>&1
```

The 02:00 job holds
`/home/jarvis/msgvault/scripts/msgvault-maintenance.lock` across both stages:

1. `sync-daily.sh` incrementally syncs each mailbox given on the command line,
   rebuilds the analytics cache, and reports account status. After each mailbox
   syncs it writes `~/.local/state/msgvault-backup/last_sync_<email>`.
2. `backup-rsync.sh` validates the local archive and mirrors it to the NAS.

The stages are deliberately decoupled. A mailbox sync failure is reported but
does not prevent the backup of the archive already on disk. The combined job
still exits non-zero if either stage fails.

`msgvault sync` exits 0 without syncing in two cases: when it is interrupted
(`Sync interrupted. Run again to resume.`) and when Gmail's history has expired
(`History ID has expired`). `sync-daily.sh` treats both as failures, logs
`ERROR: incremental sync failed for <email> (<reason>)`, and does not write the
`last_sync` marker.

If the lock is already held, for example by a manual catch-up run, a scheduled
run logs `Another msgvault maintenance run is already active; exiting.` and
exits 0. Its log still becomes `run-daily-latest.log`.

## Backup safety checks

Before copying data, `backup-rsync.sh`:

1. runs `msgvault stats` as a local archive health check;
2. refuses to continue if `msgvault.db-wal` is non-empty;
3. resolves and verifies the NAS identity;
4. runs an rsync-native, read-only preflight;
5. acquires the shared maintenance lock when invoked directly.

The transfer excludes transient or local-only runtime files:

```text
/msgvault.db-wal
/msgvault.db-shm
/scripts/logs/
/scripts/msgvault-maintenance.lock
```

Excluding the SQLite sidecars is required. A read-only SQLite connection can
create and remove empty WAL/SHM files while rsync is scanning. Treating their
disappearance as a backup failure previously produced rsync exit code 24 even
after all substantive files had transferred.

## Restricted rsync-over-SSH account

The Synology account `makingagents` is intentionally restricted. Password
authentication and the rsync remote protocol work, but arbitrary shell commands
are rejected after successful authentication.

Therefore these are invalid health checks:

```sh
ssh makingagents@lakeontario true
ssh makingagents@lakeontario 'printf ok'
```

They can print `Permission denied` even though the password is correct. Never
diagnose that output alone as an authentication failure. The supported
preflight is `rsync --list-only` against the backup destination, which exercises
the same authentication, restricted command path, and directory permissions as
the real transfer.

The warning below is expected and harmless:

```text
Could not chdir to home directory /var/services/homes/makingagents: No such file or directory
```

## NAS resolution and identity pinning

`lakeontario` is a single-label LAN name. systemd-resolved on this host can
return `SERVFAIL` even when the router knows the current DHCP address. The
script resolves the NAS in this order:

1. the system resolver;
2. a direct query to `NAS_DNS_SERVER` (default `192.168.50.1`);
3. `NAS_FALLBACK_HOST` (default `192.168.50.55`).

Regardless of the resulting IP address, SSH uses `HostKeyAlias=lakeontario`
with the pinned key file. A stale address or altered DNS response therefore
cannot silently redirect the backup to a different SSH server.

## Credentials and Git hygiene

Credentials are stored locally in the ignored dated directory beneath
`scripts/rsync-nas/`. The password file must remain mode `0600`.

Never force-add the credentials directory, `nas-rsync.pw`, OAuth material,
tokens, webhook URLs, or private host configuration. Before every operational
commit, confirm that the password remains ignored and inspect the staged diff:

```sh
git check-ignore -v scripts/rsync-nas/*-credentials-*/nas-rsync.pw
git diff --cached
```

## Success, monitoring, and recovery

Logs are written under `/home/jarvis/msgvault/scripts/logs`; the stable symlink
`backup-rsync-latest.log` points to the newest run.

Only a completely successful rsync updates:

```text
~/.local/state/msgvault-backup/last_success
```

The 09:00 watchdog checks two things:

- `last_success` is missing or older than 36 hours: a `backup-watchdog` alert.
- A mailbox's `last_sync_<email>` marker is missing or too old: a `sync-watchdog`
  alert. The limits are set in `SYNC_MAX_AGE` in `backup-watchdog.sh`: 36 hours
  for personal Gmail and 192 hours for the weekly Workspace mailbox.

The sync check exists because a successful rsync only proves that the NAS
matches local disk. From 24 September to 17 October 2026 every sync failed on
expired OAuth tokens, while every backup "succeeded" by copying an unchanged
archive. The old watchdog reported OK the whole time.

### Alerts

Failure alerts are built by `msgvault_notify_failure` in `lib.sh` and sent to:

- Discord **#alerts**, using the webhook URL in
  `~/.local/state/msgvault-backup/discord_webhook` (mode `0600`). The atsu and
  jipjip container-backup scripts post through the same file via symlinks, so
  rotate the webhook there.
- syslog, with the tag `msgvault`;
- the jubei container's OpenClaw memory file (best effort).

Each alert carries:

- a **Reason** classified by `msgvault_failure_reason`, with the fix for token
  expiry and history expiry;
- an **Impact** line;
- the last confirmed backup for backup alerts, or the last successful sync per
  mailbox for sync alerts, for example `(10 days ago)`;
- the log path.

The central monitor (`~/code/monitor`, collector `msgvault-metrics`) reads the
same markers and logs and raises its own `Msgvault*` alerts. When you add a
failure pattern to `msgvault_failure_reason`, add it to that collector's
`CAUSES` list too.

### Reading rsync results

`Total transferred file size` counts the full size of every changed file,
about 315 GB once the database changes, not the bytes sent. Check
`Literal data` and `Total bytes sent` instead. These are typically 0.2–0.3 GB a
night, and 3.7 GB for the 25-day catch-up on 18 October 2026.

A run takes about 3 h 15 min whatever the delta, because the NAS and then this
host each read the whole database. rsync writes no progress to the log until it
finishes. To watch the local read, find the `msgvault.db` descriptor of the
running `rsync` and read the `pos:` line in `/proc/<pid>/fdinfo/<fd>`.

A quiet log after the preflight does not by itself mean the process is stuck.
Confirm that the rsync and SSH processes and TCP session are still present
before interrupting it.

### Sync errors

`Errors: N` in a sync summary is mostly messages deleted from Gmail between the
history scan and the fetch. These are 404s, logged at INFO only as
`failed to fetch message (nil response)`, and cannot be recovered. Real download
failures appear as `msg="failed to fetch message"` with an `error=` field, or as
`failed to ingest`. Incremental sync moves past them; recover them with
`sync-full <email> --after <YYYY-MM-DD>`.

Useful checks:

```sh
readlink -f /home/jarvis/msgvault/scripts/backup-rsync.sh
tail -n 100 /home/jarvis/msgvault/scripts/logs/backup-rsync-latest.log
cat ~/.local/state/msgvault-backup/last_success
ps -ef | grep '[r]sync .*msgvault'
```

To start a manual backup, run:

```sh
/home/jarvis/msgvault/scripts/backup-rsync.sh
```

The lock prevents a manual run from overlapping the nightly maintenance job.

## Gmail authorization

Each mailbox authorizes through an OAuth client in its own Google Cloud project.
The personal mailbox uses `[oauth]` in `config.toml` and the Workspace mailbox
uses its named `[oauth.apps.<name>]` entry. Tokens are stored in
`/home/jarvis/msgvault/tokens/<email>.json`.

### Both OAuth apps must be "In production"

Check this under Google Auth Platform → **Audience** → Publishing status. An
External app left in **Testing** issues refresh tokens that expire after 7 days.
That caused the 2026 outage:

- re-authorized on 5 September, failing from 13 September;
- re-authorized on 16 September, failing from 24 September.

Both apps were published on 17–18 October 2026.

A one-shot cron entry verifies this: `oauth-expiry-check.sh`, tagged
`# msgvault-oauth-expiry-check (one-shot)`, runs at 09:37 on 25 October 2026.
That is the first morning a Testing-mode token from the 18 October
re-authorization could not have survived. The script checks that both mailboxes
synced since 01:00 that day, posts PASS or FAIL to the usual alert channels, and
then removes its own crontab line. The crontab from before this entry was added
is saved as
`~/.local/state/msgvault-backup/crontab.before-oauth-check-20261018`.

Staying unverified is fine for personal use:

- Sign-in shows a "Google hasn't verified this app" screen. Choose
  **Advanced → Go to … (unsafe)**.
- There is a lifetime cap of 100 users.
- Ignore the Verification Center notices ("branding is not being shown",
  "data access is not verified"), and do not submit for verification. The
  restricted Gmail scope requires a paid security assessment.

Production requires a home page, privacy policy and terms URL on an authorized
domain. They are served from the `makingmusic/msgvault-oauth` GitHub Pages site,
`https://makingmusic.github.io/msgvault-oauth/`, with authorized domain
`makingmusic.github.io`. Do not upload a logo, because that forces brand
verification.

### Re-authorizing a mailbox

The OAuth callback is `http://localhost:8089/callback` on this host, which has
no browser. Forward the port from the machine you SSH in from:

```sh
# on the laptop
ssh -L 8089:127.0.0.1:8089 <jarvis>
# then, on jarvis, once per mailbox
MSGVAULT_HOME=/home/jarvis/msgvault ~/.local/bin/msgvault-ro add-account <email> --force
```

Then:

1. Open the printed URL in the laptop's browser and pick the account.
2. Click through the unverified-app screen.
3. Tick the Gmail read permission. Without it the token is useless.
4. Wait for the command to print `authorized successfully`.

Notes on the command:

- `--force` deletes the old token first.
- `--headless` only prints instructions and cannot be combined with `--force`.
- The Workspace mailbox's named-app binding is stored, so `--oauth-app` is not
  needed.

### Catching up after an outage

Run both mailboxes plus the backup in one go:

```sh
setsid -f nohup /home/jarvis/msgvault/scripts/run-daily.sh <workspace-mailbox> <personal-gmail> >/dev/null 2>&1 </dev/null
```

Detach it as shown rather than running it under a tool with a timeout. A SIGINT
stops a sync cleanly with its progress saved; the next run resumes.

With `rate_limit_qps = 1`, messages download at about 0.9 a second. The 25-day
backlog on 18 October 2026 took:

- 61 min for the Workspace mailbox (+3,131 messages);
- 4 h 27 min for personal Gmail (+14,381 messages);
- 3 h 17 min for the rsync.
