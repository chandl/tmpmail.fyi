# tmpmail

`tmpmail` is a deliberately small, self-hosted disposable-email service for development and test environments. Point an MX record at it, send to any address on the configured domain, and inspect that inbox in a browser or through a JSON API.

It is designed to be one service on a small VPS—not a replacement for a production email provider.

## Product contract

- Accept inbound SMTP mail for any local part of one configured domain, such as `build-482@mail.example.com`.
- Never send outbound mail.
- Provide a small browser inbox and JSON API for listing and reading messages.
- Retain messages for **one hour** by default, then delete them automatically.
- Enforce per-message and global-storage limits so a traffic spike cannot exhaust the host.
- Run as a single binary (or one container), with no database service to operate.

## Architecture

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
  ├─ HTTP app: browser inbox and JSON API
	 ▲
	 └─ HTTP :8080 (public)
  └─ Prometheus listener: /metrics (optional, separate port)
         ▲
         └─ metrics :9090 (localhost by default)
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
SMTP_TLS_CERT_FILE=/certs/fullchain.pem # Optional; set both TLS paths to enable SMTP STARTTLS.
SMTP_TLS_KEY_FILE=/certs/privkey.pem
HTTP_ADDR=:8080
METRICS_ADDR=127.0.0.1:9090
MESSAGE_TTL=1h
MAX_MESSAGE_BYTES=2097152
MAX_STORAGE_BYTES=21474836480
SMTP_MAX_CONNECTIONS=100 # Global concurrent SMTP-session admission limit.
SMTP_MAX_CONNECTIONS_PER_IP=10 # Per-source SMTP-session cap; set 0 only behind a trusted SMTP proxy.
HTTP_MAX_CONCURRENT_REQUESTS=512 # Set to 0 only to disable HTTP overload shedding.
METRICS_ENABLED=false # Start the separate metrics listener when true.
HTTP_ACCESS_LOG_MODE=errors # all, errors, or off; errors avoids hot-path log pressure.
HTTP_LOG_HEADERS=User-Agent # Comma-separated request headers to include in HTTP logs.
```

`MAX_MESSAGE_BYTES` defaults to 2 MiB and `MAX_STORAGE_BYTES` defaults to 20 GiB. The global storage cap is enforced on every save: expired messages are removed first, then the oldest messages are evicted when necessary. A cleanup job also runs at startup and every minute.

SMTP allows at most 100 concurrent sessions globally and 10 sessions per source IP by default. Set `SMTP_MAX_CONNECTIONS_PER_IP=0` only when a trusted SMTP proxy makes every connection appear to originate from the same address. Set the limits according to the host's measured capacity; rejected connections receive a transient `421 4.3.2` response when possible. Successful SMTP receives log the sender IP. HTTP applies an independent 512-request concurrency limit by default and responds with `503` plus `Retry-After` under pressure.

Set both `SMTP_TLS_CERT_FILE` and `SMTP_TLS_KEY_FILE` to enable SMTP `STARTTLS`; leave both unset to retain plaintext SMTP. At startup, tmpmail verifies the certificate/key pair and requires the certificate to cover `MAIL_DOMAIN`. It advertises `STARTTLS` only when the pair is valid, requires a fresh `EHLO` after the upgrade, and supports TLS 1.2 or newer. It checks the files before each new TLS handshake, adopts a complete valid replacement, and keeps serving the last valid pair if renewal files are incomplete or invalid.

For a Docker certificate-manager sidecar, mount a dedicated `smtp-certs` volume read-only at `/certs` in tmpmail. The manager should atomically stage `fullchain.pem` and `privkey.pem` into that volume with permissions readable by the `tmpmail` container user. Do not mount any API token or the certificate manager's full working directory into tmpmail.

`MAIL_DOMAIN` is required. Only recipients at that domain are accepted; every local part is a valid disposable inbox.

## Inbox UI and message rendering

Open the UI with an inbox local part, for example `/?inbox=build-482`. The UI appends `@MAIL_DOMAIN`; do not enter a domain in the inbox field. When no inbox is selected it creates a word-based random inbox name. The last selected inbox is retained in browser local storage.

The UI provides an inbox list, pagination, refresh, random-inbox, and copy-address controls. It displays browser-local timestamps and clearly separates message headers from the body.

Messages are stored unchanged as raw `.eml` files. For the reader, tmpmail parses MIME multipart messages and displays the HTML part by default when one is present, with a **View plain text** option. HTML is sanitized and rendered in a sandboxed iframe; links open in a new tab, while scripts, forms, and remote images remain blocked.

## Local development

The application uses Go 1.27.0 and a C compiler for local development builds, or Docker for a runtime that does not have either installed.

```sh
# Edit MAIL_DOMAIN in compose.yaml.
docker compose up --build
```

Send SMTP mail to `localhost:25`; open `http://localhost:8080/?inbox=build` to inspect it. The browser UI appends the configured mail domain automatically.

To run without Docker, provide `MAIL_DOMAIN` and a writable data directory:

