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
4. Rendezvous hashing of a cache-prefix key across enabled, non-cooling accounts.

The cache-prefix key reads existing Anthropic `cache_control` breakpoints without rewriting them.
When no breakpoint exists, tools, system, and the first user message form the stable anchor. Route
keys stored in SQLite are hashes and do not contain raw prompts or client session identifiers.

Each account owns its OAuth tokens, stable account/device attribution identity, enabled state, and
model-specific cooldowns. Selection therefore occurs before minimum subscription attribution is
added. Signed CCH bodies remain immutable and are pinned to their matching imported account.

Transient failures permit at most one alternate account. Explicit account overrides and signed
account-bound requests do not fail over.

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

## 2026-07-30: relay and administration keys are separate

Model callers and account operators have different authority. The relay key authenticates native
Anthropic message and token-count requests, including the private `X-Claude-Relay-Account`
override. The administration key authenticates WebUI data, OAuth flows, and account activation.
Neither key grants the other role, and configuration rejects identical values.

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
