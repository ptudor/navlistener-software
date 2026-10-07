#!/usr/bin/env python3
"""Check a release build's actual security settings without writing device eFuses."""
import argparse
import json
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
    "CONFIG_NVF_UPDATE_PROFILE_TRUSTED": "y",
    # Commissioning evidence (docs/COMMISSIONING.md). The session proof signs keying material
    # exported from the TLS session. The microcontroller key's eFuse block is read-protected
    # after Secure Boot is enabled, which the bootloader forbids unless it is told to leave
    # read protection available; `commission seal` closes it again at the bench.
    "CONFIG_MBEDTLS_SSL_KEYING_MATERIAL_EXPORT": "y",
    "CONFIG_SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS": "y",
    "CONFIG_NVF_COMMISSION_CONSOLE": "y",
}
FORBIDDEN = ("CONFIG_NVF_MCU_KEY_UNLOCKED_TEST", "CONFIG_NVF_UPDATE_TEST_KEYS", "CONFIG_NVF_UPDATE_PROFILE_OPEN", "CONFIG_NVF_SETUP_CONSOLE_PASSWORD", "CONFIG_NVF_MANIFEST_FACTORY_INIT",
    "CONFIG_NVF_INSECURE", "CONFIG_SECURE_BOOT_BUILD_SIGNED_BINARIES", "CONFIG_SECURE_BOOT_ALLOW_JTAG",
    "CONFIG_SECURE_BOOT_ALLOW_ROM_BASIC", "CONFIG_SECURE_BOOT_ENABLE_AGGRESSIVE_KEY_REVOKE",
    "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_ENC", "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_DEC",
    "CONFIG_SECURE_FLASH_UART_BOOTLOADER_ALLOW_CACHE", "CONFIG_EFUSE_VIRTUAL")

# An open release must never be able to lock the chip it is installed on.
OPEN_REQUIRED = {
    "CONFIG_IDF_TARGET": '"esp32s3"',
    "CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE": "y",
    "CONFIG_PARTITION_TABLE_CUSTOM_FILENAME": '"partitions-s3.csv"',
    "CONFIG_PARTITION_TABLE_OFFSET": "0x10000",
    "CONFIG_APP_REPRODUCIBLE_BUILD": "y",
    "CONFIG_NVF_UPDATE_PROFILE_OPEN": "y",
    # An open board presents its record without a proof; the console installs that record.
    "CONFIG_NVF_COMMISSION_CONSOLE": "y",
}
OPEN_FORBIDDEN = ("CONFIG_NVF_MCU_KEY_UNLOCKED_TEST", "CONFIG_NVF_UPDATE_TEST_KEYS", "CONFIG_NVF_UPDATE_PROFILE_TRUSTED", "CONFIG_NVF_MANIFEST_FACTORY_INIT",
    "CONFIG_NVF_INSECURE", "CONFIG_SECURE_BOOT", "CONFIG_SECURE_FLASH_ENC_ENABLED", "CONFIG_SECURE_SIGNED_APPS_NO_SECURE_BOOT",
    "CONFIG_SECURE_BOOT_BUILD_SIGNED_BINARIES", "CONFIG_NVS_ENCRYPTION", "CONFIG_EFUSE_VIRTUAL")
# The trusted track ships only the locked production security baseline above.
PROFILES = {"trusted": (REQUIRED, FORBIDDEN), "open": (OPEN_REQUIRED, OPEN_FORBIDDEN)}
# The defaults file each profile is built from (esp32/), checked against the generated
# configuration by check_defaults.
PROFILE_DEFAULTS = {"trusted": "sdkconfig.defaults.production", "open": "sdkconfig.defaults.open"}
SYMBOL_TYPES = ("bool", "int", "hex", "string")

def settings(path):
    return dict(line.split("=", 1) for line in Path(path).read_text().splitlines() if line.startswith("CONFIG_") and "=" in line)

def generated(path):
    """Every symbol a generated sdkconfig names, reading a disabled bool as "n"."""
    values = {}
    for line in Path(path).read_text().splitlines():
        if line.startswith("CONFIG_") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
        elif line.startswith("# CONFIG_") and line.endswith(" is not set"):
            values[line[len("# "):-len(" is not set")]] = "n"
    return values

def known_symbols(sdkconfig):
    """The symbols the build's Kconfig tree declares, visible or not: idf.py writes the menu
    tree next to the generated sdkconfig. None when that build directory holds no tree."""
    menus = Path(sdkconfig).resolve().parent / "config" / "kconfig_menus.json"
    if not menus.is_file():
        return None
    names = set()

    def collect(node):
        if isinstance(node, dict):
            if node.get("type") in SYMBOL_TYPES and isinstance(node.get("name"), str):
                names.add("CONFIG_" + node["name"])
            collect(node.get("children", []))
        elif isinstance(node, list):
            for child in node:
                collect(child)
    collect(json.loads(menus.read_text()))
    return names

def check_defaults(sdkconfig, defaults):
    """Every CONFIG_ line of a profile defaults file must have reached the generated
    configuration. Kconfig silently ignores a misspelled symbol, and a bool pinned to "n" is
    indistinguishable from "absent, therefore off" unless the symbol is checked by name. A
    symbol the build declares but hides (its dependencies are unmet) is pinned to no effect
    and passes; an unknown one fails."""
    defaults = Path(defaults)
    values = generated(sdkconfig)
    symbols = known_symbols(sdkconfig)
    bad = []
    for key, value in settings(defaults).items():
        if key in values:
            if values[key] != value:
                bad.append(f"{key} is {values[key]}, pinned to {value}")
        elif symbols is None:
            bad.append(f"{key} is not in the generated sdkconfig, and no config/kconfig_menus.json beside it names the build's symbols")
        elif key not in symbols:
            bad.append(f"{key} is not a symbol of this build (misspelled, or the option no longer exists)")
    if bad:
        raise ValueError(f"{defaults.name} not applied: " + "; ".join(bad))

def verify(path, profile="trusted"):
    required, forbidden = PROFILES[profile]
    values = settings(path)
    bad = [k for k, v in required.items() if values.get(k) != v]
    bad += [k for k in forbidden if values.get(k) == "y"]
    bad += [k for k in ("CONFIG_SECURE_BOOT_SIGNING_KEY", "CONFIG_SECURE_BOOT_VERIFICATION_KEY") if values.get(k, '""') != '""']
    if bad:
        raise ValueError(f"{profile} profile rejected: " + ", ".join(bad))
    return values

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--refuse-locking-build", action="store_true")
    parser.add_argument("--profile", choices=sorted(PROFILES), default="trusted")
    parser.add_argument("--defaults", type=Path,
                        help="the profile defaults file to check against the generated sdkconfig "
                             "(default: the profile's own file next to this tool's esp32 directory)")
    parser.add_argument("sdkconfig", type=Path)
    args = parser.parse_args()
    if args.refuse_locking_build:
        values = settings(args.sdkconfig)
        if values.get("CONFIG_SECURE_BOOT") == "y" or values.get("CONFIG_SECURE_FLASH_ENC_ENABLED") == "y":
            raise ValueError("first-boot-flash refuses a build that can permanently lock the chip; use the separately reviewed production commissioning procedure")
    else:
        verify(args.sdkconfig, args.profile)
        defaults = args.defaults or Path(__file__).resolve().parent.parent / PROFILE_DEFAULTS[args.profile]
        check_defaults(args.sdkconfig, defaults)
    print("security profile check passed; no device was modified")

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        raise SystemExit(str(error)) from None
