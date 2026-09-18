"""Commissioning report extraction from captured console logs."""
import base64
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
        "board_eui64": "0004a3aabbccddee", "board_rev": 1, "mcu_family": 1, "mcu_mac": "348518010203",
        "security": 31, "secure_boot_keys_sha256": "ab" * 32, "attestation_record": "01" + "00" * 71,
        "mcu_key_alg": 1, "mcu_public_key_der": base64.b64encode(b"key").decode(), "mcu_key_sha256": "cd" * 32,
        "ds_context": base64.b64encode(b"NDS\x01ciphertext").decode(), "key_state": "ready", "key_block": 4,
        "rd_dis_sealed": True, "record": "", "firmware": "0.1.0+2.abcdef0", "trust_profile": "trusted",
    }
    value.update(changes)
    return value


def line(value):
    return "NVF-COMMISSION-REPORT " + json.dumps(value, separators=(",", ":"))


class ReportTests(unittest.TestCase):
    def test_finds_reports_among_log_noise_and_keeps_order(self):
        log = "\r\n".join([
            "\x1b[0;32mI (512) navfeeder: navfeeder-esp starting\x1b[0m",
            "\x1b[0;33mW (9000) commission: \x1b[0m" + line(report(key_state="absent", mcu_key_alg=0, mcu_public_key_der="",
                                                                  mcu_key_sha256="", ds_context="", key_block=-1, security=15,
                                                                  rd_dis_sealed=False)),
            "NVF-COMMISSION-OK keygen", line(report()), ""])
        found = commission_report.reports(log)
        self.assertEqual([item["key_state"] for item in found], ["absent", "ready"])
        self.assertEqual(found[1]["security"], 31)

    def test_unread_identifiers_are_empty_not_invented(self):
        found = commission_report.reports(line(report(atecc_serial="", attestation_record="", board_rev=None)))
        self.assertEqual(found[0]["atecc_serial"], "")
        self.assertIsNone(found[0]["board_rev"])

    def test_key_security_bit_requires_a_sealed_chip(self):
        # Lost power between the self-test and the seal: the key is ready, the bit is not set,
        # and no trusted statement can be prepared until `commission seal` has run.
        unsealed = commission_report.reports(line(report(security=15, rd_dis_sealed=False)))[0]
        self.assertEqual((unsealed["key_state"], unsealed["security"] & 0x10), ("ready", 0))
        for value in (report(security=31, rd_dis_sealed=False),
                      report(security=31, key_state="fault", mcu_key_alg=0, mcu_public_key_der="", ds_context="")):
            with self.assertRaisesRegex(ValueError, "security bit 4"):
                commission_report.reports(line(value))

    def test_rejects_malformed_and_inconsistent_reports(self):
        bad = [report(v=2), report(product=2), report(rtc_eui64="0004A31234567890"), report(rtc_eui64="0004a3"),
               report(mcu_public_key_der="not base64!"), report(key_state="ready", mcu_key_alg=0),
               report(ds_context=""), report(trust_profile="production"), report(security="31"), report(rd_dis_sealed=1)]
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
