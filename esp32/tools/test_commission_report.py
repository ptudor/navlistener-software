"""Commissioning report extraction from captured console logs."""
import base64
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import commission_report

TOOLS = Path(__file__).resolve().parent


def report(**changes):
    value = {
        "v": 1, "product": 1, "atecc_serial": "0123456789abcdef11", "rtc_eui64": "0004a31234567890",
        "identity_flags": 3, "rtc_model_id": 1, "rtc_present": True,
        "board_uid_kind": "eui64", "board_uid": "0004a3aabbccddee",
        "board_uid_address": 0x50, "eeprom_address": 0x50,
        "eeprom_uid_kind": "eui64", "eeprom_uid": "0004a3aabbccddee", "board_rev": 1, "mcu_family": 1, "mcu_mac": "348518010203",
        "security": 31, "secure_boot_keys_sha256": "ab" * 32, "attestation_record": "01" + "00" * 71,
        "mcu_key_alg": 1, "mcu_public_key_der": base64.b64encode(b"key").decode(),
        "mcu_key_sha256": hashlib.sha256(b"key").hexdigest(),
        "ds_context": base64.b64encode(b"NDS\x01ciphertext").decode(), "key_state": "ready", "key_block": 4,
        "rd_dis_sealed": True, "identity_complete": True, "record": "", "firmware": "0.1.0+2.abcdef0", "trust_profile": "trusted",
    }
    value.update(changes)
    return value


def line(value):
    return "NVF-COMMISSION-REPORT " + json.dumps(value, separators=(",", ":"))


