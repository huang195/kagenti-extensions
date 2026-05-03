# MCP Parser Plugin Demo

This guide shows how to enable the `mcp-parser` plugin so AuthBridge
parses MCP JSON-RPC requests and logs tool calls, resource reads, and
prompt invocations.

## Prerequisites

- A running Kagenti cluster (Kind or OpenShift) with the Ansible installer
  completed. The mcp-parser plugin works in `envoy-sidecar` and
  `proxy-sidecar` modes on any cluster type.
- A namespace (e.g., `team1`) labeled with `kagenti-enabled: "true"` for
  AuthBridge sidecar injection
- An MCP-based agent already deployed (e.g., the weather agent from
  [demo-ui-advanced](../weather-agent/demo-ui-advanced.md))

## How It Works

The `mcp-parser` plugin:
1. Declares `BodyAccess: true`, which triggers body buffering in all
   listener modes
2. Parses the request body as JSON-RPC 2.0
3. Populates `pctx.Extensions.MCP` with the parsed method, tool name,
   resource URI, or prompt name
4. Returns `Continue` unconditionally (never rejects)

In envoy-sidecar mode, AuthBridge uses ext_proc `ModeOverride` to
dynamically request the body from Envoy. No Envoy configuration changes
are needed — the default `request_body_mode: NONE` is overridden
per-stream when `mcp-parser` is in the pipeline.

## Step 1: Patch the Runtime Config

The `authbridge-runtime-config` ConfigMap in the agent namespace contains
the `config.yaml` that AuthBridge reads at startup. Add the `pipeline`
section:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-runtime-config
  namespace: team1
data:
  config.yaml: |
    mode: envoy-sidecar
    inbound:
      issuer: "http://keycloak.localtest.me:8080/realms/kagenti"
    outbound:
      keycloak_url: "http://keycloak-service.keycloak.svc:8080"
      keycloak_realm: "kagenti"
      default_policy: "passthrough"
    identity:
      type: "spiffe"
      client_id_file: "/shared/client-id.txt"
      client_secret_file: "/shared/client-secret.txt"
      jwt_svid_path: "/opt/jwt_svid.token"
    bypass:
      inbound_paths:
        - "/.well-known/*"
        - "/healthz"
        - "/readyz"
        - "/livez"
    pipeline:
      inbound:
        plugins:
          - jwt-validation
          - mcp-parser
EOF
```

> **Note**: The `pipeline` section is the only addition. All other fields
> should match your existing `authbridge-runtime-config`. If you use
> `client-secret` identity type instead of `spiffe`, adjust accordingly.

The `mcp-parser` is placed **after** `jwt-validation` in the inbound
pipeline. This means:
- Unauthenticated requests are rejected before body buffering occurs
- Only validated requests have their body parsed (no wasted work)
- Future policy plugins can read both `pctx.Claims` and
  `pctx.Extensions.MCP`

## Step 2: Enable Debug Logging (Optional)

To see all MCP parsing output, set `LOG_LEVEL=debug` on the authbridge
container. The easiest way:

```bash
# If using the operator-injected sidecar, patch the ConfigMap:
kubectl patch configmap authbridge-config -n team1 \
  --type merge -p '{"data":{"LOG_LEVEL":"debug"}}'
```

Or toggle at runtime without restart:

```bash
# Send SIGUSR1 to the authbridge process (PID 1 inside the container)
kubectl exec deploy/<agent-name> -n team1 -c authbridge-proxy -- kill -USR1 1
```

## Step 3: Restart the Agent Pod

The config is read at startup, so restart the pod:

```bash
kubectl rollout restart deploy/<agent-name> -n team1
```

Wait for the new pod to be ready:

```bash
kubectl rollout status deploy/<agent-name> -n team1
```

## Step 4: Send a Request Through the Agent

Use the Kagenti UI or curl to trigger a tool call through the agent.
For the weather agent:

```bash
# Via the Kagenti UI:
# Navigate to the agent, type "What's the weather in NYC?"

# Or via curl (replace TOKEN with a valid Keycloak access token):
curl -X POST http://<agent-host>/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"get_weather","arguments":{"city":"NYC"}}}'
```

## Step 5: Verify in Logs

Check the authbridge container logs for MCP parsing output:

```bash
kubectl logs deploy/<agent-name> -n team1 -c authbridge-proxy | grep mcp-parser
```

### Expected Output (LOG_LEVEL=info)

When a tool call flows through:

```
level=INFO msg="mcp-parser: parsed tools/call" tool=get_weather
```

When a resource read flows through:

```
level=INFO msg="mcp-parser: parsed resources/read" uri=file:///tmp/data.csv
```

### Expected Output (LOG_LEVEL=debug)

With debug enabled, you also see:

```
level=DEBUG msg="ext_proc: requesting body from Envoy" direction=inbound
level=DEBUG msg="ext_proc: received request body" direction=inbound bodyLen=87
level=DEBUG msg="pipeline: plugin completed" plugin=jwt-validation
level=INFO  msg="mcp-parser: parsed tools/call" tool=get_weather
level=DEBUG msg="pipeline: plugin completed" plugin=mcp-parser
```

### Requests That Are NOT MCP

Non-JSON or non-MCP requests pass through silently at debug level:

```
level=DEBUG msg="mcp-parser: body is not valid JSON-RPC" error="invalid character..." bodyLen=42
```

Or if the body is empty (e.g., GET requests):

```
level=DEBUG msg="mcp-parser: no body, skipping"
```

## Troubleshooting

### No mcp-parser logs at all

1. Confirm the ConfigMap was applied:
   ```bash
   kubectl get configmap authbridge-runtime-config -n team1 -o yaml | grep mcp-parser
   ```
2. Confirm the pod restarted after the ConfigMap change
3. Check authbridge startup logs for `"mode", "envoy-sidecar"` — if it
   says the wrong mode, the config isn't being read

### "waypoint mode does not support plugins that require body access"

This fatal error means you configured `mcp-parser` in waypoint mode.
ext_authz cannot forward request bodies (hard Envoy constraint). Use
envoy-sidecar or proxy-sidecar mode instead.

### Body not reaching the parser

If you see `mcp-parser: no body, skipping` for requests that should
have a body, check:
1. The request is POST with a JSON body (GET requests have no body)
2. The `Content-Length` header is present (some clients omit it for
   chunked encoding)
3. Envoy's `per_stream_buffer_limit_bytes` isn't set too low (default
   1MB is fine for MCP)

## What This Enables (Future)

With `pctx.Extensions.MCP` populated, future plugins can:

- **tool-policy**: Allow/deny specific tools based on caller identity
  (`pctx.Claims.Scopes` + `pctx.Extensions.MCP.Tool.Name`)
- **audit**: Log every tool invocation with full caller attribution
- **guardrails**: Inspect tool arguments for PII or injection patterns
- **rate-limit**: Per-tool rate limiting based on caller identity

These are Phase 2/3 plugins that read the `mcp` extension slot.
