# claude-relay

Minimal multi-account Anthropic Messages relay for Claude subscription credentials. It accepts the
native Anthropic request format only; there is no OpenAI compatibility layer, user billing,
round-robin rotation, or Claude Code prompt injection.

## Current scope

- Multiple imported Claude OAuth credentials, optionally fenced so chosen accounts serve official
  traffic only
- Separate compatible, official, and administration API keys
- `POST /v1/messages` and `POST /v1/messages/count_tokens`
- Transparent JSON and SSE responses
- Minimum subscription attribution for ordinary Anthropic requests
- Caller-owned billing and metadata fields preserved without interpreting unknown fields
- Sticky-first account selection, in-process load-aware routing for new work, temporary bypass of
  overloaded sticky accounts, and one bounded transient failover
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
$env:CLAUDE_RELAY_OFFICIAL_API_KEY = "replace-with-a-different-long-random-key"
$env:CLAUDE_RELAY_ADMIN_API_KEY = "replace-with-a-different-long-random-key"
.\claude-relay.exe serve -config config.json
```

Point an Anthropic client to `http://127.0.0.1:8567`. Set `upstream_proxy` to a URL such as
`http://127.0.0.1:7890` when upstream traffic must use a local proxy.

`max_inflight_per_account` is an in-process soft routing threshold, set to `8` by default. It is
not a request rejection limit: new requests prefer an account with fewer active upstream requests,
and a sticky request may temporarily use another less-busy account when its bound account reaches
the threshold. If no alternate account is less busy, the relay preserves availability and uses the
sticky account anyway. Set `CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT` in Compose deployments.

## Console

Open `http://127.0.0.1:8567/` and sign in with the administration API key from the configuration.
The console is a single dense screen with three sections:

- **账号** — every account with its pool and real routing state: enabled, cooling down (with the reason and
  remaining time), token expiry, last successful refresh, traffic totals, and live sticky bindings.
  The table can manually read the subscription's available five-hour and optional
  weekly/model-specific OAuth usage windows; it never queries them automatically. Per-account
  actions cover usage refresh, enable/disable, connectivity check,
  forced token refresh, cooldown release, rename, and deletion.
- **请求** — the recent request records described below, filterable by account and by failures only.
- **接入** — the relay endpoint, both ingress API keys, copy-ready Claude Code / PowerShell / curl
  snippets, and the effective runtime parameters.

It polls every five seconds while the tab is visible, checks `/healthz` for the status indicator,
and follows the system light/dark preference with a manual override. The management key lives in
the current tab's `sessionStorage`; it is not written to the server or to persistent browser
storage. The static login page is public, while every management API remains authenticated.

The console shows both ingress API keys so working client configurations can be copied in one step.
The administration key already controls account placement and credentials, so this does not widen
the trust boundary — but it does mean anyone who signs in to the console can obtain all three roles.
Put the service behind an HTTPS reverse proxy before exposing it over a network.

## Account management

Management endpoints accept only the administration API key and never return access or refresh
tokens. Neither ingress API key can access console data, OAuth operations, or account activation.
Every timestamp in an administration response is epoch milliseconds.

```http
GET    /admin/v1/overview
GET    /admin/v1/accounts
POST   /admin/v1/accounts/import
DELETE /admin/v1/accounts/{alias}
POST   /admin/v1/accounts/{alias}/enable
POST   /admin/v1/accounts/{alias}/disable
POST   /admin/v1/accounts/{alias}/rename
POST   /admin/v1/accounts/{alias}/pool
POST   /admin/v1/accounts/{alias}/refresh
POST   /admin/v1/accounts/{alias}/cooldown/clear
POST   /admin/v1/accounts/{alias}/check
GET    /admin/v1/accounts/{alias}/usage
POST   /admin/v1/accounts/{alias}/usage/refresh
GET    /admin/v1/usage?from={epoch_milliseconds}
DELETE /admin/v1/usage
GET    /admin/v1/usage/prices
POST   /admin/v1/usage/prices
```