class ReportTests(unittest.TestCase):
    def test_st_uid_is_preserved_and_source_must_match(self):
        uid = "20e00eff0123456789abcdef01234567"
        value = report(board_uid_kind="st_uid128", board_uid=uid,
                       eeprom_uid_kind="st_uid128", eeprom_uid=uid)
        self.assertEqual(commission_report.validate(value)["board_uid"], uid)
        for change in ({"board_uid": uid[:16]}, {"eeprom_uid": uid[:-2] + "68"},
                       {"eeprom_uid_kind": "serial128"}):
            with self.assertRaises(ValueError):
                commission_report.validate(dict(value, **change))

    def test_kind_names_follow_the_discovery_library(self):
        uid = "0123456789abcdef0123456789abcdef"
        value = report(board_uid_kind="serial128", board_uid=uid, eeprom_uid_kind="serial128", eeprom_uid=uid)
        self.assertEqual(commission_report.validate(value)["board_uid"], uid)
        for old in ("microchip_eui64", "microchip_cs128"):
            with self.assertRaises(ValueError):
                commission_report.validate(dict(value, board_uid_kind=old, eeprom_uid_kind=old))

    def test_duplicate_fields_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "duplicate report field"):
            commission_report.reports(line(report()).replace('"v":1', '"v":2,"v":1'))

    def test_rtc_is_recorded_as_found(self):
        # No RTC, an MCP79412 whose EUI-64 was not read, a DS3231 and a MAX31328 are all
        # complete reports: the RTC is not part of the board's identity.
        for flags, model in ((0, 0), (2, 1), (2, 2), (2, 3)):
            value = report(identity_flags=flags, rtc_model_id=model,
                           rtc_present=bool(flags), rtc_eui64=None)
            self.assertTrue(commission_report.validate(value)["identity_complete"])
        for model in (2, 3):
            with self.assertRaisesRegex(ValueError, "no factory EUI"):
                commission_report.validate(report(rtc_model_id=model))
        with self.assertRaisesRegex(ValueError, "RTC model"):
            commission_report.validate(report(identity_flags=2, rtc_model_id=4, rtc_eui64=None))
    def test_finds_reports_among_log_noise_and_keeps_order(self):
        log = "\r\n".join([
            "\x1b[0;32mI (512) navfeeder: navfeeder-esp starting\x1b[0m",
            "\x1b[0;33mW (9000) commission: \x1b[0m" + line(report(key_state="absent", mcu_key_alg=0, mcu_public_key_der="",
                                                                  mcu_key_sha256=None, ds_context="", key_block=-1, security=15,
                                                                  rd_dis_sealed=False)),
            "NVF-COMMISSION-OK keygen", line(report()), ""])
        found = commission_report.reports(log)
        self.assertEqual([item["key_state"] for item in found], ["absent", "ready"])
        self.assertEqual(found[1]["security"], 31)

    def test_unread_identifiers_are_null_not_invented(self):
        found = commission_report.reports(line(report(atecc_serial=None, attestation_record=None,
                                                       board_rev=None, identity_complete=False)))
        self.assertIsNone(found[0]["atecc_serial"])
        self.assertIsNone(found[0]["board_rev"])

    def test_missing_rtc_is_explicit(self):
        found = commission_report.reports(line(report(identity_flags=0, rtc_model_id=0, rtc_present=False,
                                                       rtc_eui64=None)))[0]
        self.assertFalse(found["rtc_present"])
        self.assertIsNone(found["rtc_eui64"])
        self.assertTrue(found["identity_complete"])

    def test_key_security_bit_requires_a_sealed_chip(self):
        # Lost power between the self-test and the seal: the key is ready, the bit is not set,
        # and no trusted statement can be prepared until `commission seal` has run.
        unsealed = commission_report.reports(line(report(security=15, rd_dis_sealed=False)))[0]
        self.assertEqual((unsealed["key_state"], unsealed["security"] & 0x10), ("ready", 0))
        for value in (report(security=31, rd_dis_sealed=False),
                      report(security=31, key_state="fault", mcu_key_alg=0, mcu_public_key_der="",
                             mcu_key_sha256=None, ds_context="")):
            with self.assertRaisesRegex(ValueError, "security bit 4"):
                commission_report.reports(line(value))

    def test_rejects_malformed_and_inconsistent_reports(self):
        bad = [report(v=2), report(product=2), report(rtc_eui64="0004A31234567890"), report(rtc_eui64="0004a3"),
               report(mcu_public_key_der="not base64!"), report(key_state="ready", mcu_key_alg=0),
               report(ds_context=""), report(trust_profile="production"), report(security="31"), report(rd_dis_sealed=1),
               report(identity_flags=1), report(rtc_model_id=0), report(rtc_expected=True),
               report(rtc_present=False), report(rtc_eui64=None), report(identity_flags=2),
               report(identity_complete=False), report(record="00"),
               {key: value for key, value in report().items() if key != "board_uid"},
               dict(report(), surprise=True)]
        for value in bad:
            with self.assertRaises(ValueError):
                commission_report.reports(line(value))
        with self.assertRaises(ValueError):
            commission_report.reports("NVF-COMMISSION-REPORT {truncated")

    def test_command_writes_the_newest_report_once(self):
        with tempfile.TemporaryDirectory() as temporary:
            log, output = Path(temporary) / "console.log", Path(temporary) / "report.json"
            log.write_text(line(report(key_block=3)) + "\n" + line(report(key_block=4)) + "\n")
            command = [sys.executable, str(TOOLS / "commission_report.py"), str(log), "--output", str(output)]
            self.assertEqual(subprocess.run(command, capture_output=True, text=True).returncode, 0)
            self.assertEqual(json.loads(output.read_text())["key_block"], 4)
            again = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(again.returncode, 0)  # never overwrites a factory record
            empty = Path(temporary) / "empty.log"
            empty.write_text("I (1) boot\n")
            missing = subprocess.run(command[:2] + [str(empty), "--output", str(Path(temporary) / "none.json")],
                                     capture_output=True, text=True)
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn("no NVF-COMMISSION-REPORT", missing.stderr)


if __name__ == "__main__":
    unittest.main()
