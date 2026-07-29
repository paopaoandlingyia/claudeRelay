# Protocol experiments

The relay must preserve normal Anthropic API semantics. It will not add a full Claude Code
prompt unless evidence shows that a specific upstream requirement cannot be satisfied in a
smaller way.

## Standard API requirements

These are normal Anthropic API requirements, not subscription or Claude Code mimicry:

- `anthropic-version` is required. The relay supplies `2023-06-01` only when absent.
- `content-type: application/json` is required and supplied only when absent.
- Upstream authentication is supplied by the relay.

## Baseline

Replay a captured Claude Code request through the relay without changing its body. Confirm:

1. The upstream returns a successful response.
2. Streaming and non-streaming responses are relayed unchanged.
3. The subscription records usage as expected.

The current mitmproxy export is valid JSON but is not byte-identical to the wire request:
its saved UTF-8 length differs from the captured `Content-Length`. Re-sign its existing CCH
before replay. The fixture otherwise confirms a Claude Desktop entrypoint, a five-digit CCH,
and JSON-string metadata containing `account_uuid`, `device_id`, and `session_id`.

## Minimal request matrix

Start from one captured request and change one variable at a time. Early tests recomputed CCH
after every body change; later tests explicitly disabled signing to determine whether the upstream
validates it.

| Case | Billing block | CCH | `metadata.user_id` | Claude Code identity prompt |
| --- | --- | --- | --- | --- |
| A | present | valid | present | absent |
| B | present | valid | absent | absent |
| C | present | absent | present | absent |
| D | present | invalid | present | absent |
| E | absent | absent | present | absent |
| F | present | valid | present | present |
| G | absent | absent | absent | absent |

### Observations

- Case G returned HTTP 200 with Haiku 4.5 on 2026-07-30 after the relay supplied only the standard
  `anthropic-version`, `content-type`, and OAuth authorization headers. The request contained
  ordinary system and user text, with no billing block, CCH, metadata, or Claude Code identity
  prompt. Response usage reported 33 ordinary input tokens and no cache creation/read tokens.
- The same minimal shape returned `rate_limit_error` with tested Sonnet and Opus models. This
  proves the requirement is model-dependent: the billing attribution block is not universally
  required for successful inference.
- Adding only the captured `metadata.user_id` to the minimal Sonnet 5 request still returned
  HTTP 429 `rate_limit_error`. Metadata alone does not pass the model gate.
- Adding a billing block containing `cc_version`, `cc_entrypoint`, and a correctly signed `cch`
  changed Sonnet 5 from the opaque HTTP 429 to HTTP 529 `overloaded_error` in three attempts.
  A contemporaneous request without the block still returned the opaque HTTP 429. This strongly
  indicates that the billing block passed the model gate and the 529 was a separate capacity
  condition.
- The same signed billing-only request succeeded with Opus 5 (HTTP 200). It contained no
  `metadata.user_id`, Claude Code identity prompt, or `anthropic-beta` header.
- With CCH signing disabled, Opus 5 also succeeded with literal `cch=00000`, and then succeeded
  after the `cch` field was removed entirely. Current upstream behavior therefore does not require
  a CCH value for this request path.
- A block containing only the reserved `x-anthropic-billing-header:` prefix was rejected with
  HTTP 400. Blocks containing only `cc_version` or only `cc_entrypoint` were rejected the same
  way. A block containing both fields succeeded. The smallest structure demonstrated so far is:

  ```text
  x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop;
  ```
- `anthropic-beta: oauth-2025-04-20` and adding `claude-code-20250219` did not change the opaque
  Sonnet/Opus failure when the billing block was absent.

These observations describe behavior seen on 2026-07-30, not a documented Anthropic contract.
Subscription usage classification still needs separate verification.

Record both HTTP behavior and subscription usage classification. A successful HTTP response
alone does not prove that the request followed the intended subscription path.

## Transformation rule under consideration

The current evidence supports an ordinary-client transformer with this idempotent policy:

- Preserve an existing billing block; otherwise prepend the demonstrated minimum block containing
  `cc_version` and `cc_entrypoint`.
- Do not synthesize `metadata.user_id`; preserve it if the client supplied it.
- Preserve every other system block and message in its original order.
- Preserve and re-sign an existing CCH for compatibility with real Claude Code traffic, but do not
  synthesize one for ordinary clients.
- Never add Claude Code identity or software-engineering instructions.
