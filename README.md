# claude-relay

Minimal multi-account Anthropic Messages relay for Claude subscription credentials. It accepts the
native Anthropic request format only; there is no OpenAI compatibility layer, user billing,
round-robin rotation, or Claude Code prompt injection.

## Current scope

- Multiple imported Claude OAuth credentials with separate relay and administration API keys
- `POST /v1/messages` and `POST /v1/messages/count_tokens`
- Transparent JSON and SSE responses
- Minimum subscription attribution for ordinary Anthropic requests
- Byte-for-byte body pass-through when an official client CCH is present
- Cache-affinity account selection, one-hour sticky sessions, and one bounded transient failover
- Disabled-by-default account imports and manual activation
- Embedded management console with PKCE OAuth login, credential paste-import, and account removal
- Bounded in-memory request records exposing routing, failover, and cooldown state
- On-demand rotating OAuth refresh for enabled accounts

The dated protocol conclusions and unresolved questions are centralized in
[`docs/protocol-findings.md`](docs/protocol-findings.md). Raw experiment notes remain in
[`docs/protocol-experiments.md`](docs/protocol-experiments.md).

## Build

```powershell
go build -o claude-relay.exe ./cmd/claude-relay
```

For the recommended single-instance server deployment, use the included `compose.yaml`. The
container runs as a non-root user, keeps SQLite in a private named volume, and binds only to the
host loopback interface by default. See [`docs/deployment.md`](docs/deployment.md) for migration,
HTTPS, private GHCR images, proxy, backup, and upgrade instructions.

## Import a CLIProxyAPI credential

```powershell
.\claude-relay.exe import `
  -from F:\path\to\cliproxy\auths\claude-user.json `
  -alias personal `
  -db data\claude-relay.db
```

The importer preserves the real account UUID when available and creates a persistent random
device identity when absent. Import each account under a unique, stable ASCII alias using letters,
digits, dots, underscores, or hyphens. Every import and reimport leaves the account disabled;
activation is a separate explicit action. Reimporting the same account preserves its database
identity. Unknown source fields are retained under `extra`; tokens are never printed.

Existing single-account installations that still set `credentials_file` are migrated into the
SQLite database as alias `default` the first time the new server starts with an empty database.
The old JSON file is retained as a backup and is no longer read after the database has an account.
The schema upgrade that introduced activation also disables every pre-existing account once.

## Configure and run

```powershell
Copy-Item config.example.json config.json
$env:CLAUDE_RELAY_API_KEY = "replace-with-a-long-random-key"
$env:CLAUDE_RELAY_ADMIN_API_KEY = "replace-with-a-different-long-random-key"
.\claude-relay.exe serve -config config.json
```

Point an Anthropic client to `http://127.0.0.1:8567`. Set `upstream_proxy` to a URL such as
`http://127.0.0.1:7890` when upstream traffic must use a local proxy.

## Console

Open `http://127.0.0.1:8567/` and sign in with the administration API key from the configuration.
The console is a single dense screen with three sections:

- **账号** — every account with its real routing state: enabled, cooling down (with the reason and
  remaining time), token expiry, last successful refresh, traffic totals, and live sticky bindings.
  Per-account actions cover enable/disable, connectivity check, forced token refresh, cooldown
  release, rename, and deletion.
- **请求** — the recent request records described below, filterable by account and by failures only.
- **接入** — the relay endpoint, the relay API key, copy-ready Claude Code / PowerShell / curl
  snippets, and the effective runtime parameters.

It polls every five seconds while the tab is visible, checks `/healthz` for the status indicator,
and follows the system light/dark preference with a manual override. The management key lives in
the current tab's `sessionStorage`; it is not written to the server or to persistent browser
storage. The static login page is public, while every management API remains authenticated.

The console shows the relay API key so a working client configuration can be copied in one step.
The administration key already outranks the relay key, so this does not widen the trust boundary —
but it does mean anyone who reaches the console holds both roles. Put the service behind an HTTPS
reverse proxy before exposing it over a network.

## Account management

Management endpoints accept only the administration API key and never return access or refresh
tokens. The relay API key cannot access console data, OAuth operations, or account activation.
Every timestamp in an administration response is epoch milliseconds.

```http
GET    /admin/v1/overview
GET    /admin/v1/accounts
POST   /admin/v1/accounts/import
DELETE /admin/v1/accounts/{alias}
POST   /admin/v1/accounts/{alias}/enable
POST   /admin/v1/accounts/{alias}/disable
POST   /admin/v1/accounts/{alias}/rename
POST   /admin/v1/accounts/{alias}/refresh
POST   /admin/v1/accounts/{alias}/cooldown/clear
POST   /admin/v1/accounts/{alias}/check
```

