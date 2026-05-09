# TUI subsystem

`internal/tui/` implements the interactive terminal interface launched by `msgvault tui`. It is built on Bubble Tea (Elm-style Model/Update/View) with lipgloss styling. The TUI is read-mostly: it queries an analytics-shaped data engine for fast aggregate browsing, drill-down navigation, and full-text search, plus a small write surface for staging deletions and exporting attachments.

## Architecture

### Model / Update / View / Cmd

Bubble Tea calls `Model.Init()` once for startup commands, then loops `View()` (renders the current model) and `Update(msg)` (pure transition returning a new model and an optional command). Async work is a `tea.Cmd` — a function returning a `tea.Msg` that flows back into `Update`.

In this TUI:

- `Model` is defined at `internal/tui/model.go:102`. It embeds `viewState` (per-screen data) and adds top-level concerns: backend engines, navigation stack, selection, modals, search, terminal dimensions, and request IDs for stale-response filtering.
- `Init` (`internal/tui/model.go:273`) batches `loadData`, `loadStats`, `loadAccounts`, `checkForUpdate`, and `spinnerTick`.
- `Update` (`internal/tui/model.go:821`) is a single type switch over message kinds: keys, window resizes, the seven email-mode `*LoadedMsg` types, the five Texts-mode messages, and lifecycle messages (`flashClearMsg`, `spinnerTickMsg`, `searchDebounceMsg`, `exportResultMsg`, `updateCheckMsg`).
- `View` (`internal/tui/model.go:1500`) returns `transitionBuffer` if non-empty, otherwise `renderView()` (`internal/tui/model.go:1521`) dispatches by `mode` (Email/Texts) and `level`. Each render is `header / body / footer`.

### Stale-response filtering

Every async load snapshots the matching `*RequestID` (`aggregateRequestID`, `loadRequestID`, `detailRequestID`, `searchRequestID`) into the closure. Each handler (`handleDataLoaded`, `handleMessagesLoaded`, etc.) compares the incoming message's request ID against the current one and drops mismatches. Tests in `nav_test.go` (`TestStaleAsyncResponsesIgnored`, `TestStaleDetailResponsesIgnored`) exercise this. The pattern matters because the user can switch views while a slow query is in flight; without it, late results would overwrite the new view.

### Transition flashing prevention

When the user enters a new level, the handler captures the current screen into `transitionBuffer = m.renderView()` before mutating state and starting the load (e.g. `enterDrillDown` at `internal/tui/keys.go:1186`). `View()` short-circuits to that string until the matching `handle*Loaded` clears the buffer. This prevents the brief flash of an empty table while the next query runs.

## State machine

### Email mode levels (`internal/tui/navigation.go:13`)

- `levelAggregates` — top-level table grouped by `viewType` (Senders, Recipients, Domains, Labels, Time, etc.). Loaded by `loadData()` calling `engine.Aggregate(...)`.
- `levelDrillDown` — sub-aggregate after picking a row at `levelAggregates`. Loaded by `engine.SubAggregate(drillFilter, viewType, opts)`. Selecting a sub-aggregate row drills further; `drillFilter` accumulates additional filter fields. The current dimension is skipped in `nextSubGroupView` so the user cannot sub-group by the same axis they drilled from.
- `levelMessageList` — message rows for the current `drillFilter` (or `allMessages = true` for the entire archive). Supports infinite scroll: `maybeLoadMoreMessages` (`keys.go:602`) and `maybeLoadMoreSearchResults` (`keys.go:567`) trigger paginated loads at 20 rows from the end. Page size is `messageListPageSize = 500` (`model.go:513`); search uses `searchPageSize = 100` (`model.go:510`).
- `levelMessageDetail` — single-message render with scrollable body. `←/→` (or `h/l`) iterate within `m.messages` (or `m.threadMessages` if entered from a thread, tracked by `detailFromThread`). Find-in-page (`/`, `n`, `N`) over the wrapped body lines, recomputed on resize (`updateDetailLineCount` at `model.go:1402`, `findDetailMatches` at `model.go:1371`).
- `levelThreadView` — all messages in a conversation; entered by `T`. Loaded with a hard cap (`defaultThreadMessageLimit = 1000`, `model.go:40`); the engine returns `limit + 1` rows and the handler sets `threadTruncated` if truncation occurred.

