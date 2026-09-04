# AGENTS.md — shared repository contract

This file applies to every coding agent working in this repository. Agent- or
tool-specific files may add compatible guidance, but they do not override the
shared safety, privacy, validation, or Git rules below.

## Repository purpose and safety

`msgvault` imports and searches message archives. Treat message data and account
configuration as private.

- Never commit credentials, local databases, live exports, or real personal
  data.
- Use obviously synthetic names, addresses, and identifiers in test fixtures.
- Keep sync operations read-only with respect to external message providers.
- Preserve existing database migrations and compatibility unless the requested
  change explicitly alters them.

## Validation

- For Go changes, run `go fmt ./...` and `go vet ./...`.
- Run focused tests during development and `make test` for completed Go changes.
- Run `make lint-ci` when lint-sensitive code changes.
- The repository's `prek.toml` is the portable hook definition; install it with
  `make install-hooks` where `prek` is available.

## Shared Git workflow

- Work on `main` unless the user explicitly requests another branch.
- Fetch and inspect status/divergence before editing.
- Preserve unrelated or concurrent changes; never use a destructive reset to
  make the tree appear clean.
- Stage only files belonging to the completed logical change. Formatting or
  generated changes are included only when they are caused by and required for
  that change.
- Inspect the staged diff and run relevant validation before committing.
- Commit completed requested work as one coherent change.
- Fetch again before pushing and integrate remote changes without rewriting
  shared history.
- Push normally once the requested work is complete. Never force-push unless
  the user explicitly authorizes that exact action.
- Stop and ask if a merge conflict cannot be resolved confidently from the
  repository's semantics.
