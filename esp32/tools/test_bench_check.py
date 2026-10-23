"""The bench check against captured console shapes and the shared report vectors: a
board is judged only on what its firmware printed, and the report never carries the
setup password the capture holds."""

from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest

import bench_check

FIXTURES = Path(__file__).resolve().parents[2] / "common" / "fixtures"
ELF_SHA256 = "d4249998a" + "0" * 55
OBSERVER = "board-0003-00112233445566778899aabbccddeeff"


def log(level: str, tag: str, message: str) -> str:
    colour = {"I": "32", "W": "33", "E": "31"}[level]
    return f"\x1b[0;{colour}m{level} (1234) {tag}: {message}\x1b[0m"


BOOT = [
    "ESP-ROM:esp32s3-20210327",
    "rst:0x15 (USB_UART_CHIP_RESET),boot:0x2b (SPI_FAST_FLASH_BOOT)",
    log("I", "boot", "ESP-IDF v5.5.4 2nd stage bootloader"),
    log("I", "app_init", "Application information:"),
    log("I", "app_init", "Project name:     navfeeder-esp"),
    log("I", "app_init", "App version:      0.1.0+1.b8e037c"),
    log("I", "app_init", "Compile time:     Oct 21 2026 03:16:42"),
    log("I", "app_init", "ELF file SHA256:  d4249998a..."),
    log("I", "hw_manifest", f"manifest EEPROM factory serial: {OBSERVER}"),
    log("I", "hw_manifest", "manifest boot action: use manifest; board: NEO revision A"),
    log("I", "observer_board", "ATECC Info revision=00006005; ATECC608C (CRC verified)"),
    log("E", "observer_board", "ATECC RNG FAIL: fixed/repeating output; output is not trusted"),
    log("I", "observer_board", "temperature at 0x18: ESP_OK"),
    log("I", "observer_board", "temperature IDs=0054/0400; MCP9808 verified"),
    log("I", "observer_board", "RTC at 0x6f: ESP_OK"),
    log("I", "observer_board", "humidity at 0x40: ESP_OK"),
    log("I", "receiver", "valid receiver traffic at 460800 baud"),
    log("W", "navfeeder", "DEVELOPMENT SETUP LABEL — persistent setup password: WIFI:T:WPA;S:navfeeder-0A1B2C;P:abcdefghjkmn;;"),
    log("I", "netcfg", "browser provisioning ready: SSID='navfeeder-0A1B2C' (pass 12 chars) -> http://192.168.4.1/"),
    log("I", "netcfg_prov", "app provisioning ready over the setup network: name='navfeeder-0A1B2C', Security 2"),
    log("W", "navfeeder", "unprovisioned (wifi ssid is empty): scan the device label in the Station app"),
]


def console(lines=BOOT) -> str:
    return "\r\n".join(lines) + "\r\n"


def judge(text: str, mac="02a1b2c3d4e5") -> dict:
    return {
        c["name"]: (c["result"], c["detail"])
        for c in bench_check.evaluate(bench_check.parse_console(text), elf_sha256=ELF_SHA256, mac=mac, seconds=30)
    }


