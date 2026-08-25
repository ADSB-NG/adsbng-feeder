#!/usr/bin/env bash
#
# ADSBNG contributor feeder installer.
#
#   sudo ./install.sh
#
# What this script does, and nothing else:
#
#   1. detects your CPU architecture and installs the matching feeder binary
#   2. creates an unprivileged `adsbng` system account for it to run as
#   3. writes /etc/adsbng-feeder/config.toml (owner-only) with your station
#      credentials, if it does not already exist
#   4. installs and starts a systemd service
#
# What it deliberately does NOT do:
#
#   * install readsb, dump1090, or any other third-party software
#   * install any ADSBNG decoder, analytics, or server component
#   * ask for, read, or copy any database credential
#   * contact anything other than the ADSBNG host it downloads the feeder from
#     (adsbng.app by default; override with ADSBNG_DOWNLOAD_BASE, or pass
#     --binary to install fully offline)
#
# Re-running it is safe and is how you upgrade: the binary and service file are
# replaced, and your existing config is left alone unless you pass
# --force-config.

set -euo pipefail

BIN_NAME="adsbng-feeder"
BIN_DEST="/usr/local/bin/${BIN_NAME}"
CONF_DIR="/etc/adsbng-feeder"
CONF_FILE="${CONF_DIR}/config.toml"
UNIT_NAME="adsbng-feeder.service"
UNIT_DEST="/etc/systemd/system/${UNIT_NAME}"
DOC_DIR="/usr/share/doc/adsbng-feeder"
SVC_USER="adsbng"
SVC_GROUP="adsbng"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# Where install.sh fetches the binary — and, when it is run on its own without
# the rest of the repository beside it, the systemd unit and reference docs — if
# they are not already next to this script. It defaults to ADSBNG's own
# distribution host, pinned to a specific release, so that
#
#     curl -fsSL https://adsbng.app/feeder/install.sh -o install.sh
#     sudo bash install.sh
#
# is a complete install with nothing else to fetch by hand. The binary is always
# checked against a published SHA256SUMS before it is installed. Override this to
# install from a mirror you control, or pass --binary PATH to install a locally
# built binary with no network access at all (see scripts/build.sh). Set it to
# the empty string to require a local binary and refuse to download.
DOWNLOAD_BASE="${ADSBNG_DOWNLOAD_BASE:-https://adsbng.app/feeder/v1.0.0}"

STATION_ID=""
TOKEN=""
TOKEN_FILE=""
GATEWAY=""
BEAST_SOURCE="127.0.0.1:30005"
CA_FILE=""
BINARY=""
FORCE_CONFIG=0
NO_START=0

# ---------------------------------------------------------------------------
# output helpers
# ---------------------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	C_BOLD=$'\033[1m'; C_RED=$'\033[31m'; C_YEL=$'\033[33m'
	C_GRN=$'\033[32m'; C_OFF=$'\033[0m'
else
	C_BOLD=""; C_RED=""; C_YEL=""; C_GRN=""; C_OFF=""
fi

info() { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$C_BOLD" "$C_OFF" "$*"; }
warn() { printf '%sWARNING:%s %s\n' "$C_YEL" "$C_OFF" "$*" >&2; }
ok()   { printf '%s  ok%s %s\n' "$C_GRN" "$C_OFF" "$*"; }
die()  { printf '%sERROR:%s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: sudo ./install.sh [options]

Station credentials (prompted for if omitted and no config exists yet):
  --station-id ID        local label for your station, e.g. LAGOS_01
  --token-file FILE      file containing your station token (preferred)
  --token TOKEN          your station token
                         NOTE: visible in `ps` and your shell history while the
                         installer runs. Prefer --token-file, or the
                         ADSBNG_STATION_TOKEN environment variable.
  --gateway HOST:PORT    ADSBNG ingest endpoint, e.g. ingest.adsbng.app:443

Local receiver:
  --beast-source ADDR    Beast source (default 127.0.0.1:30005)

TLS:
  --ca-file FILE         verify the gateway against this PEM bundle instead of
                         the system trust store (private-CA deployments only;
                         certificate verification stays enabled)

Installation source:
  --binary PATH          install this already-built binary, with no download
                         (default: use ./dist if present, otherwise download
                         from ADSBNG_DOWNLOAD_BASE and verify its checksum)

Other:
  --force-config         overwrite an existing config.toml
  --no-start             install but do not enable or start the service
  -h, --help             show this help

Environment:
  ADSBNG_STATION_TOKEN   station token, read if --token/--token-file are absent
  ADSBNG_DOWNLOAD_BASE   base URL to download the binary, systemd unit, and docs
                         from (default: https://adsbng.app/feeder/v1.0.0). Set it
                         to the empty string to require a local binary instead.
EOF
}

# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
		--station-id)    STATION_ID="${2:?--station-id needs a value}"; shift 2 ;;
		--token)         TOKEN="${2:?--token needs a value}"; shift 2 ;;
		--token-file)    TOKEN_FILE="${2:?--token-file needs a value}"; shift 2 ;;
		--gateway)       GATEWAY="${2:?--gateway needs a value}"; shift 2 ;;
		--beast-source)  BEAST_SOURCE="${2:?--beast-source needs a value}"; shift 2 ;;
		--ca-file)       CA_FILE="${2:?--ca-file needs a value}"; shift 2 ;;
		--binary)        BINARY="${2:?--binary needs a value}"; shift 2 ;;
		--force-config)  FORCE_CONFIG=1; shift ;;
		--no-start)      NO_START=1; shift ;;
		-h|--help)       usage; exit 0 ;;
		*)               usage >&2; die "unknown option: $1" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this with sudo: sudo ./install.sh"

