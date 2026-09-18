#!/usr/bin/env python3
"""Extract a board's commissioning report from a captured console log.

`commission report` on the USB console prints one line:

    NVF-COMMISSION-REPORT {json}

This tool finds that line in a log that was captured by other means, checks
the report's shape, and writes the JSON to a file for the factory records. It
reads files (or standard input) only: it never opens a serial port.
"""
import argparse
import base64
import json
import re
import sys
from pathlib import Path

MARKER = "NVF-COMMISSION-REPORT "
# Terminal colour sequences and the carriage returns a serial capture leaves behind.
NOISE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]|\r")
HEX_FIELDS = {"atecc_serial": 9, "rtc_eui64": 8, "board_eui64": 8, "mcu_mac": 6,
              "secure_boot_keys_sha256": 32, "attestation_record": 72, "mcu_key_sha256": 32}
BASE64_FIELDS = ("mcu_public_key_der", "ds_context")
KEY_STATES = ("absent", "orphaned", "ready", "fault")
PROFILES = ("trusted", "open", "test", "unreported")


def reports(text):
    """Every report in the log, oldest first."""
    found = []
    for line in NOISE.sub("", text).splitlines():
        at = line.find(MARKER)
        if at < 0:
            continue
        try:
            found.append(validate(json.loads(line[at + len(MARKER):])))
        except (ValueError, TypeError) as error:
            raise ValueError(f"malformed commissioning report: {error}") from None
    return found


def validate(report):
    if not isinstance(report, dict) or report.get("v") != 1:
        raise ValueError("unsupported report version")
    if report.get("product") != 1:
        raise ValueError("report is not from an observer")
    for name, size in HEX_FIELDS.items():
        value = report.get(name)
        # An identifier the firmware could not read is reported empty, never guessed.
        if not isinstance(value, str) or (value and not re.fullmatch(f"[0-9a-f]{{{2 * size}}}", value)):
            raise ValueError(f"{name} is not {size} bytes of lowercase hex")
    for name in BASE64_FIELDS:
        value = report.get(name)
        if not isinstance(value, str):
            raise ValueError(f"{name} is missing")
        base64.b64decode(value, validate=True)
    for name in ("mcu_family", "security", "mcu_key_alg", "key_block"):
        if not isinstance(report.get(name), int) or isinstance(report.get(name), bool):
            raise ValueError(f"{name} is not an integer")
    if report["board_rev"] is not None and not isinstance(report["board_rev"], int):
        raise ValueError("board_rev is neither an integer nor null")
    if report.get("key_state") not in KEY_STATES or report.get("trust_profile") not in PROFILES:
        raise ValueError("unknown key state or trust profile")
    if not isinstance(report.get("rd_dis_sealed"), bool) or not isinstance(report.get("firmware"), str):
        raise ValueError("rd_dis_sealed or firmware is missing")
    has_key = report["mcu_key_alg"] != 0
    if has_key != bool(report["mcu_public_key_der"]) or has_key != bool(report["ds_context"]) or \
            has_key != (report["key_state"] == "ready"):
        raise ValueError("key fields disagree with each other")
    # Security bit 4 is reported only for a ready, protected key on a chip whose eFuse read
    # protection is sealed. A keyed but unsealed chip is a valid report with the bit clear.
    if report["security"] & 0x10 and not (has_key and report["rd_dis_sealed"]):
        raise ValueError("security bit 4 without a ready key and sealed read protection")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("log", help="captured console log, or - for standard input")
    parser.add_argument("--output", "-o", type=Path, required=True, help="JSON file to write (must not exist)")
    args = parser.parse_args()
    text = sys.stdin.read() if args.log == "-" else Path(args.log).read_text(errors="replace")
    found = reports(text)
    if not found:
        raise ValueError("no NVF-COMMISSION-REPORT line in the log")
    # The newest report reflects the board's state after the last bench step.
    with args.output.open("x") as handle:
        json.dump(found[-1], handle, indent=2, sort_keys=True)
        handle.write("\n")
    print(f"wrote {args.output} ({len(found)} report(s) in the log; newest kept)")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        raise SystemExit(str(error)) from None
