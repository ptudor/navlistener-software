#!/bin/sh
set -e
# Build the supported ESP32-S3 observer (16 MiB flash, 8 MiB PSRAM, layout 3).
# Usage: [NVF_BOARD=universal|neo|zed-x20|max] ./build-navfeeder-esp.sh [flash]
# NVF_BOARD selects the universal image (the default, in build/s3-layout3) or a
# single-board test build, each in its own build directory.
# S3_BUILD_DIR selects a separate generated configuration; existing values survive.
case "${1:-}" in
    ""|flash) ;;
    *) echo "usage: $0 [flash]" >&2; exit 2 ;;
esac
if [ "${1:-}" = flash ] && [ -z "${PORT:-}" ]; then
    echo "set PORT to the board's serial device (for example /dev/ttyACM0)" >&2
    exit 2
fi
board=${NVF_BOARD:-universal}
case "$board" in
    universal) board_defaults=; default_dir=build/s3-layout3 ;;
    neo|zed-x20|max) board_defaults=";sdkconfig.defaults.$board"; default_dir="build/s3-$board-layout3" ;;
    *) echo "NVF_BOARD must be universal, neo, zed-x20 or max" >&2; exit 2 ;;
esac
IDF="${IDF_PATH:?Set IDF_PATH to your ESP-IDF 5.5.x checkout}"
if [ ! -f "$IDF/export.sh" ]; then
    echo "ESP-IDF not found at $IDF; install ESP-IDF and set IDF_PATH" >&2
    exit 1
fi
# shellcheck disable=SC1091
. "$IDF/export.sh"
cd "$(dirname "$0")"
build_dir=${S3_BUILD_DIR:-$default_dir}

idf.py -B "$build_dir" -D "SDKCONFIG=$build_dir/sdkconfig" \
    -D "SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3$board_defaults" \
    -D IDF_TARGET=esp32s3 build
# Preserve menuconfig overrides, while still reporting newly added defaults
# that an existing generated configuration has not picked up. Later files win.
# A defaults line naming a symbol the generated configuration does not have at
# all fails the build: Kconfig ignores an unknown symbol without a word, so a
# misspelled pin would otherwise read as "off" forever.
python - "$build_dir/sdkconfig" "${board_defaults#;}" <<'PY'
from pathlib import Path
import sys

def settings(name):
    values = {}
    for line in Path(name).read_text().splitlines():
        if line.startswith("CONFIG_") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
        elif line.startswith("# CONFIG_") and line.endswith(" is not set"):
            values[line[len("# "):-len(" is not set")]] = "n"  # a disabled bool, as kconfig writes it
    return values

expected = settings("sdkconfig.defaults")
expected.update(settings("sdkconfig.defaults.s3"))
if len(sys.argv) > 2 and sys.argv[2]:
    # The board file picks one member of the board choice; the S3 file's universal
    # default then does not apply.
    chosen = settings(sys.argv[2])
    for key in [key for key in expected if key.startswith("CONFIG_NVF_BOARD_GNSS_COLOR_") and key not in chosen]:
        del expected[key]
    expected.update(chosen)
actual = settings(sys.argv[1])
missing = [key for key in expected if key not in actual]
if missing:
    print("ERROR: the S3 defaults pin symbols the generated sdkconfig does not have "
          "(misspelled, or no longer in Kconfig): " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
drift = [key for key, value in expected.items() if actual[key] != value]
if drift:
    print("WARNING: generated sdkconfig differs from the S3 defaults: " + ", ".join(drift), file=sys.stderr)
    print("Review menuconfig overrides or use a fresh S3_BUILD_DIR to apply the current defaults.", file=sys.stderr)
PY
python tools/build_provenance.py --build-dir "$build_dir"

if [ "${1:-}" = flash ]; then
    python tools/production_profile.py --refuse-locking-build "$build_dir/sdkconfig"
    idf.py -B "$build_dir" -p "$PORT" flash monitor
fi
