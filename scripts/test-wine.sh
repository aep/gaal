#!/bin/sh
# Runs the Windows test binary under Wine. Wine has only software H.264 Media
# Foundation transforms, so this checks far less than a real Windows machine.
set -eu
GO=${GO:-go}
SECONDS_LIMIT=${TEST_SECONDS:-600}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$GO" test -c -o "$tmp/gaal.exe" .
cd "$tmp"
timeout "$SECONDS_LIMIT" wine ./gaal.exe -test.count=1 -test.run="${TEST_RUN:-.}" ${TEST_ARGS:-}
