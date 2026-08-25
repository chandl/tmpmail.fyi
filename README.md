# tmpmail

`tmpmail` is a deliberately small, self-hosted disposable-email service for development and test environments. Point an MX record at it, send to any address on the configured domain, and inspect that inbox in a browser or through a JSON API.

It is designed to be one service on a small VPS—not a replacement for a production email provider.

## Product contract

- Accept inbound SMTP mail for any local part of one configured domain, such as `build-482@mail.example.com`.
- Never send outbound mail.
- Provide a small browser inbox and JSON API for listing and reading messages.
- Retain messages for **one hour** by default, then delete them automatically.
- Enforce message-size, mailbox-size, and global-disk limits so a traffic spike cannot exhaust the host.
- Run as a single binary (or one container), with no database service to operate.

## Proposed architecture

```text
Internet sender
  │ SMTP :25
  ▼
tmpmail (one Go process)
  ├─ SMTP receiver: validates recipient domain and applies limits
  ├─ serialized persistence path: durable writes before SMTP acknowledgement
  ├─ SQLite (WAL mode): mailbox/message metadata and indexes
  ├─ /data/messages/*.eml: original raw MIME messages
  ├─ cleanup worker: one-hour TTL and disk-cap eviction
  └─ HTTP app: browser inbox and JSON API
         ▲
         └─ HTTPS :443 through Traefik
```

SQLite stores metadata such as recipient, sender, subject, size, receive time, expiry, and raw-message path. It uses the `github.com/mattn/go-sqlite3` driver, compiled into the application during the Docker build. Raw `.eml` files preserve the original message for debugging while keeping the database small. No Postgres, Redis, queue broker, or object store is required for the initial deployment.

## Capacity assumptions

The intended target is bursts or sustained traffic in the low hundreds of messages per second, subject to message size and disk capacity. At 300 messages/second, even a 10 KB average message produces roughly 260 GB/day; a one-hour TTL is therefore an essential part of the design.

The service uses SQLite WAL mode and serializes writes before SMTP acknowledgement. A production throughput claim still requires a benchmark on the intended VPS class; if that measurement shows writes are the bottleneck, the next optimization is a bounded, batched persistence queue. Storage limits prevent the service from accepting messages larger than the available retention budget.

## Configuration

Initial environment configuration:

```text
MAIL_DOMAIN=mail.example.com
DATA_DIR=/data
SMTP_ADDR=:25
HTTP_ADDR=:8080
MESSAGE_TTL=1h
MAX_MESSAGE_BYTES=2097152
MAX_STORAGE_BYTES=21474836480
```

`MAX_STORAGE_BYTES` is a hard global cap; once reached, cleanup evicts expired messages first, then oldest messages if necessary. Exact defaults will be finalized with the implementation and deployment guide.

## Local development

The application uses Go 1.27.0 and a C compiler for local development builds, or Docker for a runtime that does not have either installed.

```sh
cp .env.example .env
# edit MAIL_DOMAIN in .env
docker compose up --build
```

Send SMTP mail to `localhost:25`; open `https://mail.example.com/?inbox=build` to inspect it once Traefik has issued the certificate. The browser UI appends the configured mail domain automatically. For local HTTP-only development, temporarily add `127.0.0.1:8080:8080` to the `tmpmail` port mappings.

The API is intentionally small:

```text
GET /healthz
GET /api/inboxes/{full-recipient-address}
GET /api/messages/{message-id}
GET /openapi.json
```

The complete contract is in [openapi.yaml](openapi.yaml).

The Go HTTP models and server interface are generated from this specification with
`go tool oapi-codegen --config oapi-codegen.yaml openapi.yaml`. Generated code is
checked in; regenerate it whenever the API contract changes.

## Deployment model

1. Provision a VPS with persistent disk and a public IPv4 address.
2. Create an `A` record for `mail.example.com` and an MX record pointing at it.
3. Allow inbound TCP 25 for SMTP and TCP 443 for the inbox UI.
4. Run tmpmail with a persistent `/data` volume.
5. Run Traefik to provide HTTPS and forward web traffic to tmpmail.

