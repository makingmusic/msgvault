# internal/testutil

Shared test helpers. Anything reused across `internal/*_test.go` lands
here. **Test authors: search this package before writing a new
helper.**

## File layout

```
internal/testutil/
  testutil.go         # package doc
  assert.go           # generic assertions, MustNoErr
  builders.go         # query.MessageSummary / MessageDetail builders
  fs_helpers.go       # WriteFile/ReadFile with traversal guards
  store_helpers.go    # NewTestStore (SQLite + PG)
  archive_helpers.go  # CreateTarGz / CreateZip / CreateTempZip
  security_data.go    # PathTraversalCases vectors
  encoding.go         # EncodedSamples — Win1252/Latin1/Asian byte sequences
  encoding_test.go
  testutil_test.go

  dbtest/             # in-memory SQLite + builders. NO query/store imports.
  storetest/          # Fixture for store-layer tests (uses real Store)
  email/              # raw RFC 2822 builder (MessageBuilder, MakeRaw)
  ptr/                # Bool/Int64/String/Time pointer helpers
  tbmock/             # MockTB for testing test helpers themselves
```

## Helper catalog

### Assertions (`assert.go`)

- `MustNoErr(t, err, msg)` — fatal on error; for setup steps.
- `AssertEqual[T comparable](t, got, want)` — generic equality.
- `AssertEqualSlices[T comparable](t, got, want...)` — element-wise.
- `AssertStrings(t, got, want...)` — like above, with `%q`
  formatting for strings.
- `AssertContainsAll(t, got, subs)` — every substring must appear.
- `AssertStringSet(t, got, want...)` — unordered multiset
  comparison.
- `AssertValidUTF8(t, s)` — `utf8.ValidString` check.
- `MakeSet[T](items...) map[T]bool` — common pattern for selection
  sets.

### Filesystem (`fs_helpers.go`)

- `WriteFile(t, dir, relName, content)` — writes under `dir`, fails
  the test if `relName` is absolute, rooted, or escapes via `..`.
  Use this not raw `os.WriteFile` for test files in `t.TempDir()`.
- `ReadFile(t, path)`, `AssertFileContent(t, path, expected)`,
  `MustExist(t, path)`, `MustNotExist(t, path)`,
  `WriteAndVerifyFile(t, dir, rel, content)`.

### Store / DB

Two layers depending on what you're testing:

- **`testutil.NewTestStore(t)` (`store_helpers.go`)** — opens a real
  `*store.Store` against either a SQLite tempfile or, when
  `MSGVAULT_TEST_DB=postgres://...` is set, a per-test PostgreSQL
  schema (`msgvault_test_<random8>`, dropped on `t.Cleanup`). Uses
  the production `store.Open` path including `InitSchema()`. Use
  this when you need real Store methods.
- **`testutil/dbtest`** — in-memory SQLite with the production
  schema loaded from `schema.sql`, but **no `internal/store`
  import** (so packages that the store imports can use it without
  cycles). `NewTestDB(t, schemaPath)` plus `SeedStandardDataSet`
  (alice/bob/carol, 5 messages, 3 labels, 3 attachments). Builders:
  `AddSource`, `AddConversation`, `AddLabel`, `AddMessage`,
  `AddParticipant`, `AddMessageLabel`. `EnableFTS()` creates and
  populates `messages_fts`.
- **`testutil/storetest`** — `Fixture` wrapping `NewTestStore` plus
  one source (`test@example.com`) and one default conversation,
  with helper methods (`CreateMessage`, `CreateMessages`,
  `EnsureLabels`, `EnsureParticipant`, `StartSync`). Plus per-row
  assertions (`AssertLabelCount`, `AssertMessageDeleted`,
  `AssertActiveSync`, `AssertNoActiveSync`, ...). Plus a
  `MessageBuilder` distinct from `dbtest`'s — this one builds
  `*store.Message` and inserts via `Store.UpsertMessage`.

Rule of thumb: if your test imports `internal/store`, use
`storetest.New(t)`. If you're inside or below the store layer, use
`dbtest.NewTestDB`.

### Builders (`builders.go`)

- `NewMessageSummary(id)` → `MessageSummaryBuilder` with `WithSubject`,
  `WithFromEmail`, `WithFromName`, `WithSentAt`, `WithSize`,
  `WithLabels`, `WithAttachmentCount`, `WithSnippet`,
  `WithDeletedAt`/`WithDeleted`, etc. → `Build()` or `BuildPtr()`.
