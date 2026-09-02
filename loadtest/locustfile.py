"""End-to-end SMTP-to-HTTP Locust workload for tmpmail."""

from __future__ import annotations

import os
import secrets
import time
from dataclasses import dataclass
from urllib.parse import quote, urlparse

import gevent
from locust import HttpUser, between, task

from smtp_client import Message, build_message, deliver


POLL_INTERVAL_SECONDS = 0.2
POLL_TIMEOUT_SECONDS = 10
SMTP_TIMEOUT_SECONDS = 15


@dataclass(frozen=True)
class RuntimeConfig:
    http_url: str
    smtp_host: str
    smtp_port: int
    mail_domain: str
    body_bytes: int
    sender: str
    run_id: str

    @classmethod
    def from_environment(cls) -> "RuntimeConfig":
        required = ("TMPMAIL_HTTP_URL", "TMPMAIL_SMTP_HOST", "TMPMAIL_SMTP_PORT", "TMPMAIL_DOMAIN", "TMPMAIL_MESSAGE_BODY_BYTES")
        missing = [name for name in required if not os.getenv(name)]
        if missing:
            raise RuntimeError("missing required environment variables: " + ", ".join(missing))
        http_url = os.environ["TMPMAIL_HTTP_URL"].rstrip("/")
        parsed = urlparse(http_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise RuntimeError("TMPMAIL_HTTP_URL must be an absolute HTTP or HTTPS URL")
        try:
            smtp_port = int(os.environ["TMPMAIL_SMTP_PORT"])
            body_bytes = int(os.environ["TMPMAIL_MESSAGE_BODY_BYTES"])
        except ValueError as error:
            raise RuntimeError("TMPMAIL_SMTP_PORT and TMPMAIL_MESSAGE_BODY_BYTES must be integers") from error
        if not 1 <= smtp_port <= 65535:
            raise RuntimeError("TMPMAIL_SMTP_PORT must be between 1 and 65535")
        if body_bytes <= 0:
            raise RuntimeError("TMPMAIL_MESSAGE_BODY_BYTES must be positive")
        return cls(
            http_url=http_url,
            smtp_host=os.environ["TMPMAIL_SMTP_HOST"],
            smtp_port=smtp_port,
            mail_domain=os.environ["TMPMAIL_DOMAIN"],
            body_bytes=body_bytes,
            sender=os.getenv("TMPMAIL_SENDER", "locust@loadtest.invalid"),
            run_id=os.getenv("TMPMAIL_RUN_ID", time.strftime("%Y%m%d%H%M%S")),
        )


CONFIG = RuntimeConfig.from_environment()


class TmpmailUser(HttpUser):
    host = CONFIG.http_url
    wait_time = between(0.5, 1.5)

    @task
    def send_and_read_message(self) -> None:
        token = secrets.token_hex(16)
        inbox = f"locust-{CONFIG.run_id}-{token[:12]}"
        recipient = f"{inbox}@{CONFIG.mail_domain}"
        subject = f"locust {token}"
        try:
            message = build_message(CONFIG.sender, recipient, subject, token, CONFIG.body_bytes)
        except ValueError as error:
            self._report("SMTP", "send", 0, 0, error)
            return

        started = time.perf_counter()
        try:
            deliver(CONFIG.smtp_host, CONFIG.smtp_port, CONFIG.sender, recipient, message, SMTP_TIMEOUT_SECONDS)
        except Exception as error:
            self._report("SMTP", "send", _milliseconds_since(started), 0, error)
            self._report("flow", "smtp-to-read", _milliseconds_since(started), 0, error)
            return
        self._report("SMTP", "send", _milliseconds_since(started), len(message.raw), None)

        message_id, error = self._find_message(recipient, subject)
        if error is not None:
            self._report("flow", "smtp-to-read", _milliseconds_since(started), 0, error)
            return
        error = self._verify_message(message_id, recipient, subject, message)
        self._report("flow", "smtp-to-read", _milliseconds_since(started), len(message.raw), error)

    def _find_message(self, recipient: str, subject: str) -> tuple[str | None, Exception | None]:
        path = "/api/v1/inboxes/" + quote(recipient, safe="") + "?limit=25&offset=0"
        deadline = time.monotonic() + POLL_TIMEOUT_SECONDS
        while time.monotonic() < deadline:
            with self.client.get(path, name="/api/v1/inboxes/{inbox}", catch_response=True) as response:
                if response.status_code != 200:
                    error = RuntimeError(f"inbox API returned HTTP {response.status_code}")
                    response.failure(str(error))
                    return None, error
                try:
                    page = response.json()
                except ValueError as error:
                    response.failure("inbox API returned invalid JSON")
                    return None, error
                for item in page.get("messages", []):
                    if item.get("subject") == subject:
                        return item.get("id"), None
            gevent.sleep(POLL_INTERVAL_SECONDS)
        return None, TimeoutError(f"message did not appear in {POLL_TIMEOUT_SECONDS} seconds")

    def _verify_message(self, message_id: str | None, recipient: str, subject: str, message: Message) -> Exception | None:
        if not message_id:
            return RuntimeError("inbox response contained a matching message without an ID")
        path = "/api/v1/messages/" + quote(message_id, safe="")
        with self.client.get(path, name="/api/v1/messages/{id}", catch_response=True) as response:
            if response.status_code != 200:
                error = RuntimeError(f"message API returned HTTP {response.status_code}")
                response.failure(str(error))
                return error
            try:
                result = response.json()
            except ValueError as error:
                response.failure("message API returned invalid JSON")
                return error
            if result.get("recipient") != recipient or result.get("subject") != subject or message.token not in result.get("body", ""):
                error = RuntimeError("retrieved message did not match the SMTP payload")
                response.failure(str(error))
                return error
        return None

    def _report(self, request_type: str, name: str, response_time: float, response_length: int, error: Exception | None) -> None:
        self.environment.events.request.fire(
            request_type=request_type,
            name=name,
            response_time=response_time,
            response_length=response_length,
            exception=error,
        )


def _milliseconds_since(started: float) -> float:
    return (time.perf_counter() - started) * 1000
