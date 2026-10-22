#!/bin/sh
# Builds "Parted Magic USB Installer.app" on a Mac with the Command Line Tools.
# Usage: build-mac.sh PMUSB_CORE OUTDIR
#   PMUSB_CORE  the darwin/arm64 pmusb core (cross-built on the host:
#               GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build)
#   OUTDIR      where the .app goes
# Signs ad hoc only; the Developer ID signature and notarization come later.
set -e
here=$(cd "$(dirname "$0")" && pwd)
core=$1; out=$2
[ -x "$core" ] && [ -n "$out" ] || { echo "usage: $0 PMUSB_CORE OUTDIR" >&2; exit 2; }
app="$out/Parted Magic USB Installer.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
swiftc -O -swift-version 5 -target arm64-apple-macos26.5 \
    -o "$app/Contents/MacOS/PMUSBInstaller" "$here/PMUSBApp.swift"
cp "$core" "$app/Contents/MacOS/pmusb"
chmod 755 "$app/Contents/MacOS/pmusb" "$app/Contents/MacOS/PMUSBInstaller"
cp "$here/Info.plist" "$app/Contents/Info.plist"
# Icon from pmusb_icon_1024.png (made by make_icon.py from the Canva picture).
set_dir=$(mktemp -d)/pmusb.iconset
mkdir -p "$set_dir"
for s in 16 32 128 256 512; do
    sips -z $s $s "$here/pmusb_icon_1024.png" --out "$set_dir/icon_${s}x${s}.png" >/dev/null
    d=$((s*2))
    sips -z $d $d "$here/pmusb_icon_1024.png" --out "$set_dir/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$set_dir" -o "$app/Contents/Resources/pmusb.icns"
codesign --force --sign - "$app/Contents/MacOS/pmusb"
codesign --force --sign - "$app"
shasum -a 256 "$app/Contents/MacOS/PMUSBInstaller" "$app/Contents/MacOS/pmusb"
