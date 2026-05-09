# internal/tui

Bubble Tea + lipgloss terminal UI for browsing the local archive (and remote msgvault servers). Launched from `cmd/msgvault/cmd/tui.go`.

## Bubble Tea pattern

`Model.Init()` returns initial commands; `Update(msg) (Model, Cmd)` is a pure transition; `View() string` renders. Async work runs as `tea.Cmd` returning a `tea.Msg` consumed by `Update`. This TUI follows that contract: every async data load is a `tea.Cmd` returning a typed `*LoadedMsg` (e.g. `dataLoadedMsg`, `messagesLoadedMsg`, `searchResultsMsg`); `Update` dispatches on type in `model.go:821`.

## The Model

`Model` (`model.go:102`) embeds `viewState` (`navigation.go:21`) for per-screen state and adds top-level fields:

- `mode` (`tuiMode`): `modeEmail` or `modeTexts` — toggled with `m`. Each mode uses disjoint state.
- `engine query.Engine`: data backend (DuckDB+Parquet, SQLite, or remote HTTP).
- `textEngine query.TextEngine`: optional text-message backend; gates `m`.
- `breadcrumbs []navigationSnapshot`: navigation stack of `viewState` snapshots.
- `selection selectionState`: aggregate keys + message IDs for batch ops.
- `modal modalType`, `modalCursor`, `pendingManifest`: modal dialog state.
- `*RequestID uint64` (aggregate/load/detail/search): increment-then-compare for stale-response filtering — every async cmd snapshots its request ID and the handler ignores mismatches (`model.go:984`).
- Search: `searchInput textinput.Model`, `searchMode` (Fast/Deep), `searchOffset/searchTotalCount`, `inlineSearchActive/inlineSearchDebounce`, plus a `preSearch*` snapshot so `Esc` restores the unsearched list without a re-query (`keys.go:1352`).
- `transitionBuffer string`: cached frame returned from `View()` during async transitions to prevent flashing (`model.go:1511`).
- `flashMessage`, `spinnerFrame`, `loading`: ephemeral UI.
- `isRemote bool`: disables deletion/export when set.

`viewState` carries `level`, `viewType`, sort fields, cursor, scroll, current `rows`/`messages`/`messageDetail`, drill filter, search filter, detail-search state, thread state.

## View levels (Email mode)

`viewLevel` (`navigation.go:13`):

- `levelAggregates` — top-level grouped table.
- `levelDrillDown` — sub-grouping after selecting an aggregate (e.g. Senders → Domains within `alice@example.com`).
- `levelMessageList` — message list filtered by drill or `a` (all messages).
- `levelMessageDetail` — single-message view; `←/→` navigate within list/thread.
- `levelThreadView` — all messages in one conversation (`T` from list/detail).

`query.ViewType` (`query.ViewSenders`, `ViewSenderNames`, `ViewRecipients`, `ViewRecipientNames`, `ViewDomains`, `ViewLabels`, `ViewTime`) selects what aggregates over (`internal/query/models.go:91`). `t` cycles the time granularity in the Time view.

## Texts mode

Toggled with `m` (`keys.go:97`). State lives in `textState` (`text_state.go:24`); levels are `textLevelConversations`, `textLevelAggregate`, `textLevelDrillConversations`, `textLevelTimeline`. The timeline view (`text_view.go:495`) is a chat-style word-wrapped renderer where the cursor moves between messages but the scroll offset is in screen lines. Selection/deletion keys are no-ops here (`text_keys.go:30`).

## Navigation stack

`pushBreadcrumb()` (`navigation.go:199`) snapshots the embedded `viewState` before drill-downs, sub-aggregate switches (`tab`), thread entry (`T`), or detail entry (`enter`). `goBack()` (`navigation.go:157`) pops it and restores. Special case: returning from `levelMessageDetail` keeps the new cursor if the user navigated `←/→` between messages.

## Search and selection

