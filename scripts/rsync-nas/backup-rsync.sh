#!/usr/bin/env bash
set -Eeuo pipefail

MSGVAULT_HOME="${MSGVAULT_HOME:-/home/jarvis/msgvault}"
MSGVAULT_BIN="${MSGVAULT_BIN:-/home/jarvis/.local/bin/msgvault-ro}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib.sh"

LOCK_FILE="${LOCK_FILE:-$MSGVAULT_HOME/scripts/msgvault-maintenance.lock}"
STATE_DIR="${STATE_DIR:-$HOME/.local/state/msgvault-backup}"
SUCCESS_MARKER="${SUCCESS_MARKER:-$STATE_DIR/last_success}"
CREDENTIALS_DIR="${CREDENTIALS_DIR:-/home/jarvis/code/tooling/msgvault/scripts/rsync-nas/lakeontario-rsync-credentials-20260820}"
PASSWORD_FILE="${PASSWORD_FILE:-$CREDENTIALS_DIR/nas-rsync.pw}"
KNOWN_HOSTS_FILE="${KNOWN_HOSTS_FILE:-$CREDENTIALS_DIR/known_hosts.raw}"
NAS_NAME="${NAS_NAME:-lakeontario}"
NAS_DNS_SERVER="${NAS_DNS_SERVER:-192.168.50.1}"
NAS_FALLBACK_HOST="${NAS_FALLBACK_HOST:-192.168.50.55}"
NAS_USER="${NAS_USER:-makingagents}"
NAS_REMOTE_PATH="${NAS_REMOTE_PATH:-/volume1/takhar/projects/msgvault/}"
NAS_HOST_KEY_ALIAS="${NAS_HOST_KEY_ALIAS:-$NAS_NAME}"
BACKUP_DEST="${BACKUP_DEST:-${1:-}}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-60}"
SSH_PREFLIGHT_ATTEMPTS="${SSH_PREFLIGHT_ATTEMPTS:-3}"
RSYNC_PREFLIGHT_TIMEOUT="${RSYNC_PREFLIGHT_TIMEOUT:-30}"
RSYNC_ATTEMPTS="${RSYNC_ATTEMPTS:-3}"
RETRY_DELAY_SECONDS="${RETRY_DELAY_SECONDS:-30}"

msgvault_init_logging "backup-rsync"
msgvault_install_failure_trap "backup-rsync"

echo "[$(date -Is)] backup-rsync start"
echo "MSGVAULT_HOME=$MSGVAULT_HOME"
echo "CREDENTIALS_DIR=$CREDENTIALS_DIR"

if [[ ! -r "$PASSWORD_FILE" ]]; then
  echo "ERROR: password file is not readable: $PASSWORD_FILE" >&2
  msgvault_fail "backup-rsync" 1 "$LOG_FILE" "The NAS password file is missing or unreadable: $PASSWORD_FILE"
fi

if [[ ! -r "$KNOWN_HOSTS_FILE" ]]; then
  echo "ERROR: known_hosts file is not readable: $KNOWN_HOSTS_FILE" >&2
  msgvault_fail "backup-rsync" 1 "$LOG_FILE" "The pinned NAS known_hosts file is missing or unreadable: $KNOWN_HOSTS_FILE"
fi

if [[ ! -x "$MSGVAULT_BIN" ]]; then
  echo "ERROR: msgvault binary is not executable: $MSGVAULT_BIN" >&2
  msgvault_fail "backup-rsync" 1 "$LOG_FILE" "The configured MsgVault binary is missing or not executable: $MSGVAULT_BIN"
fi

# lakeontario is intentionally a restricted rsync-over-SSH account: password
# authentication succeeds, but arbitrary remote shell commands are denied.
# Resolve its single-label name locally, then pin SSH verification to the
# stable host-key alias rather than trusting whichever IP DHCP currently uses.
if [[ -z "$BACKUP_DEST" ]]; then
  resolved_host="$(getent ahostsv4 "$NAS_NAME" 2>/dev/null | awk 'NR == 1 { print $1; exit }' || true)"
  resolution_source="system resolver"

  if [[ -z "$resolved_host" ]] && command -v dig >/dev/null 2>&1; then
    resolved_host="$(dig +short +time=3 +tries=1 "@$NAS_DNS_SERVER" "$NAS_NAME" A 2>/dev/null | awk '/^[0-9.]+$/ { print; exit }' || true)"
    resolution_source="DNS server $NAS_DNS_SERVER"
  fi

  if [[ -z "$resolved_host" ]]; then
    resolved_host="$NAS_FALLBACK_HOST"
    resolution_source="configured fallback"
  fi

  if [[ -z "$resolved_host" ]]; then
    msgvault_fail "backup-rsync" 1 "$LOG_FILE" "Could not resolve the NAS host $NAS_NAME and no fallback address is configured."
  fi

  echo "Resolved $NAS_NAME to $resolved_host via $resolution_source"
  BACKUP_DEST="$NAS_USER@$resolved_host:$NAS_REMOTE_PATH"
fi

NAS_SSH_TARGET="${BACKUP_DEST%%:*}"
if [[ "$NAS_SSH_TARGET" == "$BACKUP_DEST" ]]; then
  msgvault_fail "backup-rsync" 2 "$LOG_FILE" "BACKUP_DEST is not an rsync-over-SSH destination: $BACKUP_DEST"
