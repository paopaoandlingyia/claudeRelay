# claude-relay

Minimal Anthropic Messages relay for one Claude subscription credential. It accepts the native
Anthropic request format only; there is no OpenAI compatibility layer, account rotation, billing,
or Claude Code prompt injection.

## Current scope

- One imported Claude OAuth credential and one downstream API key
- `POST /v1/messages` and `POST /v1/messages/count_tokens`
- Transparent JSON and SSE responses
- Minimum subscription attribution for ordinary Anthropic requests
- Byte-for-byte body pass-through when an official client CCH is present
- No automatic OAuth refresh yet

The dated protocol conclusions and unresolved questions are centralized in
[`docs/protocol-findings.md`](docs/protocol-findings.md). Raw experiment notes remain in
[`docs/protocol-experiments.md`](docs/protocol-experiments.md).

## Build

```powershell
go build -o claude-relay.exe ./cmd/claude-relay
```

## Import a CLIProxyAPI credential

```powershell
.\claude-relay.exe import `
  -from F:\path\to\cliproxy\auths\claude-user.json `
  -to data\credentials.json
```

The importer preserves the real account UUID when available and creates a persistent random
device identity when absent. Existing credential files are upgraded once on the next load.
Unknown source fields are retained under `extra`; tokens are never printed.

## Configure and run

```powershell
Copy-Item config.example.json config.json
$env:CLAUDE_RELAY_API_KEY = "replace-with-a-long-random-key"
.\claude-relay.exe serve -config config.json
```

Point an Anthropic client to `http://127.0.0.1:8317`. Set `upstream_proxy` to a URL such as
`http://127.0.0.1:7890` when upstream traffic must use a local proxy.

## Request transformation

- Requests containing a billing block with `cch=` are treated as official signed traffic and
  forwarded byte-for-byte.
- An existing billing block or `metadata.user_id` is preserved.
- Otherwise the relay prepends the smallest billing block demonstrated by the current tests and
  adds a JSON-string user identity with stable account/device identifiers.
- Token-count requests receive the same billing block but no metadata, because that endpoint's
  schema rejects metadata. The returned count therefore includes the billing text sent to models.
- `X-Claude-Session-Id`, `X-Session-Id`, or `Session-Id` produces a stable pseudonymous session
  UUID. Without one, a new session UUID is generated for that stateless request.
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