- `NewMessageDetail(id)` → `MessageDetailBuilder` with `WithFrom`,
  `WithFromAddress` (convenience for single sender), `WithTo`,
  `WithCc`, `WithBcc`, `WithBodyText`, `WithBodyHTML`,
  `WithAttachments`, `WithLabels`, `WithSize`, etc.

These build `query.MessageSummary` / `query.MessageDetail` —
appropriate for TUI/query-layer tests, NOT store-layer
(`storetest.MessageBuilder` is for that).

### Email / MIME (`email/`)

- `email.NewMessage()` — fluent MIME builder. Sensible defaults
  (`sender@example.com`, `recipient@example.com`,
  `Mon, 01 Jan 2024 12:00:00 +0000`, `Test Message`,
  `boundary123`).
- Methods: `From`, `To`, `Cc`, `Bcc`, `Subject`, `NoSubject`,
  `Date`, `ContentType`, `Body`, `Header(k,v)` (last-write-wins),
  `HeaderAppend(k,v)` (allow duplicates), `Boundary`,
  `WithAttachment(filename, contentType, data)`, `CRLF()`,
  `Bytes()`.
- `email.MakeRaw(Options)` — simpler one-shot for RFC-2822 raws
  with `\r\n`.
- Default line endings are `\n`; switch with `.CRLF()` for
  parser tests that require RFC compliance.

### Encoded byte samples (`encoding.go`)

`EncodedSamples()` returns a fresh `EncodedSamplesT` per call (deep
copies internal slices) with named byte sequences:

- Win1252: `Win1252_SmartQuoteRight`, `Win1252_EnDash`,
  `Win1252_EmDash`, `Win1252_DoubleQuotes`, `Win1252_Trademark`,
  `Win1252_Bullet`, `Win1252_Euro`.
- Latin-1: `Latin1_OAcute`, `Latin1_CCedilla`, `Latin1_UUmlaut`,
  `Latin1_NTilde`, `Latin1_Registered`, `Latin1_Degree`.
- Short Asian samples: `ShiftJIS_Konnichiwa`, `GBK_Nihao`,
  `Big5_Nihao`, `EUCKR_Annyeong`.
- Long Asian samples (long enough for `chardet` to identify
  confidently): `ShiftJIS_Long(_UTF8)`, `GBK_Long(_UTF8)`,
  `Big5_Long(_UTF8)`, `EUCKR_Long(_UTF8)`.

When adding a field, also extend the explicit `cloneBytes` block in
`EncodedSamples()` — this is intentional; reflection-based copying
was rejected (see maintainer note `encoding.go:107`).

### Path-traversal vectors (`security_data.go`)

`PathTraversalCases()` returns a fresh slice of OS-appropriate path
traversal attack vectors. On Windows it adds `C:\Windows\system32`,
UNC `\\server\share\file.txt`, and drive-relative paths (`C:foo`,
`D:subdir\file.txt`). Use this to drive table tests of any path
sanitizer.

### Archives (`archive_helpers.go`)

- `CreateTarGz(t, path, []ArchiveEntry)` — full control (typeflag,
  linkname, mode).
- `CreateZip(t, path, []ArchiveEntry)`.
- `CreateTempZip(t, map[string]string)` — convenience: returns a
  zip in `t.TempDir()` with deterministic ordering.

### Pointers (`ptr/`)

`ptr.Bool(v)`, `ptr.Int64(v)`, `ptr.String(v)`, `ptr.Time(v)`,
`ptr.Date(year, month, day)` (returns a UTC midnight).

`dbtest.StrPtr` exists for circular-import reasons (dbtest can't
import ptr without pulling testing in). Same idea.

### Mock testing.TB (`tbmock/`)

`tbmock.NewMockTB(t)` returns a `*MockTB` that intercepts `Fatal*`,
`FailNow`, and `Skip*` calls by panicking with `FatalSentinel{Msg}`
instead of calling `runtime.Goexit`. `tbmock.ExpectFatal(mtb, fn)`
runs `fn` and recovers the sentinel. **Use this only when testing
test helpers themselves** (e.g., to verify that
`MustNoErr` actually fails on non-nil error).

## When editing

- New helpers: prefer adding to the existing focused file rather than
  starting a new one. Subdirectories are for clear sub-domains
  (`email/`, `ptr/`, `dbtest/`, `storetest/`, `tbmock/`).
- The `dbtest` and `storetest` split exists to break import cycles.
  Don't merge them.
- `WriteFile` (test) is intentionally stricter than the production
  `fileutil.SecureWriteFile`: tests should never need absolute or
  rooted paths inside `t.TempDir()`.
- The path-traversal cases in `PathTraversalCases()` are the
  canonical set. New attack vectors → add here, not inline in a
  test file.
