# Architecture decisions

This file records decisions that shape the service beyond the dated Anthropic protocol findings.

## 2026-07-30: multi-account without round robin

The service is server-first and supports multiple Claude subscription accounts from the storage
layer upward. It does not implement user billing, quota accounting, weighted distribution, account
groups, or a general scheduling platform.

Account selection uses the following precedence:

1. `X-Claude-Relay-Account` explicit alias override.
2. `account_uuid` from an official client's structured `metadata.user_id` when it matches an
   imported account.
3. A successful session binding with a sliding one-hour lifetime.
4. The least-loaded enabled, non-cooling account for a new request or a locally overloaded
   sticky binding; rendezvous hashing of the cache-prefix key breaks load ties.

The cache-prefix key reads existing Anthropic `cache_control` breakpoints without rewriting them.
When no breakpoint exists, tools, system, and the first user message form the stable anchor. Route
keys stored in SQLite are hashes and do not contain raw prompts or client session identifiers.

Each account owns its OAuth tokens, stable account/device attribution identity, enabled state, and
model-specific cooldowns. Selection therefore occurs before minimum subscription attribution is
added. Caller metadata account UUIDs are soft affinity hints and fall through to the remaining
selection stages when unavailable.

Transient failures permit at most one alternate account. Explicit account overrides do not fail over.

## 2026-08-02: sticky routing yields to local load temporarily

Sticky bindings remain the default because they preserve cache affinity, but a binding is a
preference rather than an availability boundary. The relay keeps an in-process active-request
counter per account and uses `max_inflight_per_account` (8 by default) as a soft threshold. When a
sticky account reaches that threshold and another healthy account has a lower load, the current
request temporarily bypasses the binding. The original SQLite binding is retained, so later work
returns to it after the load falls; a single-account deployment remains available even above the
threshold.

The counter is reserved before the upstream request and released only after the full response body
has been copied or the client context is canceled. It is not persisted to SQLite because the
supported topology is one relay process. Explicit account aliases remain pinned, and local load
fallback does not clear or migrate a sticky binding.

## 2026-08-13: routing affinity is not session activity

The console distinguishes three account signals that answer different operational questions:

- Sticky bindings are successful routing affinities retained for their sliding one-hour lifetime.
- Active sessions are those same bindings whose last successful request was within five minutes.
- In-flight requests are the process-local reservations currently held through the end of the
  upstream response stream.

The five-minute view reuses `session_bindings.updated_at`; it adds no session table, write, or
background worker. These values are observational and do not impose a session or hard concurrency
limit. Limits remain deferred until normal small-pool usage provides a useful baseline.

## 2026-08-21: account admission and five-hour pacing guards

The relay now applies three simple per-account guards. New routing bindings are filtered when the
account already has five active sessions (recent successful bindings); existing sticky sessions
remain usable until they become inactive. In-process upstream requests have a hard configurable
limit (`max_inflight_per_account`, eight by default), and no alternate account means the request
fails rather than exceeding the limit.

Each account's latest Anthropic unified five-hour utilization header is also compared with the
elapsed fraction of its reset window plus a configurable ten-percentage-point safety margin. An
account ahead of that pacing line, or at the configured 90% throttle threshold, is removed from
ordinary selection; at 95% it is paused until a fresh window is observed. Missing or unparsable
samples remain fail-open because the upstream header is the only authoritative signal and the relay
does not invent quota values.

Rate-limit cooling follows an upstream `Retry-After` value in either delay-seconds or HTTP-date
form. On a `429` without that header, valid Anthropic unified window reset headers take precedence
over the short fallback: an explicitly exhausted window determines the cooldown, otherwise the
earliest future reset avoids conflating unrelated windows.

## 2026-07-30: activation owns OAuth refresh

The intended end state is full migration away from CLIProxyAPI rather than permanent shared
credential management. There is therefore one account switch instead of separate traffic-owner and
refresh-owner state:

