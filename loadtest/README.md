# tmpmail Locust load test

This is a standalone, configurable workload for a running tmpmail service. Its
default virtual-user mix is 80% inbox readers, 15% SMTP writers, and 5%
end-to-end users. Readers only access inboxes created for the named test run.

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
export TMPMAIL_RUN_ID=read-heavy-1
```

`TMPMAIL_MESSAGE_BODY_BYTES` is the RFC 822 body size. The serialized message is
larger because it also contains headers. The body must be large enough to hold a
unique verification token (currently at least 45 bytes).

The run ID is required: use the same value when seeding and running Locust. It
names all test-owned read inboxes as
`locust-read-<run-id>-<number>@<domain>` by default.

Optional traffic and corpus settings:

```text
TMPMAIL_SENDER=locust@loadtest.invalid
TMPMAIL_READ_INBOX_PREFIX=locust-read
TMPMAIL_READ_INBOX_COUNT=100
TMPMAIL_READ_PAGE_LIMIT=25
TMPMAIL_SEED_MESSAGES_PER_INBOX=25
TMPMAIL_READER_WEIGHT=80
TMPMAIL_WRITER_WEIGHT=15
TMPMAIL_END_TO_END_WEIGHT=5
```

The three weights control Locust user allocation. They default to an 80/15/5
read-heavy mix and must not all be zero. A reader lists a seeded inbox then
fetches one returned message; writers add messages to the same inbox corpus; an
end-to-end user sends, finds, and verifies its own unique message.

## Seed the read corpus

Create the test-owned inboxes before running reader users:

```sh
.venv/bin/python loadtest/seed.py
```

With the defaults this creates 2,500 messages: 25 messages in each of 100
run-scoped inboxes. Use `TMPMAIL_READ_INBOX_COUNT` and
`TMPMAIL_SEED_MESSAGES_PER_INBOX` to choose a smaller or larger corpus. Do not
reuse a run ID unless you intentionally want to add to its existing corpus.

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
three flow results: `read-inbox`, `write-message`, and `smtp-to-read`. Stop on
SMTP `421` responses, storage errors, or sustained tail-latency growth.
