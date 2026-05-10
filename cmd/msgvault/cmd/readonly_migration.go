package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runReadonlyMigrations performs one-time housekeeping for users
// upgrading from the writeable edition of msgvault to this read-only
// fork. It is best-effort: failures are logged and do not block
// startup. The function is idempotent.
//
// Two migrations:
//
//  1. Archive any leftover deletions/ directory. The deletion
//     subsystem and its CLI commands are removed in the read-only
//     edition, so any staged manifests are now orphans. We move them
//     aside (rather than delete) so a user can inspect what was
//     pending if they need to.
//
//  2. Warn once per Gmail OAuth token whose stored scope set is wider
//     than gmail.readonly. The token still works for read calls; the
//     warning prompts the user to re-authorize for least privilege.
//     A sibling marker file suppresses re-warning on every launch.
//
// See plans/readonly-conversion.md §7.
func runReadonlyMigrations(homeDir string) {
	archiveDeletionsDirIfPresent(homeDir)
	warnOnLegacyScopeTokens(filepath.Join(homeDir, "tokens"))
}

func archiveDeletionsDirIfPresent(homeDir string) {
	deletionsDir := filepath.Join(homeDir, "deletions")
	info, err := os.Stat(deletionsDir)
	if err != nil || !info.IsDir() {
		return // nothing to migrate
	}

	// Only archive if there's any content. An empty directory left
	// behind from a previous archive run is fine to leave alone.
	entries, err := os.ReadDir(deletionsDir)
	if err != nil || len(entries) == 0 {
		return
	}

	dst := filepath.Join(
		homeDir,
		fmt.Sprintf("deletions.archived-%s", time.Now().Format("20060102-150405")),
	)
	if err := os.Rename(deletionsDir, dst); err != nil {
		logger.Warn("read-only migration: could not archive deletions directory",
			"src", deletionsDir, "dst", dst, "error", err)
		return
	}
	logger.Warn("read-only migration: archived staged deletions from a prior msgvault build",
		"src", deletionsDir, "dst", dst,
		"note", "this fork does not perform deletions; the directory is preserved for inspection")
}

// warnOnLegacyScopeTokens scans tokens/*.json for Gmail OAuth tokens
// whose stored scopes include anything other than gmail.readonly, and
// emits a one-time WARN per such token. A sibling .legacy-scope-warned
// marker file suppresses re-warning on subsequent launches.
//
// Tokens without scope metadata (legacy pre-scope-tracking tokens) are
// not flagged: we cannot tell what they were minted with.
func warnOnLegacyScopeTokens(tokensDir string) {
	const readonlyScope = "https://www.googleapis.com/auth/gmail.readonly"

	entries, err := os.ReadDir(tokensDir)
	if err != nil {
		return // no tokens dir yet, or unreadable — nothing to warn about
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// Skip Microsoft tokens — they are scoped to Mail.Read /
		// IMAP.AccessAsUser.All by construction, not Gmail scopes.
		if strings.HasPrefix(name, "microsoft_") {
			continue
		}
		// Skip IMAP password files (different format).
		if strings.HasPrefix(name, "imap_") {
			continue
		}

		tokenPath := filepath.Join(tokensDir, name)
		markerPath := tokenPath + ".legacy-scope-warned"
		if _, err := os.Stat(markerPath); err == nil {
			continue // already warned for this token
		}

		data, err := os.ReadFile(tokenPath)
		if err != nil {
			continue // unreadable; not our problem to surface here
		}
		var tf struct {
			Scopes []string `json:"scopes"`
		}
		if err := json.Unmarshal(data, &tf); err != nil {
			continue // not a recognizable token file
		}
		if len(tf.Scopes) == 0 {
			continue // legacy token without scope metadata; cannot tell
		}

		var extra []string
		for _, s := range tf.Scopes {
			if s != readonlyScope {
				extra = append(extra, s)
			}
		}
		if len(extra) == 0 {
			continue // already minimal scope
		}

		logger.Warn(
			"read-only migration: existing OAuth token grants more than gmail.readonly",
			"token_file", name,
			"extra_scopes", strings.Join(extra, ","),
			"note", "this fork only requests gmail.readonly; the token still works for read calls. "+
				"For least privilege, re-authorize: revoke the grant at "+
				"https://myaccount.google.com/permissions then run 'msgvault add-account <email>'.")

		// Best-effort marker — suppresses re-warning on next launch.
		_ = os.WriteFile(markerPath, []byte("warned\n"), 0o600)
	}
}