class ConsoleTests(unittest.TestCase):
    def test_a_good_unprovisioned_board_passes_every_check(self):
        checks = judge(console())
        self.assertEqual(
            checks,
            {
                "firmware.image": ("pass", "ELF SHA-256 d4249998a matches the build"),
                "boot": ("pass", "one boot, no panic"),
                "board_uid": ("pass", OBSERVER),
                "mcu_mac": ("pass", "02:a1:b2:c3:d4:e5"),
                "manifest": ("pass", "NEO revision A"),
                "atecc": ("pass", "ATECC608C revision 00006005, CRC verified"),
                "i2c:humidity@0x40": ("pass", "ESP_OK"),
                "i2c:rtc@0x6f": ("pass", "ESP_OK"),
                "i2c:temperature@0x18": ("pass", "ESP_OK"),
                "receiver": ("pass", "valid traffic at 460800 baud"),
                "setup": ("pass", "setup network and app provisioning ready"),
            },
        )

    def test_another_image_fails_the_firmware_check(self):
        text = console([line.replace("d4249998a", "5d322d70d") for line in BOOT])
        self.assertEqual(judge(text)["firmware.image"], ("fail", "booted ELF 5d322d70d, build ELF d4249998a"))
        unprinted = console([line for line in BOOT if "ELF file" not in line])
        self.assertEqual(judge(unprinted)["firmware.image"], ("fail", "no ELF SHA-256 printed"))

    def test_a_crash_or_second_boot_fails(self):
        crash = console([*BOOT, "Guru Meditation Error: Core  0 panic'ed (LoadProhibited). Exception was unhandled."])
        self.assertEqual(judge(crash)["boot"][0], "fail")
        self.assertIn("LoadProhibited", judge(crash)["boot"][1])
        again = console([*BOOT, "rst:0xc (RTC_SW_CPU_RST),boot:0x2b (SPI_FAST_FLASH_BOOT)"])
        self.assertEqual(judge(again)["boot"], ("fail", "2 boots in the window"))
        self.assertEqual(judge(console(BOOT[2:]))["boot"], ("fail", "no boot seen in the window"))

    def test_missing_identity_parts_and_receiver(self):
        text = console(
            [line for line in BOOT if "factory serial" not in line and "ATECC Info" not in line]
            + [log("E", "observer_board", "pressure at 0x76: ESP_ERR_TIMEOUT"), log("W", "receiver", "no UART bytes observed; check GNSS power and UART path")]
        )
        text = text.replace("valid receiver traffic at 460800 baud", "probing")
        checks = judge(text, mac=None)
        self.assertEqual(checks["board_uid"], ("fail", "no board identity read"))
        self.assertEqual(checks["atecc"], ("fail", "no CRC-verified info read"))
        self.assertEqual(checks["mcu_mac"], ("fail", "the ROM bootloader did not answer"))
        self.assertEqual(checks["i2c:pressure@0x76"], ("fail", "ESP_ERR_TIMEOUT"))
        self.assertEqual(checks["receiver"], ("fail", "no UART bytes from the receiver"))

    def test_an_unconfigured_manifest_fails_and_board_checks_are_skipped(self):
        text = console(
            [line for line in BOOT if "ATECC Info" not in line and " at 0x" not in line]
        ).replace("use manifest; board: NEO revision A", "first-boot initialization required; board: no board")
        checks = judge(text)
        self.assertEqual(checks["manifest"], ("fail", "first-boot initialization required; board: no board"))
        self.assertEqual(checks["atecc"], ("skip", "the manifest names no board, so board checks did not run"))
        self.assertFalse(any(name.startswith("i2c:") for name in checks))
        unprinted = console([line for line in BOOT if "boot action" not in line])
        self.assertEqual(judge(unprinted)["manifest"], ("fail", "no manifest boot action printed"))

    def test_part_names_become_check_names(self):
        text = console([*BOOT, log("I", "observer_board", "I/O expander at 0x20: ESP_OK")])
        self.assertEqual(judge(text)["i2c:i_o_expander@0x20"], ("pass", "ESP_OK"))

    def test_a_flaky_part_fails_and_says_so(self):
        text = console([*BOOT, log("I", "observer_board", "temperature at 0x18: ESP_ERR_TIMEOUT")])
        self.assertEqual(judge(text)["i2c:temperature@0x18"], ("fail", "ESP_OK, then ESP_ERR_TIMEOUT"))

    def test_setup_is_judged_only_on_an_unprovisioned_board(self):
        provisioned = console([line for line in BOOT if "unprovisioned" not in line and "provisioning ready" not in line])
        self.assertEqual(judge(provisioned)["setup"], ("skip", "the board did not report itself unprovisioned"))
        no_app = console([line for line in BOOT if "app provisioning ready" not in line])
        self.assertEqual(judge(no_app)["setup"], ("fail", "app provisioning not ready"))
        ble = console([line.replace("netcfg_prov: app provisioning ready over the setup network", "netcfg_ble: BLE provisioning ready") for line in BOOT])
        self.assertEqual(judge(ble)["setup"][0], "pass")

    def test_no_receiver_verdict_is_a_skip(self):
        text = console([line for line in BOOT if "receiver traffic" not in line])
        self.assertEqual(judge(text)["receiver"], ("skip", "no receiver verdict within 30 s"))

    def test_an_all_zero_identity_is_not_a_board(self):
        text = console().replace(OBSERVER, "board-0003-" + "0" * 32)
        self.assertEqual(judge(text)["board_uid"][0], "fail")


