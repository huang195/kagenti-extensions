#!/bin/sh
# install-demo.sh — one-line installer + launcher for the Cortex local demo.
#
#   curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install-demo.sh | sh
#
# Detects your OS/arch, downloads the prebuilt `abctl` and `authbridge-proxy`
# binaries for the newest release, verifies their SHA-256 checksums, installs
# them to ~/.local/bin, and starts the demo in the background — then prints the
# commands to watch traffic and point an agent at it, plus how to stop it.
# macOS + Linux, amd64 + arm64. No cluster, Keycloak, or SPIRE needed.
#
# Modes (pass through the pipe with `sh -s --`, e.g.
#   curl -fsSL ...install-demo.sh | sh -s -- --claude-code):
#
#   (default)        install, then start the throwaway demo in ./cortex-ca
#   --claude-code    install, then set up a PERSISTENT config in ~/.cortex for
#                    cutting Claude Code token cost: writes the config, fills the
#                    tool-prune remove: list from your own transcripts, starts the
#                    proxy, and prints the exact command to run Claude Code
#                    through it. Safe to re-run; never overwrites an existing
#                    config.
#   --install-only   install the binaries and stop
#
# Flags exist because the env-var form has a trap: written
# `VAR=1 curl ... | sh` the variable reaches curl, not sh, so the script runs
# without it. `sh -s -- --flag` has no such failure mode. The env vars below
# still work for backward compatibility.
#
# Environment:
#   AUTHBRIDGE_VERSION=vX.Y.Z   install a specific release tag (default: newest)
#   AUTHBRIDGE_INSTALL_ONLY=1   same as --install-only
#   AUTHBRIDGE_SKIP_DOWNLOAD=1  use the already-installed binaries in ~/.local/bin
#                               instead of downloading (re-run setup offline)
set -eu

REPO="rossoctl/cortex"
BIN_DIR="${HOME}/.local/bin"

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- mode selection ---
MODE=demo
for arg in "$@"; do
	case "$arg" in
		--claude-code) MODE=claude-code ;;
		--install-only) MODE=install-only ;;
		--demo) MODE=demo ;;
		-h | --help)
			sed -n '2,30p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'
			exit 0
			;;
		*) die "unknown option: $arg (try --claude-code, --install-only, or no argument)" ;;
	esac
done
# Env form kept working; the flag wins if both are given.
if [ "${AUTHBRIDGE_INSTALL_ONLY:-}" = "1" ] && [ "$MODE" = "demo" ]; then
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
# with common dev tools. Keep in sync with the demo config in
# authbridge/cmd/authbridge-proxy/demo.go.
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
	*) die "unsupported OS: $os (the demo installer supports macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

# --- preflight: fail early (before downloading) if a demo port is taken ---
if [ "$MODE" = "demo" ] || [ "$MODE" = "claude-code" ]; then
	for p in "$DEMO_FORWARD_PORT" "$DEMO_SESSION_PORT" "$DEMO_STATS_PORT"; do
		if port_in_use "$p"; then
			die "port ${p} is already in use. Is the demo already running (see ./cortex-ca/demo.pid)? Otherwise free the port, or change the ports in ./cortex-ca/demo.yaml, then re-run."
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
ca_dir="$(pwd)/cortex-ca" # matches demoCADirDefault in demo.go
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
	info "Install-only mode. Start the demo with:  ${proxy_cmd} --demo"
	info "Or set up persistent Claude Code cost-cutting:  re-run with --claude-code"
	exit 0
fi

# --- claude-code mode: persistent setup under ~/.cortex, then start ---
#
# Separate from --demo because --demo regenerates ./cortex-ca/demo.yaml from a
# built-in template on every start, so any edit (like the remove: list this mode
# fills in) would be discarded on the next run. A config under ~/.cortex is
# outside that path and survives.
if [ "$MODE" = "claude-code" ]; then
	cfg_dir="${HOME}/.cortex"
	cfg="${cfg_dir}/config.yaml"
	cc_ca_dir="${cfg_dir}/ca"
	mkdir -p "$cfg_dir"

	if [ -f "$cfg" ]; then
		info ""
		info "Keeping your existing config: ${cfg}"
	else
		info ""
		info "Writing ${cfg}"
		# ${HOME} is left literal on purpose: authbridge expands ${ENV_VAR} when it
		# loads the file, so the config stays portable and needs no path rewriting
		# after the fact.
		cat >"$cfg" <<'YAML'
# Cortex — cut Claude Code token cost by pruning unused tool definitions.
# Written by install-demo.sh --claude-code. Safe to edit; the proxy hot-reloads.
mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: 127.0.0.1:47600
  session_api_addr: 127.0.0.1:47601
  # The proxy-sidecar preset turns this listener on and refills it if emptied, so
  # pin it to loopback on an uncommon port. Left at its ":8082" default it binds
  # every interface — on a laptop that means the LAN — and collides with anything
  # else already using 8082.
  transparent_proxy_addr: 127.0.0.1:47603
  # Same reasoning: the default ":9091" is every-interface and collides with any
  # other authbridge on the host.
  health_addr: 127.0.0.1:47604
