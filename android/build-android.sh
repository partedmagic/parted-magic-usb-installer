#!/bin/sh
# Builds the Android app: the pmusb core for android/arm64 (shipped as
# lib/arm64-v8a/libpmusb.so), then the app with Gradle.
#   build-android.sh            debug APK, signed with Gradle's debug key
#   build-android.sh release    release APK and Play bundle; signed with the
#                               Parted Magic key when PM_KEYSTORE and
#                               PM_KEYSTORE_PASSWORD are set
# Needs Go (GO=), JDK 17 (JAVA_HOME=) and the Android SDK (ANDROID_HOME=).
set -e
here=$(cd "$(dirname "$0")" && pwd)
GO=${GO:-go}
: "${JAVA_HOME:?set JAVA_HOME to a JDK 17}"
: "${ANDROID_HOME:?set ANDROID_HOME to the Android SDK}"
export JAVA_HOME ANDROID_HOME
out=$(mktemp -d)
( cd "$here/.." && GOOS=android GOARCH=arm64 CGO_ENABLED=0 GOFLAGS=-mod=vendor \
    "$GO" build -trimpath -ldflags='-s -w' -o "$out/libpmusb.so" . )
mkdir -p "$here/app/src/main/jniLibs/arm64-v8a"
cp "$out/libpmusb.so" "$here/app/src/main/jniLibs/arm64-v8a/libpmusb.so"
rm -rf "$out"
cd "$here"
if [ "$1" = release ]; then
    ./gradlew --no-daemon assembleRelease bundleRelease
    ls -l app/build/outputs/apk/release/ app/build/outputs/bundle/release/
else
    ./gradlew --no-daemon assembleDebug
    ls -l app/build/outputs/apk/debug/
fi
sha256sum app/src/main/jniLibs/arm64-v8a/libpmusb.so
