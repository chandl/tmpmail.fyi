"""Configurable read-heavy SMTP-to-HTTP Locust workload for tmpmail."""

from __future__ import annotations

import random
import secrets
import time
from urllib.parse import quote

import gevent
from locust import HttpUser, between, task

from config import RuntimeConfig
from smtp_client import Message, build_message, deliver


POLL_INTERVAL_SECONDS = 0.2
POLL_TIMEOUT_SECONDS = 10
SMTP_TIMEOUT_SECONDS = 15
CONFIG = RuntimeConfig.from_environment()


class TmpmailUser(HttpUser):
    abstract = True
    host = CONFIG.http_url
    wait_time = between(0.5, 1.5)

    def _send(self, recipient: str) -> tuple[Message | None, str | None, Exception | None]:
        token = secrets.token_hex(16)
        subject = f"locust {token}"
        try:
            message = build_message(CONFIG.sender, recipient, subject, token, CONFIG.body_bytes)
        except ValueError as error:
            return None, None, error
        started = time.perf_counter()
        try:
            deliver(CONFIG.smtp_host, CONFIG.smtp_port, CONFIG.sender, recipient, message, SMTP_TIMEOUT_SECONDS)
        except Exception as error:
            self._report("SMTP", "send", _milliseconds_since(started), 0, error)
            return None, None, error
        self._report("SMTP", "send", _milliseconds_since(started), len(message.raw), None)
        return message, subject, None

    def _list_inbox(self, recipient: str) -> tuple[list[dict], Exception | None]:
        path = "/api/v1/inboxes/" + quote(recipient, safe="") + f"?limit={CONFIG.read_page_limit}&offset=0"
        with self.client.get(path, name="/api/v1/inboxes/{inbox}", catch_response=True) as response:
            if response.status_code != 200:
                error = RuntimeError(f"inbox API returned HTTP {response.status_code}")
                response.failure(str(error))
                return [], error
            try:
                page = response.json()
            except ValueError as error:
                response.failure("inbox API returned invalid JSON")
                return [], error
            if not isinstance(page, dict):
                error = RuntimeError("inbox API response was not an object")
                response.failure(str(error))
                return [], error
            messages = page.get("messages")
            if not isinstance(messages, list):
                error = RuntimeError("inbox API response did not contain a messages list")
                response.failure(str(error))
                return [], error
            return messages, None

    def _get_message(self, message_id: str) -> tuple[dict | None, Exception | None]:
        path = "/api/v1/messages/" + quote(message_id, safe="")
        with self.client.get(path, name="/api/v1/messages/{id}", catch_response=True) as response:
            if response.status_code != 200:
                error = RuntimeError(f"message API returned HTTP {response.status_code}")
                response.failure(str(error))
                return None, error
            try:
                message = response.json()
            except ValueError as error:
                response.failure("message API returned invalid JSON")
                return None, error
            if not isinstance(message, dict):
                error = RuntimeError("message API response was not an object")
                response.failure(str(error))
                return None, error
            return message, None

    def _report(self, request_type: str, name: str, response_time: float, response_length: int, error: Exception | None) -> None:
        self.environment.events.request.fire(
            request_type=request_type,
            name=name,
            response_time=response_time,
            response_length=response_length,
            exception=error,
        )


class InboxReader(TmpmailUser):
    """Lists a seeded test inbox and fetches one of its messages."""

    weight = CONFIG.reader_weight

    @task
    def read_message(self) -> None:
        started = time.perf_counter()
        recipient = CONFIG.read_recipient(random.randrange(CONFIG.read_inbox_count))
        messages, error = self._list_inbox(recipient)
        if error is None:
            if not messages:
                error = RuntimeError(f"seeded inbox {recipient} was empty")
            else:
                item = random.choice(messages)
                message_id = item.get("id") if isinstance(item, dict) else None
                if not isinstance(message_id, str) or not message_id:
                    error = RuntimeError("inbox response contained a message without an ID")
                else:
                    _, error = self._get_message(message_id)
        self._report("flow", "read-inbox", _milliseconds_since(started), 0, error)


class SMTPWriter(TmpmailUser):
    """Writes new messages into the same run-scoped inbox corpus."""

    weight = CONFIG.writer_weight

    @task
    def write_message(self) -> None:
        started = time.perf_counter()
        recipient = CONFIG.read_recipient(random.randrange(CONFIG.read_inbox_count))
        _, _, error = self._send(recipient)
        self._report("flow", "write-message", _milliseconds_since(started), 0, error)


class EndToEndUser(TmpmailUser):
    """Preserves the SMTP-to-inbox-to-message correctness flow."""

    weight = CONFIG.end_to_end_weight

    @task
    def send_and_read_message(self) -> None:
        started = time.perf_counter()
        token = secrets.token_hex(16)
        recipient = f"locust-e2e-{CONFIG.run_id}-{token[:12]}@{CONFIG.mail_domain}"
        subject = f"locust {token}"
        try:
            message = build_message(CONFIG.sender, recipient, subject, token, CONFIG.body_bytes)
        except ValueError as error:
            self._report("flow", "smtp-to-read", 0, 0, error)
            return
        smtp_started = time.perf_counter()
        try:
            deliver(CONFIG.smtp_host, CONFIG.smtp_port, CONFIG.sender, recipient, message, SMTP_TIMEOUT_SECONDS)
        except Exception as error:
            self._report("SMTP", "send", _milliseconds_since(smtp_started), 0, error)
            self._report("flow", "smtp-to-read", _milliseconds_since(started), 0, error)
            return
        self._report("SMTP", "send", _milliseconds_since(smtp_started), len(message.raw), None)

        deadline = time.monotonic() + POLL_TIMEOUT_SECONDS
        error: Exception | None = None
        while time.monotonic() < deadline:
            messages, error = self._list_inbox(recipient)
            if error is not None:
                break
            matching = next((item for item in messages if isinstance(item, dict) and item.get("subject") == subject), None)
            if matching is not None:
                message_id = matching.get("id")
                if not isinstance(message_id, str) or not message_id:
                    error = RuntimeError("inbox response contained a matching message without an ID")
                    break
                result, error = self._get_message(message_id)
                if error is None and (result.get("recipient") != recipient or result.get("subject") != subject or message.token not in result.get("body", "")):
                    error = RuntimeError("retrieved message did not match the SMTP payload")
                break
            gevent.sleep(POLL_INTERVAL_SECONDS)
        else:
            error = TimeoutError(f"message did not appear in {POLL_TIMEOUT_SECONDS} seconds")
        self._report("flow", "smtp-to-read", _milliseconds_since(started), len(message.raw), error)


def _milliseconds_since(started: float) -> float:
    return (time.perf_counter() - started) * 1000