case "$(uname -s)" in
	Linux) ;;
	*) die "the ADSBNG feeder is Linux-only (detected $(uname -s))" ;;
esac

# ---------------------------------------------------------------------------
# 1. architecture
# ---------------------------------------------------------------------------

step "Detecting architecture"
MACHINE="$(uname -m)"
case "$MACHINE" in
	x86_64|amd64)  ARCH="amd64" ;;
	aarch64|arm64) ARCH="arm64" ;;
	armv7l|armv7)  ARCH="armv7" ;;
	armv6l|armv6)  ARCH="armv6" ;;
	*) die "unsupported architecture: ${MACHINE}
The feeder ships for amd64, arm64, armv7 and armv6. If you need another,
build it yourself — see scripts/build.sh, it is one Go command per target." ;;
esac
ok "${MACHINE} -> linux/${ARCH}"

# A 64-bit Pi running a 32-bit userland reports armv7l, which is correct to use.
if [ "$ARCH" = "armv6" ]; then
	info "    (armv6: original Pi / Pi Zero. Slow but entirely adequate for a feeder.)"
fi

# ---------------------------------------------------------------------------
# 2. locate the binary
# ---------------------------------------------------------------------------

ASSET="${BIN_NAME}-linux-${ARCH}"
STAGED=""
CLEANUP_DIR=""
cleanup() {
	if [ -n "$CLEANUP_DIR" ]; then
		rm -rf -- "$CLEANUP_DIR"
	fi
}
trap cleanup EXIT

step "Locating the feeder binary"
if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || die "--binary ${BINARY}: no such file"
	STAGED="$BINARY"
	ok "using ${BINARY}"
elif [ -f "${SCRIPT_DIR}/dist/${ASSET}" ]; then
	STAGED="${SCRIPT_DIR}/dist/${ASSET}"
	ok "using ${STAGED}"
elif [ -n "$DOWNLOAD_BASE" ]; then
	command -v curl >/dev/null 2>&1 || die "curl is required to download the binary"
	CLEANUP_DIR="$(mktemp -d)"
	url="${DOWNLOAD_BASE%/}/${ASSET}"
	info "    downloading ${url}"
	curl -fsSL --proto '=https' --tlsv1.2 -o "${CLEANUP_DIR}/${ASSET}" "$url" \
		|| die "download failed: ${url}"
	# Verify a checksum if the release publishes one. Absent is not fatal, but
	# it is worth saying so out loud rather than pretending we verified.
	if curl -fsSL --proto '=https' --tlsv1.2 -o "${CLEANUP_DIR}/SHA256SUMS" \
			"${DOWNLOAD_BASE%/}/SHA256SUMS" 2>/dev/null; then
		if command -v sha256sum >/dev/null 2>&1; then
			# The separator is two spaces in text mode and " *" in binary mode,
			# so match on the filename anchored to end-of-line instead.
			if ! grep -E "[ *]${ASSET}\$" "${CLEANUP_DIR}/SHA256SUMS" > "${CLEANUP_DIR}/expected"; then
				die "SHA256SUMS at the download base does not list ${ASSET}"
			fi
			( cd "$CLEANUP_DIR" && sha256sum -c expected ) \
				|| die "checksum verification failed for ${ASSET} — do not install this binary"
			ok "checksum verified"
		else
			warn "sha256sum not available; checksum not verified"
		fi
	else
		warn "no SHA256SUMS published at the download base; checksum not verified"
	fi
	STAGED="${CLEANUP_DIR}/${ASSET}"
