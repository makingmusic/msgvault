# internal/search — Gmail-syntax query parser

Tiny package: just `parser.go` (~400 LOC) and `parser_test.go`.

## What it does

`Parse(queryStr) *Query` (`parser.go:198`) tokenizes a Gmail-like search
string and returns a structured `Query`. Operator handlers register into a
single `operators` map (`parser.go:113`).

`Query` (`parser.go:12`) carries:

- Free-text terms (`TextTerms`) — bare words and `"quoted phrases"`.
- Address filters: `FromAddrs`, `ToAddrs`, `CcAddrs`, `BccAddrs`.
- `SubjectTerms`, `Labels`.
- Booleans/pointers: `HasAttachment`, `BeforeDate`, `AfterDate`,
  `LargerThan`, `SmallerThan`.
- `AccountIDs` and `HideDeleted` are NOT set by the parser — callers
  attach them after parsing (e.g. `cmd/.../search.go:249` injects scope).

## Supported operators

| Operator                                | Field           | Notes                                                                                         |
| --------------------------------------- | --------------- | --------------------------------------------------------------------------------------------- |
| `from:`, `to:`, `cc:`, `bcc:`           | address slices  | `normalizeAddr` lowercases and prefixes `@` when value looks like a domain (`looksLikeDomain` checks against `knownGTLDs` + 2-letter ccTLDs at `parser.go:75-110`). |
| `subject:`                              | SubjectTerms    | substring; no normalization                                                                   |
| `label:`, `l:`                          | Labels          | trimmed; empty values dropped                                                                 |
| `has:attachment` / `has:attachments`    | HasAttachment   | only "attachment[s]" recognized                                                               |
| `before:`, `after:`                     | dates           | `YYYY-MM-DD`, `YYYY/MM/DD`, `MM/DD/YYYY`, `DD/MM/YYYY` (`parseDate`)                          |
| `older_than:`, `newer_than:`            | dates           | relative `\d+[dwmy]` against `Parser.Now()`                                                   |
| `larger:`, `smaller:`                   | bytes           | `K`/`KB`/`M`/`MB`/`G`/`GB` suffixes via `parseSize`                                           |

Unknown operators (`foo:bar`) fall through to `TextTerms` so users don't
silently lose their query — see `parser.go:218`.

## Tokenizer notes

`tokenize` (`parser.go:250`) is hand-rolled to handle two distinct quoted
forms:

- Standalone: `"foo bar"` → one quoted phrase.
- Operator-attached: `subject:"foo bar"` → one token kept whole, value
  unquoted by `unquote` after the operator handler matches.

Single quotes also start quoting (so `'word'` works) but the closing
quote must match the opening style.

An unterminated quote emits whatever was accumulated as a plain token
rather than dropping it (`parser.go:307`).

## Consumers

- CLI: `cmd/msgvault/cmd/search.go` calls `search.Parse` then dispatches
  to `runLocalSearch` / `runHybridSearch`.
- Engines: `query.Engine.Search` and `Engine.SearchFast` accept
  `*search.Query` and translate to SQL.
- Vector hybrid: `vector/hybrid/filter.go:BuildFilter` resolves the same
  `*search.Query` into a `vector.Filter` with participant/label IDs
  pre-resolved at the Go layer.

## Testing

`Parser.Now` is injectable so `older_than:7d` parses are deterministic
in tests (`parser_test.go` does this).

## When editing

- Adding an operator: register a closure in `operators` and document it
  in the search command's `Long` string.
- Adding a TLD: extend `knownGTLDs`. Don't expand to a full Public Suffix
  List — the goal is "looks like a domain" heuristics for the
  bare-domain `from:example.com` shorthand, not strict validation.
