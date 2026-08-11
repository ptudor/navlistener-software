#!/bin/sh
# Integrity Station app icon: GNSS receiver dial + rooftop station on the
# intsat/mapintsat starfield. See ../docs/ICONS.md.
set -eu

CONVERT=${IMAGEMAGICK_CONVERT:-/opt/local/bin/convert}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SWIFT_DIR=$(dirname "$SCRIPT_DIR")
SOURCE="$SWIFT_DIR/docs/icon-source/integrity-station.svg"
SET="$SWIFT_DIR/IntegrityStation/Resources/Assets.xcassets/AppIcon.appiconset"

if [ ! -x "$CONVERT" ]; then
  echo "ImageMagick convert not found at $CONVERT" >&2
  exit 1
fi

mkdir -p "$SET"

# Build stars at the final pixel dimensions. Scaling one large starfield down
# erases its faint stars and makes the small macOS icon look empty.
render_icon() {
  size=$1
  output=$2
  "$CONVERT" -size "${size}x${size}" gradient:'#0c1322-#04060c' \
    \( -size "${size}x${size}" xc:black +noise Random -channel R -separate +channel -threshold 99.75% -blur 0x0.4 \) -compose Screen -composite \
    \( -size "${size}x${size}" xc:black +noise Random -channel R -separate +channel -threshold 99.95% -blur 0x1.2 -level 0%,70% \) -compose Screen -composite \
    -alpha off \
    \( -background none "$SOURCE" -resize "${size}x${size}" \) -gravity center -compose over -composite \
    -alpha off "$output"
}

render_icon 1024 "$SET/icon_ios_1024.png"
for size in 16 32 64 128 256 512 1024; do
  render_icon "$size" "$SET/icon_${size}.png"
done

echo "wrote Integrity Station iOS and macOS icons to $SET"
