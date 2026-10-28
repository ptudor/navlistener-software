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


def judge(text: str, mac="02a1b2c3d4e5", expect=None) -> dict:
    profile = bench_check.PROFILES[expect] if expect else None
    facts = bench_check.parse_console(text)
    return {
        c["name"]: (c["result"], c["detail"])
        for c in bench_check.evaluate(facts, elf_sha256=ELF_SHA256, mac=mac, seconds=30, expect=profile)
    }


# The RAM keys the firmware sets on a ZED-X20P, in order (ubx_probe.c).
X20_KEYS = [
    ("CFG-UART1OUTPROT-UBX", 0x10740001), ("CFG-MSGOUT-UBX_RXM_SFRBX_UART1", 0x20910232),
    ("CFG-MSGOUT-UBX_MON_RF_UART1", 0x2091035A), ("CFG-MSGOUT-UBX_NAV_SAT_UART1", 0x20910016),
    ("CFG-MSGOUT-UBX_NAV_PVT_UART1", 0x20910007), ("CFG-MSGOUT-UBX_NAV_STATUS_UART1", 0x2091001B),
    ("CFG-MSGOUT-UBX_TIM_TP_UART1", 0x2091017E), ("CFG-MSGOUT-UBX_NAV_SIG_UART1", 0x20910346),
    ("CFG-MSGOUT-UBX_NAV_CLOCK_UART1", 0x20910066), ("CFG-MSGOUT-UBX_NAV_EOE_UART1", 0x20910160),
    ("CFG-HW-ANT_CFG_SHORTDET", 0x10A3002F),
]  # fmt: skip

# A 162mm ZED-X20P board with its panel, as the universal image prints it in setup mode: the
# W5500's identity is read at every boot, and the uplink starts only once provisioned. The
# receiver powers up at 38400 baud and is moved to 460800 in its RAM before the other keys.
X20 = [
    *BOOT[:8],
    log("I", "hw_manifest", f"manifest EEPROM factory serial: {OBSERVER}"),
    log("I", "hw_manifest", "manifest boot action: use manifest; board: X20 revision A"),
    log("I", "observer_board", "temperature at 0x18: ESP_OK"),
    log("I", "observer_board", "temperature IDs=0054/0400; MCP9808 verified"),
    log("I", "observer_board", "humidity at 0x40: ESP_OK"),
    log("I", "observer_board", "humidity IDs=4954/07d0; HDC2080/HDC2022 family verified (the manifest names the variant)"),
    log("I", "observer_board", "pressure at 0x46: ESP_OK"),
    log("I", "observer_board", "pressure chip ID=0x50 revision=0x32; BMP580/BMP581 family (ID cannot distinguish them)"),
    log("I", "observer_board", "rail monitor at 0x41: ESP_OK"),
    log("I", "observer_board", "rail monitor IDs=5449/3220; INA3221 verified"),
    log("I", "observer_board", "RTC at 0x68: ESP_OK"),
    log("I", "observer_board", "DS3231-family RTC control=0x1c status=0x88: oscillator-stop flag set; clock not adopted"),
    log("I", "observer_board", "I/O expander at 0x24: ESP_OK"),
    log("I", "observer_board", "ATECC Info revision=00006005; ATECC608C (CRC verified)"),
    log("I", "io_expander", "MCP23008 at 0x24 configured: GPIO=0x3f, interrupt-on-change on GP1-GP5 to GPIO3"),
    log("I", "io_expander", "panel chain: 24 bits"),
    log("I", "ethernet", "W5500 VERSIONR=0x04; W5500 verified"),
    log("I", "receiver", "MON-VER: software=HPG 2.10                     hardware=000A0000  ; ZED-X20P=yes"),
    log("I", "receiver", "valid receiver traffic at 38400 baud"),
    log("I", "receiver", "MON-VER: software=HPG 2.10                     hardware=000A0000  ; ZED-X20P=yes"),
    log("I", "receiver", "receiver answering at 460800 baud, set in its RAM from 38400 (CFG-UART1-BAUDRATE)"),
    *(log("I", "receiver", f"RAM key {name} (0x{key:08x}): ACK") for name, key in X20_KEYS),
    log("I", "receiver", "receiver RAM configuration done: 11 of 11 keys acknowledged; the SFRBX counter confirms raw output"),
    *BOOT[-4:],
]

