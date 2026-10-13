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

The service uses SQLite WAL mode and serializes writes before SMTP acknowledgement. Expiry cleanup uses bounded transaction batches, and oldest-message eviction uses an arrival-order index. A production throughput claim still requires a benchmark on the intended VPS class; if that measurement shows writes are the bottleneck, the next optimization is a bounded, batched persistence queue. Storage limits prevent the service from accepting messages larger than the available retention budget.

## Configuration

Initial environment configuration:

```text
MAIL_DOMAIN=mail.example.com
DATA_DIR=/data
SMTP_ADDR=:25
SMTP_TLS_CERT_FILE=/certs/fullchain.pem # Optional; set both TLS paths to enable SMTP STARTTLS.
SMTP_TLS_KEY_FILE=/certs/privkey.pem
HTTP_ADDR=:8080
ADMIN_ENABLED=false
ADMIN_ADDR=127.0.0.1:8081
ANALYTICS_ENABLED=false
ANALYTICS_EVENT_TTL=720h
ANALYTICS_MAX_STORAGE_BYTES=1073741824
METRICS_ADDR=127.0.0.1:9090
MESSAGE_TTL=1h
MAX_MESSAGE_BYTES=2097152
MAX_STORAGE_BYTES=21474836480
SMTP_MAX_CONNECTIONS=100 # Global concurrent SMTP-session admission limit.
SMTP_MAX_CONNECTIONS_PER_IP=10 # Per-source SMTP-session cap; set 0 only behind a trusted SMTP proxy.
SMTP_MAX_RECIPIENTS=5 # Per-message recipient cap.
HTTP_MAX_CONCURRENT_REQUESTS=512 # Set to 0 only to disable HTTP overload shedding.
METRICS_ENABLED=false # Start the separate metrics listener when true.
HTTP_ACCESS_LOG_MODE=errors # all, errors, or off; errors avoids hot-path log pressure.
HTTP_LOG_HEADERS=User-Agent # Comma-separated request headers to include in HTTP logs.
HTTP_RATE_LIMIT_RPS=10 # Sustained per-client-IP request rate.
HTTP_RATE_LIMIT_BURST=50 # Per-client-IP burst allowance.
HTTP_RATE_LIMIT_IP_HEADER= # e.g. CF-Connecting-IP; see the rate-limit paragraph below.
```

`MAX_MESSAGE_BYTES` defaults to 2 MiB and `MAX_STORAGE_BYTES` defaults to 20 GiB. When a delivery needs space, expired messages are evicted before the oldest live messages. Incoming metadata and required evictions commit atomically, so an eviction error cannot leave a failed delivery readable. A cleanup job also runs at startup and every minute, releasing the writer between bounded batches.

The storage cap charges unique raw message blobs and files awaiting deletion. Deletion failures remain charged and are retried on later saves, cleanup, and startup. A save stages its durable raw file before the database transaction, so disk usage can temporarily exceed the cap by that incoming file's size; if deletion failures leave usage above the cap, further saves are rejected until space is reclaimed. SQLite metadata, WAL files, and filesystem overhead are outside this raw-file budget: reserve additional disk space and monitor the volume. If an orphan cannot be recorded or removed, the service blocks further saves until restart and reconciliation.

Startup reconciliation removes stale incoming files and unreferenced raw messages through the deletion queue. A referenced blob that is missing causes startup to fail with an error rather than silently dropping accepted mail. Raw files and their containing directory are synced before a FULL-sync SQLite commit and SMTP acknowledgement.

SMTP allows at most 100 concurrent sessions globally, 10 sessions per source IP, and 5 recipients per message by default. Set `SMTP_MAX_CONNECTIONS_PER_IP=0` only when a trusted SMTP proxy makes every connection appear to originate from the same address. Raise `SMTP_MAX_RECIPIENTS` only for a known workload: each accepted recipient creates a separate stored message. Set the limits according to the host's measured capacity; rejected connections receive a transient `421 4.3.2` response when possible. Successful SMTP receives log the sender IP. HTTP applies an independent 512-request concurrency limit by default and responds with `503` plus `Retry-After` under pressure.

