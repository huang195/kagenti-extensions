#!/bin/bash
set -eu

# AuthBridge combined entrypoint with process supervision.
# Manages: spiffe-helper, client-registration, go-processor, envoy
#
# Startup order preserves current multi-container timing:
#   1. spiffe-helper (background, long-running) -- writes JWT SVID
#   2. go-processor (background) -- handles missing credentials via waitForCredentials
#   3. client-registration (background one-shot) -- writes credentials when ready
#   4. envoy (background) -- inbound JWT validation works immediately
#
# Runs as UID 1337 (Envoy UID, excluded from iptables redirect).
#
# Process management: The shell stays as PID 1 and monitors critical long-running
# processes (envoy, go-processor, spiffe-helper). If any critical process exits,
# all others are killed and the container exits non-zero so Kubernetes restarts it.
# Client-registration is a one-shot process and is NOT monitored -- go-processor
# handles missing credentials gracefully via passthrough mode.

# PIDs of critical long-running processes to monitor.
CRITICAL_PIDS=""

cleanup() {
  echo "[AuthBridge] Received signal, shutting down..."
  # shellcheck disable=SC2086
  kill $CRITICAL_PIDS 2>/dev/null || true
  wait
  exit 0
}
trap cleanup TERM INT

# --- Phase 1: Start spiffe-helper (if enabled) ---
if [ "${SPIRE_ENABLED:-}" = "true" ]; then
  echo "[AuthBridge] Starting spiffe-helper..."
  /usr/local/bin/spiffe-helper -config=/etc/spiffe-helper/helper.conf run &
  CRITICAL_PIDS="$CRITICAL_PIDS $!"
fi

# --- Phase 2: Start go-processor ---
# go-processor waits internally for credential files (waitForCredentials, 60s timeout).
# Inbound JWT validation works immediately (doesn't need credentials).
echo "[AuthBridge] Starting go-processor..."
/usr/local/bin/go-processor &
CRITICAL_PIDS="$CRITICAL_PIDS $!"
sleep 2

# --- Phase 3: Start client-registration (background, non-blocking) ---
# This runs asynchronously so Envoy starts immediately.
# Failures are non-fatal: go-processor handles missing credentials gracefully.
# NOT added to CRITICAL_PIDS -- this is a one-shot process.
(
  if [ "${SPIRE_ENABLED:-}" = "true" ]; then
    echo "[AuthBridge] Waiting for SPIFFE credentials..."
    while [ ! -f /opt/jwt_svid.token ]; do sleep 1; done
    echo "[AuthBridge] SPIFFE credentials ready"

    # Extract client ID from JWT SVID payload.
    # Each step is validated individually to avoid silent failures in the pipeline.
    JWT_PAYLOAD=$(cut -d'.' -f2 < /opt/jwt_svid.token)
    if [ -z "$JWT_PAYLOAD" ]; then
      echo "[AuthBridge] ERROR: Failed to extract JWT payload from SVID" >&2
    fi
    CLIENT_ID=$(echo "${JWT_PAYLOAD}==" | base64 -d 2>/dev/null | \
      python3 -c "import sys,json; print(json.load(sys.stdin).get('sub',''))")
    if [ -z "$CLIENT_ID" ]; then
      echo "[AuthBridge] ERROR: Failed to decode client ID from JWT SVID" >&2
    fi
    echo "$CLIENT_ID" > /shared/client-id.txt
    echo "[AuthBridge] Client ID (SPIFFE ID): $CLIENT_ID"
  else
    echo "$CLIENT_NAME" > /shared/client-id.txt
    echo "[AuthBridge] Client ID: $CLIENT_NAME"
  fi

  if [ "${CLIENT_REGISTRATION_ENABLED:-}" != "false" ]; then
    echo "[AuthBridge] Starting client registration..."
    python3 /app/client_registration.py || \
      echo "[AuthBridge] WARNING: Client registration failed, continuing without"
    echo "[AuthBridge] Client registration phase complete"
  fi
) &

# --- Phase 4: Start Envoy (background) ---
# Envoy runs in the background so the shell can monitor all critical processes.
# Kubernetes sends SIGTERM to PID 1 (this shell), which forwards via trap.
echo "[AuthBridge] Starting Envoy..."
/usr/local/bin/envoy -c /etc/envoy/envoy.yaml \
  --service-cluster auth-proxy --service-node auth-proxy &
CRITICAL_PIDS="$CRITICAL_PIDS $!"

# Wait for the first critical process to exit.
# If any critical process dies, restart the container.
# shellcheck disable=SC2086
wait -n $CRITICAL_PIDS
EXIT_CODE=$?
echo "[AuthBridge] A critical process exited unexpectedly (exit code $EXIT_CODE), terminating container"
# shellcheck disable=SC2086
kill $CRITICAL_PIDS 2>/dev/null || true
wait
exit 1