stats:
  address: 127.0.0.1:47602
tls_bridge:
  mode: enabled
  ca_dir: "${HOME}/.cortex/ca"
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      # tool-prune must stay last: it rewrites the request body, and body
      # readers have to precede it to see the original bytes.
      - name: tool-prune
        config:
          remove: []
YAML
	fi

	# Fill the remove: list before starting, so no hot-reload round-trip is
	# needed. This edits the config the user just asked us to set up, which is
	# the point of the mode — but say so, and say how to undo it, because it is
	# derived from their transcripts rather than chosen by them.
	info ""
	info "Choosing tools to prune from your own ~/.claude/projects transcripts..."
	if "${BIN_DIR}/abctl" tools scan --write "$cfg"; then
		info ""
		info "Wrote the remove: list to ${cfg}"
		info "To keep a tool, delete its name from that list — the proxy hot-reloads."
	else
		warn "the scan did not complete; the remove: list is empty, so tool-prune"
		warn "will do nothing until you run:  ${abctl_cmd} tools scan --write ${cfg}"
	fi

	info ""
	info "Starting the proxy in the background..."
	cc_log="${cfg_dir}/proxy.log"
	cc_pidfile="${cfg_dir}/proxy.pid"
	nohup "$proxy" --config "$cfg" </dev/null >"$cc_log" 2>&1 &
	cc_pid=$!
	echo "$cc_pid" >"$cc_pidfile"

	ready=0
	i=0
	while [ "$i" -lt 50 ]; do
		if ! kill -0 "$cc_pid" 2>/dev/null; then
			warn "the proxy exited during startup — last log lines:"
			tail -n 15 "$cc_log" >&2 || true
			die "proxy failed to start (full log: ${cc_log})"
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
		info "Cortex is running (pid ${cc_pid}).   Logs: ${cc_log}"
	else
		info "Cortex started (pid ${cc_pid}); couldn't confirm it's listening (install lsof or nc to verify). Logs: ${cc_log}"
	fi
	info ""
	info "Run Claude Code through it:"
	info ""
	info "    HTTPS_PROXY=http://localhost:${DEMO_FORWARD_PORT} \\"
	info "      NODE_EXTRA_CA_CERTS=${cc_ca_dir}/ca.crt \\"
	info "      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 claude"
	info ""
	info "  See what it saved:   ${abctl_cmd} --endpoint http://localhost:${DEMO_SESSION_PORT}"
	info "                       (Plugin pane -> tool-prune -> Metrics)"
	info "  Stop it:             kill \$(cat ${cc_pidfile})"
	info ""
	exit 0
fi

# --- start in the background, then wait until it's actually listening ---
info ""
info "Starting the demo in the background..."
mkdir -p "$ca_dir"
log="${ca_dir}/demo.log"
pidfile="${ca_dir}/demo.pid"
nohup "$proxy" --demo </dev/null >"$log" 2>&1 &
demo_pid=$!
echo "$demo_pid" >"$pidfile"

# Confirm readiness from real signals, not the "listening" log line — that line is
# emitted just *before* the socket is bound, so a bind failure could look ready.
# A bind failure exits within ms (the proxy Fatalf's), so watch for early exit;
# and probe the forward port for a true post-bind signal.
ready=0
i=0
while [ "$i" -lt 50 ]; do
	if ! kill -0 "$demo_pid" 2>/dev/null; then
		warn "the demo exited during startup — last log lines:"
		tail -n 15 "$log" >&2 || true
		die "demo failed to start (full log: ${log})"
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
	info "Cortex demo is running (pid ${demo_pid}).   Logs: ${log}"
else
	# It didn't exit during the startup window (a bind failure would have killed
	# it), but no probe tool confirmed the port — most likely up. Say so honestly.
	info "Cortex demo started (pid ${demo_pid}); couldn't confirm it's listening (install lsof or nc to verify). Logs: ${log}"
fi
info ""

# tool-prune ships inert: the remove list is empty, so it does nothing until a
# name is added. That empty list is the guard — on_error is enforce, because the
# list is what gates the plugin. Offer the scan that fills it in. Only patch a config
# that already exists, so a first run never rewrites a file it just created
# behind the user's back -- print the command instead and let them look first.
demo_cfg="${ca_dir}/demo.yaml"
if [ -f "${demo_cfg}" ]; then
	info "  Cut tool-manifest waste (fills the remove: list; hot-reloaded, no restart):"
	info "    ${abctl_cmd} tools scan --write ${demo_cfg}"
	info "  Then watch the Metrics section of tool-prune's pane in abctl."
	info ""
fi
info "  Watch traffic:   ${abctl_cmd} --endpoint http://localhost:${DEMO_SESSION_PORT}"
info "  Send traffic through it (e.g. Claude Code):"
info "    HTTPS_PROXY=http://localhost:${DEMO_FORWARD_PORT} \\"
info "      NODE_EXTRA_CA_CERTS=${ca_dir}/ca.crt \\"
info "      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 claude"
info ""
info "  Stop the demo:   kill ${demo_pid}   (or: kill \$(cat ${pidfile}))"
info ""
