# Current protocol findings

Last reviewed: 2026-07-30

This is the compact handoff document for future work. It records what was observed, what was
experimentally accepted, and what remains an inference. Recheck it whenever Claude Code or the
available model generation changes.

## Evidence labels

- **Capture:** observed in an exact or visually inspected official-client request.
- **Acceptance test:** changed one request field and recorded the upstream response.
- **Binary inspection:** observed in the official packaged Claude Code executable.
- **Inference:** best current explanation, not an Anthropic contract.

## Stable native API behavior

- **Acceptance test:** `anthropic-version` is required by the normal Messages API. The relay adds
  `2023-06-01` only when absent.
- **Acceptance test:** `content-type: application/json` is required and added only when absent.
- The relay supports native Anthropic request and response shapes only.

## Model acceptance matrix

Observed on 2026-07-30 using one Claude subscription OAuth credential:

| Model | Minimum successful subscription attribution observed |
| --- | --- |
| Haiku 4.5 | Native API request; no billing block or metadata required |
| Sonnet 5 | Billing block with `cc_version` + `cc_entrypoint`, plus structured `metadata.user_id` |
| Opus 5 | Billing block with `cc_version` + `cc_entrypoint`; metadata not required |

These are acceptance observations, not guaranteed model contracts. A successful HTTP response also
does not by itself prove how Anthropic classified subscription usage.

## Billing attribution

- **Capture:** official Claude Desktop 2.1.219 Sonnet 5 and Opus 5 requests used:

  ```text
  x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=<5 hex>;
  ```

- **Acceptance test:** the smallest successful synthetic block contained only `cc_version` and
  `cc_entrypoint`. The reserved prefix or either field alone was rejected.
- The relay currently uses the captured `2.1.219.0a7` identifier for synthetic requests. It is a
  dated compatibility constant and must be revisited after client changes.
- A third-party algorithm claiming to derive the final three hex characters from message content
  produced `68d` for both exact captured requests, while the official value was `0a7`. It is not
  used here.

## `metadata.user_id`

- **Capture:** the official Desktop value is a JSON string containing `device_id`, `account_uuid`,
  and `session_id`.
- **Acceptance test:** Sonnet 5 accepted the real value, a random value with the same JSON shape,
  and CLIProxyAPI's legacy structured string. The embedded account UUID did not have to equal the
  subscription account UUID in the tested request.
- **Current policy:** preserve a caller value. Otherwise use the imported real account UUID when
  available, a persistent random device ID, and either a stable header-derived session UUID or a
  per-request random session UUID.
- The fields contain pseudonymous identifiers only; never derive them from an access token, email,
  or request text.

## CCH

- **Capture:** official Desktop 2.1.219 still emits a five-hex CCH for both Sonnet 5 and Opus 5.
- **Binary inspection:** the 2.1.219 attribution builder still creates `cch=00000` for qualifying
  first-party OAuth or Vertex paths. The condition is based on provider/auth path, not the
  `cli` versus `claude-desktop` entrypoint label.
- **Acceptance test:** Haiku 4.5, Sonnet 5, and Opus 5 all succeeded without CCH when their other
  model-specific requirements were met.
- Exact raw bodies recovered from mitmweb matched captured `Content-Length`, yet the older seeded
  xxHash64 algorithm did not reproduce either official CCH. The algorithm changed by 2.1.219.
- sub2api removed CCH based on an unrelated issue reference and has not introduced a replacement
  algorithm as of its 2026-07-29 main branch.
- **Current policy:** never generate or rewrite CCH online. Any request already carrying CCH is
  forwarded byte-for-byte. The old algorithm remains only in the offline research utility.

## Official prompt and title behavior

- **Capture:** official Desktop requests include their own Claude Code prompt, tools, cache
  breakpoints, thinking, and context-management fields. The relay does not recreate or replace
  those fields.
- **Capture:** title generation uses a separate Claude Web endpoint under
  `/api/organizations/{organization}/dust/generate_title_and_branch`, currently selecting Sonnet 5.
  Chinese input can produce a Chinese title. This endpoint is outside the relay scope.

## Implemented transformation contract

1. If a billing block contains `cch=`, forward the request body unchanged.
2. Otherwise preserve existing billing and metadata fields.
3. Add only a missing minimum billing block and missing structured metadata.
4. Preserve all original system instructions and messages; never inject Claude Code behavioral
   instructions.
5. Keep the transformer idempotent.

## Deferred questions

- Derive and independently verify the post-2.1.215 CCH algorithm from multiple exact raw captures.
- Recheck the model matrix after client/model releases and distinguish validation responses from
  genuine overload/rate-limit responses.
- Verify subscription-side usage classification in addition to HTTP success.
- Revisit the captured billing version constant when the official client changes.
