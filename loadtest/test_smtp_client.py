import unittest
from unittest.mock import patch

from config import RuntimeConfig
from smtp_client import _dot_stuff, build_message


class MessageTests(unittest.TestCase):
    def test_build_message_uses_requested_body_size(self):
        message = build_message("sender@example.org", "recipient@mail.test", "subject", "abc123", 64)

        body = message.raw.split(b"\r\n\r\n", 1)[1]
        self.assertEqual(64, len(body))
        self.assertIn(b"locust-token=abc123", body)

    def test_build_message_rejects_body_smaller_than_marker(self):
        with self.assertRaises(ValueError):
            build_message("sender@example.org", "recipient@mail.test", "subject", "abc123", 1)

    def test_dot_stuff_escapes_each_line_once(self):
        self.assertEqual(b"header\r\n..first\r\n..second", _dot_stuff(b"header\r\n.first\r\n.second"))


class RuntimeConfigTests(unittest.TestCase):
    def test_read_recipient_uses_run_scoped_inbox(self):
        environment = {
            "TMPMAIL_HTTP_URL": "https://tmpmail.example.com",
            "TMPMAIL_SMTP_HOST": "mail.example.com",
            "TMPMAIL_SMTP_PORT": "25",
            "TMPMAIL_DOMAIN": "mail.example.com",
            "TMPMAIL_MESSAGE_BODY_BYTES": "1024",
            "TMPMAIL_RUN_ID": "read-heavy-1",
        }
        with patch.dict("os.environ", environment, clear=True):
            config = RuntimeConfig.from_environment()

        self.assertEqual("locust-read-read-heavy-1-7@mail.example.com", config.read_recipient(7))
        self.assertEqual(80, config.reader_weight)
        self.assertEqual(15, config.writer_weight)
        self.assertEqual(5, config.end_to_end_weight)

    def test_config_rejects_all_zero_user_weights(self):
        environment = {
            "TMPMAIL_HTTP_URL": "https://tmpmail.example.com",
            "TMPMAIL_SMTP_HOST": "mail.example.com",
            "TMPMAIL_SMTP_PORT": "25",
            "TMPMAIL_DOMAIN": "mail.example.com",
            "TMPMAIL_MESSAGE_BODY_BYTES": "1024",
            "TMPMAIL_RUN_ID": "read-heavy-1",
            "TMPMAIL_READER_WEIGHT": "0",
            "TMPMAIL_WRITER_WEIGHT": "0",
            "TMPMAIL_END_TO_END_WEIGHT": "0",
        }
        with patch.dict("os.environ", environment, clear=True), self.assertRaisesRegex(RuntimeError, "at least one Locust user weight"):
            RuntimeConfig.from_environment()


if __name__ == "__main__":
    unittest.main()
