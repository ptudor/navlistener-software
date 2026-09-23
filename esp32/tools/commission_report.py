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
import hashlib
import json
import re
import sys
from pathlib import Path

MARKER = "NVF-COMMISSION-REPORT "
# Terminal colour sequences and the carriage returns a serial capture leaves behind.
NOISE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]|\r")
HEX_FIELDS = {"atecc_serial": 9, "rtc_eui64": 8, "mcu_mac": 6,
              "secure_boot_keys_sha256": 32, "attestation_record": 72, "mcu_key_sha256": 32}
BASE64_FIELDS = ("mcu_public_key_der", "ds_context")
KEY_STATES = ("absent", "orphaned", "ready", "fault")
PROFILES = ("trusted", "open", "test", "unreported")
REPORT_FIELDS = {
    "v", "product", "identity_flags", "rtc_model_id", "rtc_expected", "rtc_present",
    "atecc_serial", "rtc_eui64", "board_uid_kind", "board_uid", "board_rev", "mcu_family", "mcu_mac",
    "identity_complete", "security", "secure_boot_keys_sha256", "attestation_record",
    "mcu_key_alg", "mcu_public_key_der", "mcu_key_sha256", "ds_context", "key_state",
    "key_block", "rd_dis_sealed", "record", "firmware", "trust_profile",
    "board_uid_address", "eeprom_uid_kind", "eeprom_uid", "eeprom_address",
}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate report field: {key}")
        result[key] = value
    return result


def reports(text):
    """Every report in the log, oldest first."""
    found = []
    for line in NOISE.sub("", text).splitlines():
        at = line.find(MARKER)
        if at < 0:
            continue
        try:
            found.append(validate(json.loads(line[at + len(MARKER):], object_pairs_hook=unique_object)))
        except (ValueError, TypeError) as error:
            raise ValueError(f"malformed commissioning report: {error}") from None
    return found


