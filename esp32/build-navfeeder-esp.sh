#!/bin/sh
set -e
# Build (and optionally flash+monitor) navfeeder-esp for the Waveshare ESP32-C6-LCD-1.47.
# Usage:
#   ./build-navfeeder-esp.sh            # set-target esp32c6 + build
#   ./build-navfeeder-esp.sh flash      # build, then flash + monitor (set PORT=/dev/cu.usbmodem*)
#
# Set IDF_PATH to an installed ESP-IDF 5.5.x checkout. export.sh selects the
# toolchain and Python environment; IDF_TOOLS_PATH may override its default.
IDF="${IDF_PATH:?Set IDF_PATH to your ESP-IDF 5.5.x checkout}"
if [ ! -f "$IDF/export.sh" ]; then
    echo "ESP-IDF not found at $IDF; install ESP-IDF and set IDF_PATH" >&2
    exit 1
fi
# shellcheck disable=SC1091
. "$IDF/export.sh"

cd "$(dirname "$0")"
# `idf.py set-target` deletes and regenerates sdkconfig (renaming the old to
# sdkconfig.old), silently discarding any values a developer set via `idf.py menuconfig`
# (NVF_TOKEN/SSID/host) and flashing a Kconfig-default image. Only run it when there is no
# sdkconfig yet, or its CONFIG_IDF_TARGET isn't esp32c6; otherwise just build.
if [ ! -f sdkconfig ] || ! grep -q '^CONFIG_IDF_TARGET="esp32c6"' sdkconfig; then
    idf.py set-target esp32c6
fi

# Preserving an existing sdkconfig means a newly added default does not apply
# to sdkconfig.defaults later never takes effect on a dev box until sdkconfig is regenerated
# (CONFIG_UART_ISR_IN_IRAM shipped exactly this way — correct in source, absent from
# the built image). Warn — don't fail — when the active sdkconfig doesn't satisfy a defaults
# entry. Note a defaults "CONFIG_X=n" renders as "# CONFIG_X is not set" (or is absent) in a
# generated sdkconfig, so "=n" is only violated by an explicit "CONFIG_X=<value>" line.
drift=""
while IFS= read -r line; do
    case "$line" in
        CONFIG_*=*) ;;
        *) continue ;;
    esac
    key=${line%%=*}
    if [ "${line#*=}" = "n" ]; then
        grep -q "^${key}=" sdkconfig && drift="$drift $key"
    elif ! grep -qxF "$line" sdkconfig; then
        drift="$drift $key"
    fi
done < sdkconfig.defaults
if [ -n "$drift" ]; then
    {
        echo "WARNING: active sdkconfig does not satisfy sdkconfig.defaults for:"
        for k in $drift; do echo "    $k"; done
        echo "New sdkconfig.defaults entries never apply to an existing sdkconfig (set-target is"
        echo "skipped to preserve your menuconfig values). To land them, set the key via"
        echo "'idf.py menuconfig', or 'rm sdkconfig' and rerun (menuconfig values are lost)."
        echo "If the mismatch is a deliberate local override, ignore this warning."
    } >&2
fi

idf.py build
python tools/build_provenance.py

if [ "${1:-}" = "flash" ]; then
    PORT="${PORT:-}"
    if [ -z "$PORT" ]; then
        echo "set PORT to your board's serial device (for example /dev/ttyACM0)" >&2
        exit 1
    fi
    idf.py -p "$PORT" flash monitor
fi