else
	die "no binary to install.

No binary was found beside this script and ADSBNG_DOWNLOAD_BASE is empty, so
there is nothing to download. Choose one of:

  * Let the installer download it from ADSBNG (the default — you have set the
    download base to an empty value, which is why you are seeing this):
        unset ADSBNG_DOWNLOAD_BASE
        sudo ./install.sh

  * Build from source (needs Go 1.24+, takes a few seconds):
        ./scripts/build.sh
        sudo ./install.sh

  * Install a binary you already have, with no network access:
        sudo ./install.sh --binary /path/to/${ASSET}"
fi

# Sanity-check before it becomes the installed binary: a truncated download or
# a wrong-architecture file should fail here, not as a restart loop later.
chmod 0755 "$STAGED"
if ! VERSION_OUT="$("$STAGED" --version 2>&1)"; then
	die "the binary does not run on this machine:
${VERSION_OUT}
This usually means the wrong architecture was installed (detected ${ARCH})."
fi
ok "${VERSION_OUT}"

# ---------------------------------------------------------------------------
# 3. service account
# ---------------------------------------------------------------------------

step "Ensuring the ${SVC_USER} system account exists"
if id -u "$SVC_USER" >/dev/null 2>&1; then
	ok "user ${SVC_USER} already exists"
else
	nologin="$(command -v nologin || echo /usr/sbin/nologin)"
	if command -v useradd >/dev/null 2>&1; then
		getent group "$SVC_GROUP" >/dev/null 2>&1 || groupadd --system "$SVC_GROUP"
		useradd --system --gid "$SVC_GROUP" --no-create-home \
			--home-dir /nonexistent --shell "$nologin" \
			--comment "ADSBNG feeder" "$SVC_USER"
	elif command -v adduser >/dev/null 2>&1; then
		adduser --system --group --no-create-home --shell "$nologin" "$SVC_USER"
	else
		die "neither useradd nor adduser is available; cannot create the ${SVC_USER} account"
	fi
	ok "created unprivileged user ${SVC_USER} (no login shell, no home directory)"
fi

# ---------------------------------------------------------------------------
# 4. install the binary
# ---------------------------------------------------------------------------

step "Installing ${BIN_DEST}"
# install(1) writes to a temporary name and renames, so an upgrade never leaves
# a half-written binary in place even if the disk fills.
install -o root -g root -m 0755 "$STAGED" "$BIN_DEST"
ok "$("$BIN_DEST" --version)"

# ---------------------------------------------------------------------------
# 5. configuration
# ---------------------------------------------------------------------------

step "Configuring ${CONF_FILE}"
install -d -o root -g "$SVC_GROUP" -m 0750 "$CONF_DIR"

if [ -f "$CONF_FILE" ] && [ "$FORCE_CONFIG" -eq 0 ]; then
	ok "existing config kept (pass --force-config to replace it)"
else
	# Token, in order of preference. Reading from a file or the environment keeps
	# it out of the process list and the shell history.
	if [ -n "$TOKEN_FILE" ]; then
		[ -f "$TOKEN_FILE" ] || die "--token-file ${TOKEN_FILE}: no such file"
		TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"
	elif [ -z "$TOKEN" ] && [ -n "${ADSBNG_STATION_TOKEN:-}" ]; then
		TOKEN="$ADSBNG_STATION_TOKEN"
	elif [ -n "$TOKEN" ]; then
		warn "a token passed as --token is visible in \`ps\` and your shell history."
		warn "Prefer --token-file, or export ADSBNG_STATION_TOKEN."
	fi

	# Prompt for whatever is still missing. ADSBNG issues all three together.
	if [ -z "$STATION_ID" ] || [ -z "$TOKEN" ] || [ -z "$GATEWAY" ]; then
		if [ ! -t 0 ]; then
			die "missing station credentials and no terminal to ask on.
