package cmd

import (
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// readOnlyBannerCommands is the allowlist of command names that should
// surface the read-only-edition banner on startup.
//
// We deliberately scope this to commands a user runs interactively that
// involve sync, account changes, or local destruction — the moments
// where forgetting "this is the read-only build" would be most costly
// or most surprising. Short, scriptable read commands (list-*, query,
// search, get, version, completion) are intentionally excluded so
// pipelines stay clean.
var readOnlyBannerCommands = map[string]bool{
	"sync":           true,
	"sync-full":      true,
	"add-account":    true,
	"add-imap":       true,
	"add-o365":       true,
	"remove-account": true,
	"update-account": true,
	"serve":          true,
	"tui":            true,
	"prune-local":    true,
	"deduplicate":    true,
	"verify":         true,
	"build-cache":    true,
	"init-db":        true,
	"quickstart":     true,
}

// printReadOnlyBanner writes a one-line banner to stderr identifying
// this build as the read-only edition of msgvault. The banner only
// appears for commands in readOnlyBannerCommands, and only when stderr
// is a TTY — pipes and redirects stay clean so scripted use is not
// disturbed.
//
// Banner text and colour are deliberately conservative: yellow on
// stderr, no ANSI fluff beyond the colour wrapper, and a single line
// so it doesn't push command output off-screen on small terminals.
func printReadOnlyBanner(cmd *cobra.Command) {
	if !readOnlyBannerCommands[cmd.Name()] {
		return
	}
	if !isatty.IsTerminal(os.Stderr.Fd()) {
		return
	}

	const (
		yellow = "\x1b[33m"
		bold   = "\x1b[1m"
		reset  = "\x1b[0m"
	)
	fmt.Fprintf(os.Stderr,
		"%s%s[msgvault read-only edition]%s cannot trash, delete, or modify remote mailboxes. "+
			"For write capability, use upstream msgvault.\n",
		bold, yellow, reset,
	)
}
