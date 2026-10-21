#!/usr/bin/env python3
"""Validate a first-boot setup-label payload and prepare a physical label.

The firmware prints one of two payloads. The S3 image prints a Wi-Fi network QR,
WIFI:T:WPA;S:<device>;P:<password>;;, which a phone camera joins directly. A build
with BLE provisioning prints the ESP Provisioning BLE payload (JSON) instead."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

NAME = re.compile(r"navfeeder-[0-9A-F]{6}\Z")
PASSWORD = re.compile(r"[abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789]{12}\Z")
EXPECTED_KEYS = {"ver", "name", "username", "pop", "transport"}
WIFI_PAYLOAD = re.compile(r"WIFI:T:WPA;S:([^;]*);P:([^;]*);;")


@dataclass(frozen=True)
class SetupLabel:
    name: str
    password: str
    payload: str  # canonical QR content


def _checked(name: str, password: str) -> None:
    if not NAME.fullmatch(name):
        raise ValueError("device name must be the navfeeder-XXXXXX identity")
    if not PASSWORD.fullmatch(password):
        raise ValueError("setup password has the wrong alphabet or length")


def _wifi_label(text: str) -> SetupLabel:
    match = WIFI_PAYLOAD.match(text)
    if not match:
        raise ValueError("Wi-Fi label must be WIFI:T:WPA;S:<device>;P:<password>;;")
    name, password = match.groups()
    _checked(name, password)
    return SetupLabel(name, password, f"WIFI:T:WPA;S:{name};P:{password};;")


def _ble_label(text: str) -> SetupLabel:
    value, _ = json.JSONDecoder().raw_decode(text)
    if not isinstance(value, dict) or set(value) != EXPECTED_KEYS:
        raise ValueError("payload must contain exactly ver, name, username, pop, transport")
    if any(not isinstance(item, str) for item in value.values()):
        raise ValueError("every provisioning payload value must be a string")
    if value["ver"] != "v1" or value["transport"] != "ble":
        raise ValueError("only v1 BLE provisioning labels are supported")
    if value["username"] != value["name"]:
        raise ValueError("name and username must be the same navfeeder-XXXXXX identity")
    _checked(value["name"], value["pop"])
    order = ("ver", "name", "username", "pop", "transport")
    payload = json.dumps({key: value[key] for key in order}, separators=(",", ":"))
    return SetupLabel(value["name"], value["pop"], payload)


def extract_label(text: str) -> SetupLabel:
    """Find the setup-label payload in a captured serial line."""
    wifi = text.find("WIFI:")
    if wifi >= 0:
        return _wifi_label(text[wifi:])
    start = text.find("{")
    if start >= 0:
        return _ble_label(text[start:])
    raise ValueError("input does not contain a setup label payload")


def render_qr(payload: str, output: Path) -> None:
    executable = shutil.which("qrencode")
    if not executable:
        raise RuntimeError("qrencode is required for --qr-svg; install it on the label workstation")
    output.parent.mkdir(parents=True, exist_ok=True)
    # The payload carries the setup password. It reaches qrencode on stdin, never on the
    # command line, where any local user can read it from the process list for as long as
    # the process runs; and the SVG is created private (0600) before qrencode writes a byte
    # into it, rather than being tightened afterwards. An existing file is never overwritten:
    # a label is written once, into a fresh private capture directory.
    try:
        os.close(os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
    except FileExistsError:
        raise RuntimeError(f"refusing to overwrite an existing label file: {output}") from None
    try:
        subprocess.run(
            [executable, "-t", "SVG", "-m", "4", "-o", str(output)],
            input=payload.encode("utf-8"),
            check=True,
        )
    except BaseException:
        output.unlink(missing_ok=True)
        raise


def main() -> int:
    parser = argparse.ArgumentParser(
        description="validate the serial setup-label line and optionally create its QR SVG"
    )
    parser.add_argument("payload_file", type=Path, help="file containing the captured serial line")
    parser.add_argument("--qr-svg", type=Path, help="write the label's QR SVG")
    args = parser.parse_args()

    try:
        label = extract_label(args.payload_file.read_text(encoding="utf-8"))
        if args.qr_svg:
            render_qr(label.payload, args.qr_svg)
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"provisioning_label: {error}", file=sys.stderr)
        return 1

    print(f"Device:         {label.name}")
    print(f"Setup password: {label.password}")
    print(f"QR payload:     {label.payload}")
    if args.qr_svg:
        print(f"QR SVG:         {args.qr_svg}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
