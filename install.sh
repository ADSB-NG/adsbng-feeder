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
#   4. verifies your station token with the ADSBNG gateway, then installs and
#      starts a systemd service (skip the check with --skip-verify)
#
# It can also manage an existing install:
#
#   sudo ./install.sh --stop         stop the service (leaves it installed)
#   sudo ./install.sh --uninstall    remove the feeder from this machine
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
DOWNLOAD_BASE="${ADSBNG_DOWNLOAD_BASE:-https://adsbng.app/feeder/v1.1.0}"

STATION_ID=""
TOKEN=""
TOKEN_FILE=""
GATEWAY=""
BEAST_SOURCE="127.0.0.1:30005"
CA_FILE=""
BINARY=""
FORCE_CONFIG=0
NO_START=0
SKIP_VERIFY=0
DO_STOP=0
DO_UNINSTALL=0
KEEP_CONFIG=0
PURGE=0

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
Usage:
  sudo ./install.sh [options]          install or upgrade the feeder
  sudo ./install.sh --stop             stop the service (leave it installed)
  sudo ./install.sh --uninstall        remove the feeder from this machine

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
  --skip-verify          do not check the token with the gateway before starting
                         (the offline shape check still applies)
  -h, --help             show this help

Managing an existing install:
  --stop                 stop the running service (it stays enabled and installed)
  --uninstall            stop, disable, and remove the binary, service, docs, and
                         config from this machine. Does NOT revoke your token —
                         only ADSBNG can do that.
  --keep-config          with --uninstall, keep /etc/adsbng-feeder (your token)
  --purge                with --uninstall, also remove the adsbng system account

Environment:
  ADSBNG_STATION_TOKEN   station token, read if --token/--token-file are absent
  ADSBNG_DOWNLOAD_BASE   base URL to download the binary, systemd unit, and docs
                         from (default: https://adsbng.app/feeder/v1.1.0). Set it
                         to the empty string to require a local binary instead.
EOF
}

# ---------------------------------------------------------------------------
# lifecycle and verification helpers
# ---------------------------------------------------------------------------

# run_as_svc runs a command as the unprivileged service account and returns its
# exit status unchanged. The mechanism (runuser or su) is chosen once, up front:
# the older `runuser ... || su ...` idiom cannot tell "runuser is missing" apart
# from "the command ran and exited nonzero" — and the token check below depends
# on that real exit code.
SVC_RUNNER=""
run_as_svc() {
	if [ -z "$SVC_RUNNER" ]; then
		if command -v runuser >/dev/null 2>&1; then
			SVC_RUNNER="runuser"
		elif command -v su >/dev/null 2>&1; then
			SVC_RUNNER="su"
		else
			die "neither runuser nor su is available; cannot run as ${SVC_USER}"
		fi
	fi
	if [ "$SVC_RUNNER" = "runuser" ]; then
		runuser -u "$SVC_USER" -- "$@"
	else
		# su -c takes a single shell string; quote each argument so a path with a
		# space survives. printf %q is a bash builtin (this script runs under bash).
		local a q cmd=""
		for a in "$@"; do
			q="$(printf '%q' "$a")"
			cmd="${cmd:+$cmd }$q"
		done
		su -s /bin/sh -c "$cmd" "$SVC_USER"
	fi
}

# validate_token_shape rejects, offline and before anything is written or sent,
# a string that cannot be a token ADSBNG issued. It catches copy-paste damage —
# a truncated paste, a stray quote, an old-format string — at prompt time with
# no network. Necessary but not sufficient: a correctly shaped but non-existent
# token still passes here and is caught by the gateway check.
validate_token_shape() {
	case "$1" in
		*'"'*|*"'"*|*'#'*|*' '*)
			die "that token contains a quote, '#', or a space, which is not a shape ADSBNG issues.
Check for a copy-paste error — a trailing character from your terminal is the usual cause." ;;
	esac
	# A station token is "adsbng_" followed by 43 more URL-safe base64 characters.
	if ! printf '%s' "$1" | grep -Eq '^adsbng_[A-Za-z0-9_-]{43}$'; then
		die "that does not look like an ADSBNG station token.
