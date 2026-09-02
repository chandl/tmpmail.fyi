"""Minimal SMTP client used by the tmpmail Locust test."""

from __future__ import annotations

import socket
from dataclasses import dataclass


class SMTPDeliveryError(RuntimeError):
    """An SMTP server response or network failure prevented delivery."""


@dataclass(frozen=True)
class Message:
    raw: bytes
    token: str


def build_message(sender: str, recipient: str, subject: str, token: str, body_bytes: int) -> Message:
    marker = f"locust-token={token}".encode("ascii")
    if body_bytes < len(marker):
        raise ValueError(f"TMPMAIL_MESSAGE_BODY_BYTES must be at least {len(marker)} bytes")
    body = marker + (b"x" * (body_bytes - len(marker)))
    headers = (
        f"From: {sender}\r\n"
        f"To: {recipient}\r\n"
        f"Subject: {subject}\r\n"
        "Content-Type: text/plain; charset=utf-8\r\n"
        "\r\n"
    ).encode("ascii")
    return Message(raw=headers + body, token=token)


def deliver(host: str, port: int, sender: str, recipient: str, message: Message, timeout_seconds: float) -> None:
    with socket.create_connection((host, port), timeout=timeout_seconds) as connection:
        connection.settimeout(timeout_seconds)
        reader = connection.makefile("rb")
        try:
            _expect(reader, 220)
            _command(connection, reader, 250, "EHLO locust")
            _command(connection, reader, 250, f"MAIL FROM:<{sender}>")
            _command(connection, reader, 250, f"RCPT TO:<{recipient}>")
            _command(connection, reader, 354, "DATA")
            connection.sendall(_dot_stuff(message.raw) + b"\r\n.\r\n")
            _expect(reader, 250)
            _command(connection, reader, 221, "QUIT")
        finally:
            reader.close()


def _command(connection: socket.socket, reader, expected: int, command: str) -> None:
    connection.sendall(command.encode("ascii") + b"\r\n")
    _expect(reader, expected)


def _expect(reader, expected: int) -> None:
    line = reader.readline()
    if not line:
        raise SMTPDeliveryError("SMTP server closed the connection")
    if len(line) < 3 or not line[:3].isdigit():
        raise SMTPDeliveryError(f"invalid SMTP response: {line!r}")
    code = int(line[:3])
    separator = line[3:4]
    while separator == b"-":
        line = reader.readline()
        if not line:
            raise SMTPDeliveryError("SMTP server closed during multi-line response")
        separator = line[3:4]
    if code != expected:
        raise SMTPDeliveryError(f"SMTP response {code}, expected {expected}: {line.decode('utf-8', 'replace').strip()}")


def _dot_stuff(raw: bytes) -> bytes:
    return raw.replace(b"\n.", b"\n..")
