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
account-bound requests do not fail over. OAuth refresh remains a separate unfinished capability.

SQLite in WAL mode is the single-instance persistence boundary. The database holds OAuth secrets
and must live on a private server volume; the service requests owner-only file permissions on
platforms that support POSIX modes. Redis and horizontal multi-instance coordination are deferred
until there is a demonstrated need.