- Imports, reimports, and OAuth logins always leave an account disabled.
- Disabled accounts neither receive traffic nor perform refresh operations.
- Enabling an account explicitly makes this relay the sole owner of that refresh-token chain.
- Enabled accounts refresh on demand within five minutes of access-token expiry and atomically
  persist both the new access token and the rotated refresh token.
- `auto_refresh_enabled` is a global emergency stop, not a second ownership model.

Schema version 2 intentionally disables all accounts once during upgrade so merely starting a new
binary cannot take over a credential that another process may still be refreshing.

The PKCE OAuth flow stores its state and verifier in memory for 30 minutes, accepts a copied
authorization code or callback URL, imports the credential as disabled, and never exposes tokens
through management responses. It deliberately excludes cookie-based login.

SQLite in WAL mode is the single-instance persistence boundary. The database holds OAuth secrets
and must live on a private server volume; the service requests owner-only file permissions on
platforms that support POSIX modes. Redis and horizontal multi-instance coordination are deferred
until there is a demonstrated need.

## 2026-07-30: single-container deployment boundary

The supported production topology is one relay process backed by one private SQLite volume. Docker
Compose is a packaging and lifecycle layer, not a horizontal scaling mechanism. The container runs
without root privileges or Linux capabilities, exposes a local health check, and publishes port
8567 on the host loopback interface by default. Public access belongs behind an HTTPS reverse
proxy; the application does not duplicate TLS certificate management.

The Docker build context is allowlisted to Go source and the non-secret container configuration so
local databases, OAuth credentials, captures, and developer configuration cannot enter the image.
Runtime secrets are supplied through environment variables. Windows tray integration is deferred
because it would add a second platform-specific lifecycle without improving the server-first goal.

## 2026-07-30: model ingress and administration keys are separate

Model callers and account operators have different authority. The compatible and official ingress
keys authenticate native Anthropic message and token-count requests, including the private
`X-Claude-Relay-Account` override within the pools that ingress may reach. The administration key
authenticates
WebUI data, OAuth flows, account placement, and activation. No ingress key grants administration
authority, the administration key cannot call models, and configuration rejects identical values.

This remains a small private deployment rather than a user/role system. There is one key per role,
no billing identity, and no per-caller quota. Splitting the keys prevents an API consumer from
taking ownership of OAuth refresh tokens without introducing a general access-control platform.

## 2026-07-31: SQLite remains the single-instance state store

Redis is not part of the supported single-server topology. Adding it would introduce another
authenticated service, backup policy, expiry model, and consistency boundary without removing the
need for durable OAuth credential storage. SQLite in WAL mode already covers the current account,
cooldown, and sticky-binding workload with one private volume.

If horizontal replicas become a demonstrated requirement, persistence and refresh ownership must
be redesigned together. A durable database such as PostgreSQL would own accounts and OAuth token
rotation; Redis could then be considered only for short-lived routing state and coordination. It
must not be added independently as a partial multi-instance workaround.

## 2026-07-31: the console reports routing state, not just stored state

The first WebUI only rendered what the `accounts` table stores, so `enabled` was presented as if it
meant "receiving traffic". It does not: an enabled account can be excluded by an active cooldown,
and an account holding sticky bindings behaves differently from an idle one. The console now reads
the live routing inputs — cooldowns, sticky binding counts, and token expiry — and derives a single
status per account. Operator actions cover the full lifecycle that previously required SQLite or
shell access: paste-import, rename, delete, forced refresh, cooldown release, and a connectivity
check.

Two ownership rules stay enforced on the server rather than in the interface. A forced refresh
requires an enabled account and an active global refresh switch, because refreshing is exactly what
account activation grants. A connectivity check never refreshes, so a disabled account can be
verified without this relay taking over its refresh-token chain.

Deleting an account removes its cooldowns and sticky bindings through the existing foreign keys. It
does not revoke the Anthropic authorization, and the console says so at the confirmation step.
Pasted imports reject an existing alias or account identity by default. Replacement requires an
explicit API flag and a second console confirmation because it changes credentials and disables the
account immediately.

