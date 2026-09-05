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

# Tools + Python env live under ~/.espressif (stay inside ~/Git). idf5.5_py3.12_env is
# the pinned venv (MacPorts python3.12); prepend it so export.sh resolves that one rather
# than hunting for an env keyed to whatever `python3` currently is. It is a preference, not a
# requirement: `install.sh` builds its venv from the system interpreter, and IDF 5.5.4 does
# build this project against a py3.14 env (verified 2026-07-24, regression fix) — so an env with a
# different Python minor is accepted with a note, not an error.
export IDF_TOOLS_PATH="${IDF_TOOLS_PATH:-$HOME/.espressif}"
PYENV="$IDF_TOOLS_PATH/python_env/idf5.5_py3.12_env/bin"

# regression fix preflight. This check used to be a comment: the PATH prepend was
# `[ -d "$PYENV" ] && ...`, so a missing env silently no-op'd and export.sh then went hunting
# for an env keyed to whatever `python3` happens to be (on this machine: a py3.14 env that
# does not exist either), failing several steps later with a path nobody recognizes. Say what
# is wrong and print the exact remediation BEFORE sourcing export.sh.
if [ -d "$PYENV" ]; then
    PATH="$PYENV:$PATH"; export PATH
elif [ -n "$(ls -d "$IDF_TOOLS_PATH"/python_env/*/bin 2>/dev/null)" ]; then
    # A different env exists (other IDF release or Python minor) — the normal state after a
    # plain `install.sh`, which builds its venv from the system interpreter. Let export.sh
    # resolve it and say which, so a version-specific failure later is not a mystery.
    {
        echo "NOTE: pinned $PYENV absent; export.sh will resolve one of:"
        ls -d "$IDF_TOOLS_PATH"/python_env/*/ 2>/dev/null | sed 's/^/          /'
    } >&2
else
    PY312="${IDF_PYTHON:-/opt/local/bin/python3.12}"   # MacPorts (house macOS toolchain)
    {
        echo "ESP-IDF Python environment not found under $IDF_TOOLS_PATH/python_env."
        echo "Nothing can build until it is bootstrapped. Run ONE of:"
        echo
        echo "  # full toolchain + python env for this target (fresh machine):"
        echo "  IDF_TOOLS_PATH=$IDF_TOOLS_PATH $IDF/install.sh esp32c6"
        echo
        echo "  # python env only (toolchain already installed):"
        echo "  IDF_TOOLS_PATH=$IDF_TOOLS_PATH $PY312 $IDF/tools/idf_tools.py install-python-env"
        echo
        [ -x "$PY312" ] || echo "NOTE: $PY312 is missing too — install it (MacPorts: port install python312)"
        echo "Then re-run: $0 $*"
    } >&2
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
python tools/build_provenance.py

if [ "$1" = "flash" ]; then
    PORT="${PORT:-$(ls /dev/cu.usbmodem* 2>/dev/null | head -1)}"
    if [ -z "$PORT" ]; then
        echo "no serial port found; set PORT=/dev/cu.usbmodemXXXX" >&2
        exit 1
    fi
    idf.py -p "$PORT" flash monitor
fi
