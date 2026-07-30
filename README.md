# claude-relay

Minimal multi-account Anthropic Messages relay for Claude subscription credentials. It accepts the
native Anthropic request format only; there is no OpenAI compatibility layer, user billing,
round-robin rotation, or Claude Code prompt injection.

## Current scope

- Multiple imported Claude OAuth credentials and one downstream API key
- `POST /v1/messages` and `POST /v1/messages/count_tokens`
- Transparent JSON and SSE responses
- Minimum subscription attribution for ordinary Anthropic requests
- Byte-for-byte body pass-through when an official client CCH is present
- Cache-affinity account selection, one-hour sticky sessions, and one bounded transient failover
- Disabled-by-default account imports and manual activation
- Embedded account-management WebUI and PKCE OAuth login
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
HTTPS, proxy, backup, and upgrade instructions.

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
.\claude-relay.exe serve -config config.json
```

Point an Anthropic client to `http://127.0.0.1:8567`. Set `upstream_proxy` to a URL such as
`http://127.0.0.1:7890` when upstream traffic must use a local proxy.

## WebUI

Open `http://127.0.0.1:8567/` and sign in with the downstream API key from the configuration. The
embedded UI lists accounts, makes activation state explicit, and walks through the server-oriented
Claude OAuth copy/paste flow. OAuth imports remain disabled until manually enabled.

The UI stores the management key in the current tab's `sessionStorage`; it is not written to the
server or persistent browser storage. The static login page is public, while every management API
remains authenticated. Put the service behind an HTTPS reverse proxy before accessing the WebUI
over a network, because the shared downstream key grants both relay and account-management access.

## Account management

Management endpoints use the same downstream API key as message requests. They never return access
or refresh tokens.

```http
GET  /admin/v1/accounts
POST /admin/v1/accounts/{alias}/enable
POST /admin/v1/accounts/{alias}/disable
```

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

The alias is stripped before forwarding upstream. A missing, disabled, or cooling account produces
an error instead of silently selecting another account. A forced alias that conflicts with the
account identity in an immutable signed CCH request is also rejected. Successful responses include
`X-Claude-Relay-Account` so a trusted caller can inspect the selected alias. Anyone holding the
single downstream API key can use this override, so aliases should not contain email addresses or
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

## Legacy CCH research utility

The offline `sign-cch` command retains the old 2.1.215-era candidate algorithm for reproducibility.
It is not used by the server and is known not to reproduce official 2.1.219 captures.

```powershell
.\claude-relay.exe sign-cch `
  -in data\capture\body.json `
  -out data\capture\body.legacy-signed.json
```