def validate(report):
    if not isinstance(report, dict):
        raise ValueError("report is not an object")
    if set(report) != REPORT_FIELDS:
        missing = sorted(REPORT_FIELDS - set(report))
        unknown = sorted(set(report) - REPORT_FIELDS)
        raise ValueError(f"report fields disagree with v1 (missing={missing}, unknown={unknown})")
    if isinstance(report["v"], bool) or report["v"] != 1:
        raise ValueError("unsupported report version")
    if isinstance(report["product"], bool) or report["product"] != 1:
        raise ValueError("report is not from an observer")
    for name, size in HEX_FIELDS.items():
        value = report.get(name)
        # A value the firmware could not read is explicit JSON null, never an empty or
        # guessed identifier. RTC configuration/presence below distinguishes not fitted
        # from an expected part that could not be read.
        if value is not None and (not isinstance(value, str) or not re.fullmatch(f"[0-9a-f]{{{2 * size}}}", value)):
            raise ValueError(f"{name} is neither null nor {size} bytes of lowercase hex")
    kind, value = report["board_uid_kind"], report["board_uid"]
    sizes = {"eui64": 8, "serial128": 16, "st_uid128": 16}
    if value is None:
        if kind is not None:
            raise ValueError("board UID kind without a value")
    elif not isinstance(kind, str) or kind not in sizes or not isinstance(value, str) or not re.fullmatch(f"[0-9a-f]{{{2*sizes[kind]}}}", value):
        raise ValueError("unknown board UID kind or invalid value length")
    for uid_field, address_field in (("board_uid", "board_uid_address"), ("eeprom_uid", "eeprom_address")):
        address = report[address_field]
        if report[uid_field] is None:
            if address is not None:
                raise ValueError("address without a hardware identity")
        elif isinstance(address, bool) or not isinstance(address, int) or address not in (0x50, 0x51):
            raise ValueError("unsupported identity I2C address")
    ep_kind, ep_value = report["eeprom_uid_kind"], report["eeprom_uid"]
    if ep_value is None:
        if ep_kind is not None:
            raise ValueError("EEPROM kind without an identity")
    elif ep_kind not in ("eui64", "serial128", "st_uid128") or not isinstance(ep_value, str) or not re.fullmatch(f"[0-9a-f]{{{2*sizes[ep_kind]}}}", ep_value) or set(ep_value) in ({"0"}, {"f"}):
        raise ValueError("invalid EEPROM identity")
    if kind in ("eui64", "serial128", "st_uid128") and (kind, value) != (ep_kind, ep_value):
        raise ValueError("board UID differs from the selected EEPROM identity")
    decoded = {}
    for name in ("atecc_serial", "board_uid", "mcu_mac", "rtc_eui64"):
        value = report[name]
        if value is not None and (set(value) == {"0"} or set(value) == {"f"}):
            raise ValueError(f"{name} is blank or erased; failed reads must be null")
    for name in BASE64_FIELDS:
        value = report.get(name)
        if not isinstance(value, str):
            raise ValueError(f"{name} is missing")
        decoded[name] = base64.b64decode(value, validate=True)
    for name in ("mcu_family", "security", "mcu_key_alg", "key_block", "identity_flags", "rtc_model_id"):
        if not isinstance(report.get(name), int) or isinstance(report.get(name), bool):
            raise ValueError(f"{name} is not an integer")
    if report["board_rev"] is not None and (not isinstance(report["board_rev"], int) or
                                             isinstance(report["board_rev"], bool) or
                                             not 0 <= report["board_rev"] <= 0xffff):
        raise ValueError("board_rev is neither an integer nor null")
    if report.get("key_state") not in KEY_STATES or report.get("trust_profile") not in PROFILES:
        raise ValueError("unknown key state or trust profile")
    for name in ("rtc_expected", "rtc_present", "identity_complete", "rd_dis_sealed"):
        if not isinstance(report.get(name), bool):
            raise ValueError(f"{name} is not a boolean")
    flags = report["identity_flags"]
    if flags & ~0x0003 or bool(flags & 1) and not flags & 2:
        raise ValueError("identity_flags has an unknown or impossible RTC declaration")
    declared = bool(flags & 2)
    bound = bool(flags & 1)
    if report["rtc_expected"] != declared:
        raise ValueError("RTC declaration disagrees with the product expectation")
    if (not declared and report["rtc_model_id"] != 0) or (declared and report["rtc_model_id"] not in (1, 2)):
        raise ValueError("RTC model disagrees with the RTC declaration")
    if bound and report["rtc_model_id"] != 1:
        raise ValueError("RTC model has no factory EUI-64")
    if not bound and report["rtc_eui64"] is not None:
        raise ValueError("unbound RTC EUI-64 must be null")
    if report["rtc_eui64"] is not None and not report["rtc_present"]:
        raise ValueError("RTC EUI-64 was reported although the RTC model check failed")
    complete = all(report[name] is not None for name in
                   ("atecc_serial", "board_uid", "mcu_mac", "attestation_record")) and \
        report["board_rev"] is not None and (not declared or report["rtc_present"]) and \
        (not bound or report["rtc_eui64"] is not None)
    if report["identity_complete"] != complete:
        raise ValueError("identity_complete disagrees with the explicit read states")
    if report["mcu_family"] != 1 or report["mcu_key_alg"] not in (0, 1):
        raise ValueError("unknown microcontroller family or key algorithm")
    if report["security"] & ~0x1f:
        raise ValueError("security contains reserved bits")
    if bool(report["security"] & 1) != (report["secure_boot_keys_sha256"] is not None):
        raise ValueError("Secure Boot state and key digest disagree")
    if not isinstance(report["record"], str) or (report["record"] != "" and
            not re.fullmatch(r"[0-9a-f]{504}", report["record"])):
        raise ValueError("record is neither empty nor a 252-byte lowercase-hex value")
    if not isinstance(report["firmware"], str) or not report["firmware"]:
        raise ValueError("firmware is missing")
    has_key = report["mcu_key_alg"] != 0
    if has_key != bool(report["mcu_public_key_der"]) or has_key != bool(report["ds_context"]) or \
            has_key != (report["key_state"] == "ready") or \
            has_key != (report["mcu_key_sha256"] is not None):
        raise ValueError("key fields disagree with each other")
    if has_key and (hashlib.sha256(decoded["mcu_public_key_der"]).hexdigest() != report["mcu_key_sha256"] or
                    not decoded["ds_context"].startswith(b"NDS\x01")):
        raise ValueError("key digest or Digital Signature context is invalid")
    attestation = report["attestation_record"]
    if attestation is not None and (not attestation.startswith("01" + "00" * 7)):
        raise ValueError("slot-14 record has an invalid version or reserved bytes")
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