# A mosaic-G5 P3 board: the receiver speaks SBF on COM2.
P3 = [
    *BOOT[:8],
    log("I", "hw_manifest", f"manifest EEPROM factory serial: {OBSERVER}"),
    log("I", "hw_manifest", "manifest boot action: use manifest; board: P3 revision A"),
    *(log("I", "observer_board", f"{name} at 0x{address}: ESP_OK") for name, address in (
        ("temperature", "18"), ("humidity", "40"), ("SHT35 humidity", "44"), ("HIH8121 humidity", "27"),
        ("pressure", "77"), ("rail monitor", "41"), ("RTC", "6f"),
    )),  # fmt: skip
    log("I", "observer_board", "temperature IDs=0054/0400; MCP9808 verified"),
    log("I", "observer_board", "humidity IDs=4954/07d0; HDC2080/HDC2022 family verified (the manifest names the variant)"),
    log("I", "observer_board", "rail monitor IDs=5449/3220; INA3221 verified"),
    log("I", "observer_board", "ATECC Info revision=00006005; ATECC608C (CRC verified)"),
    log("I", "receiver", "receiver: mosaic-G5 P3 (engine mosaic-G5), firmware 4.15.0, serial 3912345"),
    log("I", "receiver", "receiver COM2 configured (1 time this boot, 0 command errors): SBF at 1 Hz"),
    *BOOT[-4:],
]


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

    def test_a_manifest_written_this_boot_says_so(self):
        text = console(
            [*BOOT[:9], log("W", "hw_manifest", "factory-init enabled: programming blank NEO first-spin revision A manifest"), *BOOT[9:]]
        )
        self.assertEqual(judge(text)["manifest"], ("pass", "NEO revision A; programmed this boot"))

    def test_a_p3_receiver_counts_as_traffic_without_expect(self):
        self.assertEqual(judge(console(P3))["receiver"], ("pass", "COM2 configured for SBF"))


