# Server deployment

The supported production shape is one Claude Relay container, one private SQLite volume, and an
HTTPS reverse proxy. Do not scale the relay to multiple replicas: account ownership, refresh-token
rotation, sticky routing, and SQLite are intentionally single-instance boundaries.

## Start with Docker Compose

Copy the environment template and replace the API key:

```powershell
Copy-Item .env.example .env
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
-join ($bytes | ForEach-Object { $_.ToString("x2") })
```

Put the generated value in `.env`, then start the service:

```powershell
docker compose up -d --build
docker compose ps
```

The default port mapping is `127.0.0.1:8567:8567`, so the API and WebUI are not directly exposed
to the network. Open `http://127.0.0.1:8567/` on the server or publish it through an HTTPS reverse
proxy. A minimal Caddy site is:

```caddyfile
relay.example.com {
    reverse_proxy 127.0.0.1:8567
}
```

Anyone holding the shared API key can relay requests, manage accounts, and choose explicit account
aliases. Treat both the key and the WebUI as administrative access. Do not publish port 8567 to the
internet without TLS and an appropriate network boundary.

## Configuration

The image contains only non-secret defaults. Compose passes these supported runtime settings:

| Environment variable | Purpose | Compose default |
| --- | --- | --- |
| `CLAUDE_RELAY_API_KEY` | Shared API and management key | Required |
| `CLAUDE_RELAY_UPSTREAM_PROXY` | Optional outbound HTTP(S) proxy | Empty |
| `CLAUDE_RELAY_MAX_REQUEST_BYTES` | Maximum request body size | `33554432` |
| `CLAUDE_RELAY_AUTO_REFRESH_ENABLED` | Emergency global OAuth refresh switch | `true` |
| `CLAUDE_RELAY_BIND_ADDRESS` | Host-side published address | `127.0.0.1` |

`CLAUDE_RELAY_BIND_ADDRESS` affects Docker's host port only. The process listens on
`0.0.0.0:8567` inside its isolated container. On Docker Desktop, a host-side Clash proxy can
usually be addressed as `http://host.docker.internal:7890`. On a Linux server, use a reachable
proxy address rather than assuming that hostname exists.

The container runs as UID/GID `10001`, drops Linux capabilities, uses a read-only root filesystem,
and writes only to `/data` and a small temporary filesystem. The health check calls `/healthz`
inside the container and does not require the API key.

## Move the existing database

Fully stop the native `claude-relay.exe` before copying SQLite. From the repository directory,
create the named volume and copy the stopped database into it:

```powershell
docker volume create claude-relay-data
docker run --rm `
  -v claude-relay-data:/target `
  -v "${PWD}/data:/source:ro" `
  alpine:3.23 `
  sh -c "cp /source/claude-relay.db* /target/ && chown 10001:10001 /target/claude-relay.db*"
```

Then run `docker compose up -d --build`. Imported account state is preserved exactly; disabled
accounts remain disabled. Never run the native executable and container against the same database
or refresh-token chain at the same time.

## Backup and upgrade

OAuth refresh tokens live in the SQLite volume, so backups must be private. Stop the container to
obtain a simple consistent archive:

```powershell
docker compose stop
docker run --rm `
  -v claude-relay-data:/data:ro `
  -v "${PWD}:/backup" `
  alpine:3.23 `
  tar czf /backup/claude-relay-data.tar.gz -C /data .
docker compose start
```

To upgrade the locally built image without deleting the named volume:

```powershell
git pull --ff-only
docker compose up -d --build
```

`docker compose down` retains the named volume. Do not use `docker compose down -v` unless the
intent is to delete all imported credentials and routing state.
