# internal/mcp

Model Context Protocol server backed by `mark3labs/mcp-go`. Drives the
`msgvault mcp` CLI command (cmd/msgvault/cmd/mcp.go) and is also reusable
from a future daemon path. No HTTP routing of its own — transport is
either stdio (`server.NewStdioServer`) or StreamableHTTP
(`server.NewStreamableHTTPServer`).

## Composition

- `server.go` — tool registration, transport setup. `ServeOptions`
  (server.go:68), `Serve` (stdio convenience, server.go:130),
  `ServeWithOptions` (stdio, server.go:140), `ServeHTTPWithOptions`
  (StreamableHTTP, server.go:156). Tool factories: one per tool
  (`searchMessagesTool`, `getMessageTool`, …).
- `handlers.go` — the `handlers` struct (handlers.go:28) holds the engine
  + optional vector wiring. One method per tool. Helpers:
  `getAccountID`, `getIDArg`, `getDateArg`, `limitArg`, `jsonResult`,
  `translateVectorErr` (handlers.go:46).

## mark3labs/mcp-go pattern

`newMCPServer` (server.go:90) constructs `server.NewMCPServer("msgvault",
"1.0.0", server.WithToolCapabilities(false))`, then registers each tool
with `s.AddTool(toolDef, handler)`. Tool definitions use the package's
fluent builder — `mcp.NewTool(name, mcp.WithDescription(...),
mcp.WithReadOnlyHintAnnotation(true), mcp.WithString/Number/Boolean(...,
mcp.Required(), mcp.Description(...), mcp.Enum(...))`. Common option
helpers (`withLimit`, `withOffset`, `withAfter`, `withBefore`,
`withAccount`, server.go:34-62) keep argument schemas consistent across
tools.

Handlers receive `(ctx, mcp.CallToolRequest)` and return
`(*mcp.CallToolResult, error)`. By convention, all errors are reported via
`mcp.NewToolResultError(...)` returning a tool error result with
`error == nil` so the protocol surfaces the message to the client. Only
infrastructure failures (rare) return a non-nil Go error.

JSON payloads are encoded via `jsonResult` (handlers.go:836) → wraps
`mcp.NewToolResultText`.

## Tools registered

All defined in server.go and bound in `newMCPServer` (server.go:107):

| Constant                  | Name                     | Handler                | Notes |
| ------------------------- | ------------------------ | ---------------------- | ----- |
| `ToolSearchMessages`      | `search_messages`        | handlers.go:151        | Gmail-style query. `mode=fts` (default) / `vector` / `hybrid`. Vector/hybrid only when `HybridEngine != nil`; tool description varies on `vectorAvailable` (server.go:183). `offset` is FTS-only; vector/hybrid reject `offset>0`. |
| `ToolGetMessage`          | `get_message`            | handlers.go:527        | Full body + attachments by id. |
| `ToolGetAttachment`       | `get_attachment`         | handlers.go:545        | Returns metadata text + base64 `BlobResourceContents` with URI `attachment:///{id}/{filename}`. Capped at `maxAttachmentSize = 50 MB` (handlers.go:543). |
| `ToolExportAttachment`    | `export_attachment`      | handlers.go:611        | Saves to `~/Downloads` (or `destination`) via `export.CreateExclusiveFile`. Filename sanitized; falls back to content hash. |
| `ToolListMessages`        | `list_messages`          | handlers.go:687        | Filters: account, from, to, label, after, before, has_attachment. |
| `ToolGetStats`            | `get_stats`              | handlers.go:746        | Returns `{stats, accounts, vector_search?}` (handlers.go:740). |
| `ToolAggregate`           | `aggregate`              | handlers.go:771        | `group_by` enum: sender, recipient, domain, label, time. |
| `ToolStageDeletion`       | `stage_deletion`         | handlers.go:847        | Either `query` OR structured filters — never both. Writes a manifest under `{dataDir}/deletions`; `manifest.CreatedBy = "mcp"`. Hard cap `maxStageDeletionResults = 100000` (handlers.go:845). Does not delete; instructs user to run `MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged`. |
| `ToolSearchByDomains`     | `search_by_domains`      | handlers.go:1017       | Comma-separated domain list, matches any participant. |
| `ToolFindSimilarMessages` | `find_similar_messages`  | handlers.go:398        | **Conditionally registered** — only when `opts.Backend != nil` (server.go:116). Uses seed message's stored embedding via `backend.LoadVector` and the active generation. |

No MCP **resources** or **prompts** are registered. Only tools.

## Sharing the `query.Engine`

`ServeOptions.Engine` (server.go:69) is the only required field. All
handlers route reads through it. The CLI mcp command builds the same
engine selection as `serve` (DuckDB over Parquet when complete, SQLite
otherwise — cmd/msgvault/cmd/mcp.go:69-88) so the MCP and HTTP API see
identical data.

The optional vector trio:
- `HybridEngine` enables `mode=vector|hybrid` on `search_messages`. When
  nil, those modes return `vector_not_enabled`.
- `Backend` enables `find_similar_messages` (also feeds `vector.CollectStats`
  in `get_stats`).
- `VectorCfg.Search.MaxPageSizeHybridClamp()` is read at request time so
  changing TOML reloads the cap without rebuilding the server.

`translateVectorErr` (handlers.go:46) maps the `vector.Err*` sentinels to
canonical error strings (`vector_not_enabled`, `index_stale`,
`index_building`, `no_active_generation`, `embedding_timeout`) — keep new
sentinels routed through it for consistent client UX.

The CLI opens the store **read-only** (cmd/msgvault/cmd/mcp.go:48) so
multiple concurrent MCP processes do not contend on the SQLite write
lock. Schema migrations and FTS backfill are handled by `init-db` /
`sync` / `tui`, not MCP.

## HTTP transport gotcha

`mcp --http` is unauthenticated by design (the StreamableHTTP server has
no built-in auth). `cmd/msgvault/cmd/mcp.go:163 normalizeMCPHTTPAddr`
forces loopback unless `--http-allow-insecure` is set, and treats an
empty host (e.g. `[]:8080`) as non-loopback. Don't relax that without a
proxy story.
