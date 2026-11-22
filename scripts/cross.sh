#!/bin/sh
# Compiles the library and its test binary for every target without cgo.
set -eu
GO=${GO:-go}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

targets="
linux/amd64 linux/arm64 linux/arm linux/386 linux/riscv64 linux/ppc64le linux/s390x linux/loong64
windows/amd64 windows/arm64
darwin/arm64
"
failed=0
for target in $targets; do
	os=${target%/*} arch=${target#*/}
	if CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$GO" build ./... &&
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$GO" vet . &&
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$GO" test -c -o "$tmp/gaal.test" .; then
		echo "ok   $target"
	else
		echo "FAIL $target"
		failed=1
	fi
done
exit $failed
