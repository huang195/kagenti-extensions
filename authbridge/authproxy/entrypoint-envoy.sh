#!/bin/bash
set -eu

# Envoy + go-processor entrypoint with process supervision.
# Both processes run in the background; the shell stays as PID 1.
# If either process exits, the other is killed and the container exits
# non-zero so Kubernetes restarts it.

cleanup() {
  echo "Received signal, shutting down..."
  kill "$GO_PROCESSOR_PID" "$ENVOY_PID" 2>/dev/null || true
  wait
  exit 0
}
trap cleanup TERM INT

# Start go-processor in the background
echo "Starting go-processor..."
/usr/local/bin/go-processor &
GO_PROCESSOR_PID=$!

# Give go-processor a moment to start
sleep 2

# Start Envoy in the background (not exec — shell must stay PID 1 for supervision)
echo "Starting Envoy..."
/usr/local/bin/envoy -c /etc/envoy/envoy.yaml \
  --service-cluster auth-proxy --service-node auth-proxy --log-level debug &
ENVOY_PID=$!

# Wait for the first child to exit. If either process dies, restart the container.
wait -n "$GO_PROCESSOR_PID" "$ENVOY_PID"
EXIT_CODE=$?
echo "A process exited unexpectedly (exit code $EXIT_CODE), terminating container"
kill "$GO_PROCESSOR_PID" "$ENVOY_PID" 2>/dev/null || true
wait
exit 1