`import` accepts a pasted CLIProxyAPI credential document and leaves the account disabled. It
rejects an existing alias or account identity unless the caller explicitly sends `replace: true`;
the console requires an additional confirmation before doing so. `check` counts tokens for a trivial prompt to
prove the account still reaches upstream; it never refreshes, so a disabled account can be verified
without this relay taking ownership of its refresh-token chain. `refresh` obeys the same ownership
rules as automatic refresh and is rejected for a disabled account or while the global emergency
stop is set. `delete` removes the account with its cooldowns and sticky bindings but does not
revoke the authorization at Anthropic.

The usage endpoint reads Anthropic's private OAuth usage surface and returns only windows actually
present in the upstream response. A missing weekly or model-specific limit is omitted rather than
invented as zero or unlimited. Successful readings are cached in memory for two minutes; the
refresh endpoint bypasses that cache. Profile lookup is optional, so an unavailable plan label does
not hide valid quota windows. Enabled accounts follow the normal token-refresh ownership rules,
while viewing a disabled account never rotates its refresh token. A forced refresh also saves the
five-hour percentage and the relay's cumulative token counters at that instant.

The five-hour window estimate does not depend on that refresh. Anthropic reports the serving
account's window in the rate limit headers of every Messages response, so the relay samples it from
traffic it was already carrying and never asks for it. A window is measured from the earliest
reading the relay holds for it to the latest: the relay value accrued over that span, divided by the
percentage the window gained, extrapolated to a whole window. The figure sharpens as the window
fills, because utilization is reported in whole percent and a wider span carries proportionally less
rounding error. It covers relayed traffic only, so an account also used elsewhere reports low.

Successful Messages responses are observed after transport-level compression is decoded. The
relay records only Anthropic's response `usage`, the serving account, model, and hour; it never
stores prompts or response content. Streaming `message_start` and cumulative `message_delta`
usage are combined, while incomplete streams remain visible as incomplete samples. Request
goroutines update an in-memory aggregate only. A background worker writes all pending account/model
hour buckets in one short SQLite transaction every five seconds and retries failed batches, so
SQLite work does not block model streaming.

The usage console values raw input, output, five-minute/one-hour cache creation, and cache reads
against versioned per-model prices. Built-in prices are only defaults. Add an exact model ID or a
prefix ending in `*` through the console or `POST /admin/v1/usage/prices`; exact matches win, and a
new effective version preserves historical valuation. Unknown models keep their raw usage and are
reported as unpriced instead of silently contributing zero dollars. API value is an estimate at
published API prices, not evidence that Anthropic deducts subscription capacity in dollars.

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

Two things are marked separately. The **key** decides which request format is admitted: requests
authenticated by `official_api_key` must have a recognized Claude Code shape or are rejected with
`403`, while `relay_api_key` places no restriction on shape. The accepted shape is
`User-Agent: claude-cli/...` together with `X-Claude-Code-Session-Id` and `X-App: cli`. This is a
traffic policy, not proof that the caller is an authentic Anthropic binary.

The **account** carries a pool that decides which traffic may reach it, and permeability is one way:

| Ingress | May select |
| --- | --- |
| `official_api_key` | every enabled account, in either pool |
| `relay_api_key` | `compatible` accounts only |

New and upgraded accounts start in `compatible`, the shared placement, so the official ingress can
use them without any placement step. Move an account to `official` only to keep compatible traffic
off it. Claude Code-shaped traffic is what a subscription is expected to produce, so letting it use
every account costs nothing while the fence still keeps chosen accounts clean, and the official
ingress keeps the full account set for load spreading and failover.

Changing an account's pool immediately clears its sticky bindings. Cooldowns and OAuth ownership
state remain with the account. The fence applies to every selection path, so `X-Claude-Relay-Account`
cannot be used to reach an `official` account from the compatible ingress.

