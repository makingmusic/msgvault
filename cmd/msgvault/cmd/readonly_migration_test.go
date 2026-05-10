package cmd

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// silenceLogger swaps the package-level logger for a discard logger
// for the duration of the test, so warnings don't pollute test output.
// Restores the previous logger on cleanup.
func silenceLogger(t *testing.T) {
	t.Helper()
	prev := logger
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { logger = prev })
}

func TestArchiveDeletionsDirIfPresent_ArchivesPopulatedDir(t *testing.T) {
	silenceLogger(t)
	home := t.TempDir()
	deletionsDir := filepath.Join(home, "deletions")
	if err := os.MkdirAll(filepath.Join(deletionsDir, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(deletionsDir, "pending", "manifest.json"),
		[]byte("{}"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	archiveDeletionsDirIfPresent(home)

	if _, err := os.Stat(deletionsDir); !os.IsNotExist(err) {
		t.Errorf("deletions/ should have been moved away; stat err = %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "deletions.archived-") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a deletions.archived-* directory in %s", home)
	}
}

func TestArchiveDeletionsDirIfPresent_NoOpWhenAbsent(t *testing.T) {
	silenceLogger(t)
	home := t.TempDir()
	// No deletions/ dir — must not error or create anything spurious.
	archiveDeletionsDirIfPresent(home)
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Errorf("home should remain empty; got %d entries", len(entries))
	}
}

func TestWarnOnLegacyScopeTokens_MarkerSuppressesRewarn(t *testing.T) {
	silenceLogger(t)
	tokensDir := t.TempDir()

	// Write a token whose scope set includes gmail.modify (legacy).
	legacy := map[string]any{
		"access_token": "x",
		"scopes": []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.modify",
		},
	}
	data, _ := json.Marshal(legacy)
	tokenPath := filepath.Join(tokensDir, "user@example.com.json")
	if err := os.WriteFile(tokenPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// First call should write a marker file.
	warnOnLegacyScopeTokens(tokensDir)
	markerPath := tokenPath + ".legacy-scope-warned"
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected marker file %s after first warn, got %v", markerPath, err)
	}

	// Second call must be a no-op (marker present); we can't easily
	// assert "no log line emitted" with the discard logger, but we
	// can assert the marker isn't recreated with a different mtime.
	info1, _ := os.Stat(markerPath)
	warnOnLegacyScopeTokens(tokensDir)
	info2, _ := os.Stat(markerPath)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("marker file was rewritten on second call; suppression failed")
	}
}

func TestWarnOnLegacyScopeTokens_SkipsAlreadyMinimalToken(t *testing.T) {
	silenceLogger(t)
	tokensDir := t.TempDir()
	minimal := map[string]any{
		"access_token": "x",
		"scopes":       []string{"https://www.googleapis.com/auth/gmail.readonly"},
	}
	data, _ := json.Marshal(minimal)
	tokenPath := filepath.Join(tokensDir, "user@example.com.json")
	if err := os.WriteFile(tokenPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	warnOnLegacyScopeTokens(tokensDir)

	if _, err := os.Stat(tokenPath + ".legacy-scope-warned"); err == nil {
		t.Errorf("marker file should not exist for already-minimal token")
	}
}
