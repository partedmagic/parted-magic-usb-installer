#!/bin/bash
# Builds the release of Parted Magic USB Installer for macOS: the app signed
# with the Developer ID Application certificate of Parted Magic LLC,
# notarized by Apple and stapled, in a .dmg that is signed, notarized and
# stapled too, so Gatekeeper opens either one even on a Mac that is offline.
#
# Run it in Terminal on the Mac, in the desktop session.  Over SSH macOS
# keeps the login keychain locked, and codesign and notarytool both need it:
# the signing key, and the notarytool profile.  The first signing shows a
# dialog asking for the login password, for codesign's use of the key: type
# it and click Always Allow.
#
# Usage: release-mac.sh PMUSB_CORE OUTDIR
#   PMUSB_CORE  the darwin/arm64 pmusb core (cross-built on the host)
#   OUTDIR      must not exist yet; it gets the app, the .dmg, Apple's
#               answers and logs (app-*.json, dmg-*.json) and release.log
# Needs, in the login keychain: the Developer ID Application identity of
# Parted Magic LLC, and the notarytool profile partedmagic-notary, saved by
#   xcrun notarytool store-credentials partedmagic-notary --team-id PMTLT7M8U6
set -euo pipefail
identity="Developer ID Application: Parted Magic LLC (PMTLT7M8U6)"
profile=partedmagic-notary
name="Parted Magic USB Installer"
here=$(cd "$(dirname "$0")" && pwd)
[ $# -eq 2 ] && [ -x "$1" ] || { echo "usage: $0 PMUSB_CORE OUTDIR" >&2; exit 2; }
core=$1; out=$2
[ ! -e "$out" ] || { echo "$out exists already; nothing done" >&2; exit 2; }
mkdir -p "$out"
out=$(cd "$out" && pwd)
exec > >(tee "$out/release.log") 2>&1
version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$here/Info.plist")
dmg="$out/parted-magic-usb-installer-$version.dmg"
echo "Parted Magic USB Installer $version for macOS, $(date)"

# Everything it needs, before anything is signed.
"$core" version | grep "^pmusb $version," ||
    { echo "the core does not report version $version"; exit 1; }
security find-identity -v -p codesigning | grep -F "\"$identity\"" ||
    { echo "no valid signing identity \"$identity\" in the keychain"; exit 1; }
xcrun notarytool history --keychain-profile "$profile" >/dev/null ||
    { echo "the notarytool profile $profile does not work (see above)"; exit 1; }

# notarize FILE TAG: sends FILE to Apple's notary service and waits for the
# answer; keeps it as TAG-result.json and Apple's log as TAG-log.json.
notarize() {
    local id status
    xcrun notarytool submit "$1" --keychain-profile "$profile" --wait \
        --output-format json > "$out/$2-result.json" || true
    cat "$out/$2-result.json"; echo
    id=$(plutil -extract id raw "$out/$2-result.json")
    status=$(plutil -extract status raw "$out/$2-result.json")
    xcrun notarytool log "$id" --keychain-profile "$profile" "$out/$2-log.json"
    [ "$status" = Accepted ] || { echo "notarization of $1: $status (Apple's log: $out/$2-log.json)"; exit 1; }
}

echo "== the app, signed"
sh "$here/build-mac.sh" "$core" "$out" "$identity"
app="$out/$name.app"
codesign --verify --strict --deep --verbose=2 "$app"
codesign -dvv "$app/Contents/MacOS/pmusb"
codesign -dvv "$app"

echo "== the app, notarized and stapled"
ditto -c -k --keepParent "$app" "$out/app.zip"
notarize "$out/app.zip" app
rm "$out/app.zip"
xcrun stapler staple "$app"
xcrun stapler validate "$app"

echo "== the .dmg, signed, notarized and stapled"
stage=$(mktemp -d)
ditto "$app" "$stage/$name.app"
ln -s /Applications "$stage/Applications"
hdiutil create -volname "$name" -srcfolder "$stage" -fs HFS+ -format UDZO "$dmg"
rm -rf "$stage"
codesign --force --timestamp --sign "$identity" "$dmg"
notarize "$dmg" dmg
xcrun stapler staple "$dmg"
xcrun stapler validate "$dmg"

echo "== what Gatekeeper says"
spctl -a -vv -t exec "$app"
spctl -a -vv -t open --context context:primary-signature "$dmg"
shasum -a 256 "$dmg"
echo "done: $dmg"