fi

SSH_COMMAND=(
  sshpass -f "$PASSWORD_FILE"
  ssh
  -o BatchMode=no
  -o NumberOfPasswordPrompts=1
  -o PreferredAuthentications=password,keyboard-interactive
  -o PubkeyAuthentication=no
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile="$KNOWN_HOSTS_FILE"
  -o HostKeyAlias="$NAS_HOST_KEY_ALIAS"
  -o ConnectTimeout="$SSH_CONNECT_TIMEOUT"
  -o ConnectionAttempts=3
  -o ServerAliveInterval=10
  -o ServerAliveCountMax=2
)

echo "BACKUP_DEST=$BACKUP_DEST"
echo "NAS_HOST_KEY_ALIAS=$NAS_HOST_KEY_ALIAS"

msgvault_acquire_lock_or_exit "$LOCK_FILE"

echo "[$(date -Is)] local archive health check"
if ! MSGVAULT_HOME="$MSGVAULT_HOME" "$MSGVAULT_BIN" stats >/dev/null; then
  msgvault_fail "backup-rsync" 1 "$LOG_FILE" "The local MsgVault database health check failed; refusing to copy an unreadable archive."
fi

if [[ -s "$MSGVAULT_HOME/msgvault.db-wal" ]]; then
  msgvault_fail "backup-rsync" 1 "$LOG_FILE" "The SQLite WAL is non-empty after sync; refusing to create a potentially inconsistent NAS copy."
fi

preflight_ok=0
for ((attempt=1; attempt<=SSH_PREFLIGHT_ATTEMPTS; attempt++)); do
  echo "[$(date -Is)] NAS rsync preflight attempt $attempt/$SSH_PREFLIGHT_ATTEMPTS"
  if preflight_output="$(rsync --list-only --timeout="$RSYNC_PREFLIGHT_TIMEOUT" -e "${SSH_COMMAND[*]}" "$BACKUP_DEST" 2>&1)"; then
    echo "NAS rsync preflight ok"
    preflight_ok=1
    break
  else
    preflight_status=$?
  fi

  printf '%s\n' "$preflight_output"
  if grep -qi 'Permission denied' <<<"$preflight_output"; then
    msgvault_fail "backup-rsync" "$preflight_status" "$LOG_FILE" "The NAS rsync preflight for $NAS_SSH_TARGET was denied. Check the saved password, account status, and permission on $NAS_REMOTE_PATH."
  fi
  if grep -qiE 'REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed' <<<"$preflight_output"; then
    msgvault_fail "backup-rsync" "$preflight_status" "$LOG_FILE" "NAS host-key verification failed. Verify the lakeontario identity before changing the pinned known_hosts file."
  fi
  if (( attempt < SSH_PREFLIGHT_ATTEMPTS )); then
    echo "[$(date -Is)] transient NAS rsync preflight failure; retrying in ${RETRY_DELAY_SECONDS}s"
    sleep "$RETRY_DELAY_SECONDS"
  fi
done

if (( preflight_ok == 0 )); then
  msgvault_fail "backup-rsync" 255 "$LOG_FILE" "NAS rsync preflight failed after $SSH_PREFLIGHT_ATTEMPTS attempts. $(msgvault_failure_reason "$LOG_FILE")"
fi

rsync_status=1
for ((attempt=1; attempt<=RSYNC_ATTEMPTS; attempt++)); do
  echo "[$(date -Is)] NAS rsync attempt $attempt/$RSYNC_ATTEMPTS"
  if rsync -aH --numeric-ids --info=stats2,progress2 \
    --partial --delay-updates --delete-after \
    -e "${SSH_COMMAND[*]}" \
    --exclude '/msgvault.db-wal' \
    --exclude '/msgvault.db-shm' \
    --exclude '/scripts/logs/' \
    --exclude '/scripts/msgvault-maintenance.lock' \
    "$MSGVAULT_HOME"/ "$BACKUP_DEST"
  then
    rsync_status=0
    break
  else
    rsync_status=$?
  fi

  reason="$(msgvault_failure_reason "$LOG_FILE")"
  if [[ "$rsync_status" -eq 255 ]] && grep -qiE 'timed out|unreachable|refused|banner|connection unexpectedly closed' <<<"$reason"; then
    if (( attempt < RSYNC_ATTEMPTS )); then
      echo "[$(date -Is)] transient rsync transport failure; retrying in ${RETRY_DELAY_SECONDS}s"
      sleep "$RETRY_DELAY_SECONDS"
      continue
    fi
  fi
  msgvault_fail "backup-rsync" "$rsync_status" "$LOG_FILE" "$reason"
done

if (( rsync_status != 0 )); then
  msgvault_fail "backup-rsync" "$rsync_status" "$LOG_FILE" "NAS rsync failed after $RSYNC_ATTEMPTS attempts. $(msgvault_failure_reason "$LOG_FILE")"
fi

mkdir -p -m 700 "$STATE_DIR"
date -Is > "$SUCCESS_MARKER"

echo "[$(date -Is)] backup-rsync complete"
