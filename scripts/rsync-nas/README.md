# MsgVault NAS backup

This directory contains the tracked operational source for the MsgVault NAS
backup. The deployed script is `/home/jarvis/msgvault/scripts/backup-rsync.sh`;
keep it identical to `backup-rsync.sh` here.

The dated `*-credentials-*` directory is local-only and ignored by Git. It
contains the password file and pinned host keys. Never force-add that directory
or any `*.pw` file.

## Restricted SSH account invariant

The Synology account `makingagents` is intentionally restricted to
rsync-over-SSH. Password authentication succeeds and the remote rsync protocol
works, but arbitrary commands such as these are denied after authentication:

```sh
ssh makingagents@lakeontario true
ssh makingagents@lakeontario 'printf ok'
```

Do not use a generic SSH command as a health check and do not interpret its
post-authentication `Permission denied` output as a bad password. Test the
actual permitted protocol instead:

```sh
rsync --list-only -e "ssh ..." \
  makingagents@NAS_IP:/volume1/takhar/projects/msgvault/
```

`backup-rsync.sh` therefore uses `rsync --list-only` for its preflight.

## NAS address and host-key verification

`lakeontario` is a single-label LAN name. On this host, systemd-resolved can
return `SERVFAIL` even while the router at `192.168.50.1` resolves the name.
The script resolves in this order:

1. system resolver;
2. direct DNS query to `NAS_DNS_SERVER` (default `192.168.50.1`);
3. `NAS_FALLBACK_HOST` (default `192.168.50.55`).

The resolved address is never trusted by itself. SSH always verifies the
pinned `lakeontario` host key via `HostKeyAlias`, so a stale DHCP address or
DNS response cannot silently redirect the backup to a different SSH server.

## Deployment

After reviewing and testing a change, deploy the tracked script beside the
existing `lib.sh`:

```sh
install -m 0755 scripts/rsync-nas/backup-rsync.sh \
  /home/jarvis/msgvault/scripts/backup-rsync.sh
```

Validate with `bash -n`, an rsync list-only preflight, and then a real backup.
The full backup owns `/home/jarvis/msgvault/scripts/msgvault-maintenance.lock`
and updates `~/.local/state/msgvault-backup/last_success` only after rsync exits
successfully.
