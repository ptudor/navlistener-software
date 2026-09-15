#!/usr/bin/env python3
"""Pair through the setup AP and manage authenticated firmware updates."""
import argparse
import datetime
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import time
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


def exchange(device, path, data=None, headers=None, *, method=None, timeout=10):
    request = urllib.request.Request(device_url(device) + path, data=data, headers=headers or {}, method=method)
    # Pairing credentials must go directly to the protected AP, never a proxy
    # selected by a workstation environment variable or an HTTP redirect.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=timeout) as response:
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


def api_authorization(key, nonce, method, path, body):
    if method not in ("POST", "PUT") or path not in (
        "/ota/v1/check", "/ota/v1/download", "/ota/v1/install", "/ota/v1/cancel", "/ota/v1/policy"
    ):
        raise ValueError("unsupported update request")
    binding = method.encode("ascii") + b"\n" + path.encode("ascii") + b"\n" + body
    return authorization(key, nonce, binding, b"navfeeder-ota-api-v1\n")


def api_status(device):
    return json.loads(exchange(device, "/ota/v1/status"))


def api_mutation(device, key, command, value):
    path = "/ota/v1/" + command
    method = "PUT" if command == "policy" else "POST"
    body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    status = api_status(device)
    signature = api_authorization(key, status.get("nonce"), method, path, body)
    return json.loads(exchange(device, path, body, {
        "Content-Type": "application/json", "X-OTA-Nonce": status["nonce"],
        "X-OTA-Authorization": signature,
    }, method=method, timeout=75 if value.get("discard_backlog") else 10))


def wait_for(device, predicate, *, timeout=600):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status = api_status(device)
        if predicate(status):
            return status
        if status.get("error") not in (None, "OK") and not status.get("busy"):
            raise ValueError(f"{status.get('state')}: {status.get('error')}. {status.get('next_action', '')}")
        time.sleep(1)
    raise ValueError("update is still pending; inspect status before issuing another request")


def sequence(value):
    if not re.fullmatch(r"0|[1-9][0-9]*", value) or int(value) >= 2**64:
        raise ValueError("release sequence must be an unsigned 64-bit decimal number")
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--device", required=True, help="device IP/hostname, optionally :port")
    commands = parser.add_subparsers(dest="command", required=True)
    pair = commands.add_parser("pair", help="join the password-protected setup AP first")
    pair.add_argument("--key-file", required=True, help="private pairing file; created if absent")
    commands.add_parser("status")
    journal = commands.add_parser("journal", help="read the persistent diagnostic FIFO")
    journal.add_argument("--key-file", required=True)
    journal.add_argument("--lane", choices=("life", "health"), default="life")
    journal.add_argument("--limit", type=int, default=256)
    journal.add_argument("--json", action="store_true")
    for name in ("check", "download", "install", "update", "cancel", "set-policy"):
        command = commands.add_parser(name)
        command.add_argument("--key-file", required=True)
        if name in ("download", "install", "cancel"):
            command.add_argument("--release", type=sequence, help="defaults to the available/staged release; cancel defaults to current")
        if name == "install":
            command.add_argument("--discard-backlog", action="store_true", help="attended emergency: allow loss of queued observations")
        if name == "set-policy":
            command.add_argument("--mode", required=True, choices=("manual", "download", "install"))
            command.add_argument("--channel", required=True, choices=("stable", "canary", "lab"))
    recovery = commands.add_parser("service-recovery", help="attended direct-image recovery; use normal update for signed repository releases")
    recovery.add_argument("--key-file", required=True)
    recovery.add_argument("--image", required=True)
    recovery.add_argument("--url", required=True)
    recovery.add_argument("--acknowledge-reboot-and-backlog-loss", action="store_true", required=True)
    args = parser.parse_args()
    if args.command == "pair":
        key = read_key(args.key_file, create=True)
        print(exchange(args.device, "/ota/pair", key.hex().encode("ascii"), {"Content-Type": "text/plain"}).decode().strip())
    elif args.command == "status":
        status = api_status(args.device)
        status.pop("nonce", None)
        print(json.dumps(status, indent=2))
    elif args.command == "journal":
        if not 1 <= args.limit <= 1024:
            raise ValueError("limit must be between 1 and 1024")
        rows = read_journal(args.device, read_key(args.key_file), args.lane, args.limit)
        print(json.dumps(rows, indent=2)) if args.json else print_journal(rows)
    elif args.command == "service-recovery":
        body = update_body(args.image, args.url)
        key = read_key(args.key_file)
        status = json.loads(exchange(args.device, "/ota"))
        if not status.get("confirmed"):
            raise ValueError("running firmware has not passed its startup checks")
        signature = authorization(key, status.get("nonce"), body)
        print(exchange(args.device, "/ota", body, {"Content-Type": "text/plain", "X-OTA-Nonce": status["nonce"],
            "X-OTA-Authorization": signature}).decode().strip())
    else:
        key = read_key(args.key_file)
        if args.command == "set-policy":
            result = api_mutation(args.device, key, "policy", {"mode": args.mode, "channel": args.channel})
        elif args.command == "check":
            result = api_mutation(args.device, key, "check", {})
        elif args.command == "update":
            started = int(time.time())
            api_mutation(args.device, key, "check", {})
            # The worker persists a successful check before availability is used.
            status = wait_for(args.device, lambda s: int(s.get("last_check", 0)) >= started and
                not s.get("busy") and s.get("state") in ("idle", "available", "staged"))
            release = sequence(status.get("available_release", "0"))
            if int(release) <= int(status.get("running_release", "0")):
                print("The selected channel has no newer eligible release.")
                return
            api_mutation(args.device, key, "download", {"release_sequence": release})
            wait_for(args.device, lambda s: s.get("staged_release") == release and not s.get("busy"))
            result = api_mutation(args.device, key, "install", {"release_sequence": release, "discard_backlog": False})
        else:
            status = api_status(args.device)
            default = "0" if args.command == "cancel" else status.get("staged_release" if args.command == "install" else "available_release", "0")
            release = sequence(args.release or default)
            if args.command != "cancel" and release == "0":
                raise ValueError("no matching release; run check and inspect status first")
            value = {"release_sequence": release}
            if args.command == "install":
                value["discard_backlog"] = args.discard_backlog
            result = api_mutation(args.device, key, args.command, value)
        print(json.dumps(result, indent=2))
        print("Request accepted. Device status reports the actual result.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, urllib.error.URLError) as error:
        raise SystemExit(str(error)) from None