```sh
MAIL_DOMAIN=mail.example.com DATA_DIR=./data SMTP_ADDR=:2525 go run ./cmd/tmpmail
```

The API is intentionally small:

```text
GET /healthz
GET /api/v1/inboxes/{full-recipient-address}?limit=25&offset=0
GET /api/v1/messages/{message-id}
GET /openapi.json
```

## Metrics and logs

Set `METRICS_ENABLED=true` to start a dedicated Prometheus listener at `METRICS_ADDR`, which defaults to `127.0.0.1:9090`. It serves only `GET /metrics`; `GET /metrics` on the public UI/API listener returns `404`.

The Compose file changes `METRICS_ADDR` to `:9090`, but does not publish it to the host, so a Prometheus container on the private Compose network can scrape `tmpmail:9090`. Do not add a public `9090` port mapping. Metrics cover SMTP admission/outcomes, accepted bytes, active sessions, and limit rejections; normalized HTTP request counts, outcome-class latency, response bytes, in-flight requests, cancellations, and overload rejections; cleanup activity; current stored message/byte usage; SQLite pool state; and storage or cleanup errors. Storage metrics distinguish persistence stages from writer-lock wait. Alert on sustained SMTP or HTTP admission rejections, storage lock wait, and storage errors.

tmpmail logs successful SMTP receives and cleanup work. HTTP request logging defaults to errors only, avoiding a synchronous log write for every successful request during a traffic burst; set `HTTP_ACCESS_LOG_MODE=all` for temporary diagnostics or `off` when an edge proxy supplies access logs. HTTP entries include `method`, `path`, `status`, and `duration_ms`, plus the comma-separated request-header allowlist in `HTTP_LOG_HEADERS`. Do not use forwarding headers for SMTP admission: source-IP limits are deliberately not implemented.

The complete contract is in [openapi.yaml](openapi.yaml).

The Go HTTP models and server interface are generated from this specification with
`go tool oapi-codegen --config oapi-codegen.yaml openapi.yaml`. Generated code is
checked in; regenerate it whenever the API contract changes.

## Deployment model

1. Provision a VPS with persistent disk and a public IPv4 address.
2. Create an `A` record for `mail.example.com` and an MX record pointing at it.
3. Allow inbound TCP 25 for SMTP and TCP 8080 (or a reverse proxy on 443) for the inbox UI. Do not allow inbound TCP 9090.
4. Run tmpmail with a persistent `/data` volume.
5. Open the inbox UI at `http://your-vps:8080`. Add a reverse proxy later if you want HTTPS on port 443.

The service accepts inbound mail only; reverse DNS and outbound-email reputation are not in scope.

## Docker

Docker is a packaging and deployment convenience, not an additional service dependency. The production shape remains one `tmpmail` process plus a persistent local data directory.

### Image

- Use a multi-stage build: compile the CGO SQLite build in the builder, then copy the binary into a small non-root Alpine runtime image.
- Include no database server or application runtime in the final image.
- Expose TCP `25` for SMTP, `8080` for HTTP, and `9090` for the optional metrics listener. `EXPOSE` does not publish a port.
- Add a health endpoint and Docker `HEALTHCHECK` that verifies the HTTP process is responsive.
- Run as an unprivileged user. The container listens on port 2525 and Docker maps host port 25 to it, avoiding the need for a privileged port capability.

### Compose deployment

The included `compose.yaml` contains one service:

```text
tmpmail
  image: ghcr.io/chandl/tmpmail.fyi:latest
  ports: 25:2525, 8080:8080
  volumes: tmpmail-data:/data
  environment: MAIL_DOMAIN, addresses, TTL, limits, and optional metrics settings
  restart: unless-stopped
```

The named volume holds both `mail.db` and raw `.eml` message files, so container recreation does not remove mail. Configuration is kept directly in `compose.yaml` for this minimal deployment.

### HTTPS (optional)

The included Compose file serves the UI directly on port `8080` over HTTP. To use HTTPS, place your preferred reverse proxy in front of `http://127.0.0.1:8080`; certificate management is deliberately outside this minimal deployment.

### Operations

- Back up the mounted data volume (or at minimum `mail.db`) only if short-lived debugging history is valuable; the one-hour TTL means backups are optional.
- Limit container memory and disk through host/volume monitoring; application-level limits remain the primary safeguard.
- Use `docker compose pull && docker compose up -d` for upgrades after providing a tagged image.
- Do not expose port `8080` directly to the public Internet when using an HTTPS reverse proxy.

## Container publishing

GitHub Actions tests every pull request and builds multi-architecture (`linux/amd64`, `linux/arm64`) container images. Pushes to `main` publish both images with `latest`, branch, SHA, and automatically assigned `v1.0.<workflow-run>` tags, then create a GitHub Release using that version tag and generated release notes. Version tags such as `v0.1.0` also publish matching container-image tags. The workflow is [`.github/workflows/container.yml`](.github/workflows/container.yml).

## Status

The service, unit tests, container build, Compose configuration, and container-publishing workflow are present. Before relying on a particular messages-per-second target, run a load test on the intended VPS and tune based on observed disk and SQLite performance.
