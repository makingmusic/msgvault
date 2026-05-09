# internal/deletion

Stage-and-execute deletion of Gmail messages with on-disk manifests
and resumable progress. **High-stakes**: this code deletes real Gmail
data. Read this whole file before changing it.

## File layout

- `manifest.go` — `Manifest`, `Manager`, status directories, JSON
  persistence, cancellation.
- `executor.go` — `Executor`, `Execute` (per-message), `ExecuteBatch`
  (Gmail batch API), checkpointing, retry of previously-failed IDs.

## The staging pattern

Deletions are never one-shot. Every deletion goes through three
states on disk under `<DataDir>/deletions/`:

```
deletions/
  pending/      # staged, not yet executed
  in_progress/  # executor is running
  completed/    # finished successfully
  failed/       # all messages failed
  cancelled/    # cancelled before completion
```

A `Manifest` is a JSON file in one of those directories. Status is
authoritative by **directory**, not by the inline `Status` field —
this matters during cancellation (see below). Filenames are
`<id>.json` where `id` = `YYYYMMDD-HHMMSS-<sanitized-description>`.

## Trash vs Permanent

`Method` controls the destructive action:

- `MethodTrash` → `gmail.API.TrashMessage`. Reversible for 30 days
  via Gmail UI/API.
- `MethodDelete` → `gmail.API.DeleteMessage` (per-message) or
  `BatchDeleteMessages` (up to 1000 IDs). **Permanent.** Gmail
  requires the more sensitive `gmail.modify` scope absent for OAuth
  apps not granted it.

`DefaultExecuteOptions()` returns `MethodTrash` with `BatchSize=100,
Resume=true`. The TUI defaults to trash; CLI users opt into permanent
with `--permanent`.

## Idempotency

`isNotFoundError` (`executor.go:18`) treats Gmail 404 as success on
the assumption the message was already deleted. Local DB is still
marked deleted via `MarkMessageDeletedByGmailID`. This makes resume
safe even if a previous run partially succeeded but didn't checkpoint.

## Insufficient-scope detection

`isInsufficientScopeError` (`executor.go:23`) recognizes Google's
scope errors and returns `resultFatal`. The executor halts immediately
and writes a checkpoint — partial progress is not lost.

Three substring matches: `ACCESS_TOKEN_SCOPE_INSUFFICIENT`,
`insufficient authentication scopes`, `Insufficient Permission`. Don't
narrow these without testing both the device and browser OAuth flows.

## Resume and checkpointing

Both `Execute` and `ExecuteBatch` save checkpoints:

- `Execution.LastProcessedIndex` — index into `manifest.GmailIDs` of
  the next message to attempt.
- `Execution.Succeeded`, `Execution.Failed`, `Execution.FailedIDs` —
  running totals.

`Execute` checkpoints every `BatchSize` messages (default 100) and
on context cancellation. `ExecuteBatch` checkpoints every 1000-message
Gmail batch and on individual fallback deletes.

`ExecuteBatch` additionally **retries previously failed IDs first**
(`executor.go:333`). When resumed, `manifest.Execution.FailedIDs` is
moved into a retry queue; the per-message loop runs against that
queue before continuing the main run. Successes increment `succeeded`,
remaining failures form the new `FailedIDs`. This is why a
re-execution can decrease the failure count.

## Cancellation order

`Manager.CancelManifest` (`manifest.go:386`) is careful about crash
ordering. The directory is authoritative, so it:

1. **Renames first** (atomic on same fs): pending/<id>.json →
   cancelled/<id>.json.
2. Then rewrites the inline `Status` field at the new location.

Reverse order would leave a manifest in pending/ with `Status:
cancelled`, contradicting the dir. The forward order leaves a window
where readers see a manifest in cancelled/ with `Status: pending` —
acceptable because the dir wins and the field self-heals on next
save.

## finalizeExecution failure-mode logic

`finalizeExecution` (`executor.go:180`) takes a `failOnAllErrors` flag.

- `Execute` (per-message) passes `true` → if `failed > 0 && succeeded
  == 0`, manifest goes to `failed/`, otherwise `completed/`.
- `ExecuteBatch` passes `false` → always lands in `completed/` even
  with failures. Batch semantics expect partial progress.

This asymmetry is intentional. Don't unify the paths without thinking
about what "all 50,000 IDs failed because the API was down for 30
seconds" should be classified as.

## When editing

- Never widen the executor surface without also widening
  `gmail.DeletionMockAPI` — tests rely on per-message error
  injection there.
- The DB write (`MarkMessageDeletedByGmailID`) follows the API call.
  If the DB write fails, log+continue — losing a local mark is
  better than losing the API success record. Don't reverse this.
- `Filters` is metadata only, not used by the executor; the TUI
  populates it for human review. Don't make the executor re-derive
  IDs from filters at execution time.
- New `Status` values must be added to BOTH `statusDirMap` and
  `persistedStatuses` (`manifest.go:201`). Forgetting either causes
  silent fall-through to the `pending` directory.
- The deletion manifest is the human's review surface. Never write
  to `in_progress/` from a brand-new manifest — staged manifests
  always start in `pending/`.
