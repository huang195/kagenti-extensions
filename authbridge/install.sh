#!/bin/sh
# install.sh — one-line installer for Cortex on a local machine.
#
#   curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh | sh
#
# Detects your OS/arch, downloads the prebuilt `abctl` and `authbridge-proxy`
# binaries for the newest release, verifies their SHA-256 checksums, installs
# them to ~/.local/bin, and starts Cortex in the background — then prints the
# commands to watch traffic and point an agent at it, plus how to stop it.
# macOS + Linux, amd64 + arm64. No cluster, Keycloak, or SPIRE needed.
#
# By default it installs, starts Cortex with its built-in config in ~/.cortex,
# fills in tool-prune's remove list from your own transcripts, and prints the
# command to send an agent through it.
#
# Options (pass through the pipe with `sh -s --`, e.g.
#   curl -fsSL ...install.sh | sh -s -- --install-only):
#
#   --install-only   install the binaries and stop
#   --no-prune       set up and start, but leave tool-prune's list empty
#
# There is deliberately only one config. It carries the parsers AND tool-prune,
# and the proxy preserves edits to it, so a second "cost-optimised" config had
# nothing to do that filling in one list did not already do — while costing a
# second CA, a second set of paths, and a second page of instructions that read
# identically to the first.
#
# No compatibility aliases here: this script accepted no flags at all until now,
# so there is no earlier spelling for anyone to still be using. (The proxy's
# --demo -> --local alias is different: that flag really did ship.)
#
# Flags rather than env vars: written `VAR=1 curl ... | sh` the variable reaches
# curl, not sh, so the script runs without it. `sh -s -- --flag` has no such
# failure mode. The env vars below still work.
#
# Environment:
#   AUTHBRIDGE_VERSION=vX.Y.Z   install a specific release tag (default: newest)
#   AUTHBRIDGE_INSTALL_ONLY=1   same as --install-only
#   AUTHBRIDGE_SKIP_DOWNLOAD=1  use the already-installed binaries in ~/.local/bin
#                               instead of downloading (re-run setup offline)
set -eu

REPO="rossoctl/cortex"
BIN_DIR="${HOME}/.local/bin"
# Every file Cortex writes for this user lives here: config, CA, keys, logs,
# pidfiles. One directory to inspect, back up, or delete.
CORTEX_DIR="${HOME}/.cortex"

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- mode selection ---
MODE=local
PRUNE=1
for arg in "$@"; do
	case "$arg" in
		--install-only) MODE=install-only ;;
		--no-prune) PRUNE="" ;;
		# --local is the default; accepted so writing it out explicitly works, and
		# so it mirrors the proxy flag of the same name.
		--local) MODE=local ;;
		-h | --help)
			sed -n '2,30p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'
			exit 0
			;;
		*) die "unknown option: $arg (try --install-only, --no-prune, or no argument)" ;;
	esac
done
# Env form kept working; the flag wins if both are given.
if [ "${AUTHBRIDGE_INSTALL_ONLY:-}" = "1" ] && [ "$MODE" = "local" ]; then
	MODE=install-only
fi

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar  >/dev/null 2>&1 || die "tar is required"

# Verify the checklist file passed as $1 (run from the directory holding the
# files). shasum is preferred: it's always present on macOS and its -c reads the
# GNU-style checksums.txt reliably, whereas some non-GNU sha256sum builds reject
# -c. Linux without shasum falls back to sha256sum (GNU coreutils).
sha_check() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c "$1"
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c "$1"
	else
		die "need shasum or sha256sum to verify downloads"
	fi
}

# Demo listener ports — loopback, and deliberately uncommon to avoid colliding
# with common dev tools. Keep in sync with the built-in config in
# authbridge/cmd/authbridge-proxy/local.go.
DEMO_FORWARD_PORT=47600
DEMO_SESSION_PORT=47601
DEMO_STATS_PORT=47602

# port_in_use exits 0 if something is already listening on the given loopback
# port. Best-effort: uses lsof, then nc; if neither exists, it assumes free.
port_in_use() {
	if command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP@127.0.0.1:"$1" -sTCP:LISTEN >/dev/null 2>&1
	elif command -v nc >/dev/null 2>&1; then
		nc -z 127.0.0.1 "$1" >/dev/null 2>&1
	else
		return 1
	fi
}

