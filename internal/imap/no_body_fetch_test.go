package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBodyFetch is the load-bearing regression test against
// silent-mutation in the IMAP client. It scans every non-test .go file
// in this package and asserts that no source line contains `BODY[`
// outside an allowlisted comment.
//
// Background: IMAP has two ways to fetch a message body — `FETCH BODY[]`
// (which the server treats as a read and sets the \Seen flag as a side
// effect) and `FETCH BODY.PEEK[]` (which fetches the same bytes without
// touching any flags). This fork is the read-only edition: marking
// thousands of messages as read during a sync would visibly mutate the
// user's mailbox state, contradicting the product promise.
//
// At the time this test was written the IMAP client used `Peek: true`
// in its only body-fetch site (client.go), and `BODY[` did not appear
// in production code. This test stops a future contributor from
// copy-pasting a fetch from another project and silently reintroducing
// mark-as-read-on-fetch.
//
// Allowlist: lines that are *only* a comment about BODY[ vs BODY.PEEK[
// (e.g. the comment on the actual fetch site explaining the choice)
// are permitted. Code lines that contain `BODY[` are not.
func TestNoBodyFetch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		for lineNum, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "BODY[") {
				continue
			}
			// Allow comment lines (whole-line // comments) that
			// document the BODY[ vs BODY.PEEK[ distinction.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			t.Errorf(
				"%s:%d contains forbidden `BODY[` outside an "+
					"allowlisted comment. Use `BODY.PEEK[` (or the "+
					"library equivalent `Peek: true`) instead. "+
					"Reason: BODY[ marks messages as read on the "+
					"server, which contradicts msgvault's read-only "+
					"contract. Line: %q",
				name, lineNum+1, strings.TrimSpace(line),
			)
		}
	}
}
