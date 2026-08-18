#!/usr/bin/env bash
#
# Cross-compile the ADSBNG feeder for every architecture a contributor might
# plausibly run, into ./dist.
#
#   ./scripts/build.sh            # all targets, version from git or 0.0.0-dev
#   ./scripts/build.sh 1.2.0      # all targets, explicit version
#   TARGETS=linux/arm64 ./scripts/build.sh
#
# Produces:
#   dist/adsbng-feeder-linux-amd64
#   dist/adsbng-feeder-linux-arm64
#   dist/adsbng-feeder-linux-armv7
#   dist/adsbng-feeder-linux-armv6
#   dist/SHA256SUMS
#
# install.sh looks in dist/ for the binary matching the local architecture, so
# `./scripts/build.sh && sudo ./install.sh` is a complete from-source install.
#
# Everything here is stdlib-only Go with CGO disabled, so a single machine can
# build every target with no toolchain beyond Go itself. That is the practical
# reason the feeder has no dependencies: cross-compiling for an armv6 Pi stops
# being a project the moment cgo is involved.

set -euo pipefail

cd -- "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

BIN_NAME="adsbng-feeder"
OUT_DIR="dist"

# Version precedence: argument, then git describe, then a marker that makes it
# obvious the binary came from an untagged tree.
VERSION="${1:-}"
if [ -z "$VERSION" ]; then
	if git rev-parse --git-dir >/dev/null 2>&1; then
		VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")"
	else
		VERSION="0.0.0-dev"
	fi
fi

# GOARM only matters for GOARCH=arm; the suffix is what install.sh looks for.
# armv6 covers the original Pi and Pi Zero/Zero W, which are still in use as
# feeders and would fault on an armv7 binary.
DEFAULT_TARGETS="linux/amd64 linux/arm64 linux/arm:7 linux/arm:6"
TARGETS="${TARGETS:-$DEFAULT_TARGETS}"

command -v go >/dev/null 2>&1 || {
	echo "go is not installed. The feeder needs Go 1.24 or newer." >&2
	exit 1
}

echo "building ${BIN_NAME} ${VERSION}"
echo "go: $(go version)"
echo

rm -rf -- "$OUT_DIR"
mkdir -p -- "$OUT_DIR"

for target in $TARGETS; do
	goos="${target%%/*}"
	rest="${target#*/}"
	goarch="${rest%%:*}"
	goarm=""
	suffix="$goarch"

	if [ "$rest" != "$goarch" ]; then
		goarm="${rest#*:}"
		suffix="armv${goarm}"
	fi

	out="${OUT_DIR}/${BIN_NAME}-${goos}-${suffix}"

	# -trimpath strips local filesystem paths from the binary. Contributors can
	# and should inspect what we ship them; there is no reason for it to disclose
	# the layout of a build machine.
	#
	# -s -w drop the symbol table and DWARF data, which roughly halves the
	# binary. Debugging happens on our own builds, not on a Pi in someone's loft.
	#
	# The environment is exported inside a subshell rather than written as a
	# `VAR=x go build` prefix: bash recognises assignment prefixes before
	# expansion, so a conditional `${goarm:+GOARM=$goarm}` is parsed as a command
	# name and fails with "GOARM=7: command not found".
	(
		export CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch"
		if [ -n "$goarm" ]; then
			export GOARM="$goarm"
		fi
		go build \
			-trimpath \
			-ldflags "-s -w -X main.version=${VERSION}" \
			-o "$out" \
			./cmd/adsbng-feeder
	)

	size="$(du -h "$out" | cut -f1)"
	printf '  %-40s %s\n' "$out" "$size"
done

echo
( cd "$OUT_DIR" && sha256sum "${BIN_NAME}"-* > SHA256SUMS )
cat "${OUT_DIR}/SHA256SUMS"

cat <<EOF

Built into ${OUT_DIR}/.

Install on this machine:
    sudo ./install.sh

Install on another machine (copy the repo, or just the binary + install.sh +
systemd/ + config.toml.example):
    scp -r . pi@station:adsbng-feeder/
    ssh pi@station 'cd adsbng-feeder && sudo ./install.sh'

Publish SHA256SUMS alongside the binaries. install.sh verifies it when
downloading, and a contributor can check a binary by hand:
    sha256sum -c SHA256SUMS --ignore-missing
EOF