### Modals (`internal/tui/model.go:67`)

`modalDeleteConfirm`, `modalDeleteResult`, `modalAccountSelector`, `modalFilterToggle`, `modalExportAttachments`, `modalExportResult`, `modalQuitConfirm`, `modalHelp`, `modalError`. Modal handling takes precedence over inline search and per-level keys (`model.go:1314`).

### Texts mode levels (`internal/tui/text_state.go:14`)

`textLevelConversations`, `textLevelAggregate`, `textLevelDrillConversations`, `textLevelTimeline`. State is segregated in `Model.textState` so switching modes does not clobber email state; `m` toggles via `handleGlobalKeys` (`keys.go:97`). Texts mode is read-only — the deletion/selection keys are filtered out at `text_keys.go:30`. The timeline is a chat-style word-wrapped renderer (`text_view.go:495`) where the cursor advances by message but scroll is in screen lines.

### Transitions

- `enter` at aggregates → `enterDrillDown` (`keys.go:1186`): pushes breadcrumb, builds/extends `drillFilter`, sets `level = levelMessageList`, kicks `loadMessages` (or `loadSearch` if a search query is active).
- `tab`/`g` at message list with a drill filter → `levelDrillDown` with the next sub-aggregate dimension (`keys.go:411`).
- `T` from list/detail → `levelThreadView`.
- `enter` at message list → `levelMessageDetail` (cursor preserved on goBack).
- `Esc`/`backspace` everywhere → `goBack()` pops the breadcrumb and restores the previous `viewState`.

## Data flow

The TUI talks to data through one interface: `query.Engine`. Concrete implementations:

1. **`query.NewDuckDBEngine`** — DuckDB over Parquet shards in `~/.msgvault/analytics/`, with a sqlite_scanner fallback for queries that need raw SQLite tables (e.g. message body fetch, FTS5 deep search). This is the default whenever `query.HasCompleteParquetData(analyticsDir)` returns true (`cmd/msgvault/cmd/tui.go:124`).
2. **`query.NewSQLiteEngine`** — direct `*sql.DB` queries against the SQLite store. Used as a fallback when no Parquet cache exists or when `--force-sql` is passed; expected to be slow on large archives.
3. **`remote.NewEngine`** — HTTP client for remote msgvault servers (`cmd/msgvault/cmd/tui.go:73`). When `cfg.Remote.URL` is set and `--local` is not passed, the TUI runs in remote mode: `Options.IsRemote = true` makes `d`/`D` and `e` show "not available in remote mode" flashes (`keys.go:213`, `keys.go:392`, `keys.go:805`).

If the engine implements `query.TextEngine` it is also used for Texts mode (`tui.go:151`).

### Why DuckDB + Parquet for analytics

The TUI is interactive; aggregate queries (group-by sender, group-by domain, time histogram) need to feel snappy at 100k–1M+ rows. The CLAUDE.md project notes report a roughly 3000x speedup over SQLite JOINs for these aggregations. The Parquet shards are denormalized and partitioned by `year=`, with array columns for labels and recipients, which lets DuckDB scan only the relevant year shards and avoid joins entirely. The SQLite store remains the system of record for raw bodies, FTS5, and write-side state — the TUI uses sqlite_scanner (or a direct SQLite fallback) to read those tables when needed (e.g. the `messageDetail` body, deep-search FTS, attachment streams).

The fast/deep search distinction (`searchModeKind` at `model.go:83`) reflects this split: Fast hits Parquet metadata only (subject, sender, recipient — `engine.SearchFastWithStats`); Deep uses SQLite FTS5 over body text (`engine.Search`). Deep search has a longer debounce (500ms vs 100ms) since it is heavier (`model.go:495`).

### Auto-build of the Parquet cache on TUI launch

`cmd/msgvault/cmd/tui.go:109` calls `cacheNeedsBuild(dbPath, analyticsDir)` (`tui.go:197`) before opening any engine. The function inspects `~/.msgvault/analytics/_last_sync.json` and the SQLite store, collecting all staleness signals before returning so that mixed sync states report cleanly:

