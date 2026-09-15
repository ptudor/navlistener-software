#!/usr/bin/env python3
"""Validate a first-boot provisioning payload and prepare a physical label."""

from __future__ import annotations

import argparse
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


def extract_payload(text: str) -> dict[str, str]:
    start = text.find("{")
    if start < 0:
        raise ValueError("input does not contain a JSON provisioning payload")
    decoder = json.JSONDecoder()
    value, _ = decoder.raw_decode(text[start:])
    if not isinstance(value, dict) or set(value) != EXPECTED_KEYS:
        raise ValueError("payload must contain exactly ver, name, username, pop, transport")
    if any(not isinstance(item, str) for item in value.values()):
        raise ValueError("every provisioning payload value must be a string")
    if value["ver"] != "v1" or value["transport"] != "ble":
        raise ValueError("only v1 BLE provisioning labels are supported")
    if not NAME.fullmatch(value["name"]) or value["username"] != value["name"]:
        raise ValueError("name and username must be the same navfeeder-XXXXXX identity")
    if not PASSWORD.fullmatch(value["pop"]):
        raise ValueError("setup password has the wrong alphabet or length")
    return value


def canonical(value: dict[str, str]) -> str:
    order = ("ver", "name", "username", "pop", "transport")
    return json.dumps({key: value[key] for key in order}, separators=(",", ":"))


def render_qr(payload: str, output: Path) -> None:
    executable = shutil.which("qrencode")
    if not executable:
        raise RuntimeError("qrencode is required for --qr-svg; install it on the label workstation")
    output.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        [executable, "-t", "SVG", "-m", "4", "-o", str(output), payload],
        check=True,
    )
    os.chmod(output, 0o600)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="validate the one-time serial label line and optionally create its QR SVG"
    )
    parser.add_argument("payload_file", type=Path, help="file containing the captured serial line")
    parser.add_argument("--qr-svg", type=Path, help="write an ESPProvision-compatible QR SVG")
    args = parser.parse_args()

    try:
        value = extract_payload(args.payload_file.read_text(encoding="utf-8"))
        payload = canonical(value)
        if args.qr_svg:
            render_qr(payload, args.qr_svg)
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"provisioning_label: {error}", file=sys.stderr)
        return 1

    print(f"Device:         {value['name']}")
    print(f"Setup password: {value['pop']}")
    print(f"QR payload:     {payload}")
    if args.qr_svg:
        print(f"QR SVG:         {args.qr_svg}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
