"""Shared configuration for the tmpmail Locust workload and corpus seeder."""

from __future__ import annotations

import os
import re
from dataclasses import dataclass
from urllib.parse import urlparse


_SAFE_IDENTIFIER = re.compile(r"^[A-Za-z0-9_-]+$")
_MIN_BODY_BYTES = len("locust-token=") + 32


@dataclass(frozen=True)
class RuntimeConfig:
    http_url: str
    smtp_host: str
    smtp_port: int
    mail_domain: str
    body_bytes: int
    sender: str
    run_id: str
    read_inbox_prefix: str
    read_inbox_count: int
    read_page_limit: int
    seed_messages_per_inbox: int
    reader_weight: int
    writer_weight: int
    end_to_end_weight: int

    @classmethod
    def from_environment(cls) -> "RuntimeConfig":
        required = (
            "TMPMAIL_HTTP_URL",
            "TMPMAIL_SMTP_HOST",
            "TMPMAIL_SMTP_PORT",
            "TMPMAIL_DOMAIN",
            "TMPMAIL_MESSAGE_BODY_BYTES",
            "TMPMAIL_RUN_ID",
        )
        missing = [name for name in required if not os.getenv(name)]
        if missing:
            raise RuntimeError("missing required environment variables: " + ", ".join(missing))
        http_url = os.environ["TMPMAIL_HTTP_URL"].rstrip("/")
        parsed = urlparse(http_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise RuntimeError("TMPMAIL_HTTP_URL must be an absolute HTTP or HTTPS URL")
        run_id = os.environ["TMPMAIL_RUN_ID"]
        prefix = os.getenv("TMPMAIL_READ_INBOX_PREFIX", "locust-read")
        if not _SAFE_IDENTIFIER.fullmatch(run_id) or not _SAFE_IDENTIFIER.fullmatch(prefix):
            raise RuntimeError("TMPMAIL_RUN_ID and TMPMAIL_READ_INBOX_PREFIX may contain only letters, numbers, hyphens, and underscores")

        smtp_port = _integer("TMPMAIL_SMTP_PORT", minimum=1, maximum=65535)
        body_bytes = _integer("TMPMAIL_MESSAGE_BODY_BYTES", minimum=_MIN_BODY_BYTES)
        read_inbox_count = _integer("TMPMAIL_READ_INBOX_COUNT", default=100, minimum=1)
        read_page_limit = _integer("TMPMAIL_READ_PAGE_LIMIT", default=25, minimum=1, maximum=100)
        seed_messages_per_inbox = _integer("TMPMAIL_SEED_MESSAGES_PER_INBOX", default=25, minimum=1)
        reader_weight = _integer("TMPMAIL_READER_WEIGHT", default=80, minimum=0)
        writer_weight = _integer("TMPMAIL_WRITER_WEIGHT", default=15, minimum=0)
        end_to_end_weight = _integer("TMPMAIL_END_TO_END_WEIGHT", default=5, minimum=0)
        if reader_weight + writer_weight + end_to_end_weight == 0:
            raise RuntimeError("at least one Locust user weight must be greater than zero")
        return cls(
            http_url=http_url,
            smtp_host=os.environ["TMPMAIL_SMTP_HOST"],
            smtp_port=smtp_port,
            mail_domain=os.environ["TMPMAIL_DOMAIN"],
            body_bytes=body_bytes,
            sender=os.getenv("TMPMAIL_SENDER", "locust@loadtest.invalid"),
            run_id=run_id,
            read_inbox_prefix=prefix,
            read_inbox_count=read_inbox_count,
            read_page_limit=read_page_limit,
            seed_messages_per_inbox=seed_messages_per_inbox,
            reader_weight=reader_weight,
            writer_weight=writer_weight,
            end_to_end_weight=end_to_end_weight,
        )

    def read_recipient(self, index: int) -> str:
        return f"{self.read_inbox_prefix}-{self.run_id}-{index}@{self.mail_domain}"


def _integer(name: str, default: int | None = None, minimum: int = 0, maximum: int | None = None) -> int:
    value = os.getenv(name)
    if value is None:
        if default is None:
            raise RuntimeError(f"missing required environment variable: {name}")
        return default
    try:
        parsed = int(value)
    except ValueError as error:
        raise RuntimeError(f"{name} must be an integer") from error
    if parsed < minimum or (maximum is not None and parsed > maximum):
        bounds = f"at least {minimum}" if maximum is None else f"between {minimum} and {maximum}"
        raise RuntimeError(f"{name} must be {bounds}")
    return parsed
