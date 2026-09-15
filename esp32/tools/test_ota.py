#!/usr/bin/env python3
import hashlib
import hmac
import tempfile
from pathlib import Path
import unittest
import ota


class OtaClientTest(unittest.TestCase):
    def test_authorization_binds_url_digest_and_challenge(self):
        key = bytes(range(32))
        nonce = "ab" * 32
        body = b"f" * 64 + b"\nhttps://example.invalid/app.bin"
        expected = hmac.new(key, b"navfeeder-ota-v1\n" + nonce.encode() + b"\n" + body,
                            hashlib.sha256).hexdigest()
        self.assertEqual(ota.authorization(key, nonce, body), expected)
        self.assertNotEqual(ota.authorization(key, "cd" * 32, body), expected)
        self.assertNotEqual(ota.authorization(key, nonce, body + b"x"), expected)
        with self.assertRaises(ValueError):
            ota.authorization(key, "bad", body)

    def test_image_policy_and_exact_digest(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "app.bin"
            image = bytearray(304)
            image[0] = 0xe9
            image[12] = 9
            image[288:300] = b"NVFOTA1\0\x01\x01\x01\x00"
            path.write_bytes(image)
            body = ota.update_body(path, "https://example.invalid/app.bin")
            self.assertEqual(body[:64].decode(), hashlib.sha256(image).hexdigest())
            for url in ("http://example.invalid/app.bin", "https://u@example.invalid/app.bin", "https://example.invalid/#x"):
                with self.assertRaises(ValueError):
                    ota.update_body(path, url)
            image[299] = 1
            path.write_bytes(image)
            with self.assertRaises(ValueError):
                ota.update_body(path, "https://example.invalid/app.bin")

    def test_key_generation_reuses_private_file(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "key"
            first = ota.read_key(path, create=True)
            self.assertEqual(len(first), 32)
            self.assertEqual(ota.read_key(path, create=True), first)
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                ota.read_key(path)

    def test_refuses_device_redirect_and_userinfo(self):
        with self.assertRaises(ValueError):
            ota.NoRedirect().redirect_request(None, None, 302, "", {}, "http://example.invalid/")
        for device in ("user@host", "host/path", "host?key=secret"):
            with self.assertRaises(ValueError):
                ota.device_url(device)


if __name__ == "__main__":
    unittest.main()
