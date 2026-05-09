# internal/oauth

Google OAuth2 flows and token persistence. Three credential pathways
land here:

1. **Browser flow** (`Manager.Authorize`) — desktop OAuth2 with a local
   callback server on `localhost:8089`.
2. **Manual / re-auth flow** (`Manager.AuthorizeManual`) — same flow,
   but prints the URL instead of launching a browser. Used during sync
   re-auth and on machines where `xdg-open` is unreliable.
3. **Service account** (`ServiceAccountManager` in `serviceaccount.go`)
   — Google Workspace domain-wide delegation. No per-user token
   storage; JWTs are signed and refreshed transparently.

Headless servers do **not** get device flow: Google's device endpoint
doesn't support Gmail scopes. `PrintHeadlessInstructions`
(`oauth.go:115`) tells the user to authorize on a workstation and `scp`
the token file across.

User-facing setup steps live at msgvault.io; this file documents the
internals.

## Scopes

`Scopes` (`oauth.go:28`) — read+modify, used for sync/search.
`ScopesDeletion` — `https://mail.google.com/`, required for the
`batchDelete` API. `gmail.modify` allows trash but **not**
`batchDelete`.

`Manager` exposes `HasScope(email, scope)` so the deletion command can
detect that the stored token doesn't carry the deletion scope and
prompt for re-authorization without an API round-trip first.

## Browser flow details

`browserFlow` (`oauth.go:231`) uses CSRF state, requests
`AccessTypeOffline + ApprovalForce` (so we always get a refresh token),
and passes `login_hint=<email>` to pre-select the account in the
consent screen. `openBrowser` only allows `http://`/`https://` URLs to
prevent shell-injection via custom URI schemes.

After the code-exchange, `resolveTokenEmail` (`oauth.go:307`) calls the
Gmail profile endpoint to verify the authorized account matches the
expected email. Mismatches return a typed `*TokenMismatchError` (with
`Expected` / `Actual`) so the CLI can suggest re-running with the
canonical address. `sameGoogleAccount` accepts dot-insensitivity,
`+`-aliases, and `googlemail.com ↔ gmail.com` aliasing for personal
Gmail; for Workspace domains it is exact-match only.

## Token storage

Tokens land in `<tokens_dir>/<sanitized_email>.json` (typically
`~/.msgvault/tokens/`). On Unix the directory is `0700` and files are
`0600`. Writes go via `os.CreateTemp` + `os.Rename` to avoid TOCTOU
symlink races (`saveToken` at `oauth.go:447`); see comments there about
the residual race window between `tokenPath()` and `os.Rename`.

`tokenPath()` (`oauth.go:497`) sanitizes `/`, `\`, and `..` from the
email, then defends against path escapes via `hasPathPrefix` and a
sha256 fallback.

The on-disk format is `oauth.tokenFile`:

```
{
  "access_token": "...",
  "refresh_token": "...",
  "expiry": "...",
  "token_type": "Bearer",
  "scopes": ["..."],          // tracked since scope-aware deletion
  "client_id": "..."          // tracked so multi-app setups detect mismatches
}
```

`scopes` and `client_id` are absent in legacy tokens — the manager
treats absent metadata as "unknown, assume reauth required" for
deletion-scope checks but still allows the token to refresh.

## Refresh

`Manager.TokenSource(ctx, email)` (`oauth.go:76`) loads the file, wraps
`oauth2.Config.TokenSource`, immediately calls `.Token()` to trigger a
refresh if needed, and writes the new token back when `AccessToken`
changes. Refreshed tokens preserve the original `scopes` so that scope
metadata doesn't drift across refreshes.

## Multi-OAuth-app support (`--oauth-app`)

`config.OAuth.ClientSecretsFor(name)` resolves a named app to its JSON
client-secrets file (e.g. an Acme-issued Workspace OAuth client). The
default empty name maps to the global `client_secrets`. The `Source`
table stores a `oauth_app` column so per-account routing is sticky
across runs — see `cmd/.../syncfull.go:sourceOAuthApp`.

`TokenMatchesClient(email)` lets the CLI detect that a stored token was
minted by a different OAuth client (e.g. user re-ran `add-account
--oauth-app acme` after originally using the default app) and force a
re-authorization without first having to fail a refresh.

## Service accounts

`ServiceAccountManager` (`serviceaccount.go`) reads a Workspace JSON
key (chmod-checked: `0o077` mode bits cause an error on non-Windows),
parses it via `google.JWTConfigFromJSON`, and produces an
`oauth2.TokenSource` with `Subject = email` set on each call —
domain-wide delegation requires that subject claim. There is no
per-user token file: tokens are JWT-signed on demand and cached by the
oauth2 library. `oauth.ValidateTokenEmail` does the same Gmail-profile
sanity check used by browser-flow tokens.

## When editing

- Never lower the `0700` / `0600` modes. Don't bypass
  `fileutil.SecureMkdirAll` / `SecureChmod` (Windows DACL handling).
- Token writes must stay atomic (temp + rename). A non-atomic write
  during a crash leaves a half-written JSON file that breaks load.
- The `*TokenMismatchError` type is a public contract — at least the
  `add-account`, `add-o365`, and re-auth paths inspect it. Don't fold
  it into a plain wrapped error.
- Profile validation (`fetchTokenProfileEmail`) must keep the 10s
  timeout (`resolveTimeout`) — without it the entire `add-account`
  command can hang on a slow network.
