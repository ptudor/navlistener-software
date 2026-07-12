#!/bin/sh
# Build (and optionally flash+monitor) navfeeder-esp for the Waveshare ESP32-C6-LCD-1.47.
# Usage:
#   ./build-navfeeder-esp.sh            # set-target esp32c6 + build
#   ./build-navfeeder-esp.sh flash      # build, then flash + monitor (set PORT=/dev/cu.usbmodem*)
#
# Sources the house ESP-IDF at ~/esp/esp-idf (stay inside ~/Git).
set -e

IDF="${IDF_PATH:-$HOME/esp/esp-idf}"
if [ ! -f "$IDF/export.sh" ]; then
    echo "ESP-IDF not found at $IDF — set IDF_PATH or install to ~/esp/esp-idf" >&2
    exit 1
fi

# Tools + Python env live under ~/.espressif (stay inside ~/Git). The IDF v5.5 venv is
# built on Python 3.12 (idf5.5_py3.12_env); prepend it so export.sh resolves the matching env
# instead of hunting for one keyed to the system python. Rebuild the env with:
#   /opt/local/bin/python3.12 $IDF/tools/idf_tools.py install-python-env
export IDF_TOOLS_PATH="${IDF_TOOLS_PATH:-$HOME/.espressif}"
PYENV="$IDF_TOOLS_PATH/python_env/idf5.5_py3.12_env/bin"
[ -d "$PYENV" ] && PATH="$PYENV:$PATH" && export PATH
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

# A1 (specification review): the regression fix guard above keeps an existing sdkconfig, so a key added
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

if [ "$1" = "flash" ]; then
    PORT="${PORT:-$(ls /dev/cu.usbmodem* 2>/dev/null | head -1)}"
    if [ -z "$PORT" ]; then
        echo "no serial port found; set PORT=/dev/cu.usbmodemXXXX" >&2
        exit 1
    fi
    idf.py -p "$PORT" flash monitor
fi