Schema version 3 adds `accounts.last_refresh_at` so a healthy account is distinguishable from one
whose refresh chain silently stopped rotating. The upgrade only adds a column and changes no
account state.

## 2026-07-31: request records are bounded, in-memory, and metadata only

Routing was previously unobservable: cache affinity, sticky sessions, and bounded failover decided
which subscription served a request, and nothing surfaced those decisions. A fixed-size ring of
request records now backs the console, holding request ID, timestamp, path, model, selected account,
selection source, status, duration, and the account a request failed over from.

The ring stores no prompts, response bodies, headers, or credentials, is never persisted, and is
cleared on restart. `request_log_size` bounds it and `0` disables it, so the feature cannot grow
into an unbounded local log. Container logs remain the retained record; this is a live view.

A failed attempt is attributed to the account that failed even when the retry succeeded on another
account. Without that, the account being rate-limited would be invisible in every per-account total
while a healthy account absorbed its traffic.

## 2026-07-31: client-shape classification is observable and policy-scoped

The relay classifies the raw incoming request before transforming it. Version 3 reports
`cc_candidate` only for a `claude-cli` User-Agent together with `X-Claude-Code-Session-Id` and
`X-App: cli`. Billing, structured metadata, or a partial header set is `ambiguous`; no evidence is
`compatible`.

The in-memory request record stores presence booleans and the relay action only. It never stores the
raw billing values, User-Agent, account/device/session identities, prompts, or response content.
Classification does not authenticate an official client. It is enforced only as the admission
policy for the official ingress; compatible ingress forwarding remains independent of the result.

Response content is not retained. This section's earlier decision to defer usage accounting was
superseded on 2026-08-09 by a byte-preserving response usage observer described below.

## 2026-08-06: account pool permeability is one way

Supersedes the 2026-07-31 decision that made the two pools mutually isolated.

The deployment serves two traffic policies without turning account selection into a general group
scheduler. Format admission belongs to the key: the compatible ingress accepts ordinary native
Anthropic requests, and the optional official ingress accepts only requests classified as
`cc_candidate`. The classification is a policy signal, not authentication of the Claude Code
executable; possession of either API key remains the actual authentication boundary.

Account placement is a separate axis, and the two traffic shapes carry asymmetric risk.
Claude Code-shaped traffic is the shape a subscription is expected to produce, so the official
ingress may select from every pool. The compatible ingress is fenced to the compatible pool.
An official-pool account therefore never serves a non-Claude-Code request, while the official
ingress keeps the full account set for load spreading and bounded failover instead of stranding
idle capacity behind a partition.

Two-way isolation was rejected because it bought nothing the fence does not already buy. It halved
the accounts available to the dominant traffic class, capped the load-aware routing added on
2026-08-02, and returned an explicit unavailable error whenever the official pool was empty — which
was the default state after an import.

The fence is enforced in every account lookup, so explicit aliases, structured account UUIDs,
sticky bindings, rendezvous candidates, and bounded failover all respect it. Routing keys stay
scoped per ingress so the two key holders are treated as different clients and cannot collide on a
sticky binding. Moving an account clears its bindings in the same SQLite transaction, while
preserving its enabled state, cooldowns, and OAuth tokens. Existing and newly imported accounts
default to `compatible`, which is now the shared placement; the stored column and its two values are
unchanged, so this decision needs no schema migration.

There is one API key per ingress rather than arbitrary named groups. This directly matches the two
New API token/channel groups in scope and avoids introducing group administration, per-user ACLs,
quota logic, or weighted routing.

## 2026-07-31: subscription usage is a cached management observation

Account capacity is read from Anthropic's private OAuth usage endpoint rather than estimated from
relayed token counts. Only named windows actually returned upstream are exposed; absent weekly or
model-specific fields carry no inferred meaning. The optional profile request may add a plan label
but cannot make an otherwise valid usage reading fail.

