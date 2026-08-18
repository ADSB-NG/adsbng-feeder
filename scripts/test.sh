#!/usr/bin/env bash
#
# Run the feeder's full check suite: formatting, vet, and the Go tests including
# the security suite.
#
#   ./scripts/test.sh              # with -race where the toolchain allows it
#   NO_RACE=1 ./scripts/test.sh    # skip the race detector explicitly
#
# This is the script to run if you are a contributor auditing what you are about
# to install. It needs nothing but a Go toolchain — the feeder has no
# dependencies, so there is no module download step and nothing to trust beyond
# the standard library.
#
# Part of the suite checks properties of the repository rather than of a running
# feeder: that no ADSBNG credential or service name appears anywhere in it, that
# go.mod stays dependency-free, that no private ADSBNG source has been copied in,
# and that the token cannot reach a log line. Those tests are the machine-checked
# half of docs/SECURITY.md.

set -euo pipefail

cd -- "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail=0
note() { printf '\n==> %s\n' "$*"; }

note "gofmt"
unformatted="$(gofmt -l ./cmd ./internal)"
if [ -n "$unformatted" ]; then
	echo "these files are not gofmt-clean:"
	printf '  %s\n' $unformatted
	echo "fix with: gofmt -w ./cmd ./internal"
	fail=1
else
	echo "clean"
fi

note "go vet"
if go vet ./...; then
	echo "clean"
else
	fail=1
fi

# -race needs cgo, which needs a C compiler. Rather than failing on a machine
# that has not got one, say plainly that the run was weaker than a full one —
# a silently-skipped race detector is how a race reaches production. It is worth
# having here because the feeder runs four goroutines per session and a race
# between the Beast reader and the gateway writer would corrupt frames rather
# than crash.
RACE_FLAG="-race"
RACE_NOTE="with the race detector"
if [ -n "${NO_RACE:-}" ]; then
	RACE_FLAG=""
	RACE_NOTE="WITHOUT the race detector (NO_RACE was set)"
elif [ "$(go env CGO_ENABLED)" != "1" ] || ! command -v gcc >/dev/null 2>&1; then
	RACE_FLAG=""
	RACE_NOTE="WITHOUT the race detector (no cgo/gcc on this machine — run this on Linux before releasing)"
fi

note "go test ${RACE_NOTE}"
# shellcheck disable=SC2086
if go test ./... -count=1 $RACE_FLAG; then
	echo "tests passed"
else
	fail=1
fi

note "summary"
if [ "$fail" -ne 0 ]; then
	echo "FAILED"
	exit 1
fi
echo "all checks passed (${RACE_NOTE})"
echo
echo "Note: these tests exercise the feeder in-process against a stub gateway."
echo "The end-to-end path over real TLS to the real gateway lives in the private"
echo "adsbng-ingest repository, because the two modules must not import each other."
