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

Runtime helpers such as `run-daily.sh`, `sync-daily.sh`, `lib.sh`, logs, and the
lock file remain under `/home/jarvis/msgvault/scripts`.

## Schedule and execution flow

The `jarvis` user crontab contains:

```cron
0 2 * * * /home/jarvis/msgvault/scripts/run-daily.sh >/dev/null 2>&1
0 9 * * * /home/jarvis/msgvault/scripts/backup-watchdog.sh >/dev/null 2>&1
```

The 02:00 job holds
`/home/jarvis/msgvault/scripts/msgvault-maintenance.lock` across both stages:

1. `sync-daily.sh` incrementally syncs each configured mailbox, rebuilds the
   analytics cache, and reports account status.
2. `backup-rsync.sh` validates the local archive and mirrors it to the NAS.

The stages are deliberately decoupled. A mailbox sync failure is reported but
does not prevent the backup of the archive already on disk. The combined job
still exits non-zero if either stage fails.

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

The 09:00 watchdog alerts when that marker is missing or older than 36 hours.
An rsync transfer can run for several hours because it computes deltas for the
large SQLite database; a quiet log after the preflight does not by itself mean
the process is stuck. Confirm that the rsync and SSH processes and TCP session
are still present before interrupting it.

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
