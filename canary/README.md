# Canary

`canary` is a small standalone Go service that continuously performs the tmpmail end-to-end contract: it sends a unique SMTP message, finds that exact message in the recipient inbox API, fetches it from the message API, and verifies recipient, sender, subject, and body. It then loads the browser inbox page and verifies that the probe body is rendered and the UI assets are cache-busted. It exposes JSON health on `GET /healthz`; it returns `200` when all checks pass and `503` when a check fails. `GET /livez` always returns `200` while the process is running.

## Run

```sh
cd canary
CANARY_MAIL_DOMAIN=mail.example.com \
CANARY_SMTP_ADDR=localhost:2525 \
CANARY_API_URL=http://localhost:8080 \
go run ./cmd/canary
```

The process runs every minute by default and also checks immediately at startup.

## Docker

Build and run the canary as an independent, continuously running container:

```sh
cd canary
docker build -t tmpmail-canary .
docker run -d --name tmpmail-canary --restart unless-stopped \
  -p 8081:8081 \
  -e CANARY_INTERVAL=30s \
  -e CANARY_MAIL_DOMAIN=mail.example.com \
  -e CANARY_SMTP_ADDR=host.docker.internal:25 \
  -e CANARY_API_URL=http://host.docker.internal:8080 \
  tmpmail-canary
```

GitHub Actions publishes the same image to `ghcr.io/chandl/tmpmail.fyi-canary` on pushes to `main` and version tags. Use a version tag in production rather than `latest`.

`CANARY_INTERVAL` is the delay between completed schedule ticks; it accepts Go durations such as `30s`, `5m`, and `1h`. The initial run happens immediately. Docker uses `/livez` for its container health check; the monitored-check result remains available from `/healthz`.

For a ready-to-edit deployment, `docker compose up --build -d` from this directory uses [compose.yaml](compose.yaml). `host.docker.internal` in that example reaches a service running on the Docker host; replace it with the target hostname or service name for your environment.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `CANARY_HTTP_ADDR` | `:8081` | Address for `/healthz` and `/livez`. |
| `CANARY_INTERVAL` | `1m` | Interval between runs (Go duration). |
| `CANARY_TIMEOUT` | `10s` | Total deadline for one run. |
| `CANARY_MAIL_DOMAIN` | — | Required tmpmail recipient domain; must equal `MAIL_DOMAIN`. |
| `CANARY_SMTP_ADDR` | `tmpmail:2525` | tmpmail SMTP listener address. |
| `CANARY_API_URL` | `http://tmpmail:8080` | tmpmail HTTP API base URL. |
| `CANARY_FROM` | `canary@monitor.invalid` | Sender used for the probe message. |

Every run checks the tmpmail HTTP health endpoint, then exercises SMTP receipt, inbox listing, individual-message retrieval, and the rendered inbox page. The canary creates a unique disposable recipient per run, so it never confuses an old message with the new probe.