- `/` activates the inline search bar; debounced (Fast 100ms / Deep 500ms) live updates fire `searchDebounceMsg`. `Tab` toggles Fast (Parquet metadata) vs Deep (SQLite FTS5 body) — only meaningful at message-list level (`keys.go:24`). `Enter` commits, `Esc` cancels and restores the pre-search snapshot (`keys.go:1326`).
- In message detail, `/` opens find-in-page; `n`/`N` jump matches.
- `Space` toggles the cursor row's selection; `S` selects all visible; `x` clears. Aggregate selections are scoped by `viewType` so switching views resets them.
- `d`/`D` invokes `stageForDeletion()` (`model.go:1416`) → `ActionController.StageForDeletion` (`actions.go:65`) which resolves selections + drill filter into Gmail IDs, builds a `deletion.Manifest`, and shows a confirmation modal. Confirming saves the manifest under `<dataDir>/deletions/`. Disabled in remote mode.

## Local vs remote backend

`cmd/msgvault/cmd/tui.go:62` chooses the engine: when `[remote].url` is configured and `--local` is not passed, it uses `remote.NewEngine` (HTTP) and sets `Options.IsRemote = true`; deletion and export then short-circuit with a flash (`keys.go:213`, `keys.go:805`). Otherwise it opens SQLite, runs migrations, conditionally rebuilds the Parquet cache via `cacheNeedsBuild`/`buildCache`, and prefers `query.NewDuckDBEngine` over `query.NewSQLiteEngine` based on `query.HasCompleteParquetData`. `--force-sql` skips Parquet entirely. The TUI always installs a file-only logger while running so slog writes do not corrupt the alt-screen render (`tui.go:170`).

## Keybindings (cite `keys.go`)

Top-level dispatch is `handleKeyPress` (`model.go:1307`): modal first, then inline search, then per-level handler. Global keys (`q`/`?`/`m`/`Ctrl+C`) flow through `handleGlobalKeys` (`keys.go:86`). Aggregate keys: `keys.go:118`. Message-list keys: `keys.go:314` (includes infinite-scroll pagination via `maybeLoadMoreSearchResults`/`maybeLoadMoreMessages`). Detail keys: `keys.go:651`. Thread keys: `keys.go:826`. Modal keys: `keys.go:903`. The user-facing list lives in `rawHelpLines` (`view.go:1186`).

## Adding things

- **New view-mode (aggregate)**: add a `query.ViewType` constant in `internal/query/models.go` (and bump `ViewTypeCount`), implement `Aggregate`/`SubAggregate` for it across all engines, extend `setDrillFilterForView` (`keys.go:1147`), `drillFilterKey` (`model.go:728`), `nextSubGroupView` (`keys.go:292`), `buildMessageFilter` (`model.go:622`), and the breadcrumb/abbrev helpers in `view.go`.
- **New navigation level**: add a `viewLevel` constant in `navigation.go:13`, add a render branch in `renderView` (`model.go:1521`), add a key handler, and ensure `goBack()` restores correctly. Snapshot the level via `pushBreadcrumb()` before transitioning.
- **New keybinding**: add a case to the appropriate `handle*Keys` switch. Update `rawHelpLines` (`view.go:1186`) and the long-help in `cmd/msgvault/cmd/tui.go:36`.
- **New modal**: add a `modalType` constant (`model.go:67`), a `render*Modal` in `view.go`, a `handle*Keys` (`keys.go:903`), and an `overlayModal` call site.

When changing async loads, update the matching `requestID` so stale responses are dropped, and clear `transitionBuffer` in the handler (see `handleDataLoaded` `model.go:982`).

## Tests to update

`model_test.go` (Model construction + dispatch), `nav_test.go` (stale-async, navigation primitives), `nav_drill_test.go` (drill/sub-aggregate, breadcrumbs, context stats), `nav_detail_test.go` (detail scroll/resize/prev-next), `nav_modal_test.go` (modals), `nav_view_test.go` (view rendering branches), `selection_test.go` (selection + staging), `search_test.go` (search + debounce + pagination), `view_render_test.go` (full-frame golden-style renders), `view_test.go` (highlight), `actions_test.go` (`ActionController`), `setup_test.go` (shared mock-engine helpers).
