"""Populate the run-scoped inbox corpus used by InboxReader users."""

from __future__ import annotations

import secrets
import sys

from config import RuntimeConfig
from smtp_client import build_message, deliver


def main() -> int:
    config = RuntimeConfig.from_environment()
    total = config.read_inbox_count * config.seed_messages_per_inbox
    print(f"seeding {total} messages across {config.read_inbox_count} inboxes for run {config.run_id}")
    sent = 0
    for inbox_index in range(config.read_inbox_count):
        recipient = config.read_recipient(inbox_index)
        for _ in range(config.seed_messages_per_inbox):
            token = secrets.token_hex(16)
            message = build_message(config.sender, recipient, f"locust seed {token}", token, config.body_bytes)
            deliver(config.smtp_host, config.smtp_port, config.sender, recipient, message, timeout_seconds=15)
            sent += 1
    print(f"seeded {sent} messages")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"seed failed: {error}", file=sys.stderr)
        raise SystemExit(1)