The relay first honors an explicit private account alias, then an account UUID already present in
official-client metadata, then a persisted sticky binding while that account is below the local
in-flight threshold. New requests and temporarily overloaded sticky requests use the healthy
account with the lowest in-process active-request count; the existing cache-affinity hash breaks
ties. Cache affinity is derived from the caller's existing cache breakpoint; requests without one
use tools, system, and the first user message as a stable anchor.

An overloaded sticky request is a temporary bypass, not a binding deletion or migration. The
original binding remains available for later requests when its account becomes less busy. The
threshold is process-local because the supported deployment is a single relay instance; active
request counts are not persisted to SQLite.
The relay does not add, remove, or relocate caller cache controls.

Private deployments can force an account for one request:

```http
X-Claude-Relay-Account: personal
```

The alias is available to callers authenticated with either ingress API key, resolved only among the
accounts that key may reach, and stripped before forwarding upstream. A missing, disabled, or cooling
account produces an error instead of silently selecting another account. Successful responses include
`X-Claude-Relay-Account` so a trusted caller can inspect the selected alias. Anyone holding the
corresponding ingress API key can use this override, so aliases should not contain email addresses or
other sensitive data.

Transient `429`, `529`, network, upstream `5xx`, and token-refresh failures may move an unpinned
request to one other account at most once. Local load-aware bypass may also move a sticky request
temporarily when a less-busy account is available. Explicitly selected requests never fail over.

## Request transformation

- An existing billing block or `metadata.user_id` is preserved.
- Unknown caller-owned billing fields are passed through without validation and have no effect on
  classification, account selection, pinning, or failover.
- Otherwise the relay prepends the smallest billing block demonstrated by the current tests and
  adds a JSON-string user identity with stable account/device identifiers.
- Token-count requests receive the same billing block but no metadata, because that endpoint's
  schema rejects metadata. The returned count therefore includes the billing text sent to models.
- `X-Claude-Code-Session-Id`, `X-Claude-Session-Id`, `X-Session-Id`, or `Session-Id` produces a
  stable pseudonymous session UUID. The official Claude Code header takes precedence. Without one,
  the cache-affinity routing key provides a stable fallback identity.
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
Each record holds only metadata: request ID, timestamp, ingress key, path, model, selected account, why that
account was selected, status, duration, the account a request failed over from, and a versioned
client-shape observation. That observation stores booleans for billing-block, structured
metadata, known entrypoint, version, Claude User-Agent, Claude Code session header, and `X-App: cli`
presence plus whether the relay passed the body through or added minimal attribution. The console
also shows whether the request used `messages` or `count_tokens`. Prompts, response bodies, raw
headers, billing values, metadata identities, and credentials are never recorded, nothing is written
to disk, and restarting the process clears the history. Set `request_log_size` to `0` to disable it
entirely.

When another gateway sits in front of the relay, pass the three Claude Code identification headers
without trusting them as authentication. New API channel header overrides can copy them explicitly:

```json
{
  "User-Agent": "{client_header:User-Agent}",
  "X-Claude-Code-Session-Id": "{client_header:X-Claude-Code-Session-Id}",
  "X-App": "{client_header:X-App}"
}
```

A failed attempt counts against the account that failed even when the retry succeeded elsewhere,
so a rate-limited account is visible in its own totals rather than hidden behind the failover.

## New API grouping

Create two Anthropic channels pointing at the same relay URL. Use `relay_api_key` for the channel
available to the compatible New API group, and `official_api_key` for the channel available to the
official group. On the official channel, preserve the three client identification headers with the
override shown above. Request-body pass-through alone does not preserve them.

The official New API group should be issued only to Claude Code users. The relay rejects a request
that reaches the official key without the required shape before selecting an account, and does not
fall back to the compatible behaviour — assigning callers to the right group is the operator's job.
Conversely, compatible traffic never consumes an `official` account even when it supplies an account
alias.
