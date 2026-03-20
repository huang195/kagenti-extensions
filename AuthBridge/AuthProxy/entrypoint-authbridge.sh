#!/bin/sh
set -eu

# AuthBridge combined entrypoint
# Manages: spiffe-helper, client-registration, go-processor, envoy
#
# Startup order preserves current multi-container timing:
#   1. spiffe-helper (background, long-running) -- writes JWT SVID
#   2. go-processor (background) -- handles missing credentials via waitForCredentials
#   3. client-registration (background one-shot) -- writes credentials when ready
#   4. envoy (exec, foreground) -- inbound JWT validation works immediately
#
# Runs as UID 1337 (Envoy UID, excluded from iptables redirect).

# --- Phase 1: Start spiffe-helper (if enabled) ---
if [ "${SPIRE_ENABLED}" = "true" ]; then
  echo "[AuthBridge] Starting spiffe-helper..."
  /usr/local/bin/spiffe-helper -config=/etc/spiffe-helper/helper.conf run &
fi

# --- Phase 2: Start go-processor ---
# go-processor waits internally for credential files (waitForCredentials, 60s timeout).
# Inbound JWT validation works immediately (doesn't need credentials).
echo "[AuthBridge] Starting go-processor..."
/usr/local/bin/go-processor &
sleep 2

# --- Phase 3: Start client-registration (background, non-blocking) ---
# This runs asynchronously so Envoy starts immediately.
# Failures are non-fatal: go-processor handles missing credentials gracefully.
(
  if [ "${SPIRE_ENABLED}" = "true" ]; then
    echo "[AuthBridge] Waiting for SPIFFE credentials..."
    while [ ! -f /opt/jwt_svid.token ]; do sleep 1; done
    echo "[AuthBridge] SPIFFE credentials ready"

    # Extract client ID from JWT SVID payload
    JWT_PAYLOAD=$(cat /opt/jwt_svid.token | cut -d'.' -f2)
    CLIENT_ID=$(echo "${JWT_PAYLOAD}==" | base64 -d 2>/dev/null | \
      python3 -c "import sys,json; print(json.load(sys.stdin).get('sub',''))")
    echo "$CLIENT_ID" > /shared/client-id.txt
    echo "[AuthBridge] Client ID (SPIFFE ID): $CLIENT_ID"
  else
    echo "$CLIENT_NAME" > /shared/client-id.txt
    echo "[AuthBridge] Client ID: $CLIENT_NAME"
  fi

  if [ "${CLIENT_REGISTRATION_ENABLED}" != "false" ]; then
    echo "[AuthBridge] Starting client registration..."
    python3 /app/client_registration.py || \
      echo "[AuthBridge] WARNING: Client registration failed, continuing without"
    echo "[AuthBridge] Client registration phase complete"
  fi
) &

# --- Phase 4: Start Envoy (foreground) ---
# exec replaces this shell with Envoy (becomes PID 1).
# Kubernetes sends SIGTERM to PID 1 on termination.
echo "[AuthBridge] Starting Envoy..."
exec /usr/local/bin/envoy -c /etc/envoy/envoy.yaml \
  --service-cluster auth-proxy --service-node auth-proxy