Pass --station-id, --token-file and --gateway (see --help)."
		fi
		info ""
		info "Enter the three values from your ADSBNG provisioning message."
		info "They are the only credentials this software needs — ADSBNG will never"
		info "ask you for a database password, an API key, or a server login."
		info ""
		while [ -z "$STATION_ID" ]; do
			read -r -p "Receiver ID (e.g. LAGOS_01) : " STATION_ID
		done
		while [ -z "$TOKEN" ]; do
			read -r -p "Station token               : " TOKEN
		done
		while [ -z "$GATEWAY" ]; do
			read -r -p "Gateway [ingest.adsbng.app:443] : " GATEWAY
			GATEWAY="${GATEWAY:-ingest.adsbng.app:443}"
		done
		read -r -p "Beast source [${BEAST_SOURCE}] : " reply
		BEAST_SOURCE="${reply:-$BEAST_SOURCE}"
		info ""
	fi

	# A token containing a quote or a '#' would silently corrupt the file, and
	# the resulting failure ("unauthorized") gives no clue why.
	case "$TOKEN" in
		*'"'*|*"'"*|*'#'*|*' '*)
			die "that token contains a quote, '#', or a space, which is not a shape ADSBNG issues.
Check for a copy-paste error — a trailing character from your terminal is the usual cause." ;;
	esac

	# umask before creation: the file must never exist, even briefly, in a
	# world-readable state.
	old_umask="$(umask)"
	umask 077
	tmp_conf="${CONF_FILE}.new.$$"
	{
		printf '# /etc/adsbng-feeder/config.toml\n'
		printf '# Written by install.sh. Contains your station token — keep it at mode 0600.\n'
		printf '# Full reference: %s/config.toml.example\n\n' "$DOC_DIR"
		printf 'station_id   = "%s"\n' "$STATION_ID"
		printf 'token        = "%s"\n' "$TOKEN"
		printf 'gateway      = "%s"\n' "$GATEWAY"
		printf 'beast_source = "%s"\n' "$BEAST_SOURCE"
		if [ -n "$CA_FILE" ]; then
			printf 'ca_file      = "%s"\n' "$CA_FILE"
		fi
	} > "$tmp_conf"
	umask "$old_umask"

	chown "${SVC_USER}:${SVC_GROUP}" "$tmp_conf"
	chmod 0600 "$tmp_conf"
	mv -f "$tmp_conf" "$CONF_FILE"
	ok "wrote ${CONF_FILE} (mode 0600, owner ${SVC_USER})"
fi

# Enforce ownership and permissions on every run, including upgrades over an
# install where someone loosened them by hand.
chown "${SVC_USER}:${SVC_GROUP}" "$CONF_FILE"
chmod 0600 "$CONF_FILE"

step "Validating the configuration"
if ! CHECK_OUT="$(runuser -u "$SVC_USER" -- "$BIN_DEST" --check-config --config "$CONF_FILE" 2>&1)" &&
   ! CHECK_OUT="$(su -s /bin/sh -c "'$BIN_DEST' --check-config --config '$CONF_FILE'" "$SVC_USER" 2>&1)"; then
	info "$CHECK_OUT"
	die "the configuration is not usable. Fix ${CONF_FILE} and re-run:
    sudo ${BIN_DEST} --check-config --config ${CONF_FILE}"
fi
# --check-config prints the config with the token masked, never in full.
info "    ${CHECK_OUT}"
ok "configuration valid, and readable by the ${SVC_USER} user"

# ---------------------------------------------------------------------------
# 6. documentation
# ---------------------------------------------------------------------------

install -d -m 0755 "$DOC_DIR"
# Reference copies for /usr/share/doc. Present in a checkout or release archive;
# fetched best-effort from the download host when install.sh is run on its own.
# None is needed for the service to run, so a missing or un-fetchable file is
# skipped silently rather than failing the install.
for doc in config.toml.example CONTRIBUTOR_SETUP.md OPERATIONS.md SECURITY.md; do
	if [ -f "${SCRIPT_DIR}/${doc}" ]; then
		install -m 0644 "${SCRIPT_DIR}/${doc}" "$DOC_DIR/"
	elif [ -f "${SCRIPT_DIR}/docs/${doc}" ]; then
		install -m 0644 "${SCRIPT_DIR}/docs/${doc}" "$DOC_DIR/"
	elif [ -n "$DOWNLOAD_BASE" ] && command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 \
			-o "${DOC_DIR}/${doc}" "${DOWNLOAD_BASE%/}/${doc}" 2>/dev/null || true
	fi
done

# ---------------------------------------------------------------------------
# 7. systemd service
# ---------------------------------------------------------------------------

if ! command -v systemctl >/dev/null 2>&1; then
	warn "systemd was not found, so the service was not installed."
	warn "The binary is at ${BIN_DEST}; run it under whatever supervisor you use:"
	warn "    ${BIN_DEST} --config ${CONF_FILE}"
	exit 0
fi