- New messages since the last build (`MAX(messages.id) > LastMessageID`).
- Deletions since the last build (`deleted_from_source_at >= LastSyncAt`) — forces a full rebuild.
- Dedup-hidden rows (`deleted_at IS NOT NULL` AND not source-deleted) — forces a full rebuild because the export query filters them out.
- Updated messages from `sync_runs` whose ID is greater than `LastCompletedSyncRunID` — forces a full rebuild.
- Empty cache directory or missing required Parquet tables — forces a full rebuild.

If `NeedsBuild` is true, the launcher prints "Building analytics cache (<reason>)..." and runs `buildCache(dbPath, analyticsDir, FullRebuild)`. Failures fall back to SQLite with a warning. `--no-cache-build` skips the check entirely; `--force-sql` skips both the check and the DuckDB engine.

Edge cases:

- `MAX(id) == 0 && state.LastMessageID == 0` — empty archive: returns `cacheStaleness{}` (no build needed, no warning).
- A missing `_last_sync.json` triggers a full rebuild even if Parquet files exist (treated as unrecoverable state mismatch).
- The `deleted_from_source_at IS NULL` clause in the dedup-hidden count keeps it disjoint from the deletion count so a row that is both deleted-at-source and dedup-hidden after the last sync is reported once, as a deletion.

The TUI also kicks `store.BackfillFTS` in a goroutine if `NeedsFTSBackfill()` returns true (`tui.go:100`), so deep search becomes available once that finishes.

## Selection and deletion staging

Selection is two disjoint sets in `selectionState` (`model.go:89`):

- `aggregateKeys map[string]bool` — keys (sender emails, domains, labels, time periods) scoped by `aggregateViewType`. Switching the view type clears them.
- `messageIDs map[int64]bool` — selected `message.id` values from the message list.

`Space` toggles, `S` selects visible (with scroll-aware bounds), `x` clears. `d`/`D` invoke `stageForDeletion()` (`model.go:1416`) which delegates to `ActionController.StageForDeletion` (`actions.go:65`):