It should start with \`adsbng_\` and then 43 more characters — the whole string
ADSBNG issued you, pasted with nothing trimmed and no surrounding quotes.
Check for a copy-paste error and try again."
	fi
}

# write_config writes config.toml atomically and privately: created under umask
# 077 so it is never briefly world-readable, owned by the service user, mode
# 0600, then renamed into place. Factored out so the verify step can rewrite it
# after a contributor re-enters a corrected token.
write_config() {
	local old_umask tmp_conf
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
}

# verify_station asks the gateway whether the configured token belongs to an
# active station, and refuses to enable a service that could only fail. This is
# the authoritative check "against existing active contributors": it runs the
# installed binary's --verify, which authenticates and disconnects without
# opening the local Beast source or streaming anything.
#
# --verify's exit codes are contractual: 0 active, 3 rejected (unknown or
# revoked), 4 the gateway was unreachable (token unproven, not known-bad). On a
# fresh interactive install a rejected token is almost always a paste error, so
# we reprompt (bounded) and rewrite the config in place.
verify_station() {
	local attempt=1 max=3 rc out reply
	while : ; do
		step "Verifying your token with the ADSBNG gateway"
		rc=0
		out="$(run_as_svc "$BIN_DEST" --verify --config "$CONF_FILE" 2>&1)" || rc=$?
		if [ "$rc" -eq 0 ]; then
			info "    ${out}"
			ok "the gateway confirmed this station is active"
			return 0
		fi

		if [ "$rc" -eq 3 ]; then
			warn "the gateway did not accept this token:"
			info "    ${out}"
			if [ ! -t 0 ] || [ "$attempt" -ge "$max" ]; then
				info ""
				info "The binary and configuration are in place, but the service has NOT"
				info "been enabled or started, because the token is not an active station."
				info "Check it against your ADSBNG provisioning message, then re-run either:"
				info "    sudo ${BIN_DEST} --verify --config ${CONF_FILE}   # re-test the token"
				info "    sudo ./install.sh --force-config                 # re-enter it"
				info "If it should be active, contact ADSBNG — it may not be provisioned yet."
				die "token not verified as an active station; not starting the service."
			fi
			info ""
			info "Re-enter your station token (attempt $((attempt + 1)) of ${max})."
			TOKEN=""
			while [ -z "$TOKEN" ]; do
				read -r -p "Station token : " TOKEN
			done
			validate_token_shape "$TOKEN"
			write_config
			attempt=$((attempt + 1))
			continue
		fi

		# rc 4 or anything else: the gateway could not be reached, so the token
		# was not proven bad. Usually a network, DNS, TLS, or firewall problem.
		warn "could not reach the ADSBNG gateway to verify the token:"
		info "    ${out}"
		if [ ! -t 0 ]; then
			die "gateway unreachable and no terminal to confirm on; not starting the service.
Re-run when connectivity is restored, or pass --skip-verify to install anyway."
		fi
		info ""
		info "The token was not confirmed. You can install anyway and the feeder will"
		info "keep retrying until the gateway is reachable, or stop here and re-run later."
		reply=""
		read -r -p "Install and start anyway without confirming the token? [y/N] : " reply
		case "$reply" in
			[yY]|[yY][eE][sS])
				warn "proceeding without gateway confirmation — the token is NOT verified."
				return 0 ;;
			*)
				die "stopped at your request; the service was not enabled or started.
Re-run install.sh when the gateway is reachable, or pass --skip-verify." ;;
		esac
	done
}

# do_stop stops the running service but leaves it installed and enabled, so a
# reboot or `systemctl start` brings it back. For permanent removal use
# --uninstall.
do_stop() {
	command -v systemctl >/dev/null 2>&1 || die "systemctl was not found; nothing to stop."
	if [ ! -f "$UNIT_DEST" ]; then
		info "adsbng-feeder does not appear to be installed (${UNIT_DEST} not found)."
		exit 0
	fi
	step "Stopping adsbng-feeder"
	systemctl stop "$UNIT_NAME" || true
	if systemctl is-active --quiet "$UNIT_NAME"; then
		die "the service is still active after stop — check: systemctl status adsbng-feeder"
	fi
	ok "adsbng-feeder is stopped (still installed; it will start again on boot)"
	info ""
	info "    sudo systemctl start adsbng-feeder     # start it again now"
	info "    sudo ./install.sh --uninstall          # remove it completely"
	exit 0
}

# do_uninstall removes the feeder from this machine: it stops and disables the
# service, then deletes the binary, unit, docs, and — unless --keep-config — the
# config directory that holds the token. The unprivileged adsbng account is left
# in place unless --purge is given.
#
# It does not and cannot revoke the token: that lives on ADSBNG's side. Deleting
# the local config stops THIS machine using it, but only ADSBNG can deactivate
# the token itself. The reminder is printed at the end.
do_uninstall() {
	step "Uninstalling adsbng-feeder"

	if command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now "$UNIT_NAME" >/dev/null 2>&1 || true
		if [ -f "$UNIT_DEST" ]; then
			rm -f "$UNIT_DEST"
			systemctl daemon-reload || true
			systemctl reset-failed "$UNIT_NAME" >/dev/null 2>&1 || true
			ok "removed the systemd service"
		fi
	else
		warn "systemctl was not found; skipping service removal."
	fi

	if [ -f "$BIN_DEST" ]; then
		rm -f "$BIN_DEST"
		ok "removed ${BIN_DEST}"
	fi
	if [ -d "$DOC_DIR" ]; then
		rm -rf "$DOC_DIR"
		ok "removed ${DOC_DIR}"
	fi

	if [ "$KEEP_CONFIG" -eq 1 ]; then
		info "kept ${CONF_DIR} (--keep-config); it still contains your station token."
	elif [ -d "$CONF_DIR" ]; then
		rm -rf "$CONF_DIR"
		ok "removed ${CONF_DIR} (including the stored token)"
	fi

	if [ "$PURGE" -eq 1 ]; then
		if id -u "$SVC_USER" >/dev/null 2>&1; then
			userdel "$SVC_USER" >/dev/null 2>&1 || true
			if getent group "$SVC_GROUP" >/dev/null 2>&1; then
				groupdel "$SVC_GROUP" >/dev/null 2>&1 || true
			fi
			ok "removed the ${SVC_USER} system account (--purge)"
		fi
	else
		info "left the ${SVC_USER} system account in place (pass --purge to remove it)."
	fi

	info ""
	info "${C_BOLD}Uninstalled.${C_OFF}"
	info ""
	warn "This did NOT revoke your station token. Uninstalling only removes the"
	warn "software from this machine; the token can only be deactivated by ADSBNG."
	warn "If you are decommissioning the station for good, ask ADSBNG to revoke it."
	exit 0
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
		--skip-verify)   SKIP_VERIFY=1; shift ;;
		--stop)          DO_STOP=1; shift ;;
		--uninstall)     DO_UNINSTALL=1; shift ;;
		--keep-config)   KEEP_CONFIG=1; shift ;;
		--purge)         PURGE=1; shift ;;
		-h|--help)       usage; exit 0 ;;
		*)               usage >&2; die "unknown option: $1" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this with sudo: sudo ./install.sh"

# Management actions run here: after the root check (they touch systemd and
# /usr/local/bin, so they need root) but before any architecture detection,
# download, or config work, because they act on what is already installed.
# Each of these exits when it is done.
if [ "$DO_UNINSTALL" -eq 1 ]; then do_uninstall; fi
if [ "$DO_STOP" -eq 1 ]; then do_stop; fi

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

	# Reject a token that cannot be one ADSBNG issued (Layer 1, offline) before
	# it is written anywhere. A well-formed but non-existent token still passes
	# this; the gateway check below is what confirms it is an active station.
	validate_token_shape "$TOKEN"

	write_config
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
# 5b. verify the token with the gateway
# ---------------------------------------------------------------------------
#
# The step above only proved the config parses. This one proves the token
# actually belongs to an active station, by asking the gateway — the same
# authenticated TLS handshake the feeder makes when it runs, except it
# disconnects instead of streaming. A fresh install will not enable or start
# the service for a token the gateway rejects. --skip-verify overrides this.

if [ "$SKIP_VERIFY" -eq 1 ]; then
	warn "skipping the gateway token check (--skip-verify)."
	warn "The token has NOT been confirmed as an active station. If it is wrong the"
	warn "service will start but the gateway will refuse it; confirm later with:"
	warn "    sudo ${BIN_DEST} --verify --config ${CONF_FILE}"
else
	verify_station
fi

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
  Verify token : sudo ${BIN_DEST} --verify --config ${CONF_FILE}
  Stop         : sudo systemctl stop adsbng-feeder
  Uninstall    : sudo ./install.sh --uninstall

Look for a line like:

  connected to ADSBNG as <YOUR_RECEIVER_ID>

That receiver ID comes from the gateway, not from this machine — it is derived
from your token, so it is the authoritative name your data is filed under.

Thank you for contributing coverage. Nothing on this machine has been given
access to ADSBNG systems, and nothing needs to be opened on your network:
the feeder makes one outbound TLS connection and sends ADS-B frames.
EOF
