#!/bin/bash
# Builds the signed release of Parted Magic USB Installer for Android on this
# host: the app bundle (.aab) for Google Play and the APK for GitHub
# Releases, both signed with the Parted Magic release key.
#
# It asks for the key's password itself. The password stays in this shell
# and the build it starts, and is never written anywhere.
#
# The build runs in a temporary copy, so this folder stays clean. The
# results go to release/ beside this script.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
key=/media/documents/Patrick/parted-magic-release.jks
out=$here/release
[ -f "$key" ] || { echo "missing: $key"; exit 2; }

read -r -s -p "Password for the Parted Magic release key: " pw < /dev/tty
echo
[ -n "$pw" ] || { echo "empty password, nothing built"; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
rsync -a --exclude '/pmusb' --exclude '/*.exe' --exclude '/signed/' --exclude '/signed-keep/' \
    --exclude 'android/app/build/' --exclude 'android/.gradle/' --exclude 'android/app/src/main/jniLibs/' \
    --exclude 'android/play-store/' --exclude 'android/release/' "$here/../" "$work/pmusb/"
cd "$work/pmusb/android"
PM_KEYSTORE=$key PM_KEYSTORE_PASSWORD=$pw \
    GO=/media/slackware15/usr/lib64/go1.27.2/go/bin/go GOTOOLCHAIN=local \
    JAVA_HOME=/opt/jdk-17.0.19+10 ANDROID_HOME=/opt/android-sdk \
    sh build-android.sh release
unset pw

version=$(sed -n 's/.*versionName = "\(.*\)".*/\1/p' app/build.gradle.kts)
mkdir -p "$out"
cp app/build/outputs/apk/release/app-release.apk "$out/parted-magic-usb-installer-$version.apk"
cp app/build/outputs/bundle/release/app-release.aab "$out/parted-magic-usb-installer-$version.aab"
cd "$out"
sha256sum "parted-magic-usb-installer-$version.apk" "parted-magic-usb-installer-$version.aab"
echo "done: the signed files are in $out"
