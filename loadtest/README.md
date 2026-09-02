# tmpmail Locust load test

This is a standalone end-to-end workload for a running tmpmail service. It
sends SMTP messages, finds them through the inbox API, and verifies them through
the message API.

## Setup

Install the Python dependency:

```sh
python3 -m venv .venv
.venv/bin/pip install -r loadtest/requirements.txt
```

Set the target and payload settings:

```sh
export TMPMAIL_HTTP_URL=https://tmpmail.example.com
export TMPMAIL_SMTP_HOST=mail.example.com
export TMPMAIL_SMTP_PORT=25
export TMPMAIL_DOMAIN=mail.example.com
export TMPMAIL_MESSAGE_BODY_BYTES=1024
```

`TMPMAIL_MESSAGE_BODY_BYTES` is the RFC 822 body size. The serialized message is
larger because it also contains headers. The body must be large enough to hold a
unique verification token (currently at least 45 bytes).

Optional settings:

```text
TMPMAIL_SENDER=locust@loadtest.invalid
TMPMAIL_RUN_ID=manual-run-1
```

If `TMPMAIL_RUN_ID` is omitted, the test generates a timestamp-based one. Set it
explicitly to make messages from a run easy to find.

## Run

For a first production run, temporarily set
`SMTP_MAX_CONNECTIONS_PER_IP=0`, confirm the private Prometheus endpoint is
being scraped, and start at a low rate:

```sh
.venv/bin/locust -f loadtest/locustfile.py \
  --headless \
  -u 10 -r 1 -t 10m \
  --html loadtest/results/run-1.html
```

Run successively at 10, 25, 50, 75, and 100 users only after reviewing the
previous result. The global 100-session SMTP cap remains active. Restore
`SMTP_MAX_CONNECTIONS_PER_IP=10` after testing.

The report includes individual SMTP send timing, normal HTTP request timing, and
an end-to-end `flow / smtp-to-read` result. Stop on SMTP `421` responses, storage
errors, or sustained tail-latency growth.