# --- detect platform ---
os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS: $os (the installer supports macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

# stop_previous_cortex stops a Cortex a previous run of this script started, if
# one is still holding the ports the next one needs.
#
# Re-running the installer while Cortex is already up is ordinary — following the
# README and then the token-cost guide does exactly that. Both use the same
# loopback ports, so without this the second command dies on a bind conflict.
#
# Deliberately narrow. It only kills a pid from OUR pidfile whose process name is
# still authbridge-proxy — a pidfile can outlive its process and the number can
# be recycled onto something unrelated. Anything else holding the port is left
# alone and reported by the preflight below.
stop_previous_cortex() {
	pidfile="${CORTEX_DIR}/proxy.pid"
	[ -f "$pidfile" ] || return 0
	pid=$(cat "$pidfile" 2>/dev/null) || return 0
	case "$pid" in
		'' | *[!0-9]*) return 0 ;;
	esac
	if ! kill -0 "$pid" 2>/dev/null; then
		rm -f "$pidfile"
		return 0
	fi
	name=$(ps -p "$pid" -o comm= 2>/dev/null || true)
	case "$name" in
		*authbridge-proxy*) ;;
		*) return 0 ;; # pid recycled onto something else — never touch it
	esac
	info "Stopping the Cortex started earlier (pid ${pid}); the new one replaces it."
	kill "$pid" 2>/dev/null || true
	i=0
	while [ "$i" -lt 25 ] && kill -0 "$pid" 2>/dev/null; do
		sleep 0.2
		i=$((i + 1))
	done
	rm -f "$pidfile"
}

# --- preflight: fail early (before downloading) if a listener port is taken ---
if [ "$MODE" = "local" ]; then
	# Clear our own previous instance first, so switching between the two setups
	# is one command rather than a bind error and a manual kill.
	for p in "$DEMO_FORWARD_PORT" "$DEMO_SESSION_PORT" "$DEMO_STATS_PORT"; do
		if port_in_use "$p"; then
			stop_previous_cortex
			break
		fi
	done
	for p in "$DEMO_FORWARD_PORT" "$DEMO_SESSION_PORT" "$DEMO_STATS_PORT"; do
		if port_in_use "$p"; then
			die "port ${p} is already in use. Is Cortex already running (see ${CORTEX_DIR}/proxy.pid)? Otherwise free the port, or change the ports in the config, then re-run."
		fi
	done
fi

# --- skip the download entirely when asked (offline re-run) ---
if [ "${AUTHBRIDGE_SKIP_DOWNLOAD:-}" = "1" ]; then
	for b in abctl authbridge-proxy; do
		[ -x "${BIN_DIR}/${b}" ] || die "AUTHBRIDGE_SKIP_DOWNLOAD=1 but ${BIN_DIR}/${b} is missing"
	done
	version="(already installed)"
	info "Using the binaries already in ${BIN_DIR}"
else

# --- resolve the release tag ---
# `releases/latest` excludes prereleases, and the project ships prereleases, so
# list releases (newest first) and take the first tag_name instead.
version="${AUTHBRIDGE_VERSION:-}"
if [ -z "$version" ]; then
	info "Resolving newest release..."
	version=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=1" \
		| grep -m1 '"tag_name"' | sed -e 's/.*"tag_name": *"//' -e 's/".*//')
	[ -n "$version" ] || die "could not resolve the newest release (set AUTHBRIDGE_VERSION=vX.Y.Z)"
fi
info "Release: $version"

# --- download + verify ---
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

base="https://github.com/${REPO}/releases/download/${version}"
abctl_tgz="abctl_${version}_${os}_${arch}.tar.gz"
proxy_tgz="authbridge-proxy_${version}_${os}_${arch}.tar.gz"

info "Downloading binaries for ${os}/${arch}..."
curl -fsSL "${base}/${abctl_tgz}" -o "${tmp}/${abctl_tgz}" || die "download failed: ${abctl_tgz}"
curl -fsSL "${base}/${proxy_tgz}" -o "${tmp}/${proxy_tgz}" || die "download failed: ${proxy_tgz}"
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" || die "download failed: checksums.txt"

info "Verifying checksums..."
# Match exactly the two archives we downloaded (anchored to the end of the line),
# not every entry for this platform — so an unrelated future artifact in
# checksums.txt can't make verification fail on a file we never fetched.
grep -E "(${abctl_tgz}|${proxy_tgz})\$" "${tmp}/checksums.txt" > "${tmp}/checksums.filtered" \
	|| die "no checksum entries for ${abctl_tgz} / ${proxy_tgz} in checksums.txt"
( cd "$tmp" && sha_check checksums.filtered ) || die "checksum verification failed"

# --- extract + install ---
info "Installing to ${BIN_DIR}..."
mkdir -p "$BIN_DIR"
tar -xzf "${tmp}/${abctl_tgz}" -C "$tmp"
tar -xzf "${tmp}/${proxy_tgz}" -C "$tmp"
for b in abctl authbridge-proxy; do
	[ -f "${tmp}/${b}" ] || die "archive did not contain expected binary: ${b}"
	chmod +x "${tmp}/${b}"
	mv -f "${tmp}/${b}" "${BIN_DIR}/${b}"
done

