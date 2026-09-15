#!/usr/bin/env python3
"""Check a build's actual security settings without writing device eFuses."""
import argparse
from pathlib import Path

REQUIRED = {
    "CONFIG_IDF_TARGET": '"esp32s3"',
    "CONFIG_SECURE_BOOT": "y", "CONFIG_SECURE_BOOT_V2_ENABLED": "y",
    "CONFIG_SECURE_SIGNED_APPS_RSA_SCHEME": "y",
    "CONFIG_SECURE_FLASH_ENC_ENABLED": "y",
    "CONFIG_SECURE_FLASH_ENCRYPTION_MODE_RELEASE": "y",
    "CONFIG_NVS_ENCRYPTION": "y", "CONFIG_NVS_SEC_KEY_PROTECT_USING_FLASH_ENC": "y",
    "CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE": "y",
    "CONFIG_SECURE_ENABLE_SECURE_ROM_DL_MODE": "y",
    "CONFIG_PARTITION_TABLE_CUSTOM_FILENAME": '"partitions-s3.csv"',
    "CONFIG_PARTITION_TABLE_OFFSET": "0x10000",
    "CONFIG_APP_REPRODUCIBLE_BUILD": "y",
}
FORBIDDEN = ("CONFIG_NVF_UPDATE_TEST_KEYS", "CONFIG_NVF_SETUP_CONSOLE_PASSWORD", "CONFIG_NVF_MANIFEST_FACTORY_INIT",
    "CONFIG_NVF_INSECURE", "CONFIG_SECURE_BOOT_BUILD_SIGNED_BINARIES", "CONFIG_SECURE_BOOT_ALLOW_JTAG",
    "CONFIG_SECURE_BOOT_ALLOW_ROM_BASIC", "CONFIG_SECURE_BOOT_V2_AGGRESSIVE_KEY_REVOKE",
    "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_ENC", "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_DEC",
    "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_CACHE", "CONFIG_EFUSE_VIRTUAL")

def settings(path):
    return dict(line.split("=", 1) for line in Path(path).read_text().splitlines() if line.startswith("CONFIG_") and "=" in line)

def verify(path):
    values = settings(path)
    bad = [k for k, v in REQUIRED.items() if values.get(k) != v]
    bad += [k for k in FORBIDDEN if values.get(k) == "y"]
    bad += [k for k in ("CONFIG_SECURE_BOOT_SIGNING_KEY", "CONFIG_SECURE_BOOT_VERIFICATION_KEY") if values.get(k, '""') != '""']
    if bad:
        raise ValueError("production profile rejected: " + ", ".join(bad))
    return values

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--refuse-locking-build", action="store_true")
    parser.add_argument("sdkconfig", type=Path)
    args = parser.parse_args()
    if args.refuse_locking_build:
        values = settings(args.sdkconfig)
        if values.get("CONFIG_SECURE_BOOT") == "y" or values.get("CONFIG_SECURE_FLASH_ENC_ENABLED") == "y":
            raise ValueError("first-boot-flash refuses a build that can permanently lock the chip; use the separately reviewed production commissioning procedure")
    else:
        verify(args.sdkconfig)
    print("security profile check passed; no device was modified")

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        raise SystemExit(str(error)) from None
