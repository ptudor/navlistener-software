#!/usr/bin/env bash
set -eo pipefail

tool_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
esp_dir=$(CDPATH= cd -- "$tool_dir/.." && pwd)
cd "$esp_dir"

# NVF_BOARD selects the board, as in build-navfeeder-esp.sh (default neo).
board=${NVF_BOARD:-neo}
case "$board" in
    neo) board_defaults=; default_dir=build/s3-layout3 ;;
    zed-x20|max) board_defaults=";sdkconfig.defaults.$board"; default_dir="build/s3-$board-layout3" ;;
    *) echo "NVF_BOARD must be neo, zed-x20 or max" >&2; exit 2 ;;
esac
build_dir=${S3_BUILD_DIR:-$default_dir}

cat <<EOF
=== ATTENDED ESP32-S3 FIRST-BOOT FLASH ===
Watch the console for: NEW SETUP LABEL or DEVELOPMENT SETUP LABEL
The JSON on that line contains the persistent BLE/SoftAP setup password.
Keep it private, print the QR plus text password, and attach the label before deployment.
Development builds reprint an existing credential whenever setup starts.
This S3 baseline uses partition layout 3. Moving from an older layout requires
reprovisioning and a new setup label; back up configuration before USB migration.
Exit the ESP-IDF monitor with Ctrl-].
EOF

if [[ -z ${PORT:-} ]]; then
    echo "first-boot-flash: set PORT to the S3 USB serial device" >&2
    exit 2
fi
if [[ -z ${IDF_PATH:-} || ! -f $IDF_PATH/export.sh ]]; then
    echo "first-boot-flash: set IDF_PATH to an ESP-IDF 5.5.x checkout" >&2
    exit 2
fi

umask 077
mkdir -p "$build_dir"
if [[ -n ${FIRST_BOOT_DIR:-} ]]; then
    capture_dir=$FIRST_BOOT_DIR
    if [[ -e $capture_dir ]]; then
        echo "first-boot-flash: FIRST_BOOT_DIR must name a new directory: $capture_dir" >&2
        exit 2
    fi
    mkdir -p "$capture_dir"
else
    capture_dir=$(mktemp -d "$build_dir/first-boot.XXXXXX")
fi
console_log=$capture_dir/console.log
label_line=$capture_dir/setup-label.txt
qr_svg=$capture_dir/setup-qr.svg
echo "Private capture directory: $capture_dir"
echo "Console log:              $console_log"
echo "Label line:               $label_line"

: > "$console_log"
chmod 600 "$console_log"

# ESP-IDF resolves managed components for the selected target in this tracked
# file. Preserve the checkout's resolution while archiving the S3 result through
# build_provenance.py below.
lock_backup=$(mktemp "${TMPDIR:-/tmp}/navfeeder-dependencies.XXXXXX")
lock_existed=0
if [[ -f dependencies.lock ]]; then
    cp dependencies.lock "$lock_backup"
    lock_existed=1
fi
restore_lock() {
    if ((lock_existed)); then
        cp "$lock_backup" dependencies.lock
    else
        rm -f dependencies.lock
    fi
    rm -f "$lock_backup"
}
trap restore_lock EXIT

# shellcheck disable=SC1090
. "$IDF_PATH/export.sh"

idf.py -B "$build_dir" \
    -D "SDKCONFIG=$build_dir/sdkconfig" \
    -D "SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3$board_defaults" \
    -D IDF_TARGET=esp32s3 build
python tools/build_provenance.py --build-dir "$build_dir"
python tools/production_profile.py --refuse-locking-build "$build_dir/sdkconfig"
restore_lock
trap - EXIT

set +e
idf.py -B "$build_dir" -p "$PORT" flash monitor 2>&1 | tee -a "$console_log"
monitor_status=${PIPESTATUS[0]}
set -e

if LC_ALL=C grep -m 1 -E '(NEW|DEVELOPMENT) SETUP LABEL' "$console_log" > "$label_line"; then
    chmod 600 "$label_line"
    echo
    echo "Captured the persistent setup credential:"
    if command -v qrencode >/dev/null 2>&1; then
        python3 tools/provisioning_label.py "$label_line" --qr-svg "$qr_svg"
    else
        python3 tools/provisioning_label.py "$label_line"
        echo "QR SVG not created because qrencode is unavailable. After installing it, run:"
        echo "  python3 tools/provisioning_label.py '$label_line' --qr-svg '$qr_svg'"
    fi
    echo "Private capture directory: $capture_dir"
else
    rm -f "$label_line"
    echo >&2
    echo "No setup label line was captured." >&2
    echo "The device may be provisioned, or production logging may suppress the label." >&2
    echo "In development, reset configuration to reprint the same setup credential." >&2
    echo "The physical label/display also works. Erasing all NVS creates a new secret" >&2
    echo "and invalidates the old label and other NVS state." >&2
    echo "Diagnostic console log retained in: $console_log" >&2
fi

# Ctrl-C still permits label extraction above; preserve other monitor failures.
if ((monitor_status != 0 && monitor_status != 130)); then
    exit "$monitor_status"
fi