# macOS: clear the quarantine flag so Gatekeeper doesn't block the unsigned binaries.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
	xattr -dr com.apple.quarantine "${BIN_DIR}/abctl" "${BIN_DIR}/authbridge-proxy" 2>/dev/null || true
fi

rm -rf "$tmp"
trap - EXIT
fi # end of download block

# --- report ---
proxy="${BIN_DIR}/authbridge-proxy"
ca_dir="${CORTEX_DIR}/ca" # matches defaultCortexDir()+caDirName in local.go
case ":${PATH}:" in
	*":${BIN_DIR}:"*) abctl_cmd="abctl" proxy_cmd="authbridge-proxy" ;;
	*) abctl_cmd="${BIN_DIR}/abctl" proxy_cmd="$proxy" ;;
esac

info ""
info "Installed abctl and authbridge-proxy (${version}) to ${BIN_DIR}"
case ":${PATH}:" in
	*":${BIN_DIR}:"*) ;;
	*)
		warn "${BIN_DIR} is not on your PATH."
		warn "Add it for future sessions:  export PATH=\"${BIN_DIR}:\$PATH\""
		;;
esac

if [ "$MODE" = "install-only" ]; then
	info ""
	info "Install-only mode. Start it with:  ${proxy_cmd} --local"
	exit 0
fi

# --- start in the background, then wait until it's actually listening ---
info ""
info "Starting Cortex in the background..."
# 0700 on the Cortex directory: a CA private key is written beneath it.
mkdir -p "$CORTEX_DIR" && chmod 700 "$CORTEX_DIR"
mkdir -p "$ca_dir"
log="${CORTEX_DIR}/proxy.log"
pidfile="${CORTEX_DIR}/proxy.pid"
nohup "$proxy" --local </dev/null >"$log" 2>&1 &
proxy_pid=$!
echo "$proxy_pid" >"$pidfile"

# Confirm readiness from real signals, not the "listening" log line — that line is
# emitted just *before* the socket is bound, so a bind failure could look ready.
# A bind failure exits within ms (the proxy Fatalf's), so watch for early exit;
# and probe the forward port for a true post-bind signal.
ready=0
i=0
while [ "$i" -lt 50 ]; do
	if ! kill -0 "$proxy_pid" 2>/dev/null; then
		warn "Cortex exited during startup — last log lines:"
		tail -n 15 "$log" >&2 || true
		die "Cortex failed to start (full log: ${log})"
	fi
	if port_in_use "$DEMO_FORWARD_PORT"; then
		ready=1
		break
	fi
	sleep 0.2
	i=$((i + 1))
done

info ""
if [ "$ready" -eq 1 ]; then
	info "Cortex is running (pid ${proxy_pid}).   Logs: ${log}"
else
	# It didn't exit during the startup window (a bind failure would have killed
	# it), but no probe tool confirmed the port — most likely up. Say so honestly.
	info "Cortex started (pid ${proxy_pid}); couldn't confirm it's listening (install lsof or nc to verify). Logs: ${log}"
fi
info ""

# tool-prune ships inert: the remove list is empty, so it does nothing until a
# name is added. Fill it now — that is the whole point of installing this — and
# say so plainly, because it is derived from the user's transcripts rather than
# chosen by them. The config is hot-reloaded, so this needs no restart.
#
# `abctl tools scan` refuses to write anything when it saw no tool calls to reason
# from, which is what makes doing this unattended safe: a brand-new install with
# no history gets an empty list and a message, not a guess.
local_cfg="${CORTEX_DIR}/config.yaml"
if [ -n "${PRUNE:-}" ] && [ -f "${local_cfg}" ]; then
	info "Choosing unused tools to prune from your own ~/.claude/projects transcripts..."
	if "${BIN_DIR}/abctl" tools scan --write "${local_cfg}"; then
		info ""
		info "To keep a tool, delete its name from the remove: list in ${local_cfg}."
	else
		info ""
		info "  Nothing pruned yet. Once you have used Claude Code for a while:"
		info "    ${abctl_cmd} tools scan --write ${local_cfg}"
	fi
	info ""
elif [ -f "${local_cfg}" ]; then
	info "  Fill the prune list when you are ready (hot-reloaded, no restart):"
	info "    ${abctl_cmd} tools scan --write ${local_cfg}"
	info ""
fi
info "  Watch traffic:   ${abctl_cmd} --endpoint http://localhost:${DEMO_SESSION_PORT}"
info "  Send traffic through it (e.g. Claude Code):"
info "    HTTPS_PROXY=http://localhost:${DEMO_FORWARD_PORT} \\"
info "      NODE_EXTRA_CA_CERTS=${ca_dir}/ca.crt \\"
info "      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 claude"
info ""
info "  Stop it:         kill ${proxy_pid}   (or: kill \$(cat ${pidfile}))"
info ""
