# claude-relay

Minimal, experimental Anthropic Messages relay for one Claude subscription credential.

The baseline build intentionally does not rewrite request bodies. Its first purpose is to
replay real Claude Code traffic without invalidating the existing billing attribution or
CCH value. Minimal transformations for ordinary API clients will be added only after
capture-based tests identify the fields the upstream actually requires.

## Current scope

- One imported Claude OAuth credential
- One downstream API key
- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- Transparent JSON and SSE responses
- Re-signing of an existing five-digit CCH without adding prompt content
- No format conversion, account rotation, billing, UI, or prompt injection
- No automatic OAuth refresh in the baseline build

## Build

```powershell
go build -o claude-relay.exe ./cmd/claude-relay
```

## Import a CLIProxyAPI credential

Stop using the same Claude account for refresh operations in other programs before enabling
automatic refresh in a future build. The current baseline imports a private copy and only
uses its access token.

```powershell
.\claude-relay.exe import `
  -from F:\path\to\cliproxy\auths\claude-user.json `
  -to data\credentials.json
```

Unknown source fields are retained under `extra`. Tokens are never printed.

## Configure and run

```powershell
Copy-Item config.example.json config.json
$env:CLAUDE_RELAY_API_KEY = "replace-with-a-long-random-key"
.\claude-relay.exe serve -config config.json
```

The environment variable overrides `api_key` in the JSON file. Point an Anthropic client to
`http://127.0.0.1:8317` and authenticate with the configured downstream key.

Set `upstream_proxy` to an HTTP proxy URL such as `http://127.0.0.1:7890` when Anthropic
traffic must go through a local proxy.

## Baseline behavior

The relay preserves the incoming request body byte-for-byte except for one controlled field:
when the first system block is a billing attribution block with a five-digit CCH, it recomputes
those five hexadecimal digits over the final body. It does not add a billing block or any prompt.
Set `sign_existing_cch` to `false` to disable this behavior.

The relay copies end-to-end headers, removes the downstream `x-api-key`, replaces upstream
authentication with the imported OAuth access token, and adds `?beta=true` to the Anthropic
endpoint.

Do not send ordinary Anthropic API traffic yet unless it already contains the subscription
fields under test. The next milestone is a replay matrix for billing attribution, CCH, and
`metadata.user_id`.

For an offline captured-body check, write a re-signed copy without contacting Anthropic:

```powershell
.\claude-relay.exe sign-cch `
  -in data\capture\body.json `
  -out data\capture\body.signed.json
```