`import` accepts a pasted CLIProxyAPI credential document and leaves the account disabled. It
rejects an existing alias or account identity unless the caller explicitly sends `replace: true`;
the console requires an additional confirmation before doing so. `check` counts tokens for a trivial prompt to
prove the account still reaches upstream; it never refreshes, so a disabled account can be verified
without this relay taking ownership of its refresh-token chain. `refresh` obeys the same ownership
rules as automatic refresh and is rejected for a disabled account or while the global emergency
stop is set. `delete` removes the account with its cooldowns and sticky bindings but does not
revoke the authorization at Anthropic.

Disabled accounts cannot be selected automatically or through `X-Claude-Relay-Account`, and they
never trigger token refresh. Enabling an account means this relay becomes the sole owner of its
refresh-token chain. Stop managing that account in CLIProxyAPI before enabling it here.

Enabled accounts refresh on demand when their access token has less than five minutes remaining.
The access token and rotated refresh token are persisted together before the model request is sent.
Set `auto_refresh_enabled` to `false` for an emergency global refresh stop. Still-valid access
tokens continue to work; expired tokens return an explicit error.

## OAuth login API

The server-oriented PKCE flow uses authorization-code copy/paste so it does not depend on a callback
to the user's local computer:

```http
POST /admin/v1/oauth/claude/start
Content-Type: application/json

{"alias":"personal"}
```

Open the returned `authorization_url`, complete authorization, then submit the resulting code (or
callback URL):

```http
POST /admin/v1/oauth/claude/exchange
Content-Type: application/json

{"session_id":"...","code":"..."}
```

The OAuth session and PKCE verifier live in memory for 30 minutes. A completed login is imported as
disabled and must be enabled separately. Anthropic does not document this subscription OAuth flow
as a stable public API, so the real browser exchange still needs an interactive integration test
whenever its endpoints or client behavior change.

## Account selection

The relay first honors an explicit private account alias, then an account UUID already present in
official-client metadata, then a persisted sticky binding, and finally cache-affinity rendezvous
hashing across healthy accounts. Cache affinity is derived from the caller's existing cache
breakpoint; requests without one use tools, system, and the first user message as a stable anchor.
The relay does not add, remove, or relocate caller cache controls.

Private deployments can force an account for one request:

```http
X-Claude-Relay-Account: personal
```

The alias is available to callers authenticated with the relay API key and is stripped before
forwarding upstream. A missing, disabled, or cooling account produces
an error instead of silently selecting another account. A forced alias that conflicts with the
account identity in an immutable signed CCH request is also rejected. Successful responses include
`X-Claude-Relay-Account` so a trusted caller can inspect the selected alias. Anyone holding the
relay API key can use this override, so aliases should not contain email addresses or
other sensitive data.

Transient `429`, `529`, network, upstream `5xx`, and token-refresh failures may move an unpinned
request to one other account at most once. Explicitly selected and signed account-bound requests
never fail over.

## Request transformation

- Requests containing a billing block with `cch=` are treated as official signed traffic and
  forwarded byte-for-byte.
- An existing billing block or `metadata.user_id` is preserved.
- Otherwise the relay prepends the smallest billing block demonstrated by the current tests and
  adds a JSON-string user identity with stable account/device identifiers.
- Token-count requests receive the same billing block but no metadata, because that endpoint's
  schema rejects metadata. The returned count therefore includes the billing text sent to models.
- `X-Claude-Session-Id`, `X-Session-Id`, or `Session-Id` produces a stable pseudonymous session
  UUID. Without one, the cache-affinity routing key provides a stable fallback identity.
- No Claude Code identity or software-engineering system prompt is added.

The relay supplies `anthropic-version: 2023-06-01` and `content-type: application/json` only when
the client omits them. It replaces downstream authentication with the imported OAuth token and
adds `?beta=true` to the upstream endpoint.

Each response includes `X-Claude-Relay-Request-ID`. A caller may supply a short ID containing only
letters, digits, dots, colons, underscores, or hyphens; otherwise the relay generates one. The ID
appears in request logs for correlation and is not forwarded to Anthropic.

## Request records

```http
GET /admin/v1/requests?limit=100
```

The relay keeps the last `request_log_size` requests in a fixed-size in-memory ring, 500 by default.
Each record holds only metadata: request ID, timestamp, path, model, selected account, why that
account was selected, status, duration, the account a request failed over from, and a versioned
client-shape observation. That observation stores booleans for billing-block, CCH, structured
metadata, known entrypoint, version, and Claude User-Agent presence plus whether the relay passed
the body through or added minimal attribution. Prompts, response bodies, raw headers, CCH values,
metadata identities, and credentials are never recorded, nothing is written to disk, and restarting
the process clears the history. Set `request_log_size` to `0` to disable it entirely.

A failed attempt counts against the account that failed even when the retry succeeded elsewhere,
so a rate-limited account is visible in its own totals rather than hidden behind the failover.

## Legacy CCH research utility

The offline `sign-cch` command retains the old 2.1.215-era candidate algorithm for reproducibility.
It is not used by the server and is known not to reproduce official 2.1.219 captures.

```powershell
.\claude-relay.exe sign-cch `
  -in data\capture\body.json `
  -out data\capture\body.legacy-signed.json
```
