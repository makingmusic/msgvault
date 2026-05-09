# internal/microsoft

Microsoft / Azure AD OAuth2 for Office 365 and personal Outlook
accounts. Produces an access-token callback consumable by
`internal/imap` via XOAUTH2 SASL — this package issues no IMAP traffic
itself.

## Scope

`oauth.go` only. Sibling `oauth_test.go` has fixtures for the OIDC
verifier, browser-flow callback, and tenant-scope correction logic.

The manager covers:

- Browser PKCE flow (S256) on `localhost:8089/callback/microsoft`
- ID-token verification via `coreos/go-oidc` (signature, issuer,
  audience, expiry, nonce)
- Tenant ID detection from the `tid` claim, used to pick the correct
  IMAP scope (`outlook.office.com` for personal accounts vs
  `outlook.office365.com` for org tenants)
- Tenant-aware refresh against the same tenant the token was minted in
- Best-effort refresh-token revocation on `DeleteToken`

## Personal vs organizational accounts

Microsoft serves IMAP from two different resources:

- `ScopeIMAPPersonal = https://outlook.office.com/IMAP.AccessAsUser.All`
  for consumer accounts (hotmail, outlook.com, live, msn — see
  `isPersonalMicrosoftAccount` for the full domain list).
- `ScopeIMAPOrg = https://outlook.office365.com/IMAP.AccessAsUser.All`
  for everything else.

The manager initially guesses by domain (`scopesForEmail`), then
**re-runs** the entire browser flow if the ID-token's `tid` claim says
otherwise (`oauth.go:158`). The well-known consumer tenant ID
(`MicrosoftConsumerTenantID = 9188040d-…`) is the discriminator. This
re-auth is interactive: consent for a different IMAP resource cannot
be obtained via silent refresh.

`Manager.IMAPHost(email)` reads the persisted scope list and returns
the matching hostname for `cmd/.../addo365.go` to wire into the IMAP
config.

## Token file layout

Tokens land in `<tokens_dir>/microsoft_<sanitized_email>.json` (note
the `microsoft_` prefix — distinguishes from Google tokens that share
the dir). Format:

```
oauth2.Token + { "scopes": [...], "tenant_id": "..." }
```

`tenant_id` was added with the scope-correction feature; pre-migration
tokens are auto-bound to the manager's configured tenant on next load
(`oauth.go:223`). On load, scope is validated against `tid` and a
mismatch returns an error directing the user to re-run `add-o365`
(`oauth.go:241`).

Same atomic-write + `0600` storage pattern as `internal/oauth`. Email
sanitization is stricter (`filepath.Base` defense in depth).

## Token refresh

`Manager.TokenSource(ctx, email)` (`oauth.go:209`) returns a `func(ctx)
(string, error)` rather than `oauth2.TokenSource` so it can plug
straight into `imap.WithTokenSource`. Each call:

- Runs `ts.Token()` in a goroutine bounded by `tokenRefreshTimeout =
  30s`. Caller's `ctx` cancels the wait but the underlying refresh
  uses `context.Background` so it isn't tied to a sync-scoped context
  that may be narrower than the token source's lifetime.
- Persists the refreshed token if any field changed; a save failure
  fails the call (refreshing without persisting would silently break
  on next run).

## Security notes worth preserving

- Nonce check on the ID token (`oauth.go:535`) — replay protection.
- `redactAuthURL` strips `state`, `nonce`, `code_challenge` before
  printing the auth URL to stdout. The full URL still goes to the
  browser.
- `openBrowser` allows **only** `https://` (Microsoft's auth URL is
  always HTTPS) — stricter than the Google browser opener which also
  allows `http://`.
- Browser flow times out after 5 minutes (`browserFlow` at
  `oauth.go:336`) so port 8089 is released even if the user abandons
  authorization.
- Server `Shutdown` runs in a fresh `context.Background` — caller's
  ctx may already be cancelled (Ctrl-C path) and we still want
  in-flight requests to drain.

## Out of scope

- Microsoft Graph API: not used. Mail data flows over IMAP with
  XOAUTH2.
- Multi-OAuth-app: there is exactly one `[microsoft]` config block in
  `config.toml` (one client_id per install). No analogue to Google's
  named-apps feature today. TODO(verify): confirm this matches the
  config docs.