class PreEnrollmentTests(unittest.TestCase):
    """--expect: the board must be the named board's revision A, with every part it lists."""

    def test_a_good_x20_passes_every_check(self):
        checks = judge(console(X20), expect="zed-x20")
        self.assertEqual(
            checks,
            {
                "firmware.image": ("pass", "ELF SHA-256 d4249998a matches the build"),
                "boot": ("pass", "one boot, no panic"),
                "board_uid": ("pass", OBSERVER),
                "mcu_mac": ("pass", "02:a1:b2:c3:d4:e5"),
                "manifest": ("pass", "X20 revision A, as expected"),
                "atecc": ("pass", "ATECC608C revision 00006005, CRC verified"),
                "i2c:humidity@0x40": ("pass", "ESP_OK"),
                "i2c:i_o_expander@0x24": ("pass", "ESP_OK"),
                "i2c:pressure@0x46": ("pass", "ESP_OK"),
                "i2c:rail_monitor@0x41": ("pass", "ESP_OK"),
                "i2c:rtc@0x68": ("pass", "ESP_OK"),
                "i2c:temperature@0x18": ("pass", "ESP_OK"),
                "identity:temperature@0x18": ("pass", "MCP9808 verified"),
                "identity:humidity@0x40": ("pass", "HDC2080/HDC2022 family verified (the manifest names the variant)"),
                "identity:pressure@0x46": ("pass", "BMP580/BMP581 family (ID cannot distinguish them)"),
                "identity:rail_monitor@0x41": ("pass", "INA3221 verified"),
                "receiver": ("pass", "valid traffic at 38400 baud, running at 460800 baud"),
                "receiver.model": ("pass", "MON-VER confirms ZED-X20P"),
                "receiver.config": (
                    "pass",
                    "UART1 at 460800 baud, set in its RAM from 38400; 11 of 11 RAM keys acknowledged",
                ),
                "ethernet": ("pass", "W5500 verified; it starts once the board is provisioned"),
                "panel_chain": ("pass", "24-bit chain read back"),
                "setup": ("pass", "setup network and app provisioning ready"),
            },
        )

    def test_the_receiver_config_reports_what_the_firmware_changed(self):
        # A receiver already at the configured rate, as after an ESP32 reset: nothing moved.
        kept = console(X20).replace("valid receiver traffic at 38400 baud", "valid receiver traffic at 460800 baud").replace(
            "set in its RAM from 38400 (CFG-UART1-BAUDRATE)", "the configured rate"
        )
        checks = judge(kept, expect="zed-x20")
        self.assertEqual(checks["receiver"], ("pass", "valid traffic at 460800 baud"))
        self.assertEqual(
            checks["receiver.config"], ("pass", "UART1 at 460800 baud, the configured rate; 11 of 11 RAM keys acknowledged")
        )
        # A refused or unanswered key fails the check and is named.
        refused = (
            console(X20)
            .replace("CFG-MSGOUT-UBX_NAV_EOE_UART1 (0x20910160): ACK", "CFG-MSGOUT-UBX_NAV_EOE_UART1 (0x20910160): NAK (unsupported)")
            .replace("CFG-HW-ANT_CFG_SHORTDET (0x10a3002f): ACK", "CFG-HW-ANT_CFG_SHORTDET (0x10a3002f): no ACK; output unconfirmed")
            .replace("11 of 11 keys", "9 of 11 keys")
        )
        self.assertEqual(
            judge(refused, expect="zed-x20")["receiver.config"],
            ("fail", "9 of 11 RAM keys acknowledged; CFG-MSGOUT-UBX_NAV_EOE_UART1 NAK; CFG-HW-ANT_CFG_SHORTDET no ACK"),
        )
        # Still configuring when the window closed.
        unfinished = console([line for line in X20 if "configuration done" not in line])
        self.assertEqual(judge(unfinished, expect="zed-x20")["receiver.config"], ("fail", "no RAM configuration result within 30 s"))
        # An unconfirmed model is never configured, which receiver.model already fails.
        unconfirmed = judge(console(X20).replace("ZED-X20P=yes", "ZED-X20P=unconfirmed"), expect="zed-x20")
        self.assertEqual(unconfirmed["receiver.config"], ("skip", "not configured: MON-VER did not confirm the listed model"))
        # Without --expect, the receiver line still shows the running rate.
        self.assertNotIn("receiver.config", judge(console(X20)))
        self.assertEqual(judge(console(X20))["receiver"], ("pass", "valid traffic at 38400 baud, running at 460800 baud"))

    def test_a_good_p3_passes(self):
        checks = judge(console(P3), expect="mosaic-p3")
        self.assertEqual(checks["receiver.model"], ("pass", "ReceiverSetup names mosaic-G5 P3"))
        self.assertNotIn("receiver.config", checks)  # the mosaic-G5 is configured in SBF
        self.assertNotIn("ethernet", checks)
        self.assertNotIn("panel_chain", checks)
        self.assertEqual({result for result, _ in checks.values()}, {"pass"})

    def test_another_board_fails_the_manifest(self):
        checks = judge(console(), expect="zed-x20")
        self.assertEqual(checks["manifest"], ("fail", "NEO revision A, expected X20 revision A"))
        # The NEO's RTC is not the X20's, and the X20's parts were never probed.
        self.assertEqual(checks["i2c:rtc@0x6f"], ("fail", "probed, but X20 revision A does not list it"))
        self.assertEqual(checks["i2c:rail_monitor@0x41"], ("fail", "not probed; X20 revision A lists it"))
        self.assertEqual(checks["identity:pressure@0x46"], ("fail", "no identity read"))

    def test_an_unprogrammed_board_fails_every_board_check(self):
        text = console(
            [line for line in X20 if "observer_board" not in line and "io_expander" not in line and "ethernet" not in line]
        ).replace("use manifest; board: X20 revision A", "first-boot initialization required; board: no board")
        checks = judge(text, expect="zed-x20")
        self.assertEqual(checks["manifest"], ("fail", "first-boot initialization required; board: no board"))
        self.assertEqual(checks["i2c:temperature@0x18"], ("fail", "not probed; X20 revision A lists it"))
        self.assertEqual(checks["ethernet"], ("fail", "no W5500 identity reported"))
        self.assertEqual(checks["panel_chain"], ("fail", "no panel chain verdict within 30 s"))

    def test_a_wrong_part_that_answers_fails_its_identity(self):
        text = console(X20).replace("INA3221 verified", "unexpected identity")
        checks = judge(text, expect="zed-x20")
        self.assertEqual(checks["i2c:rail_monitor@0x41"], ("pass", "ESP_OK"))
        self.assertEqual(checks["identity:rail_monitor@0x41"], ("fail", "unexpected identity"))
        # Without --expect the identity is not judged.
        self.assertNotIn("identity:rail_monitor@0x41", judge(text))

    def test_a_missing_receiver_verdict_fails(self):
        text = console([line for line in X20 if "receiver traffic" not in line and "MON-VER" not in line])
        checks = judge(text, expect="zed-x20")
        self.assertEqual(checks["receiver"], ("fail", "no receiver verdict within 30 s"))
        self.assertEqual(checks["receiver.model"], ("fail", "no MON-VER within 30 s"))
        self.assertEqual(judge(text)["receiver"][0], "skip")

    def test_the_receiver_must_be_the_listed_model(self):
        unconfirmed = judge(console(X20).replace("ZED-X20P=yes", "ZED-X20P=unconfirmed"), expect="zed-x20")
        self.assertEqual(unconfirmed["receiver.model"], ("fail", "MON-VER does not confirm ZED-X20P"))
        other = judge(console(X20).replace("ZED-X20P=yes", "NEO-M9N=yes"), expect="zed-x20")
        self.assertEqual(other["receiver.model"], ("fail", "the firmware looked for NEO-M9N, expected ZED-X20P"))
        unlisted = judge(console(X20).replace("ZED-X20P=yes", "no receiver listed, none configured"), expect="zed-x20")
        self.assertEqual(unlisted["receiver.model"], ("fail", "the manifest lists no receiver this firmware configures"))
        not_mosaic = console(P3).replace("serial 3912345", "serial 3912345" + bench_check._NOT_MOSAIC)
        self.assertEqual(judge(not_mosaic, expect="mosaic-p3")["receiver.model"][0], "fail")

    def test_ethernet_and_panel_failures(self):
        text = console(X20).replace("W5500 VERSIONR=0x04; W5500 verified", "W5500 VERSIONR=0xff; unexpected identity").replace(
            "panel chain: 24 bits",
            "panel chain not detected or wrong length: 3 readbacks matched neither 24 nor 16 bits "
            "(last read 0x000000 for frame 0xa5a5a5)",
        )
        checks = judge(text, expect="zed-x20")
        self.assertEqual(checks["ethernet"], ("fail", "W5500 VERSIONR=0xff; unexpected identity"))
        self.assertEqual(checks["panel_chain"], ("fail", "panel chain not detected; is the panel fitted?"))
        failed = console(X20).replace("W5500 VERSIONR=0x04; W5500 verified", "W5500 identity read failed: ESP_ERR_TIMEOUT")
        self.assertEqual(judge(failed, expect="zed-x20")["ethernet"], ("fail", "W5500 identity read failed: ESP_ERR_TIMEOUT"))
        # Only the first verdict counts.
        twice = console(X20) + console([log("E", "ethernet", "W5500 VERSIONR=0x00; unexpected identity")])
        self.assertEqual(judge(twice, expect="zed-x20")["ethernet"][0], "pass")

    def test_a_provisioned_board_must_also_start_its_w5500(self):
        provisioned = X20[:-4]  # no setup network: the board starts its uplink
        up = console(provisioned + [log("I", "ethernet", "W5500 up at 10 MHz, polled every 10 ms, MAC 02:a1:b2:c3:d4:e8")])
        self.assertEqual(judge(up, expect="zed-x20")["ethernet"], ("pass", "W5500 verified and started"))
        failed = console(
            provisioned + [log("E", "navfeeder", "ethernet unavailable (ESP_ERR_TIMEOUT); no uplink until it is repaired")]
        )
        self.assertEqual(
            judge(failed, expect="zed-x20")["ethernet"],
            ("fail", "W5500 verified, but ethernet unavailable (ESP_ERR_TIMEOUT); no uplink until it is repaired"),
        )
        silent = judge(console(provisioned), expect="zed-x20")
        self.assertEqual(silent["ethernet"], ("fail", "W5500 verified, but no start reported"))
        # A start without the boot identity read is still a failure: the identity is the test.
        unread = console([line for line in provisioned if "VERSIONR" not in line]
                         + [log("I", "ethernet", "W5500 up at 10 MHz, polled every 10 ms, MAC 02:a1:b2:c3:d4:e8")])
        self.assertEqual(judge(unread, expect="zed-x20")["ethernet"], ("fail", "no W5500 identity reported"))

    def test_a_late_detection_does_not_clear_a_failed_chain(self):
        text = console(X20).replace(
            "panel chain: 24 bits", "panel chain not detected or wrong length: 3 readbacks matched neither 24 nor 16 bits"
        )
        text += console([log("I", "io_expander", "panel chain: 24 bits")])
        self.assertEqual(judge(text, expect="zed-x20")["panel_chain"][0], "fail")

    def test_every_profile_checks_identities_of_parts_it_lists(self):
        for key, profile in bench_check.PROFILES.items():
            with self.subTest(board=key):
                names = [name for name, _ in profile.parts]
                self.assertEqual(len(names), len(set(names)))
                self.assertTrue(set(profile.identities) <= set(names))

    def test_a_strict_report_is_a_valid_v1_report(self):
        text = console(X20)
        facts = bench_check.parse_console(text)
        checks = bench_check.evaluate(
            facts, elf_sha256=ELF_SHA256, mac="02a1b2c3d4e5", seconds=30, expect=bench_check.PROFILES["zed-x20"]
        )
        build = bench_check.Build(
            path=Path("build"), image_sha256="3f" * 32, elf_sha256=ELF_SHA256, project_revision="b8e037c4" * 5,
            project_dirty=False, idf_revision="v5.5.4", target="esp32s3",
        )  # fmt: skip
        started = datetime(2026, 10, 27, 18, 0, 0, tzinfo=timezone.utc)
        report = bench_check.build_report(
            station="bench-1", started=started, finished=started + timedelta(seconds=40), facts=facts,
            mac="02a1b2c3d4e5", build=build, checks=checks, console=text.encode(),
        )  # fmt: skip
        self.assertEqual((report["tool"], report["result"]), ("bench_check.py 4", "pass"))


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
