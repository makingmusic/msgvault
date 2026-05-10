package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var pruneLocalCmd = &cobra.Command{
	Use:   "prune-local",
	Short: "Prune dedup-hidden messages from the local archive (does not affect any remote mailbox)",
	Long: `Prune dedup-hidden messages from the local archive. This affects only
the local SQLite database; no remote mailbox is touched. msgvault
(this fork) is a read-only email backup tool and cannot mutate any
remote server.

Use --batch <id> to prune rows hidden by a specific dedup batch.
Use --all-hidden to prune every dedup-hidden row regardless of batch.

Pruned rows cannot be recovered with --undo (a database backup is
written by default; use --no-backup to skip).

Parquet analytics and the vector index may contain stale entries for
pruned rows until rebuilt; the rebuild commands are separate. Run
'msgvault build-cache --full-rebuild' for parquet analytics and
'msgvault build-embeddings --full-rebuild' for the vector index.`,
	RunE: runPruneLocal,
}

var (
	pruneLocalBatchIDs  []string
	pruneLocalAllHidden bool
	pruneLocalNoBackup  bool
	pruneLocalYes       bool
)

func runPruneLocal(cmd *cobra.Command, _ []string) error {
	// prune-local mutates local SQLite directly, has no remote API
	// equivalent, and the local DB is not reachable in remote mode.
	// Reject upfront so the user gets a clear error rather than the
	// generic "must specify --batch or --all-hidden" hint.
	if IsRemoteMode() {
		return fmt.Errorf("prune-local is local-only; not supported in remote mode")
	}

	if len(pruneLocalBatchIDs) == 0 && !pruneLocalAllHidden {
		return fmt.Errorf("must specify --batch or --all-hidden")
	}

	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// compute pre-delete stats and totalN before prompting.
	var totalN int64
	if pruneLocalAllHidden {
		// Match DeleteAllDeduped's predicate exactly: only rows that
		// the dedup pipeline soft-hid (deleted_at IS NOT NULL AND
		// delete_batch_id IS NOT NULL) are eligible for purge, so the
		// prompt counts must use the same gate. A bare deleted_at row
		// would over-report compared to the actual delete.
		var distinctBatches int64
		err = st.DB().QueryRow(
			st.Rebind("SELECT COUNT(*) FROM messages WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL"),
		).Scan(&totalN)
		if err != nil {
			return fmt.Errorf("count hidden messages: %w", err)
		}
		err = st.DB().QueryRow(
			st.Rebind("SELECT COUNT(DISTINCT delete_batch_id) FROM messages WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL"),
		).Scan(&distinctBatches)
		if err != nil {
			return fmt.Errorf("count distinct batches: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Will permanently prune %d hidden message(s) from the local archive across %d distinct batch(es).\n",
			totalN, distinctBatches)
	} else {
		type batchStat struct {
			id  string
			cnt int64
		}
		stats := make([]batchStat, 0, len(pruneLocalBatchIDs))
		for _, id := range pruneLocalBatchIDs {
			var cnt int64
			err = st.DB().QueryRow(
				st.Rebind("SELECT COUNT(*) FROM messages WHERE delete_batch_id = ? AND deleted_at IS NOT NULL"),
				id,
			).Scan(&cnt)
			if err != nil {
				return fmt.Errorf("count rows for batch %q: %w", id, err)
			}
			totalN += cnt
			stats = append(stats, batchStat{id: id, cnt: cnt})
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "Will permanently prune %d hidden message(s) from the local archive across %d batch(es):\n",
			totalN, len(pruneLocalBatchIDs))
		for _, s := range stats {
			_, _ = fmt.Fprintf(out, "  %s: %d row(s)\n", s.id, s.cnt)
		}
	}

	if totalN == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Nothing to prune.")
		return nil
	}

	// --all-hidden always prompts, even when --yes is set; spec rung 03 invariant.
	// Mode picks how EOF is handled: AllHidden treats closed stdin as a contract
	// violation (must not be silently bypassed), YesNo treats it as cancel.
	if !pruneLocalYes || pruneLocalAllHidden {
		mode := ConfirmModeYesNo
		if pruneLocalAllHidden {
			mode = ConfirmModeAllHidden
		}
		ok, err := confirmDestructive(cmd.InOrStdin(), cmd.OutOrStdout(), mode)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	if !pruneLocalNoBackup {
		// Resolve the DSN to a real filesystem path so backups work
		// when [data].database_url is a "file:" URI; reject non-file
		// DSNs (postgres://, etc.) which the VACUUM INTO backup path
		// can't operate on.
		dbFilePath, err := cfg.DatabasePath()
		if err != nil {
			return fmt.Errorf("resolve database path: %w", err)
		}
		backupPath := filepath.Join(
			filepath.Dir(dbFilePath),
			filepath.Base(dbFilePath)+".prune-local-backup-"+time.Now().Format("20060102-150405"),
		)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backing up database to %s...\n", filepath.Base(backupPath))
		if err := backupDatabase(st, backupPath); err != nil {
			return fmt.Errorf("backup database: %w", err)
		}
	}

	// Note: parquet analytics and the vector index may contain entries
	// for deleted rows; the post-run summary recommends rebuilding each
	// separately ('build-cache --full-rebuild' and
	// 'build-embeddings --full-rebuild').

	var deletedTotal int64
	var batchCount int64
	if pruneLocalAllHidden {
		deleted, distinct, err := st.DeleteAllDeduped()
		if err != nil {
			return fmt.Errorf("delete all dedup-hidden: %w", err)
		}
		deletedTotal = deleted
		batchCount = distinct
	} else {
		batchCount = int64(len(pruneLocalBatchIDs))
		for _, id := range pruneLocalBatchIDs {
			deleted, err := st.DeleteDedupedBatch(id)
			if err != nil {
				return fmt.Errorf("delete dedup batch %q: %w", id, err)
			}
			deletedTotal += deleted
		}
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "\nPruned %d message(s) from %d batch(es) in the local archive.\n\n", deletedTotal, batchCount)
	_, _ = fmt.Fprintln(out, "Caches may have stale entries; rebuild each separately:")
	_, _ = fmt.Fprintln(out, "  'msgvault build-cache --full-rebuild'        (parquet analytics)")
	_, _ = fmt.Fprintln(out, "  'msgvault build-embeddings --full-rebuild'   (vector index, if enabled)")

	return nil
}

func init() {
	rootCmd.AddCommand(pruneLocalCmd)
	pruneLocalCmd.Flags().StringArrayVar(&pruneLocalBatchIDs, "batch", nil,
		"Prune rows hidden by this batch ID (repeat for multiple batches)")
	pruneLocalCmd.Flags().BoolVar(&pruneLocalAllHidden, "all-hidden", false,
		"Prune every dedup-hidden row regardless of batch")
	pruneLocalCmd.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	pruneLocalCmd.Flags().BoolVar(&pruneLocalNoBackup, "no-backup", false,
		"Skip database backup before pruning")
	pruneLocalCmd.Flags().BoolVarP(&pruneLocalYes, "yes", "y", false,
		"Skip confirmation prompt")
}
