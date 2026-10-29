#!/bin/sh
# Explicit local-only integration. Supply independently built official servers.
set -eu
: "${DISTRIBUTION_V3_BIN:?set to an official Distribution v3.0.0 executable}"
: "${DISTRIBUTION_V2_BIN:?set to an official Distribution v2.8.3 executable}"
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
cd "$root"
binary=${CLEANER_TEST_OUTPUT:-"$tmp/registry-cleaner"}
go build -buildvcs=false -o "$binary" ./cmd/registry-cleaner
printf 'Exact cleaner binary SHA-256: '
sha256sum "$binary"
export CLEANER_BIN="$binary"
echo 'Distribution v3 native tag-delete:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" go test -count=1 -v ./integration
echo 'Distribution v3 with controlled 405 fallback:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_FORCE_405=1 go test -count=1 -v ./integration
echo 'Distribution v3 with controlled fallback PUT failure:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_FORCE_405=1 INTEGRATION_FAIL_FALLBACK=1 go test -count=1 -v ./integration
echo 'Distribution v2 unsupported response fails closed:'
DISTRIBUTION_BIN="$DISTRIBUTION_V2_BIN" INTEGRATION_EXPECT_REJECT=1 go test -count=1 -v ./integration
echo 'Distribution v2 explicit compatibility fallback:'
DISTRIBUTION_BIN="$DISTRIBUTION_V2_BIN" INTEGRATION_V2_ENABLE=1 go test -count=1 -v ./integration
echo 'Native DELETE applied but response lost; no blind retry:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_DROP_DELETE=1 go test -count=1 -v ./integration
echo 'Concurrent state change causes stale preflight rejection:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_STALE=1 go test -count=1 -v ./integration
echo 'Paginated listing second page failure blocks plan and all writes:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_PAGE_FAIL=1 go test -count=1 -v ./integration
echo 'No-op 202 response rejected by postflight, no false success event:'
DISTRIBUTION_BIN="$DISTRIBUTION_V3_BIN" INTEGRATION_NOOP_202=1 go test -count=1 -v ./integration