class ReportTests(unittest.TestCase):
    def setUp(self):
        self.build = bench_check.Build(
            path=Path("build"), image_sha256="3f" * 32, elf_sha256=ELF_SHA256, project_revision="b8e037c4" * 5,
            project_dirty=False, idf_revision="v5.5.4", target="esp32s3",
        )  # fmt: skip
        self.started = datetime(2026, 10, 23, 14, 2, 11, 400000, tzinfo=timezone.utc)

    def report(self, text=None, mac="02a1b2c3d4e5"):
        text = text or console()
        facts = bench_check.parse_console(text)
        checks = bench_check.evaluate(facts, elf_sha256=ELF_SHA256, mac=mac, seconds=30)
        return bench_check.build_report(
            station="bench-1", started=self.started, finished=self.started + timedelta(seconds=38), facts=facts,
            mac=mac, build=self.build, checks=checks, console=text.encode(),
        )  # fmt: skip

    def test_the_report_names_the_board_and_build_but_not_the_password(self):
        text = console()
        report = self.report(text)
        self.assertEqual(report["result"], "pass")
        self.assertEqual(report["started_at"], "2026-10-23T14:02:11Z")
        self.assertEqual(report["board"], {"observer_id": OBSERVER, "mcu_mac": "02a1b2c3d4e5"})
        self.assertEqual(report["firmware"]["app_version"], "0.1.0+1.b8e037c")
        self.assertEqual(report["console_sha256"], hashlib.sha256(text.encode()).hexdigest())
        self.assertNotIn("abcdefghjkmn", json.dumps(report))

    def test_a_failed_board_is_still_a_valid_report(self):
        report = self.report(console(BOOT[:3]), mac=None)
        self.assertEqual(report["result"], "fail")
        self.assertEqual(report["board"], {"observer_id": None, "mcu_mac": None})
        self.assertIsNone(report["firmware"]["app_version"])

    def test_an_overlong_version_is_left_out_rather_than_cut(self):
        text = console().replace("0.1.0+1.b8e037c", "v" * 40)
        self.assertIsNone(self.report(text)["firmware"]["app_version"])

    def test_files_are_private_and_never_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "bench" / "run"
            report_path, console_path = bench_check.write_private(out, self.report(), b"console")
            for path in (report_path, console_path):
                self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(os.stat(out).st_mode), 0o700)
            with self.assertRaises(FileExistsError):
                bench_check.write_private(out, self.report(), b"console")


class BuildTests(unittest.TestCase):
    def make(self, tmp: Path, *, recorded: str | None = None) -> Path:
        (tmp / "app.bin").write_bytes(b"image")
        (tmp / "app.elf").write_bytes(b"elf")
        (tmp / "project_description.json").write_text(
            json.dumps({"app_bin": "app.bin", "app_elf": "app.elf", "target": "esp32s3"})
        )
        provenance = {
            "project_revision": "a" * 40, "project_dirty": True, "idf_revision": "v5.5.4", "target": "esp32s3",
            "firmware_sha256": recorded or hashlib.sha256(b"image").hexdigest(),
        }  # fmt: skip
        (tmp / "firmware-provenance.json").write_text(json.dumps(provenance))
        return tmp

    def test_the_build_is_read_from_its_provenance(self):
        with tempfile.TemporaryDirectory() as tmp:
            build = bench_check.load_build(self.make(Path(tmp)))
        self.assertEqual(build.image_sha256, hashlib.sha256(b"image").hexdigest())
        self.assertEqual(build.elf_sha256, hashlib.sha256(b"elf").hexdigest())
        self.assertEqual((build.project_dirty, build.target), (True, "esp32s3"))

    def test_stale_provenance_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, "another image"):
                bench_check.load_build(self.make(Path(tmp), recorded="0" * 64))
            with self.assertRaisesRegex(ValueError, "not a recorded build"):
                bench_check.load_build(Path(tmp) / "missing")


def _strict(text: str):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate member {key}")
            result[key] = value
        return result

    def finite(value):
        raise ValueError(f"non-finite {value}")

    return json.loads(text, object_pairs_hook=unique, parse_constant=finite)


class SharedVectorTests(unittest.TestCase):
    """common/fixtures/bench-report-v1.json: the factory holds its validator to the same cases."""

    vectors = json.loads((FIXTURES / "bench-report-v1.json").read_text())

    def test_valid_reports_are_accepted(self):
        for report in self.vectors["valid"]:
            with self.subTest(result=report["result"]):
                bench_check.validate_report(report)

    def test_invalid_reports_are_refused(self):
        for case in self.vectors["invalid"]:
            with self.subTest(case["why"]):
                with self.assertRaises(ValueError):
                    bench_check.validate_report(case["document"] if "document" in case else _strict(case["text"]))


if __name__ == "__main__":
    unittest.main()
