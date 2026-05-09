# internal/scheduler

Cron-based scheduling for periodic Gmail syncs and (optionally) the
vector-embedding worker. Wraps `robfig/cron/v3`.

## Composition

- `scheduler.go` — `Scheduler`, `New`, `AddAccount`, `RemoveAccount`,
  `Start`, `Stop`, `TriggerSync`, `Status`, `IsScheduled`, `IsRunning`,
  `ValidateCronExpr`, `SetEmbedJob`. `AccountStatus` (scheduler.go:20)
  is what `api.handleSchedulerStatus` returns.
- `embed_job.go` — `EmbedJob` and the `EmbedRunner` interface that the
  embed worker satisfies.

## robfig/cron usage

`cron.New(cron.WithParser(cron.NewParser(Minute|Hour|Dom|Month|Dow)))`
(scheduler.go:60). Standard 5-field cron, no seconds. `ValidateCronExpr`
(scheduler.go:364) is exported so callers (`api.handleAddAccount`,
`SetEmbedJob`) can validate user input before mutating state.

`AddAccount` (scheduler.go:83) registers a closure with `cron.AddFunc`.
The closure short-circuits when `s.stopped || s.running[email]`, otherwise
it acquires the per-account `running[email]=true` flag, increments
`wg`, and calls `runSync` synchronously inside the cron worker
goroutine. Re-adding an existing email replaces the prior entry.

`SetEmbedJob` (scheduler.go:160) follows the same pattern but stores a
single optional `cron.EntryID` (`embedEntry`/`embedEntrySet` — `EntryID
0` is valid, hence the separate bool). Validates the cron expression
*before* removing the previous entry so a bad input cannot leave the
scheduler with a half-removed job.

`Stop` (scheduler.go:235) sets `stopped=true`, calls `cron.Stop()` (which
returns a context that completes when in-flight cron callbacks finish),
cancels `s.ctx`, and waits on `wg` for sync goroutines + post-sync
embed passes to drain. Returns a context that callers can wait on (with
their own timeout — `serve.go` uses 30s).

## How accounts get scheduled

The serve daemon calls `sched.AddAccountsFromConfig(cfg)`
(scheduler.go:122) which iterates `cfg.ScheduledAccounts()` (only
entries with `enabled = true`) and registers each. Per-account schedule
comes from `[[accounts]] schedule = "0 2 * * *"` in `config.toml`. The
sync callback is `runScheduledSync` in `cmd/msgvault/cmd/serve.go:338`
— which performs incremental Gmail sync, runs identity migrations,
optionally enqueues new message IDs into the vector pipeline, and then
rebuilds the Parquet cache when stale.

The `POST /api/v1/accounts` endpoint also calls `sched.AddAccount` after
persisting the new entry, so additions take effect without a restart
(api/handlers.go:929).

## Concurrency

- **Per-account lock:** `running[email]` ensures only one sync runs per
  account at any time. A second cron tick for the same email while the
  first is in-flight is a silent skip (scheduler.go:97). Same gate
  blocks `TriggerSync` (scheduler.go:319) — the caller gets
  `"sync already running for %s"`.
- **Across accounts:** independent. cron's default worker pool runs
  callbacks concurrently; each `runSync` is its own goroutine.
- **Embed job:** uses `sync.Mutex.TryLock` (`EmbedJob.running`,
  embed_job.go:54) — drop-not-queue semantics. A cron tick that
  arrives while the previous embed pass is still draining returns
  immediately and waits for the next tick.
- **Post-sync hook:** when `runEmbedAfterSync` is true, `runSync` calls
  `embedJob.Run(s.ctx)` synchronously after a successful sync
  (scheduler.go:294-305). It runs in the same goroutine that's already
  counted in `wg`, so `Stop` naturally waits for it.

## EmbedJob

`EmbedJob.Run` (embed_job.go:61) is the daemon entry point for the
vector worker. Each invocation:

1. Calls `Worker.ReclaimStale(ctx)` to recover dropped leases.
2. `pickTarget` (embed_job.go:168) — prefers a *building* generation
   matching `Fingerprint` over the active one. A mismatched-fingerprint
   building generation is left alone (only the CLI can resolve);
   building with empty `Fingerprint` is also refused (would risk
   silently swapping the active model).
3. For building generations: `Backend.EnsureSeeded` first to guard
   against the `CreateGeneration` crash window where the building row
   exists but the seed never landed.
4. `Worker.RunOnce(ctx, target)` drains a batch.
5. **Activation gate** for building generations: only flip to active
   when `pendingCount` for that generation is zero
   (embed_job.go:142-151). Non-atomic by design — see the comment
   block at embed_job.go:122 for why concurrent
   `sync.EnqueueMessages` is safe.

`VectorsDB` is required for the activation gate; if nil, the daemon
logs and skips auto-activation rather than activating an unfinished
index.

`EmbedRunner` (embed_job.go:16) is the test seam — fakes need
`RunOnce` and `ReclaimStale`.
