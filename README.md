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
  ├─ bounded persistence queue: applies backpressure under load
  ├─ SQLite (WAL mode): mailbox/message metadata and indexes
  ├─ /data/messages/*.eml: original raw MIME messages
  ├─ cleanup worker: one-hour TTL and disk-cap eviction
  └─ HTTP app: browser inbox and JSON API
         ▲
         └─ HTTPS :443 through Caddy or Nginx
```

SQLite stores metadata such as recipient, sender, subject, size, receive time, expiry, and raw-message path. Raw `.eml` files preserve the original message for debugging while keeping the database small. No Postgres, Redis, queue broker, or object store is required for the initial deployment.

## Capacity assumptions

The intended target is bursts or sustained traffic in the low hundreds of messages per second, subject to message size and disk capacity. At 300 messages/second, even a 10 KB average message produces roughly 260 GB/day; a one-hour TTL is therefore an essential part of the design.

The service will use a bounded write queue and SQLite WAL mode with batched commits. If storage or persistence cannot keep up, SMTP delivery will receive a temporary failure rather than the VPS exhausting memory or disk. Before claiming a throughput target, we will benchmark on the intended VPS class.

## Configuration

Initial environment configuration:

```text
MAIL_DOMAIN=mail.example.com
DATA_DIR=/data
SMTP_ADDR=:25
HTTP_ADDR=:8080
MESSAGE_TTL=1h
MAX_MESSAGE_BYTES=10485760
MAX_STORAGE_BYTES=5368709120
```

`MAX_STORAGE_BYTES` is a hard global cap; once reached, cleanup evicts expired messages first, then oldest messages if necessary. Exact defaults will be finalized with the implementation and deployment guide.

## Deployment model

1. Provision a VPS with persistent disk and a public IPv4 address.
2. Create an `A` record for `mail.example.com` and an MX record pointing at it.
3. Allow inbound TCP 25 for SMTP and TCP 443 for the inbox UI.
4. Run tmpmail with a persistent `/data` volume.
5. Put Caddy or Nginx in front of HTTP to provide HTTPS.

The service accepts inbound mail only; reverse DNS and outbound-email reputation are not in scope.

## Implementation plan

1. Create the Go service skeleton and configuration validation.
2. Implement SMTP intake, recipient-domain validation, size limits, and durable persistence.
3. Implement SQLite schema, raw-message storage, recovery behavior, expiry cleanup, and disk-cap eviction.
4. Implement inbox UI plus JSON endpoints for inbox and message retrieval.
5. Add Docker deployment, Caddy example, health checks, metrics/logging, and VPS setup documentation.
6. Add SMTP/API/persistence/retention tests and a load-test harness; benchmark before setting a supported rate.

## Non-goals for v1

- Outbound email or reply support
- User accounts, passwords, or multi-tenant administration
- Spam filtering, antivirus, attachment previews, or full MIME rendering
- Guaranteed delivery or archival storage
- High availability across multiple machines

## Status

Planning complete. The next milestone is the single-binary implementation.