The service accepts inbound mail only; reverse DNS and outbound-email reputation are not in scope.

## Docker plan

Docker is a packaging and deployment convenience, not an additional service dependency. The production shape remains one `tmpmail` process plus a persistent local data directory.

### Image

- Use a multi-stage build: compile a static Linux Go binary in the build stage, then copy it into a small non-root Alpine runtime image.
- Include no database server or application runtime in the final image.
- Expose TCP `25` for SMTP and TCP `8080` for the internal HTTP server.
- Add a health endpoint and Docker `HEALTHCHECK` that verifies the HTTP process is responsive.
- Run as an unprivileged user. The container listens on port 2525 and Docker maps host port 25 to it, avoiding the need for a privileged port capability.

### Compose deployment

The initial `compose.yaml` will contain only:

```text
traefik
  image: traefik
  ports: 80:80, 443:443
  volumes: read-only Docker socket, traefik-data:/letsencrypt

tmpmail
  image: tmpmail
  ports: 25:2525
  volumes: tmpmail-data:/data
  labels: HTTPS router for MAIL_DOMAIN
  environment: MAIL_DOMAIN, MESSAGE_TTL=1h, storage and message limits
  restart: unless-stopped
```

The named volume holds both `mail.db` and raw `.eml` message files, so container recreation does not remove mail. Configuration belongs in an `.env` file that is excluded from Git.

### HTTPS with Traefik

The Compose deployment includes Traefik. It discovers tmpmail from Docker labels, obtains and renews a Let's Encrypt certificate using the configured `TRAEFIK_ACME_EMAIL`, and forwards HTTPS traffic internally to port `8080`. Tmpmail's HTTP port is not exposed on the host.

Traefik needs read-only access to the Docker socket for service discovery and a persistent `traefik-data` volume for ACME certificate state. Port 80 is exposed for normal HTTP reachability; port 443 serves the inbox UI.

```text
Internet
  ├─ TCP 25 ──────────────────────► tmpmail SMTP
  └─ TCP 80/443 ─► Traefik ──────► tmpmail HTTP on Docker network
```

### Operations

- Back up the mounted data volume (or at minimum `mail.db`) only if short-lived debugging history is valuable; the one-hour TTL means backups are optional.
- Limit container memory and disk through host/volume monitoring; application-level limits remain the primary safeguard.
- Use `docker compose pull && docker compose up -d` for upgrades after providing a tagged image.
- Do not expose port `8080` directly to the public Internet in production.

## Container publishing

GitHub Actions tests every pull request and builds a multi-architecture (`linux/amd64`, `linux/arm64`) container image. Pushes to `main` publish `ghcr.io/<owner>/tmpmail:latest` and a branch/SHA tag; version tags such as `v0.1.0` also publish the matching version tag. The workflow is [`.github/workflows/container.yml`](.github/workflows/container.yml).

## Implementation plan

1. Create the Go service skeleton and configuration validation.
2. Implement SMTP intake, recipient-domain validation, size limits, and durable persistence.
3. Implement SQLite schema, raw-message storage, recovery behavior, expiry cleanup, and disk-cap eviction.
4. Implement inbox UI plus JSON endpoints for inbox and message retrieval.
5. Add the multi-stage Dockerfile, Compose configuration, Traefik labels, health checks, metrics/logging, and VPS setup documentation.
6. Add SMTP/API/persistence/retention tests and a load-test harness; benchmark before setting a supported rate.

## Non-goals for v1

- Outbound email or reply support
- User accounts, passwords, or multi-tenant administration
- Spam filtering, antivirus, attachment previews, or full MIME rendering
- Guaranteed delivery or archival storage
- High availability across multiple machines

## Status

Initial implementation is present. It still needs a Go or Docker-capable environment for compile, integration, and load verification.
