# TESTING.md — Test Surface Audit

Snapshot date: 2026-05-09. Scope: every `*_test.go` file outside `vendor/`.

This document maps the test surface of msgvault, names the helpers worth
reusing, and calls out gaps with prioritized recommendations. It is opinion,
not policy: where I think the project is over-mocked or under-tested, I say
so. Treat the P0/P1/P2 labels as a starting point for triage.

---

## 1. Overview

- **Total test/bench functions:** 1864 across 170 test files (1856 `Test*`,
  8 `Benchmark*`).
- **Top-tested packages by test count:**
  - `internal/query` (~180 across `sqlite_*`, `duckdb_test.go`,
    `views_test.go`, `text_search_live_test.go`, `benchmark_test.go`)
  - `internal/store` (~180 across `store_test.go`, `dedup_*`,
    `account_identities_test.go`, `subset_test.go`, `db_logger_test.go`,
    `sources_test.go`, `inspect_test.go`, etc.)
  - `internal/api` (~85: `handlers_test.go` alone has 68 funcs)
  - `internal/tui` (~240; mostly state-machine / nav coverage)
  - `internal/sync` (62 funcs in `sync_test.go` alone)
  - `internal/fbmessenger` (~70 across format-specific parser tests)
  - `cmd/msgvault/cmd` (~190 across CLI command files; many are
    flag-validation and confirm-prompt unit tests)
- **Packages with notably few tests:**
  - `internal/remote` only has 24 funcs covering the HTTP store and 2 for
    the engine adapter (`engine_test.go:14`, `:70`); plenty of remote
    methods are uncovered.
  - `internal/textimport` has 1 integration test and 1 phone unit test —
    nothing for the sources-route.
  - `internal/imap` is shallow (config+labels+1 xoauth2 test); no
    integration test exercising `client_xoauth2.go` against a real or
    fake IMAP server.
  - `internal/oauth/profile.go` and `internal/oauth/serviceaccount.go`
    each have 1–4 unit tests.
  - `internal/microsoft/oauth.go` has 46 tests (unusually well-covered)
    but still no live test against an Entra ID stub.
- **Makefile targets:**
  - `make test` — `go test -tags "fts5 sqlite_vec" ./...` (default).
  - `make test-v` — verbose.
  - `make test-pg` — PostgreSQL backend; **expected to fail**: see
    `docs/PG_STATUS.md`. Set `MSGVAULT_TEST_DB=postgres://...` first.
  - `make bench` — runs `internal/query/...` benchmarks only.
  - `make lint` / `make lint-ci`.
- **Test wall-time:** not measured here (instructions say not to run
  the full suite repeatedly). The PST integration test
  (`internal/importer/pst_integration_test.go`) reads real `.pst`
  fixtures and is by far the slowest single test; expect ~tens of
  seconds for a clean pass.

---

## 2. Test taxonomy

