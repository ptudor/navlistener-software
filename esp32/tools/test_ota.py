#!/usr/bin/env python3
import hashlib
import hmac
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch
import json
import ota


class OtaClientTest(unittest.TestCase):
    def test_update_api_mac_binds_method_path_exact_body_and_nonce(self):
        key=bytes(range(32));nonce="ab"*32;body=b'{"release_sequence":"9007199254740993","discard_backlog":false}'
        signed=ota.api_authorization(key,nonce,"POST","/ota/v1/install",body)
        expected=hmac.new(key,b"navfeeder-ota-api-v1\n"+nonce.encode()+b"\nPOST\n/ota/v1/install\n"+body,hashlib.sha256).hexdigest()
        self.assertEqual(signed,expected)
        for method,path,changed in (("PUT","/ota/v1/install",body),("POST","/ota/v1/download",body),("POST","/ota/v1/install",body+b" ")):
            self.assertNotEqual(signed,ota.api_authorization(key,nonce,method,path,changed))
        self.assertNotEqual(signed,ota.api_authorization(key,"cd"*32,"POST","/ota/v1/install",body))
        with self.assertRaises(ValueError):ota.api_authorization(key,nonce,"POST","/ota/v1/install?extra=1",body)
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
            image[288:304] = b"NVFOTA1\0\x02\x01\x01\x00\x03\x00\x00\x00"
            path.write_bytes(image)
            body = ota.update_body(path, "https://example.invalid/app.bin")
            self.assertEqual(body[:64].decode(), hashlib.sha256(image).hexdigest())
            for url in ("http://example.invalid/app.bin", "https://u@example.invalid/app.bin", "https://example.invalid/#x"):
                with self.assertRaises(ValueError):
                    ota.update_body(path, url)
            for board in (2, 3):
                image[297] = board
                path.write_bytes(image)
                ota.update_body(path, "https://example.invalid/app.bin")
            for board in (0, 4):
                image[297] = board
                path.write_bytes(image)
                with self.assertRaises(ValueError):
                    ota.update_body(path, "https://example.invalid/app.bin")
            image[297] = 1
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

    def test_journal_authentication_and_pagination(self):
        key, nonce = bytes(range(32)), "ab" * 32
        bodies = []
        def exchange(device, path, data=None, headers=None):
            if path == "/ota":
                return json.dumps({"nonce": nonce}).encode()
            self.assertEqual(path, "/journal")
            bodies.append(data)
            signature = headers["X-OTA-Authorization"]
            self.assertEqual(signature, ota.authorization(key, nonce, data, b"navfeeder-journal-v1\n"))
            self.assertNotEqual(signature, ota.authorization(key, nonce, data))
            page = {"records": [{"sequence": "9"}], "next": "9"} if len(bodies) == 1 else {
                "records": [{"sequence": "8"}], "next": "0"}
            return json.dumps(page).encode()
        with patch.object(ota, "exchange", side_effect=exchange):
            self.assertEqual(ota.read_journal("device", key, "life", 10), [{"sequence": "9"}, {"sequence": "8"}])
        self.assertEqual(bodies, [b"life\n0", b"life\n9"])

    def test_journal_decodes_hardware_trust_and_commissioning_events(self):
        self.assertEqual(ota.journal_detail(7, 3), "trusted")
        self.assertEqual(ota.journal_detail(7, 0 | 9 << 8), "none/proof")
        self.assertEqual(ota.journal_detail(7, 0 | 10 << 8), "none/product")
        self.assertEqual(ota.journal_detail(7, 0x20 | 0x21 << 8), "withheld/key-not-ready")
        self.assertEqual(ota.journal_detail(7, 0x10), "unreported")
        self.assertEqual(ota.journal_detail(8, 1 | 3 << 16), "keygen ok")
        self.assertEqual(ota.journal_detail(8, 2 | 1 << 8), "install failed")
        self.assertEqual(ota.journal_detail(5, 1234), "")

    def test_journal_stuck_cursor_fails_without_looping(self):
        replies = [{"nonce": "ab" * 32}, {"records": [], "next": "9"}] * 2
        with patch.object(ota, "exchange", side_effect=[json.dumps(r).encode() for r in replies]):
            with self.assertRaisesRegex(ValueError, "cursor did not advance"):
                ota.read_journal("device", bytes(32), "health", 1024)


if __name__ == "__main__":
    unittest.main()
