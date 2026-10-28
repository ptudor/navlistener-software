#!/bin/sh
set -e
# Build the supported ESP32-S3 observer (16 MiB flash, 8 MiB PSRAM, layout 3).
# Usage: [NVF_BOARD=universal|neo|zed-x20|max|mosaic-p3] ./build-navfeeder-esp.sh [flash]
#        NVF_MANIFEST_INIT=neo|zed-x20|zed-x20-b|max|mosaic-p3 NVF_MANIFEST_HDC=hdc2080|hdc2022 \
#            ./build-navfeeder-esp.sh [flash]
# NVF_BOARD selects the universal image (the default, in build/s3-layout3) or a
# single-board test build, each in its own build directory.
# NVF_MANIFEST_INIT builds the manufacturing image of the pre-enrollment sequence
# (docs/COMMISSIONING.md): the universal image with NVF_MANIFEST_FACTORY_INIT, which
# writes a blank, never-seen manifest EEPROM with that board revision's list and
# the batch's humidity sensor, in build/s3-manifest-init-<board>-<hdc>-layout3.
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
    neo|zed-x20|max|mosaic-p3) board_defaults=";sdkconfig.defaults.$board"; default_dir="build/s3-$board-layout3" ;;
    *) echo "NVF_BOARD must be universal, neo, zed-x20, max or mosaic-p3" >&2; exit 2 ;;
esac
init=${NVF_MANIFEST_INIT:-}
if [ -n "$init" ]; then
    if [ "$board" != universal ]; then
        echo "NVF_MANIFEST_INIT builds the universal image; leave NVF_BOARD unset" >&2
        exit 2
    fi
    case "$init" in
        neo) init_board=NEO_A ;;
        zed-x20) init_board=ZED_X20_A ;;
        zed-x20-b) init_board=ZED_X20_B ;;
        max) init_board=MAX_A ;;
        mosaic-p3) init_board=MOSAIC_P3_A ;;
        *) echo "NVF_MANIFEST_INIT must be neo, zed-x20, zed-x20-b, max or mosaic-p3" >&2; exit 2 ;;
    esac
    # The humidity part is written into every manifest and the firmware cannot tell
    # the two apart afterwards, so the batch's BOM must name it: there is no default.
    case "${NVF_MANIFEST_HDC:-}" in
        hdc2080) init_hdc=HDC2080 ;;
        hdc2022) init_hdc=HDC2022 ;;
        *) echo "NVF_MANIFEST_HDC must name the batch's humidity sensor: hdc2080 or hdc2022" >&2; exit 2 ;;
    esac
    default_dir="build/s3-manifest-init-$init-$NVF_MANIFEST_HDC-layout3"
elif [ -n "${NVF_MANIFEST_HDC:-}" ]; then
    echo "NVF_MANIFEST_HDC applies only with NVF_MANIFEST_INIT" >&2
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
build_dir=${S3_BUILD_DIR:-$default_dir}

# The manifest-initialization settings, generated from the two variables above and
# checked against the generated sdkconfig after the build.
init_defaults=
if [ -n "$init" ]; then
    init_defaults="$build_dir/sdkconfig.defaults.manifest-init"
    mkdir -p "$build_dir"
    printf 'CONFIG_NVF_MANIFEST_FACTORY_INIT=y\nCONFIG_NVF_MANIFEST_BOARD_%s=y\nCONFIG_NVF_MANIFEST_%s=y\n' \
        "$init_board" "$init_hdc" > "$init_defaults"
fi

idf.py -B "$build_dir" -D "SDKCONFIG=$build_dir/sdkconfig" \
    -D "SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3$board_defaults${init_defaults:+;$init_defaults}" \
    -D IDF_TARGET=esp32s3 build
# Preserve menuconfig overrides, while still reporting newly added defaults
# that an existing generated configuration has not picked up. Later files win.
# A defaults line naming a symbol the generated configuration does not have at
# all fails the build: Kconfig ignores an unknown symbol without a word, so a
# misspelled pin would otherwise read as "off" forever. A manifest-initialization
# build fails unless its sdkconfig has exactly the requested settings, before its
# provenance is recorded, so bench_check.py refuses the image.
python - "$build_dir/sdkconfig" "${board_defaults#;}" "$init_defaults" <<'PY'
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
required = settings(sys.argv[3]) if len(sys.argv) > 3 and sys.argv[3] else {}
actual = settings(sys.argv[1])
missing = [key for key in [*expected, *required] if key not in actual]
if missing:
    print("ERROR: the S3 defaults pin symbols the generated sdkconfig does not have "
          "(misspelled, or no longer in Kconfig): " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
wrong = [key for key, value in required.items() if actual[key] != value]
if wrong:
    print("ERROR: this manifest-initialization build needs " + ", ".join(f"{key}={required[key]}" for key in wrong) +
          "; its generated sdkconfig differs (a menuconfig change?). Build it in a fresh S3_BUILD_DIR.",
          file=sys.stderr)
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
