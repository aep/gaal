#!/bin/sh
# Builds the tests with the NDK and runs them on an Android device or emulator.
# Android needs cgo for Go's runtime, but gaal itself contains no C.
set -eu
GO=${GO:-go}
ARCH=${ANDROID_ARCH:-arm64}
API=${ANDROID_API:-29}
SECONDS_LIMIT=${TEST_SECONDS:-600}

ndk=${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-}}
if [ -z "$ndk" ]; then
	: "${ANDROID_HOME:?set ANDROID_NDK_HOME or ANDROID_HOME}"
	ndk=$(ls -d "$ANDROID_HOME"/ndk/* | sort -V | tail -1)
fi
case $ARCH in
arm64) triple=aarch64-linux-android ;;
amd64) triple=x86_64-linux-android ;;
*) echo "unsupported ANDROID_ARCH $ARCH" >&2; exit 2 ;;
esac
host=$(ls "$ndk/toolchains/llvm/prebuilt" | head -1)
cc=$ndk/toolchains/llvm/prebuilt/$host/bin/$triple$API-clang

tmp=$(mktemp -d)
remote=/data/local/tmp/gaal.test.$$
trap 'rm -rf "$tmp"; adb shell rm -f "$remote" >/dev/null 2>&1 || true' EXIT

CGO_ENABLED=1 GOOS=android GOARCH=$ARCH CC=$cc "$GO" test -c -o "$tmp/gaal.test" .
adb push "$tmp/gaal.test" "$remote" >/dev/null
# adb shell does not reliably return the exit status on old devices.
out=$tmp/out.txt
adb shell "chmod 755 $remote; cd /data/local/tmp; GAAL_REQUIRE='${GAAL_REQUIRE:-}' timeout $SECONDS_LIMIT $remote -test.count=1 -test.run='${TEST_RUN:-.}' ${TEST_ARGS:-}; echo gaal-exit=\$?" | tee "$out"
grep -q 'gaal-exit=0' "$out"