1. `resolveGmailIDs` walks the aggregate selections, building a `MessageFilter` per key (cloning the drill filter so map mutations don't leak across keys), and calls `engine.GetGmailIDsByFilter` for each. It unions the result with selected `messageIDs` (looking up `SourceMessageID` in the loaded messages).
2. `buildManifestDescription` produces a short, human-readable label.
3. `applyManifestFilters` populates `Manifest.Filters` (Account, Senders, Recipients, SenderDomains, Labels) from the selection.
4. The Manifest is shown in `modalDeleteConfirm`. On `y`, `confirmDeletion` (`model.go:1443`) calls `ActionController.SaveManifest`, which lazily constructs `deletion.NewManager(<dataDir>/deletions)` and writes the manifest as JSON.

The TUI never executes deletions itself — the user runs `msgvault delete-staged` afterwards (instructions are in the result modal). Remote mode short-circuits with a flash before any of this runs (`keys.go:213`). See `docs/subsystems/deletion.md` (TODO(verify): exact filename) for the full deletion manifest workflow.

## Search

Two mechanisms:

1. **Inline search bar** (`/`) — an embedded `textinput.Model` on the info line. `searchDebounceMsg` fires after a delay (`inlineSearchDebounceDelay = 100ms` for Fast, `deepSearchDebounceDelay = 500ms` for Deep) carrying a `debounceID` checked against `inlineSearchDebounce`; later keystrokes invalidate earlier timers. `Enter` commits, `Tab` toggles Fast/Deep (only at message-list level), `Esc` cancels and restores the pre-search snapshot (`preSearchMessages/Cursor/ScrollOffset/ContextStats`) — see `restorePreSearchSnapshot` at `keys.go:1354`. The snapshot also captures inherited search context from a drill-down so the first local `/` inside a drilled view can still rewind cleanly.
2. **Find-in-page in detail view** (`/` while at `levelMessageDetail`). Implemented as a separate `detailSearchInput` with `findDetailMatches` walking `buildDetailLines()` (the same wrapped lines the renderer uses). Matches are line indices; `n`/`N` scroll to each (centered via `scrollToDetailMatch` at `model.go:1386`). Match indices are recomputed on `WindowSizeMsg` because wrapping changes line counts (`model.go:967`).

`searchTotalCount = -1` is a sentinel meaning "unknown" (used by deep FTS where no separate count query runs); pagination logic respects it (`keys.go:585`).

There is no vector / semantic search in the TUI today.

## Tests

What is covered:

- `model_test.go` — `New()` defaults, option overrides, `Init` command shape, basic key dispatch.
- `nav_test.go` — stale async response filtering, window-size clamping, default loading state, page sizing, list navigation primitives, thread page-up/down, `visibleRows()`.
- `nav_drill_test.go` — sub-grouping, sub-aggregate drill-down, stats updates and restoration, breadcrumb push, selection clear vs preserve on top-level vs sub-aggregate drill, drill-filter preservation through detail navigation.
- `nav_detail_test.go` — detail line count reset/recompute on resize, scroll clamping, prev/next at boundaries, h/l aliases, empty-list and out-of-bounds-index handling, cursor preservation on `goBack`.
- `nav_modal_test.go` — quit confirm, account selector, filter toggle (in aggregate, message-list, drill-down).
- `nav_view_test.go` — view rendering across many states (large; functions as quasi-golden).
- `selection_test.go` — toggle, select-all-visible, scroll-aware select, clear, view-switch clearing, staging shape, "auto-select current row when none selected" behavior.
- `search_test.go` — search activation, debounce, pagination, snapshot/restore, fast/deep mode toggle, drill+search interaction.
- `view_render_test.go` — frame-level rendering invariants (footer position, header layout, modal overlay).
- `view_test.go` — `applyHighlight` (rune-aware case-insensitive overlap merging).
- `actions_test.go` — `ActionController` deletion staging across selection shapes, view types, account filters, drill filters, and attachment export (success / partial / no-selection / nil-detail / write-error).
- `setup_test.go` — shared `MockEngine` builders.

What is not (or is thinly) covered:

- Texts mode end-to-end interactions and timeline rendering — there is no `text_*_test.go` file. TODO(verify): treat as a coverage gap.
- Remote-mode behavior beyond "deletion disabled" flash.
- The auto-cache-build path in `cmd/msgvault/cmd/tui.go` (the staleness function is non-trivial; tests for it would live under `cmd/...`).
- `store.BackfillFTS` background behavior triggered by the launcher.
- Spinner animation timing, flash-message expiry, and update-check command shape are not exercised end-to-end.

## Known issues / smells

- `view.go` is ~43k bytes and `model.go` is ~48k bytes. They are large for single files. Splitting render branches per level (e.g. `view_aggregate.go`, `view_detail.go`) and load commands per concern would improve navigability. `keys.go` (~40k) is similar.
- Test files reference `m.helpScroll`, `m.helpMaxVisible()`, `rawHelpLines`, `m.exportSelection`, `m.exportCursor` — modal state is intermingled with the main `Model` rather than encapsulated in a dedicated struct, which makes unit tests heavier than necessary.
- `Model` carries both Email-mode `viewState` (embedded) and `textState`; there is no shared abstraction over the two modes' navigation stacks despite their similar shape (push/pop snapshots). TODO(verify): worth a refactor pass if a third mode is ever added.
- `wrapText` (`format.go:189`) and `buildDetailLines` (in `view.go`) recompute on every render; on extremely large message bodies the cost shows. The detail line count is cached but the wrapped lines are not.
- The TUI uses a file-only logger while running because Bubble Tea owns the alt-screen (`cmd/msgvault/cmd/tui.go:170`). Restoring `slog.Default` happens via `defer`, so a panic that escapes `p.Run()` would leave the file-only logger installed for the rest of the process. TODO(verify): in practice the process exits, so this is benign.
- Pagination signals are a mix of `searchTotalCount`, `msgListComplete`, modulo-page-size heuristics, and `contextStats.MessageCount` checks (`keys.go:602`). Each one is correct in isolation but the combination is harder to reason about than necessary.
- The deletion modal text mentions `MSGVAULT_ENABLE_REMOTE_DELETE=1` (`model.go:1459`); the TUI also has its own `IsRemote` flag with different semantics. The naming overlap could confuse users. TODO(verify): cross-reference with the deletion subsystem doc once written.
