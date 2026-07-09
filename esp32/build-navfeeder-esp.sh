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
idf.py set-target esp32c6
idf.py build

if [ "$1" = "flash" ]; then
    PORT="${PORT:-$(ls /dev/cu.usbmodem* 2>/dev/null | head -1)}"
    if [ -z "$PORT" ]; then
        echo "no serial port found; set PORT=/dev/cu.usbmodemXXXX" >&2
        exit 1
    fi
    idf.py -p "$PORT" flash monitor
fi