HTTP also rate-limits each client to `HTTP_RATE_LIMIT_RPS` requests/second (default 10, burst `HTTP_RATE_LIMIT_BURST`, default 50), responding `429` plus `Retry-After` beyond that. `GET` and `HEAD /healthz` bypass this per-client bucket so local proxy traffic cannot starve health probes; the global concurrency limit still applies. The client IP is `RemoteAddr` by default. Set `HTTP_RATE_LIMIT_IP_HEADER` (for example `CF-Connecting-IP`) only when a trusted reverse proxy sets/overwrites that header itself and the origin is not reachable except through that proxy (e.g. firewalled to the proxy's IP ranges) — otherwise a client can set the header directly and bypass or misattribute the limit.

Set both `SMTP_TLS_CERT_FILE` and `SMTP_TLS_KEY_FILE` to enable SMTP `STARTTLS`; leave both unset to retain plaintext SMTP. At startup, tmpmail verifies the certificate/key pair and requires the certificate to cover `MAIL_DOMAIN`. It advertises `STARTTLS` only when the pair is valid, requires a fresh `EHLO` after the upgrade, and supports TLS 1.2 or newer. It checks the files before each new TLS handshake, adopts a complete valid replacement, and keeps serving the last valid pair if renewal files are incomplete or invalid.

For a Docker certificate-manager sidecar, mount a dedicated `smtp-certs` volume read-only at `/certs` in tmpmail. The manager should atomically stage `fullchain.pem` and `privkey.pem` into that volume with permissions readable by the `tmpmail` container user. Do not mount any API token or the certificate manager's full working directory into tmpmail.

`MAIL_DOMAIN` is required. Only recipients at that domain are accepted; every local part is a valid disposable inbox. Recipient domains are normalized to lowercase for SMTP delivery and API lookup, so `build@MAIL.EXAMPLE.COM` and `build@mail.example.com` reach the same inbox. Local-part case is preserved: `Build` and `build` are separate inboxes. Existing stored recipient domains are normalized when the service starts.

## Inbox UI and message rendering

Open the UI with an inbox local part, for example `/?inbox=build-482`. The UI appends `@MAIL_DOMAIN`; the address field only edits the name (pasting a full address on the configured domain also works). When no inbox is selected it creates a word-based random inbox name. The last selected inbox and the light/dark theme choice are retained in browser local storage.

The UI is a two-pane mail reader (list first on small screens) with pagination, refresh, new-address, and copy-address controls. The initial page renders only the selected message body, defaulting to the newest message; other bodies load when selected. Message links include a `message` query parameter so reading also works without JavaScript. While the tab is visible it checks the first page every 10 seconds and reloads when its ordered message IDs or pagination state change. This removes expired entries, keeps the displayed page bounded, and handles bursts larger than a page. The selected message is retained when it remains on that page; otherwise selection falls back to the newest message. Older pages pause automatic checks. It displays browser-local timestamps, keeps original headers behind a disclosure, and supports `↑`/`↓` (or `j`/`k`) to move between messages and `R` to refresh. Templates, CSS, and JS live in `internal/app/assets/ui` and are embedded in the binary; there is no frontend build step.

Messages are stored unchanged as raw `.eml` files. For the reader, tmpmail parses MIME multipart messages and displays the HTML part by default when one is present, with a **Formatted / Plain text** toggle. Text parts are converted from their declared character set to UTF-8 after transfer decoding; missing or unknown character sets use UTF-8 with invalid bytes replaced. Attachment downloads preserve their transfer-decoded bytes. HTML is sanitized and rendered in a sandboxed iframe; links open in a new tab, while scripts, forms, and remote images remain blocked. Blocked images are drawn as placeholders and counted above the message (1–2px images are flagged as tracking pixels). In the dark theme an optional **Dark preview** approximates dark mode by inverting the email's colors. Each message also offers copyable `curl` commands for the JSON API.

## Local development

The application uses Go 1.27.0 and a C compiler for local development builds, or Docker for a runtime that does not have either installed.

```sh
# Create your local runtime configuration, then set MAIL_DOMAIN in .env.
cp .env.example .env
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

## Private admin dashboard

Enable `ADMIN_ENABLED=true` for a separate admin UI/API on `127.0.0.1:8081`.
Enable `ANALYTICS_ENABLED=true` to collect metadata in `DATA_DIR/analytics.db`,
using its own SQLite WAL connection pool. The public listener and metrics listener
keep their existing roles. The two admin pages are **Overview** and **Activity**:
process/runtime, SMTP capacity and TLS, mail storage and cleanup, delivery/request
counts, traffic buckets, rankings, and a filtered, paginated metadata log.

The dashboard refreshes every five seconds while visible. Use the Live control
to pause updates or Refresh to fetch a new snapshot manually. Traffic uses labeled
count/time axes and per-interval details. Runtime and analytics details appear in a compact service panel
on Overview. The service panel also shows effective startup limits,
client-IP attribution, mail expiration, logging, and metrics settings; hover a
setting to see its environment variable name. Activity shows history freshness
without repeating runtime details and uses compact table rows with expandable metadata.
Its event labels match the filter options: SMTP delivery, SMTP rejection, HTTP
request, and HTTP poll; HTTP · all types includes both HTTP types.
Its field filters distinguish exact inbox/domain/IP matches from sender/user-agent
text matches and apply only when submitted; removable chips show active filters,
and Clear all restores the default view. Response-code filtering covers HTTP statuses
and SMTP accepted deliveries (250). SMTP rejections recorded by the application
include connection overload (421), invalid recipient domains (550), oversized DATA
(552), read failures (554), and storage failures (451). DATA failures are recorded
per accepted recipient; connection failures may have no sender or recipient.
Protocol-parser rejections before application callbacks are not recorded. Response times across HTTP, SMTP deliveries, and SMTP rejections can be sorted
fastest or slowest first. SMTP delivery duration covers DATA reading and storage,
shared by recipients in that transaction. Older deliveries without timing sort last. Live refresh preserves unapplied edits.

The public header, inbox introduction, and privacy page display the configured
`MESSAGE_TTL` in readable units, using the same lifetime as message storage.

Admin access has **no application authentication**. A separate port is only a
network boundary: keep it private using loopback, a private network, or an SSH
tunnel. For Docker, set `ADMIN_ADDR=:8081` inside the container and uncomment only
`127.0.0.1:8081:8081` in Compose. Never publish it on all host interfaces or route
it through the public proxy. Other containers on the same Docker network can reach
that container listener, so use a trusted Compose network. On remote hosts, use
`ssh -L 8081:127.0.0.1:8081 your-host` and open `http://127.0.0.1:8081` locally.

Detailed events default to `ANALYTICS_EVENT_TTL=720h` (30 days); shorter positive
retention is allowed, values above 30 days are rejected. Analytics history survives
mail expiry. `ANALYTICS_MAX_STORAGE_BYTES` defaults to 1 GiB. Cleanup removes expired
events and evicts the oldest retained events under storage pressure. This is an
application storage budget, not an exact filesystem quota: SQLite indexes, free
pages, checkpointing, WAL and temporary growth need disk headroom. Thirty days is
a maximum, never guaranteed available history. Overview shows the configured TTL,
earliest retained timestamp, early evictions, storage and ingestion status.

Collection is best-effort through one bounded in-memory queue and a background
batch writer. Queue overflow or bounded write retries drop events without rejecting
mail or public requests. Restart/crash losses are possible and may be uncounted;
there is no outbox, cross-database transaction, or historical rollup. Dropped-event
and ingestion freshness indicators help identify incomplete or stale history.

A delivery means one successfully persisted recipient: three accepted recipients
produce three events. SMTP source IPs and HTTP caller IPs have separate meanings;
IPs are callers, not verified users. Envelope senders and raw bounded user agents
are unverified claims. Automatic inbox polls carry `X-Tmpmail-Poll: 1`; this
identifiable but spoofable marker separates polling from other HTTP requests,
which are not necessarily intentional user actions. HTTP attribution uses the same
explicit trusted-proxy header policy as rate limiting. Analytics never stores MIME
bodies, attachments, arbitrary headers or query strings. Admin traffic is excluded
from product HTTP metrics and event collection. Keep the data volume persistent
and protect analytics metadata as sensitive operational data.

## Metrics and logs

Set `METRICS_ENABLED=true` to start a dedicated Prometheus listener at `METRICS_ADDR`, which defaults to `127.0.0.1:9090`. It serves only `GET /metrics`; `GET /metrics` on the public UI/API listener returns `404`.

The Compose file changes `METRICS_ADDR` to `:9090`, but does not publish it to the host, so a Prometheus container on the private Compose network can scrape `tmpmail:9090`. Do not add a public `9090` port mapping. Metrics cover SMTP admission/outcomes, accepted bytes, active sessions, protocol sessions labeled by TLS state, delivery rejections, and `tmpmail_smtp_tls_certificate_not_after_timestamp`; normalized HTTP request counts, outcome-class latency, response bytes, in-flight requests, cancellations, and overload rejections; cleanup activity; current stored message/byte usage; SQLite pool state; and storage or cleanup errors. Storage metrics distinguish persistence stages from writer-lock wait. Alert on sustained SMTP or HTTP admission rejections, certificate expiry, storage lock wait, and storage errors.

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
- Add a health endpoint and Docker `HEALTHCHECK` that requests `/healthz` on the configured `HTTP_ADDR` with a two-second timeout and requires HTTP `204`. Wildcard bind addresses are probed through loopback; redirects and error responses fail the check.
- Run as an unprivileged user. The container listens on port 2525 and Docker maps host port 25 to it, avoiding the need for a privileged port capability.

### Compose deployment

The included `compose.yaml` contains one service:

```text
tmpmail
  image: ghcr.io/chandl/tmpmail.fyi:latest
  ports: 25:2525, 8080:8080
  volumes: tmpmail-data:/data
  env_file: .env
  restart: unless-stopped
```

The named volume holds `mail.db`, optional `analytics.db` (including its WAL), and raw `.eml` message files, so container recreation does not remove mail. Copy [`.env.example`](.env.example) to `.env` and edit it for the deployment; `.env` is ignored by Git. The Compose file accepts a missing `.env` for inspection, but tmpmail requires `MAIL_DOMAIN`, so create it before starting the service.

### HTTPS (optional)

The included Compose file serves the UI directly on port `8080` over HTTP. To use HTTPS, place your preferred reverse proxy in front of `http://127.0.0.1:8080`; or enable built-in automatic TLS with the standalone deployment below.

The private admin overview includes separate **Web TLS** and **SMTP TLS** cards showing automatic/manual/off mode, configured hostname, readiness, expiry and time remaining. Staging certificates and certificates expiring within seven days are highlighted. Expand certificate details to inspect the issuer, validity start, DNS names and SHA-256 fingerprint. These details refresh with the existing admin polling, read active certificate state without starting ACME requests, and exclude private keys, file paths, account credentials and contact email.

### Manual HTTPS certificates (optional)

Supply a certificate chain and private key readable by tmpmail, and disable automatic web TLS:

```dotenv
AUTO_TLS_WEB_ENABLED=false
AUTO_TLS_SMTP_ENABLED=false
WEB_DOMAIN=inbox.example.com
HTTPS_TLS_CERT_FILE=/certs/web-fullchain.pem
HTTPS_TLS_KEY_FILE=/certs/web-privkey.pem
```

Use `docker compose -f compose.tls.yaml up -d --build` for the public port mappings. Stage the files in the existing read-only `/certs` mount (the `smtp-certs` volume), or replace that mount with a read-only directory containing your certificate files. Both HTTPS file settings are required together. `WEB_DOMAIN` defaults to `MAIL_DOMAIN`; the certificate must cover that hostname and be currently valid. `HTTPS_ADDR` controls the HTTPS listener (default `:8443`). HTTP redirects to HTTPS and retains the local health endpoint.

Manual HTTPS uses the same file reload mechanism as SMTP: a complete valid replacement is adopted on the next handshake, while incomplete or invalid replacements leave the last valid pair active. An external certificate manager must handle issuance and renewal. Expired HTTPS certificates fail handshakes and health checks. No CA account, staging setting or terms acceptance is needed when both automatic TLS toggles are disabled.

Manual web certificates can be combined with automatic SMTP TLS: set `AUTO_TLS_SMTP_ENABLED=true` and `ACME_ACCEPT_TOS=true`, keeping `AUTO_TLS_WEB_ENABLED=false`. HTTP validation remains available for the mail hostname, and no automatic certificate is requested for a distinct manual web hostname. You can also supply manual SMTP certificates using the existing SMTP file settings, independently of the web mode. Automatic and manual certificates cannot both be configured for the same service.

### Built-in automatic TLS (optional)

Copy `.env.example` to `.env`, set the domain and enable automatic TLS:

```dotenv
MAIL_DOMAIN=mail.example.com
AUTO_TLS_ENABLED=true
ACME_ACCEPT_TOS=true
ACME_EMAIL=admin@example.com
# Optional separate UI/API hostname; otherwise MAIL_DOMAIN is used:
# WEB_DOMAIN=inbox.example.com
```

Leave `SMTP_TLS_CERT_FILE` and `SMTP_TLS_KEY_FILE` empty; manual SMTP certificate files and automatic SMTP TLS are mutually exclusive. Manual SMTP certificates can be used alongside automatic web TLS. `MAIL_DOMAIN` must also be the SMTP hostname used by the MX record when automatic SMTP TLS is enabled. Hostnames with automatic TLS enabled must have public A/AAAA records pointing to the host; any published IPv6 address must also work. Allow public TCP 25, 80, and 443. Keep port 80 reachable for renewals. No DNS API credentials are needed.

```sh
docker compose -f compose.tls.yaml up -d --build
```

`compose.tls.yaml` is a standalone alternative to `compose.yaml`, not an override. It maps public ports 80/443 to unprivileged container listeners 8080/8443 and does not publish port 8080 separately. Stop the old Compose deployment before switching, retaining its data volume. Use the same Compose project name so the existing `tmpmail-data` volume is reused. Open `https://mail.example.com` (or the configured `WEB_DOMAIN`).

| Setting | Default | Purpose |
| --- | --- | --- |
| `AUTO_TLS_ENABLED` | `false` | Default for both service toggles. |
| `AUTO_TLS_WEB_ENABLED` | `AUTO_TLS_ENABLED` | Override automatic HTTPS for the UI/API. |
| `AUTO_TLS_SMTP_ENABLED` | `AUTO_TLS_ENABLED` | Override automatic SMTP STARTTLS. |
| `WEB_DOMAIN` | `MAIL_DOMAIN` | Optional separate UI/API hostname. |
| `ACME_ACCEPT_TOS` | `false` | Required acceptance of the CA subscriber terms in automatic mode. |
| `ACME_EMAIL` | Empty | Optional ACME account contact address; changing it does not update an already registered account. |
| `ACME_DEBUG` | `false` | Log ACME request/response metadata for troubleshooting. Network errors and HTTP error statuses are logged even when disabled. |
| `ACME_STAGING` | `false` | Use the Let's Encrypt test environment; its certificates are not publicly trusted. |
| `ACME_CACHE_DIR` | `DATA_DIR/acme` | Persistent certificate and ACME account storage, separated into production/staging directories. |
| `HTTP_ADDR` | `:8080` | Challenges, health checks, and redirects in automatic mode. |
| `HTTPS_TLS_CERT_FILE` / `HTTPS_TLS_KEY_FILE` | Empty | Manual HTTPS certificate chain and private key; incompatible with automatic web TLS. |
| `HTTPS_ADDR` | `:8443` | HTTPS application listener; expose it publicly on port 443. |

For example, keep HTTPS but disable automatic SMTP TLS:

```dotenv
AUTO_TLS_ENABLED=true
AUTO_TLS_WEB_ENABLED=true
AUTO_TLS_SMTP_ENABLED=false
```

For SMTP-only automatic TLS, use `AUTO_TLS_WEB_ENABLED=false` and `AUTO_TLS_SMTP_ENABLED=true`. The UI/API continues serving HTTP, and the HTTP listener answers certificate challenges. No HTTPS listener or redirects are started in that mode; keep public port 80 reachable for SMTP certificate validation. Set both overrides to `false` to disable automatic TLS entirely. Disabling automatic SMTP TLS preserves manually supplied SMTP certificates, or plaintext SMTP if none are configured. Only enabled services' hostnames are requested; no certificate is requested for a disabled service's distinct hostname. A valid web certificate allows the UI to open even if SMTP provisioning fails, and vice versa.

Autocert handles issuance and lifetime-based renewal. HTTP-01 validation is enabled; autocert may also use TLS-ALPN-01 on port 443. The service requests only configured hostnames. SMTP uses an RSA certificate for broad client compatibility and supports clients without TLS SNI; HTTPS uses the configured web hostname's certificate. Certificates are served from memory, with renewal updates adopted within a minute, and persisted atomically by autocert. No manual PEM installation or restart is needed.

At first start, HTTP and HTTPS listeners run so validation can complete, but normal TLS handshakes fail until the web certificate is available. With web auto TLS enabled, HTTP visitors see a loading page with status 503 that refreshes every five seconds; once the web certificate is ready, the same request redirects to HTTPS with its path and query preserved. Pending pages and redirects are not cached, so visitors can see the loading page again if a certificate expires. Each service becomes available independently: SMTP with automatic TLS waits only for its mail certificate, and HTTPS redirects wait only for the web certificate. SMTP without automatic TLS starts normally. `/healthz` on the HTTP listener returns 503 while provisioning is pending and 204 when the certificates are ready. Initial issuance failures retry with backoff capped at one hour. During renewal failures, the current valid certificates remain active; expired certificates fail TLS handshakes and make health checks unhealthy. STARTTLS remains advertised after expiry, and TLS upgrades fail rather than downgrade. As with the existing SMTP receiver, senders may choose plaintext SMTP; automatic mode does not require encryption for inbound delivery. Private metrics and admin listeners remain private and use their existing configuration.

Try `ACME_STAGING=true` first to verify DNS and port routing, then switch it to `false` and recreate the container for trusted certificates. Do not repeatedly delete the cache: it holds private keys and account state, avoids unnecessary issuance, and should be preserved across upgrades. Its directory is created with mode 0700 and cache files with mode 0600; protect backups accordingly. This deployment supports one instance owning the cache. If another proxy already owns ports 80/443, use the existing manual certificate/reverse proxy deployment instead.

To troubleshoot provisioning, watch `docker compose -f compose.tls.yaml logs -f --tail=200 tmpmail`. Startup logs show the CA environment, configured domains and cache directory. Pending attempts log their start, progress every 30 seconds, completion and next retry time. Network errors and ACME HTTP error statuses also appear during background renewal. For more detail, set `ACME_DEBUG=true` in `.env`, recreate the container with `docker compose -f compose.tls.yaml up -d --build --force-recreate`, and follow the logs again. Debug logging includes methods, endpoints, status codes and durations, but excludes account keys, request/response bodies, headers and URL queries. Turn it off after troubleshooting.

Check public A/AAAA records for **every** hostname with automatic TLS enabled, port mappings, firewall rules, and any proxy in front of the server. The public `/healthz` endpoint reports 503 until all certificates are ready. A request to `http://HOST/.well-known/acme-challenge/probe` should reach tmpmail and return 404 for the nonexistent token, without authentication or a redirect to HTTPS. HTTPS handshake failures are expected before initial issuance completes. Staging certificates are not trusted by browsers; use production once validation works. Do not delete the certificate cache to retry.

Dependency license texts are included in `THIRD_PARTY_NOTICES` and copied with the project license to `/usr/share/licenses/tmpmail/` in the container. Include those files when distributing a standalone binary.

### Operations

- Back up the mounted data volume (or at minimum `mail.db`) only if short-lived debugging history is valuable; the one-hour TTL means backups are optional.
- Limit container memory and disk through host/volume monitoring; application-level limits remain the primary safeguard.
- Use `docker compose pull && docker compose up -d` for upgrades after providing a tagged image.
- Do not expose port `8080` directly to the public Internet when using an HTTPS reverse proxy.

## Container publishing

GitHub Actions tests every pull request and builds multi-architecture (`linux/amd64`, `linux/arm64`) container images. Pushes to `main` publish both images with `latest`, branch, SHA, and automatically assigned `v1.0.<workflow-run>` tags, then create a GitHub Release using that version tag and generated release notes. Version tags such as `v0.1.0` also publish matching container-image tags. The workflow is [`.github/workflows/container.yml`](.github/workflows/container.yml).

## Status

The service, unit tests, container build, Compose configuration, and container-publishing workflow are present. Before relying on a particular messages-per-second target, run a load test on the intended VPS and tune based on observed disk and SQLite performance.
