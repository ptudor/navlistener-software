"""Release profile checks against disposable sdkconfig and root fixtures."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from production_profile import OPEN_REQUIRED, REQUIRED, verify

TOOLS = Path(__file__).resolve().parent


def sdkconfig(directory, values):
    path = Path(directory) / "sdkconfig"
    path.write_text("".join(f"{key}={value}\n" for key, value in values.items()))
    return path


class ProfileTests(unittest.TestCase):
    def test_each_release_profile_accepts_only_its_own_settings(self):
        with tempfile.TemporaryDirectory() as temporary:
            verify(sdkconfig(temporary, REQUIRED))
            verify(sdkconfig(temporary, OPEN_REQUIRED), "open")
            with self.assertRaisesRegex(ValueError, "CONFIG_NVF_UPDATE_PROFILE_OPEN"):
                verify(sdkconfig(temporary, REQUIRED), "open")
            with self.assertRaisesRegex(ValueError, "CONFIG_NVF_UPDATE_PROFILE_TRUSTED"):
                verify(sdkconfig(temporary, OPEN_REQUIRED))

    def test_open_release_cannot_lock_a_chip_or_carry_test_trust(self):
        with tempfile.TemporaryDirectory() as temporary:
            for setting in ("CONFIG_SECURE_BOOT", "CONFIG_SECURE_FLASH_ENC_ENABLED", "CONFIG_NVF_UPDATE_TEST_KEYS",
                            "CONFIG_NVF_MANIFEST_FACTORY_INIT", "CONFIG_NVF_INSECURE"):
                with self.assertRaisesRegex(ValueError, setting):
                    verify(sdkconfig(temporary, {**OPEN_REQUIRED, setting: "y"}), "open")
            with self.assertRaisesRegex(ValueError, "CONFIG_SECURE_BOOT_SIGNING_KEY"):
                verify(sdkconfig(temporary, {**OPEN_REQUIRED, "CONFIG_SECURE_BOOT_SIGNING_KEY": '"key.pem"'}), "open")

    def test_trusted_release_can_prove_its_microcontroller(self):
        # A locked unit cannot be reflashed over USB, so the release build itself must be able
        # to make and protect the microcontroller key and to bind a proof to a TLS session.
        with tempfile.TemporaryDirectory() as temporary:
            for setting in ("CONFIG_MBEDTLS_SSL_KEYING_MATERIAL_EXPORT", "CONFIG_SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS",
                            "CONFIG_NVF_COMMISSION_CONSOLE"):
                self.assertIn(setting, REQUIRED)
                with self.assertRaisesRegex(ValueError, setting):
                    verify(sdkconfig(temporary, {key: value for key, value in REQUIRED.items() if key != setting}))
            # The unlocked-board key option is a bench aid and never ships on either track.
            with self.assertRaisesRegex(ValueError, "CONFIG_NVF_MCU_KEY_UNLOCKED_TEST"):
                verify(sdkconfig(temporary, {**REQUIRED, "CONFIG_NVF_MCU_KEY_UNLOCKED_TEST": "y"}))
            with self.assertRaisesRegex(ValueError, "CONFIG_NVF_MCU_KEY_UNLOCKED_TEST"):
                verify(sdkconfig(temporary, {**OPEN_REQUIRED, "CONFIG_NVF_MCU_KEY_UNLOCKED_TEST": "y"}), "open")
            # An open release changes no eFuse, so it must not ask the bootloader for anything.
            self.assertNotIn("CONFIG_SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS", OPEN_REQUIRED)

    def test_trusted_release_refuses_open_and_test_trust(self):
        with tempfile.TemporaryDirectory() as temporary:
            for setting in ("CONFIG_NVF_UPDATE_PROFILE_OPEN", "CONFIG_NVF_UPDATE_TEST_KEYS"):
                with self.assertRaisesRegex(ValueError, setting):
                    verify(sdkconfig(temporary, {**REQUIRED, setting: "y"}))


class EmbeddedRootTests(unittest.TestCase):
    def embed(self, directory, signed, profile):
        root = Path(directory) / "root.json"
        root.write_text(json.dumps({"signed": signed, "signatures": []}))
        return subprocess.run([sys.executable, str(TOOLS / "embed_update_root.py"), str(root),
            str(Path(directory) / "update_root.c"), "--profile", profile], capture_output=True, text=True)

    def test_root_marker_must_name_the_build_profile(self):
        with tempfile.TemporaryDirectory() as temporary:
            signed = {"_type": "root", "keys": {}, "x_navlisten_profile": "open"}
            self.assertEqual(self.embed(temporary, signed, "open").returncode, 0)
            self.assertIn("nvf_update_root_length", (Path(temporary) / "update_root.c").read_text())
            for profile in ("trusted", "test"):
                result = self.embed(temporary, signed, profile)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f"{profile} build profile marker", result.stderr)
            unmarked = {"_type": "root", "keys": {}}
            for profile in ("trusted", "open", "test"):
                self.assertNotEqual(self.embed(temporary, unmarked, profile).returncode, 0)


if __name__ == "__main__":
    unittest.main()
