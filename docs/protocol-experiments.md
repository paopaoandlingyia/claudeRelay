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

Start from one captured request and change one variable at a time. Recompute CCH after every
body change.

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
  proves the requirement is model-dependent: the Claude Code attribution fields are not
  universally required for successful inference, but non-Haiku models need controlled field
  isolation. Subscription usage classification still needs separate verification.

Record both HTTP behavior and subscription usage classification. A successful HTTP response
alone does not prove that the request followed the intended subscription path.

## Transformation rule under consideration

If the matrix supports it, the ordinary-client transformer will be idempotent:

- Preserve an existing billing block; otherwise prepend the minimum valid block.
- Preserve an existing `metadata.user_id`; otherwise generate the minimum valid value.
- Preserve every other system block and message in its original order.
- Compute CCH only after the final body is stable.
- Never add Claude Code identity or software-engineering instructions.
