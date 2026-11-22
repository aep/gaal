#!/bin/sh
# Cross-compiles the tests and runs them on a Mac over ssh. The binary is
# streamed into a temporary directory that is always removed, so nothing needs
# to be installed on the Mac and nothing is left behind.
set -eu
GO=${GO:-go}
: "${MAC_HOST:?set MAC_HOST to an ssh host}"
ARCH=${MAC_ARCH:-arm64}
SECONDS_LIMIT=${TEST_SECONDS:-600}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

CGO_ENABLED=0 GOOS=darwin GOARCH=$ARCH "$GO" test -c -o "$tmp/gaal.test" .
ssh -o BatchMode=yes "$MAC_HOST" "d=\$(mktemp -d /tmp/gaal.XXXXXX); trap 'rm -rf \"\$d\"' EXIT; \
	cat > \"\$d/t\"; chmod +x \"\$d/t\"; cd \"\$d\"; \
	GAAL_REQUIRE='${GAAL_REQUIRE:-}' perl -e 'alarm shift; exec @ARGV' $SECONDS_LIMIT ./t -test.count=1 -test.run='${TEST_RUN:-.}' ${TEST_ARGS:-}" \
	<"$tmp/gaal.test"