step "Installing the systemd service"
UNIT_SRC="${SCRIPT_DIR}/systemd/${UNIT_NAME}"
if [ ! -f "$UNIT_SRC" ]; then
	# install.sh was run on its own — the curl'd one-line install — with no
	# repository beside it. Fetch the unit from the same host as the binary. It
	# rides the same verified-HTTPS connection this script arrived on; the binary
	# was additionally checked against SHA256SUMS above.
	[ -n "$DOWNLOAD_BASE" ] || die "no systemd unit beside this script and ADSBNG_DOWNLOAD_BASE is empty.
Run install.sh from a full checkout or release archive, or leave ADSBNG_DOWNLOAD_BASE at its default."
	command -v curl >/dev/null 2>&1 || die "curl is required to download the systemd unit"
	[ -n "$CLEANUP_DIR" ] || CLEANUP_DIR="$(mktemp -d)"
	UNIT_SRC="${CLEANUP_DIR}/${UNIT_NAME}"
	unit_url="${DOWNLOAD_BASE%/}/${UNIT_NAME}"
	info "    downloading ${unit_url}"
	curl -fsSL --proto '=https' --tlsv1.2 -o "$UNIT_SRC" "$unit_url" \
		|| die "download failed: ${unit_url}"
fi
install -o root -g root -m 0644 "$UNIT_SRC" "$UNIT_DEST"
systemctl daemon-reload
ok "installed ${UNIT_DEST}"

if [ "$NO_START" -eq 1 ]; then
	info ""
	info "Not starting the service (--no-start). When you are ready:"
	info "    sudo systemctl enable --now adsbng-feeder"
	exit 0
fi

# An advisory check on the local Beast source. The feeder retries indefinitely,
# so this is not fatal — but "nothing is listening on 30005" is by far the most
# common reason a new station shows no data, and it is better said now.
#
# Skipped rather than guessed at when the address is not a plain host:port, or
# when `timeout` is unavailable: a spurious warning about the one thing that
# usually IS wrong would teach contributors to ignore it.
beast_host="${BEAST_SOURCE%:*}"
beast_port="${BEAST_SOURCE##*:}"
if [ "$beast_host" != "$BEAST_SOURCE" ] &&
   [ -n "$beast_host" ] &&
   case "$beast_port" in ''|*[!0-9]*) false ;; *) true ;; esac &&
   command -v timeout >/dev/null 2>&1; then
	if ! timeout 3 bash -c ": < /dev/tcp/${beast_host}/${beast_port}" 2>/dev/null; then
		warn "nothing is listening on ${BEAST_SOURCE} right now."
		warn "The feeder will keep retrying, but you will not contribute any data until"
		warn "your receiver publishes Beast output there. With readsb, that means:"
		warn "    --net --net-beast-reduce-out-port 30005   (or --net-bo-port 30005)"
		warn "See ${DOC_DIR}/CONTRIBUTOR_SETUP.md."
	else
		ok "a Beast source is reachable at ${BEAST_SOURCE}"
	fi
fi

step "Enabling and starting adsbng-feeder"
systemctl enable "$UNIT_NAME" >/dev/null 2>&1 || true
systemctl restart "$UNIT_NAME"

# Give it long enough to connect, authenticate, and log the outcome.
sleep 3
if systemctl is-active --quiet "$UNIT_NAME"; then
	ok "adsbng-feeder is running"
else
	warn "the service is not active. Recent log output:"
	journalctl -u "$UNIT_NAME" -n 30 --no-pager || true
	die "start failed — see the log above, then: sudo systemctl restart adsbng-feeder"
fi

info ""
systemctl status "$UNIT_NAME" --no-pager --lines=12 || true

cat <<EOF

${C_BOLD}Installed.${C_OFF}

  Live log     : sudo journalctl -u adsbng-feeder -f
  Status       : systemctl status adsbng-feeder
  Restart      : sudo systemctl restart adsbng-feeder
  Config       : ${CONF_FILE}   (mode 0600, owner ${SVC_USER})
  Check config : sudo ${BIN_DEST} --check-config --config ${CONF_FILE}

Look for a line like:

  connected to ADSBNG as <YOUR_RECEIVER_ID>

That receiver ID comes from the gateway, not from this machine — it is derived
from your token, so it is the authoritative name your data is filed under.

Thank you for contributing coverage. Nothing on this machine has been given
access to ADSBNG systems, and nothing needs to be opened on your network:
the feeder makes one outbound TLS connection and sends ADS-B frames.
EOF