Successful results live in a two-minute in-memory cache, but the console never reads or refreshes
them automatically. Only an explicit per-account action calls the cache-bypassing refresh endpoint,
so opening the console and its five-second operations poll generate no Anthropic usage requests.
Ordinary cached reads are not persisted. An explicit refresh now writes a calibration snapshot as
described in the 2026-08-09 decision; no scheduling decision is derived from it.

Usage reads share the configured outbound proxy. Enabled accounts may use the existing synchronized
OAuth refresh path when their access token is near expiry. Disabled accounts are read only while
their current access token remains valid, preserving the rule that observation cannot take over a
refresh-token chain.

## 2026-08-09: response usage is observed and persisted as aggregates

The relay now measures API-price-equivalent value without treating dollars as Anthropic's internal
subscription unit. It observes only billing metadata from successful Messages responses. The relay
does not forward the caller's `Accept-Encoding` upstream: Go's HTTP transport negotiates and
decodes gzip so the observer never mistakes compressed SSE for a stream without usage. The decoded
response continues through the existing streaming copy; the observer parses `message_start`, the
cumulative `message_delta`, and `message_stop`, skipping content deltas without decoding them.
Non-streaming JSON is inspected incrementally with a bounded two-megabyte tail. Neither path stores
prompt or response content. A successful Messages response without recognizable usage emits a
rate-limited diagnostic warning.

The hot path adds completed usage to an in-memory map keyed by UTC hour, account ID, and serving
model. Every five seconds a background worker swaps that map and performs one short SQLite
transaction. Failed writes are merged back for retry, shutdown performs a final flush, and clearing
statistics is serialized against flushing. This preserves the Store's single-connection invariant
and avoids a database commit per request. Incomplete streams may retain partial usage but are
counted explicitly; samples with no upstream usage are not invented.

Hourly rows retain raw token categories rather than only a calculated dollar value. Model prices
are separate, versioned rules with effective timestamps; exact IDs take precedence over `*` suffix
prefix rules. Unknown models remain unpriced while their raw usage stays available. Default prices
seed a fresh schema but UI-added versions are data, so new models and price changes need no build.

Every Messages response carries the serving account's five-hour window in its unified rate limit
headers, so sampling follows relayed traffic and issues no upstream request of its own. The console
still never polls the private OAuth usage surface, and nothing on this path rotates a token. The
headers are read before the body is streamed, because a streaming body runs for minutes and a
reading timestamped once it finished could be recorded as later than one taken after it. A reading
is stored only when it differs from the last one written for that account, which is rate limit
enough because utilization arrives rounded to whole percent. Each stored reading carries the
five-hour percentage, the reset identity, and the cumulative per-model totals as of a flush. An
explicit refresh still writes a comparable reading through the same table.

The reset instant identifies a window. The OAuth surface recomputes that timestamp to the
microsecond on every read and the headers report it a second early on some responses, so both are
reduced to the epoch second and readings within a few seconds of each other are one window. A window
is measured from its own earliest reading rather than from the reading before it: utilization moves
in whole percent, so adjacent readings are the ones most likely to differ by nothing, while
anchoring lets the denominator accumulate until the extrapolation is worth reading. The anchor need
not be the true start of the window, because only the numerator and the denominator have to describe
the same span. Relay cost across that span divided by the percentage it gained yields the
full-window estimate. Readings are kept for thirty days.

## 2026-08-01: CCH has no relay semantics

The supported official path is Claude Code configured with a third-party API URL. Captures of that
path showed the complete three-header identity shape and no CCH. Model acceptance tests also did
not require CCH. The relay therefore does not generate, calculate, validate, record, or route on
CCH, and the obsolete offline signer has been removed.

Caller-owned billing text remains opaque request content: existing fields are preserved, but they
cannot grant official-ingress admission, bind an account UUID, pin a request, or disable bounded
failover. This keeps the native Anthropic body transparent without retaining a second, unverified
client protocol inside the routing layer.
