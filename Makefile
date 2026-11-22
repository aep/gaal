# gaal builds without cgo except on Android and iOS, where Go itself requires it.
GO      ?= go
TIMEOUT ?= 15m
BENCH   ?= .
MAC_HOST ?=
ANDROID_SERIAL ?=
ANDROID_ARCH ?= arm64

export CGO_ENABLED ?= 0

.PHONY: all check fmt vet test conformance allocs bench cross \
	test-mac test-android test-wine test-all clean

all: check

## check: everything that runs on this machine alone
check: fmt vet test cross

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

## test: the whole suite, including conformance and allocation budgets
test:
	$(GO) test -count=1 -timeout $(TIMEOUT) ./...

## conformance: every encoder and decoder pair of every video codec
conformance:
	$(GO) test -count=1 -timeout $(TIMEOUT) -run 'TestRequiredBackends|TestConformance' -v .

## allocs: fails if any codec path allocates more than its budget
allocs:
	$(GO) test -count=1 -timeout $(TIMEOUT) -run 'Alloc|TestAudioRoundTrip' -v .

## bench: 1080p latency for every path this machine supports
bench:
	$(GO) test -run '^$$' -bench '$(BENCH)' -benchmem -benchtime 400x .

## cross: compile the library and its tests for every supported target
cross:
	GO="$(GO)" scripts/cross.sh

## test-mac: run on MAC_HOST over ssh, from a temp directory that is removed
test-mac:
	GO="$(GO)" MAC_HOST="$(MAC_HOST)" scripts/test-mac.sh

## test-android: run on the device selected by ANDROID_SERIAL
test-android:
	GO="$(GO)" ANDROID_SERIAL="$(ANDROID_SERIAL)" ANDROID_ARCH="$(ANDROID_ARCH)" scripts/test-android.sh

## test-wine: run the Windows build under Wine
test-wine:
	GO="$(GO)" scripts/test-wine.sh

## test-all: this machine plus every remote target that is configured
test-all: check test-wine
	@if [ -n "$(MAC_HOST)" ]; then $(MAKE) test-mac; fi
	@if [ -n "$(ANDROID_SERIAL)" ]; then $(MAKE) test-android; fi

clean:
	$(GO) clean -testcache
