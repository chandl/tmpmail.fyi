import unittest

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


if __name__ == "__main__":
    unittest.main()
