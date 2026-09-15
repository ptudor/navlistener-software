#!/usr/bin/env python3
"""Pair through the setup AP, then authorize one HTTPS firmware download."""
import argparse
import datetime
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("device redirects are refused")


def device_url(device):
    parsed = urllib.parse.urlsplit("http://" + device)
    if not parsed.hostname or parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment:
        raise ValueError("device must be a hostname or IP address, optionally followed by :port")
    return "http://" + device


def exchange(device, path, data=None, headers=None):
    request = urllib.request.Request(device_url(device) + path, data=data, headers=headers or {})
    # Pairing credentials must go directly to the protected AP, never a proxy
    # selected by a workstation environment variable or an HTTP redirect.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=10) as response:
        body = response.read(8193)
        if len(body) > 8192:
            raise ValueError("oversized device response")
        return body


def read_key(path, create=False):
    path = Path(path)
    if create and not path.exists():
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as out:
            out.write(secrets.token_hex(32) + "\n")
    if os.name != "nt" and path.stat().st_mode & 0o077:
        raise ValueError("key file must be private (chmod 600)")
    text = path.read_text().strip()
    if not re.fullmatch(r"[0-9a-f]{64}", text):
        raise ValueError("key file must contain 64 lowercase hexadecimal characters")
    return bytes.fromhex(text)


def update_body(image, url):
    if not url.startswith("https://") or len(url) >= 768 or any(
        ord(c) <= 32 or ord(c) >= 127 or c in "@#\\" for c in url
    ):
        raise ValueError("firmware URL must be HTTPS without credentials, fragments or whitespace")
    parsed = urllib.parse.urlsplit(url)
    if not parsed.hostname:
        raise ValueError("firmware URL has no hostname")
    data = Path(image).read_bytes()
    if not 304 <= len(data) <= 0x200000 or data[0] != 0xe9 or data[12:14] != b"\x09\x00":
        raise ValueError("expected an ESP32-S3 app binary fitting the 2 MiB OTA slot")
    if data[288:300] != b"NVFOTA1\0\x01\x01\x01\x00":
        raise ValueError("image lacks the board/rollback marker or enables manufacturing writes")
    return (hashlib.sha256(data).hexdigest() + "\n" + url).encode("ascii")


def authorization(key, nonce, body, domain=b"navfeeder-ota-v1\n"):
    if not isinstance(nonce, str) or not re.fullmatch(r"[0-9a-f]{64}", nonce):
        raise ValueError("invalid device challenge")
    message = domain + nonce.encode("ascii") + b"\n" + body
    return hmac.new(key, message, hashlib.sha256).hexdigest()


def read_journal(device, key, lane, limit):
    rows, before = [], 0
    while len(rows) < limit:
        status = json.loads(exchange(device, "/ota"))
        body = f"{lane}\n{before}".encode("ascii")
        signature = authorization(key, status.get("nonce"), body, b"navfeeder-journal-v1\n")
        page = json.loads(exchange(device, "/journal", body, {
            "Content-Type": "text/plain", "X-OTA-Nonce": status["nonce"],
            "X-OTA-Authorization": signature,
        }))
        if not isinstance(page.get("records"), list) or len(page["records"]) > 8:
            raise ValueError("invalid journal page")
        rows.extend(page["records"])
        next_cursor = int(page["next"])
        if not next_cursor:
            break
        if next_cursor < 0 or (before and next_cursor >= before):
            raise ValueError("journal cursor did not advance")
        before = next_cursor
    return rows[:limit]


def print_journal(rows):
    events = {1: "boot", 2: "time-anchor", 3: "confirmed", 4: "OTA-ready",
              5: "OTA-failed", 6: "checkpoint"}
    sources = {0: "unknown", 1: "RTC", 2: "GNSS"}
    print("UTC                  SOURCE  BOOT   UPTIME(s) EVENT        FIRMWARE                         RESET FLAGS DROP")
    for row in rows:
        utc = int(row["utc"])
        date = datetime.datetime.fromtimestamp(utc, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S") if utc else "unknown"
        print(f"{date:20} {sources.get(row['time_source'], '?'):7} {row['boot']:6} "
              f"{int(row['uptime_ms']) // 1000:9} {events.get(row['event'], '?'):12} "
              f"{row['firmware']:32} {row['reset_reason']:5} {row['flags']:02x} {row['dropped']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--device", required=True, help="device IP/hostname, optionally :port")
    commands = parser.add_subparsers(dest="command", required=True)
    pair = commands.add_parser("pair", help="join the password-protected setup AP first")
    pair.add_argument("--key-file", required=True, help="private file; created if absent")
    commands.add_parser("status")
    journal = commands.add_parser("journal", help="read the bounded persistent diagnostic FIFO")
    journal.add_argument("--key-file", required=True)
    journal.add_argument("--lane", choices=("life", "health"), default="life")
    journal.add_argument("--limit", type=int, default=256)
    journal.add_argument("--json", action="store_true", help="include every stored field")
    update = commands.add_parser("update")
    update.add_argument("--key-file", required=True)
    update.add_argument("--image", required=True, help="local app binary whose SHA-256 the device must verify")
    update.add_argument("--url", required=True, help="direct HTTPS URL serving the same app binary")
    args = parser.parse_args()
    if args.command == "pair":
        key = read_key(args.key_file, create=True)
        print(exchange(args.device, "/ota/pair", key.hex().encode("ascii"),
                       {"Content-Type": "text/plain"}).decode().strip())
    elif args.command == "status":
        print(json.dumps(json.loads(exchange(args.device, "/ota")), indent=2))
    elif args.command == "journal":
        if not 1 <= args.limit <= 1024:
            raise ValueError("limit must be between 1 and 1024")
        rows = read_journal(args.device, read_key(args.key_file), args.lane, args.limit)
        if args.json:
            print(json.dumps(rows, indent=2))
        else:
            print_journal(rows)
    else:
        body = update_body(args.image, args.url)
        key = read_key(args.key_file)
        status = json.loads(exchange(args.device, "/ota"))
        if not status.get("confirmed"):
            raise ValueError("running firmware has not passed its startup checks")
        signature = authorization(key, status.get("nonce"), body)
        result = exchange(args.device, "/ota", body, {
            "Content-Type": "text/plain", "X-OTA-Nonce": status["nonce"],
            "X-OTA-Authorization": signature,
        })
        print(result.decode().strip())
        print("Use status after reboot to inspect the running version and partition.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, urllib.error.URLError) as error:
        raise SystemExit(str(error)) from None
