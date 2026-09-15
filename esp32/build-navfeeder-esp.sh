#!/bin/sh
set -e
# Build the supported ESP32-S3 observer (16 MiB flash, 8 MiB PSRAM, layout 3).
# Usage: ./build-navfeeder-esp.sh [flash]
# S3_BUILD_DIR selects a separate generated configuration; existing values survive.
case "${1:-}" in
    ""|flash) ;;
    *) echo "usage: $0 [flash]" >&2; exit 2 ;;
esac
if [ "${1:-}" = flash ] && [ -z "${PORT:-}" ]; then
    echo "set PORT to the board's serial device (for example /dev/ttyACM0)" >&2
    exit 2
fi
IDF="${IDF_PATH:?Set IDF_PATH to your ESP-IDF 5.5.x checkout}"
if [ ! -f "$IDF/export.sh" ]; then
    echo "ESP-IDF not found at $IDF; install ESP-IDF and set IDF_PATH" >&2
    exit 1
fi
# shellcheck disable=SC1091
. "$IDF/export.sh"
cd "$(dirname "$0")"
build_dir=${S3_BUILD_DIR:-build/s3-layout3}

idf.py -B "$build_dir" -D "SDKCONFIG=$build_dir/sdkconfig" \
    -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3' \
    -D IDF_TARGET=esp32s3 build
# Preserve menuconfig overrides, while still reporting newly added defaults
# that an existing generated configuration has not picked up. Later files win.
python - "$build_dir/sdkconfig" <<'PY'
from pathlib import Path
import sys

def settings(name):
    return dict(line.split("=", 1) for line in Path(name).read_text().splitlines()
                if line.startswith("CONFIG_") and "=" in line)

expected = settings("sdkconfig.defaults")
expected.update(settings("sdkconfig.defaults.s3"))
actual = settings(sys.argv[1])
drift = [key for key, value in expected.items() if actual.get(key, "n") != value]
if drift:
    print("WARNING: generated sdkconfig differs from the S3 defaults: " + ", ".join(drift), file=sys.stderr)
    print("Review menuconfig overrides or use a fresh S3_BUILD_DIR to apply the current defaults.", file=sys.stderr)
PY
python tools/build_provenance.py --build-dir "$build_dir"

if [ "${1:-}" = flash ]; then
    python tools/production_profile.py --refuse-locking-build "$build_dir/sdkconfig"
    idf.py -B "$build_dir" -p "$PORT" flash monitor
fi