| Category                | Count | Examples                                                                                                                 |
| ----------------------- | ----- | ------------------------------------------------------------------------------------------------------------------------ |
| Pure unit               | ~700  | `internal/dedup/normalize_test.go`, `internal/textutil/encoding_test.go`, `internal/search/parser_test.go`               |
| Store/DB-backed         | ~400  | `internal/store/store_test.go`, `internal/query/sqlite_*_test.go`, all importer tests                                    |
| Integration             | ~30   | `internal/textimport/integration_test.go`, `internal/importer/pst_integration_test.go`                                   |
| E2E (CLI command)       | ~10   | `cmd/msgvault/cmd/import_mbox_e2e_test.go`, `cmd/msgvault/cmd/import_messenger_e2e_test.go`                              |
| HTTP via httptest       | ~80   | `internal/api/handlers_test.go`, `internal/remote/store_test.go`, `internal/vector/embed/client_test.go`                 |
| TUI state machine       | ~240  | `internal/tui/nav_test.go`, `internal/tui/model_test.go`, `internal/tui/selection_test.go`                               |
| Mock-API based          | ~50   | `internal/gmail/mock_test.go`, `internal/gmail/deletion_mock_test.go`, `internal/deletion/executor_test.go`              |
| Build-tag-gated `sqlite_vec` | 14 files | `internal/vector/sqlitevec/*_test.go`, `internal/vector/embed/queue_test.go`, `cmd/msgvault/cmd/embed_vector_test.go` |
| Build-tag-gated `fts5`  | 1     | `internal/fbmessenger/importer_fts_test.go`                                                                              |
| Build-tag-gated `!sqlite_vec` | 1 | `cmd/msgvault/cmd/serve_vector_stub_test.go`                                                                             |
| Postgres-conditional    | ~100  | Any test using `testutil.NewTestStore` opt-in switches when `MSGVAULT_TEST_DB` is set; 2 dedicated dialect unit tests at `internal/store/dialect_pg_test.go`, `internal/store/postgres_internal_test.go` |
| Benchmarks              | 8     | `internal/query/benchmark_test.go`                                                                                       |
| Snapshot/golden         | 0     | (none — no `golden/`, no `update-golden`)                                                                                |
| Live/external (network) | 0     | (none gated by env var; no `_live_test.go`. The file `internal/query/text_search_live_test.go` is misnamed — it's local) |

The "live" suffix on `text_search_live_test.go` refers to the project's
"live messages" filter (excluding tombstoned rows), not to a network test.
That naming will mislead future readers.

---

## 3. testutil catalog

The `internal/testutil` tree is the source of truth for shared helpers.
Every helper that survives the next few PRs should live here, not
duplicated per package.

### `internal/testutil/testutil.go` (root file)
Doc-only. Inventories the file split.

### `internal/testutil/assert.go`
| Helper                            | Purpose                                                  |
| --------------------------------- | -------------------------------------------------------- |
| `MakeSet[T]`                      | Build a set from variadic items                          |
| `AssertEqualSlices[T]`            | Slice equality, generic                                  |
| `AssertStrings`                   | Slice equality with `%q` formatting                      |
| `AssertValidUTF8`                 | UTF-8 validity check                                     |
| `AssertContainsAll`               | Substring containment                                    |
| `AssertStringSet`                 | Order-insensitive multiset equality                      |
| `MustNoErr`                       | Fatal on error with message                              |
| `AssertEqual[T]`                  | Generic equality                                         |

### `internal/testutil/store_helpers.go`
| Helper                            | Purpose                                                  |
| --------------------------------- | -------------------------------------------------------- |
| `NewTestStore(t)`                 | The canonical "give me a *store.Store" — switches between SQLite (default) and PostgreSQL when `MSGVAULT_TEST_DB` is set. Each PG test gets its own random schema (`msgvault_test_<hex>`) which is dropped on cleanup. |

This is the single most important helper. Every store-backed test uses it.

### `internal/testutil/builders.go`
`MessageSummaryBuilder` (`testutil.go:14`) and `MessageDetailBuilder`
(`testutil.go:108`) — fluent builders that fill a `query.MessageSummary` /
`query.MessageDetail` with sensible defaults. Used heavily by API and
remote tests. Use these instead of constructing structs by hand.

### `internal/testutil/fs_helpers.go`
| Helper                            | Purpose                                                  |
| --------------------------------- | -------------------------------------------------------- |
| `WriteFile(t, dir, name, data)`   | Write under a tempdir; rejects absolute or escape paths  |
| `ReadFile(t, path)`               | Read or fail                                             |
| `AssertFileContent(t, path, str)` | Assert contents                                          |
| `MustExist`, `MustNotExist`       | Path existence                                           |
| `WriteAndVerifyFile`              | Round-trip helper                                        |
| `validateRelativePath` (private)  | Windows-aware traversal guard                            |

### `internal/testutil/archive_helpers.go`
`CreateTarGz`, `CreateZip`, `CreateTempZip`. Consumed by mbox-zip, emlx,
and security tests. Note the asymmetric API surface — `CreateTempZip`
takes a `map[string]string`, the others take `[]ArchiveEntry`. Picking
between them is non-obvious; prefer `CreateZip`/`CreateTarGz` for
anything that wants typeflag/symlinks.

### `internal/testutil/security_data.go`
`PathTraversalCases()` returns the canonical set of traversal vectors
(rooted, dot-dot, drive-relative on Windows, UNC). Reuse this in any
new test that opens user-controlled paths.

### `internal/testutil/encoding.go`
`EncodedSamples()` returns a fresh copy of byte sequences for ShiftJIS,
GBK, Big5, EUC-KR, Win1252, Latin1 in both short and long forms (long
samples are the threshold for `chardet` to commit). Used by
`internal/textutil`, `internal/sync`, and `internal/store` repair tests.
The maintainer note is right: keep it explicit, not reflective.

### `internal/testutil/dbtest/dbtest.go`
A second test-DB seam, deliberately divorced from `internal/query` to
avoid import cycles. Use `dbtest.NewTestDB(t, schemaPath)` when the
package being tested already imports `internal/store` (which `testutil`
does) and would otherwise loop.

Helpers: `SeedStandardDataSet` (5 messages from Alice/Bob to
Carol with labels and attachments), `MustLookupParticipant`,
`AddSource`, `AddConversation`, `AddLabel`, `AddMessageLabel`,
`AddParticipant`, `AddMessage` (which auto-fills sentinel defaults and
auto-resolves source_id from conversation), `EnableFTS` (skips if FTS5
unavailable), `MarkDeletedByID`, `MarkDeletedBySourceID`.

### `internal/testutil/email/email.go` and `email/builder.go`
| Helper                            | Purpose                                                  |
| --------------------------------- | -------------------------------------------------------- |
| `email.MakeRaw(opts)`             | Quick raw RFC2822 with CRLF                              |
| `email.MessageBuilder` (fluent)   | Multipart, attachments, custom headers, CRLF-vs-LF       |
| `email.AssertStringSliceEqual`    | Slice assert with label                                  |

`MessageBuilder.Bytes()` is the primary "make me a real email" tool — use
this for ingest, MIME parse, sync mock, and importer tests.

### `internal/testutil/storetest/storetest.go`
The high-level fixture. `Fixture.New(t)` gives you:

- A `*store.Store` from `testutil.NewTestStore`
- A pre-created source `test@example.com`
- A default conversation

Plus convenience helpers: `CreateMessage`, `CreateMessages(n)`,
`EnsureLabels`, `EnsureParticipant`, `StartSync`, getters for message
fields/body/labels/recipients, and asserters for label count,
recipient count, deletion state, and active sync.

There's also a fluent `MessageBuilder` (`storetest.NewMessage`) — use
`f.NewMessage()` for deterministic per-test IDs (`fixture-msg-N`)
rather than the package-level counter.

### `internal/testutil/tbmock/mock_tb.go`
`MockTB` wraps a `testing.TB` to intercept `Fatal`/`Skip` via a panic
sentinel, so meta-tests (testing the helpers themselves) can verify
fail-fast behavior. Used in `testutil_test.go` and
`storetest_test.go`. Don't import this from production tests.

### `internal/testutil/ptr/ptr.go`
Pointer constructors. Tiny but used for sql.NullX pointer-builder cases.

### Mock APIs (per package, not in testutil)
| Mock                          | File                                            | Use                                                   |
| ----------------------------- | ----------------------------------------------- | ----------------------------------------------------- |
| `gmail.NewMockClient`         | `internal/gmail/mock_test.go`                   | Mock Gmail client for `internal/sync` tests           |
| `gmail.NewDeletionMockAPI`    | `internal/gmail/deletion_mock_test.go`          | Mock for `internal/deletion/executor.go` tests        |
| `querytest.MockEngine`        | `internal/query/querytest`                      | Stand-in `query.Engine`; used by API+MCP tests        |
| `mockStore` (api package)     | `internal/api/handlers_test.go:38-79`           | Stand-in `Store` interface for handler tests          |
| `mockScheduler`               | `internal/api/handlers_test.go`                 | Stand-in `SchedulerInterface`                         |

`mockStore`/`mockScheduler` live in the test file and aren't shared. If
they ever need to be reused by a sibling test package, lift them into
`internal/api/apitest` or similar.

---

## 4. Per-subsystem coverage

| Package                          | Tests | Quality       | Obvious gaps                                                                                                                           |
| -------------------------------- | ----- | ------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/store`                 | ~180  | Strong        | No tests for the Postgres functional path (blocker — see PG_STATUS); `LastInsertId` failure modes; concurrent write contention         |
| `internal/query` (SQLite)        | ~125  | Strong        | DuckDB + sqlite_scanner attached path is exercised by `duckdb_test.go` (78 funcs) but mostly happy-path; no test for the search-cache eviction race; encoding_hint coverage is thin |
| `internal/query` (DuckDB hybrid) | 78    | Good          | `searchCacheTable` cleanup on context-cancel mid-query; concurrent same-query races; no test for `probeColumns` on a stale Parquet     |
| `internal/query` (Postgres)      | 1     | Scaffold      | All real methods return ErrNotImplemented; no test confirms which methods are wired                                                    |
| `internal/sync`                  | 62    | Strong        | Resume tested (`TestFullSyncResume`, `TestFullSyncResumeWithCursor`) but no test for "checkpoint blocked, then operator fixes config and resumes"; no test for sync against a hung Gmail upstream |
| `internal/api`                   | 85    | Strong        | One timeout test (`TestHandleSearch_HybridEmbeddingTimeoutFiresChi`); no fuzz of `/api/v1/query` SQL injection; no test that hot-reloads CORS config; no test that `validate_secure` blocks insecure binds |
| `internal/remote` (HTTP store)   | 22    | Mid           | `engine_test.go` covers two methods; ~20 remote engine methods uncovered; no test for transient 502/503 retry behavior; no test for partial-response truncation |
| `internal/scheduler`             | 40    | Strong        | TestStopCancelsRunningSync covers happy cancel; no test that a panicking sync callback doesn't poison the per-account `running` flag (deadlock risk) |
| `internal/deletion`              | 50    | Strong        | Excellent error/scope/idempotency coverage; no test for "fs becomes read-only between staging and execute"                             |
| `internal/gmail`                 | 29    | Good          | Mock-heavy; `client_test.go` has only 3 tests; rate limiter has 15. No real HTTP-layer test against a fake Gmail HTTP server           |
| `internal/oauth`                 | 24    | Mid           | `serviceaccount_test.go`/`profile_test.go` are thin (1–4 funcs); no test for refresh-token replay or rotation                          |
| `internal/microsoft/oauth`       | 46    | Strong (!)    | Notable outlier — well-covered for an OAuth flow                                                                                       |
| `internal/imap`                  | 11    | Weak          | Only 1 client test; no integration test against go-imap server; no XOAUTH2 round-trip                                                  |
| `internal/dedup`                 | 16    | Strong        | Extensive scenario coverage; consider an N+1 regression test (count statements per group)                                              |
| `internal/importer/mbox`         | 21    | Strong        | Resume + checkpoint-blocked + multi-file all covered                                                                                   |
| `internal/importer/emlx`         | 15    | Strong        | Resume + label-merge across mailboxes covered                                                                                          |
| `internal/importer/pst`          | 10    | Strong        | Real fixture-driven integration test; `ContextCancelledMidImport` (not just "before open") would be a useful add                       |
| `internal/importer/ingest`       | 4     | Mid           | Heart of the import path; deserves more failure-mode tests                                                                             |
| `internal/fbmessenger`           | 70    | Strong        | Per-format parsers heavily fuzzed; convergence test exists                                                                             |
| `internal/whatsapp`              | 24    | Mid           | Mapping/contacts/queries each have 6–12 tests; no end-to-end import test                                                               |
| `internal/imessage`              | 6     | Weak          | One parser test file. No import_test                                                                                                   |
| `internal/gvoice`                | 12    | Mid           | Parser-only                                                                                                                            |
| `internal/textimport`            | 2     | Weak          | One integration test, one phone test; the multi-source merge logic deserves more                                                       |
| `internal/applemail`             | 7     | Mid           | Account discovery only                                                                                                                 |
| `internal/mbox`                  | 15    | Strong        | Reader is small surface, well covered                                                                                                  |
| `internal/mime`                  | 14    | Strong        | Comprehensive `parse_test.go`                                                                                                          |
| `internal/mcp`                   | 31    | Strong        | All major tools covered                                                                                                                |
| `internal/tui`                   | 240   | Strong (nav)  | No remote-backend path test — `cmd/tui.go` switches between local Store and `remote.NewEngine`; only the local path is exercised      |
| `internal/vector` (root)         | 22    | Mid           | `stats_test.go`, `generations_test.go`, `config_test.go` all reasonable. No tests for `generations.ResolveActiveForFingerprint` corner cases like multiple active rows |
| `internal/vector/embed`          | 65    | Strong        | Excellent client-level coverage including 4xx/5xx/retry/cancel; worker concurrency tested                                              |
| `internal/vector/hybrid`         | 30    | Strong        | RRF + filter parser tested                                                                                                             |
| `internal/vector/sqlitevec`      | 64    | Strong        | Build-tag-gated; without `sqlite_vec` these don't run                                                                                  |
| `internal/scheduler` embed job   | (in scheduler tests) | Mid | EmbedJob's activation gate (`pendingCount==0`) deserves an explicit test                                                                |
| `internal/config`                | 44    | Strong        | TOML round-trip, defaults, validation                                                                                                  |
| `internal/search`                | 5     | Mid           | Parser coverage thin — only 5 funcs                                                                                                    |
| `internal/export`                | 13    | Mid           | Attachment store tests; no test for content-hash collision behavior                                                                    |
| `internal/update`                | 17    | Strong        | Self-update logic well covered                                                                                                         |
| `internal/logging`               | 6     | Mid           | Structured logging level tests                                                                                                         |
| `internal/fileutil`              | 6     | Mid           | Secure-write helpers                                                                                                                   |
| `internal/textutil`              | 14    | Strong        | Encoding repair                                                                                                                        |
| `cmd/msgvault/cmd`               | ~190  | Strong        | Mostly flag-validation and confirm-prompt; e2e tests for mbox + messenger only                                                          |

---

## 5. Fixture conventions

- **`testdata/` directories** exist in two places only:
  - `internal/pst/testdata/` — `support.pst`, `32-bit.pst` (real PST
    files used by `internal/importer/pst_integration_test.go` via the
    relative path `"../pst/testdata"`).
  - `internal/fbmessenger/testdata/` — directory-per-scenario layout
    (`html_simple/`, `json_group/`, `e2ee_simple/`, `corrupt/`, etc.).
    Each subdir holds a tree the importer can ingest as-is.
- **No golden files.** No `update-golden` flag, no `*.golden` extension,
  no environment variable to regenerate. If you want to add snapshot
  testing for TUI rendering or MCP responses, you're starting from
  zero.
- **Per-test tempdirs.** Standard pattern is `t.TempDir()` + `store.Open`
  with cleanup. `testutil.NewTestStore(t)` does this for you.
- **In-memory SQLite.** Used for fast pure-store tests (e.g.
  `dbtest.NewTestDB`). For Postgres mode, `testutil.NewTestStore` opens
  the same URL with a unique `search_path=msgvault_test_<hex>` schema.
- **Synthetic data only.** Per `CLAUDE.md`: `alice`, `bob`, `Carol`,
  `user@example.com`. The PST fixture is the exception (it's the
  upstream Hacking Team `support.pst` — already public). Don't introduce
  new real-data fixtures.

---

## 6. Build tag gating

Default tags applied by `make test` and `make build`: `fts5 sqlite_vec`.

| Tag           | Tests gated                                                                                                                           |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `sqlite_vec`  | All `internal/vector/sqlitevec/*_test.go` (5 files), `internal/vector/embed/{queue,enqueue,worker,testsupport}_test.go`, `internal/vector/hybrid/engine_test.go`, `cmd/msgvault/cmd/{search_vector,embed_vector}_test.go` |
| `!sqlite_vec` | `cmd/msgvault/cmd/serve_vector_stub_test.go` (asserts the stub error message)                                                          |
| `fts5`        | `internal/fbmessenger/importer_fts_test.go`                                                                                            |

The default `make test` invocation runs everything because both tags
are set. **A `go test ./...` without `-tags` would silently skip 14
test files.** That's a CI footgun if anyone runs vanilla `go test` —
recommend documenting the requirement (or fail loudly when the binary
is built without `sqlite_vec` and someone tries to use vector search,
which the stub already does).

---

## 7. Live / external tests

There are **no live tests against external services** in the codebase.

- No env-gated Gmail integration test.
- No env-gated IMAP server test.
- No env-gated Microsoft Graph test.
- No env-gated embedding-endpoint test (`internal/vector/embed/client_test.go`
  uses `httptest.NewServer` exclusively).

`MSGVAULT_TEST_DB` is the only external-service env var, and it
selects between SQLite-in-memory and PostgreSQL for the store layer.
Per `docs/PG_STATUS.md` the PG path is scaffold-only, so tests under
`MSGVAULT_TEST_DB=postgres://...` are expected to fail until the
blockers there are resolved.

`internal/query/text_search_live_test.go` is **not a live test** —
"live" refers to the live-messages predicate. Rename for clarity.

---

## 8. Gap analysis

Priorities reflect impact on data safety (P0), regression risk (P1),
or cleanup (P2). I've named tests in the "would-add" form so they map
1:1 onto a follow-up PR.

### P0 — data-safety risk

1. **Deletion: read-only filesystem mid-execute.**
   `internal/deletion/executor.go` writes manifest checkpoints during
   execute. If `<DataDir>/deletions/in_progress/` becomes unwritable
   between staging and a checkpoint, the executor probably continues
   deleting from Gmail without persisting progress — a silent
   data-loss vector on resume.
   - Add `TestExecutor_Execute_FailsClosed_WhenManifestRenameFails`:
     simulate `os.Rename` failure (use a read-only test FS); assert
     the executor halts at the next checkpoint boundary and reports a
     fatal error.
   - Add `TestExecutor_Execute_RecoversFromCheckpointAfterRenameRecovers`.

2. **Sync: hung Gmail upstream blocks scheduler indefinitely.**
   `internal/scheduler` runs `runSync` synchronously inside the cron
   goroutine. There's no per-account timeout enforced by the
   scheduler itself — it relies on the Gmail client's internal
   timeouts. If `internal/gmail/client.go` ever has a code path
   without `context.WithTimeout`, a single hung account hangs that
   slot forever (and `Stop()` won't return).
   - Add `TestScheduler_RunSyncRespectsAccountTimeout`: register a
     callback that blocks on `<-ctx.Done()` for >2s, set a 100ms
     timeout via a new `Scheduler` option, assert callback returns
     within 200ms.
   - Audit `internal/gmail/client.go` for any HTTP call without an
     explicit deadline — global rule from `~/.claude/CLAUDE.md`.

3. **PostgreSQL store: LastInsertId path is unimplemented but
   `EnsureConversation`/`EnsureParticipant` still call it.**
   Per `docs/PG_STATUS.md` and `internal/store/CLAUDE.md`, this
   regresses to silent zero-value IDs on Postgres. Today this is
   masked because nobody runs `MSGVAULT_TEST_DB=postgres://...`
   successfully.
   - Add `TestStore_EnsureConversation_ReturnsNonZeroID_OnPostgres`
     (gated by `MSGVAULT_TEST_DB`). It should fail loudly on Postgres
     until conversion to `RETURNING id` is complete.
   - Same for `EnsureParticipant`, `StartSync`, `EnsureLabel`.

4. **Embed worker: vector activation race.**
   `internal/vector/embed/worker.go` activates a building generation
   when `pendingCount==0`. The doc warns this is non-atomic by design;
   concurrent `Enqueuer.EnqueueMessages` after the count check could
   activate an incomplete index.
   - Add `TestEmbedJob_Run_DoesNotActivateWhenConcurrentEnqueueLands`:
     interleave `pickTarget` → `Worker.RunOnce` (drains to zero) with
     a second goroutine inserting into `pending_embeddings`; assert
     the activation does NOT happen until pending is zero AND quiesced.

5. **Deletion: scope error mid-batch leaves stale `in_progress`
   manifest if the process crashes between API error and checkpoint
   write.** Currently tested only when the checkpoint write succeeds.
   - Add `TestExecutor_Execute_ScopeError_CrashBeforeCheckpoint_RecoversCleanlyOnResume`.

### P1 — regression risk

6. **HTTP fan-out timeouts on `/api/v1/query` and `/api/v1/search`.**
   `TestHandleSearch_HybridEmbeddingTimeoutFiresChi`
   (`internal/api/handlers_test.go:1614`) is the only timeout test.
   The chi `Timeout` middleware is "gentle" — it cancels the request
   context but doesn't actually stop the handler. If a handler does a
   long blocking SQL query that ignores its context, the request
   hangs until `WriteTimeout`. Tests don't currently exercise this.
   - Add `TestHandleQuery_BlockingSQLRespectsContextDeadline`:
     install an engine that blocks `QuerySQL` until ctx canceled;
     assert response time ≤ requestTimeout + small slack.
   - Add `TestHandleSearch_FastSearch_DBContextHonored` similarly.

7. **N+1 detection on dedup, collections, identities.**
   Per global `CLAUDE.md`: "When adapting a single-item method into a
   multi-item variant, rewrite the query — don't wrap the original in
   a loop." `internal/store/dedup_test.go` covers semantics
   beautifully but doesn't count statements.
   - Add `TestStore_GetDuplicateGroupMessages_NoNPlusOne`: wrap the
     `*sql.DB` in a counting driver, assert query count is O(1) for
     a 1000-row dedup group, not O(rows).
   - Same for `Store.GetMessagesSummariesByIDs` (the contract at
     `engine.go:36`).
   - Same for collection list/get/add/remove paths.

8. **Sync: incremental against a stale history-id (history expired)
   only tests the happy reset.** Currently `TestIncrementalSyncHistoryExpired`
   confirms reset; no test confirms checkpoint integrity post-reset.
   - Add `TestIncrementalSyncHistoryExpired_PreservesCompletedSyncRow`.

9. **TUI remote backend.** `cmd/msgvault/cmd/tui.go:73` switches between
   `remote.NewEngine` and a local engine based on config. None of the
   240 TUI tests exercise the remote path.
   - Add `TestTUIModel_LoadAggregates_UsesRemoteEngineWhenConfigured`:
     spin up `httptest.NewServer` mimicking `/api/v1/aggregates`,
     point a `*remote.Engine` at it, drive the model with `Update`.

10. **MCP/HTTP route surface coverage.** Verifying every route is
    actually mounted:
    - Add `TestServerRouter_AllExpectedRoutesMounted`: walk the chi
      router via `chi.Walk`, assert the set against a hard-coded list
      of `(method, path)` tuples from
      `internal/api/CLAUDE.md`'s route table. Any drift fails the test.

11. **Importer round-trip parity.** `pst_integration_test.go` proves
    PST → store survives idempotent re-import. There's no test that
    proves "import mbox → re-export → re-import → identical row
    counts/hashes." This is the canonical regression-catcher for any
    importer change.
    - Add `TestImportMbox_RoundTrip_ExportThenReimport_PreservesRowCount`.

12. **Sync: checkpoint-recovery after partial flush.**
    `TestFullSyncResumeWithCursor` covers cursor recovery. There's no
    test that simulates "process killed between writing checkpoint
    and committing message batch."
    - Add `TestFullSync_KilledMidBatch_RestartedSyncDoesNotDuplicate`:
      use a sync hook that panics after N messages, restart sync,
      assert exact count.

13. **Vector pipeline: `EnsureSeeded` re-seed on resume.**
    Documented in `internal/vector/CLAUDE.md` as critical for the
    crash-window. Need an explicit test.
    - Add `TestBackend_EnsureSeeded_RehydratesAfterCrashedSeed`.

14. **DuckDB: `searchCacheTable` cleanup.** The cache (`duckdb.go:47-57`)
    is keyed on conditions+args. No test for the eviction or
    concurrent-different-query case.
    - Add `TestDuckDBEngine_SearchCache_EvictsBetweenDifferentQueries`.

15. **API: insecure-bind validation.** `cfg.Server.ValidateSecure()`
    is called in `Server.Start()`. No test confirms it actually
    refuses a non-loopback bind without an APIKey.
    - Add `TestServer_Start_RefusesInsecureBindWithoutAPIKey`.

### P2 — nice to have

16. **PII-in-logs prevention.** Add a meta-test that scans
    `slog.Info` call sites for known PII fields (`body`, `subject`,
    `content`, `from_email`). Either via golangci-lint custom rule or
    a `TestNoPIIAtInfoLevel` that reads source files. Aligns with
    global rule.

17. **Snapshot tests for TUI render.** `internal/tui/view_render_test.go`
    has 27 funcs but they assert string substrings, not full output.
    Adding a `-update-golden` flag would let the team diff TUI
    redesigns.

18. **DuckDB: invalid Parquet schema cache.** `cacheSchemaVersion` is
    bumped to invalidate on column changes. No test confirms a stale
    Parquet directory triggers re-build.
    - Add `TestBuildCache_StaleSchemaVersion_TriggersFullRebuild`.

19. **Embed config: fingerprint changes activate `ErrIndexStale`.**
    Implicitly tested via generations test, but no explicit
    "config-changed-mid-run" test.
    - Add `TestEmbedJob_FingerprintMismatch_RefusesActivation`.

20. **`internal/imap`** — currently 11 tests across 4 files; for a
    component that auths to Outlook/Yahoo and pulls real mail, this
    is too thin. At minimum, add a fake IMAP server fixture and
    round-trip the XOAUTH2 handshake.

21. **Search parser fuzz.** `internal/search/parser_test.go` only has
    5 funcs covering Gmail-style operator parsing. Add `FuzzParseQuery`
    (Go 1.18+ native fuzzing).

22. **Store concurrent write.** SQLite uses `MaxOpenConns=4`. No test
    exercises 4 simultaneous writers + 1 reader. A `TestStore_ConcurrentWrites`
    would catch SQL_BUSY handling regressions.

---

## 9. Quick wins — top 10 tests to add tomorrow

These are P0/P1 items ordered by ratio of value-to-effort. Most are <50 LOC.

1. `TestNoSecretsAtInfoLevel` — grep-style meta-test that fails the
   build if `slog.Info`/`Infof` is called with `body`, `subject`,
   or `from_email` as a key. Literal substring match in
   `internal/sync`, `internal/api`, `internal/gmail`,
   `internal/importer`. Most of the work is enumerating the keys.

2. `TestServerRouter_AllExpectedRoutesMounted` — chi.Walk over
   `setupRouter`'s output, assert against `internal/api/CLAUDE.md`'s
   route table. Drift detection. ~30 LOC.

3. `TestExecutor_Execute_FailsClosedOnManifestWriteError` — wrap
   `os.Rename` via a `Manager.fs` seam (currently uses `os` directly;
   small refactor). Asserts the executor halts on filesystem failure.

4. `TestStore_GetDuplicateGroupMessages_NoNPlusOne` — wrap `*sql.DB`
   in a counting `driver.Connector`, count `Query`/`QueryRow` calls,
   assert constant.

5. `TestHandleQuery_BlockingSQLRespectsContextDeadline` — in-process
   engine that blocks `QuerySQL`, exercise the chi gentle timeout.

6. `TestImportPst_ContextCancelledMidImport` — extend the existing
   `TestImportPst_SupportPST_ContextCancelled` (which cancels before
   open) to cancel mid-folder, assert checkpoint persists.

7. `TestTUIModel_LoadAggregates_UsesRemoteEngineWhenConfigured` —
   smallest possible end-to-end of the remote path: `httptest.NewServer`
   returns canned aggregates, model.Update receives them.

8. `TestScheduler_RunSyncRespectsAccountTimeout` — wire a per-account
   timeout, prove a hung callback returns within deadline.

9. `TestEmbedJob_Run_DoesNotActivateWhenConcurrentEnqueueLands` —
   interleave `EnqueueMessages` with the activation gate; uses
   existing `internal/vector/embed/queue_test.go` machinery.

10. `TestStore_EnsureConversation_ReturnsNonZeroID_OnPostgres` (gated
    by `MSGVAULT_TEST_DB`) — pins the Postgres blocker as a failing
    test. Better than a status doc; CI proves whether it's fixed.

---

## 10. Anti-patterns observed

A list of test smells worth fixing or at least watching.

### Misleading filenames
- **`internal/query/text_search_live_test.go`** — "live" here means
  "not soft-deleted," not "tests against a real service." Rename to
  `text_search_livefilter_test.go` or merge into
  `sqlite_search_test.go`.

### Mocked when they shouldn't be
- **`internal/gmail/mock_test.go` is 4 tests of the mock itself.**
  That's fine, but the rest of the project tests sync entirely
  against the mock without ever hitting a fake HTTP layer (e.g.,
  `httptest.NewServer` returning canned Gmail JSON). A bug in the
  Gmail HTTP-client code path — auth header construction, retry
  conditions, JSON decoding — would not be caught. Recommend
  introducing a `gmail/httptest` fake server or an integration test.
- **`internal/api/handlers_test.go:38` — `mockStore`/`mockScheduler`**
  are private fakes maintained per-test-file. The DTO surface
  (`APIMessage`, `AccountStatus`) is shared with the real types via
  alias, but the mock can drift. Consider promoting these to
  `internal/api/apitest` and using a single struct across handler
  tests.

### Always-passes assertions / weak signals
- **`internal/api/handlers_test.go:445` `TestMessageSummaryNilSlices`**
  and adjacent tests assert on the JSON shape, but several only check
  that the field is present, not its value. Tightening `cc`/`bcc`
  ordering and casing would prevent regressions.
- **`internal/tui/view_render_test.go`** — many tests use
  `strings.Contains` against a substring of the rendered output. A
  layout change that reorders, recolors, or renames a column will not
  fail the test even if the user-visible output is wrong. Consider
  either (a) adding 1–2 golden-file tests for canonical screens, or
  (b) asserting full-line equality for the header row.

### Test data that drifts from production reality
- **`internal/testutil/dbtest/dbtest.go:64` `SeedStandardDataSet`**
  inserts hardcoded rows referencing schema details (5 messages, 3
  participants, etc.). When the schema gains required columns, this
  function silently breaks. Worth either:
  - Auditing it on every `internal/store/schema.sql` change, or
  - Replacing with the `storetest.Fixture` builder pattern, which
    delegates to `store.UpsertMessage` and naturally inherits
    column changes.

### Missing-coverage smells
- **`internal/store/export_test.go` has 0 test functions.** The file
  exists but defines only fixture helpers. That's fine, but the
  package's `subset.go` (the SQLite-only bulk exporter) deserves
  direct test coverage somewhere.
- **`internal/sync/fixtures_test.go` and `internal/sync/testenv_test.go`
  have 0 tests** — both are infrastructure files. Move them into a
  `synctest/` subpackage or an internal helper file (no `_test`
  suffix) so the file count better reflects what's actually tested.
- **`internal/query/sqlite_testhelpers_test.go` has 0 tests.** Same
  pattern.

### TODO(verify) — items I couldn't confirm without running code
- The vector "L2 vs cosine" comment in `internal/vector/CLAUDE.md`:
  "with default L2, `1 - dist` is not a true cosine similarity." If
  that's correct, ranking via `1 - dist` is monotonic for L2 only
  within a fixed query — fine for ranking but confusing as a "score."
  A test asserting score ordering matches distance ordering would
  pin this contract.
- `make test` wall time. I did not run it. The PST integration test
  reads a real fixture and is the most likely slow point.
