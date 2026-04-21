# AuthBridge Binary

A single binary that replaces three separate codebases (go-processor, waypoint, klaviger) with a unified auth proxy supporting three deployment modes.

## Images

| Image | Dockerfile | Size | Contents |
|-------|-----------|------|----------|
| `authbridge-envoy` | `Dockerfile` | 140 MB | Envoy + authbridge (UBI9-micro) |
| `authbridge-light` | `Dockerfile.light` | 29 MB | authbridge only (distroless) |
| `authbridge-unified` | `Dockerfile` | 140 MB | Deprecated alias (same image as `authbridge-envoy`) |

## Modes

| Mode | Image | Interception | Listeners |
|------|-------|-------------|-----------|
| `envoy-sidecar` | `authbridge-envoy` | Envoy iptables + ext_proc | gRPC ext_proc on :9090 |
| `proxy-sidecar` | `authbridge-light` | HTTP_PROXY env + port-stealing | HTTP reverse proxy + forward proxy |
| `waypoint` | `authbridge-light` | Istio ambient + ext_authz | gRPC ext_authz + HTTP forward proxy |

### proxy-sidecar port reassignment

In proxy-sidecar mode, the kagenti-operator webhook transparently reassigns the agent's port to interpose the reverse proxy:

1. The reverse proxy takes over the agent's original port (e.g., `:8000`)
2. The agent is moved to a free port (e.g., `:8001`) via `PORT` env var
3. `HTTP_PROXY`/`HTTPS_PROXY` env vars are injected into the agent container
4. The Service targetPort remains unchanged — traffic flows through the reverse proxy

The operator passes the dynamically assigned ports via env vars (`REVERSE_PROXY_ADDR`, `REVERSE_PROXY_BACKEND`, `FORWARD_PROXY_ADDR`) which are expanded via `${...}` in the config YAML.

## Selecting a Mode

The operator selects the mode via annotation on the workload's pod template:

```yaml
# Default (envoy-sidecar) — no annotation needed
metadata:
  labels:
    kagenti.io/type: agent

# Proxy-sidecar mode
metadata:
  labels:
    kagenti.io/type: agent
  annotations:
    kagenti.io/authbridge-mode: "proxy-sidecar"
```

## Building

All builds run from the **repo root** with `authbridge/` as the build context:

```bash
# Envoy variant (envoy-sidecar mode)
podman build -f authbridge/cmd/authbridge/Dockerfile -t authbridge-envoy:local authbridge/

# Lightweight variant (proxy-sidecar / waypoint modes)
podman build -f authbridge/cmd/authbridge/Dockerfile.light -t authbridge-light:local authbridge/

# Load into Kind
kind load docker-image authbridge-envoy:local --name kagenti
kind load docker-image authbridge-light:local --name kagenti
```

The Envoy image contains both Envoy and the authbridge binary. The entrypoint starts both processes with `wait -n` supervision (if either dies, the container restarts). The light image runs the authbridge binary directly as the entrypoint.

## Running

```bash
authbridge --mode envoy-sidecar --config /etc/authbridge/config.yaml
```

The `--mode` flag can also be set in the YAML config. The flag overrides the config file value.

## Configuration

YAML with `${ENV_VAR}` expansion. Undefined env vars are preserved as-is (not expanded to empty).

### envoy-sidecar mode

Drop-in replacement for `envoy-with-processor`. Used as a sidecar alongside Envoy in each agent pod.

```yaml
mode: envoy-sidecar
inbound:
  jwks_url: "${JWKS_URL}"                    # or derived from token_url
  issuer: "${ISSUER}"                        # or derived from keycloak_url + keycloak_realm
outbound:
  token_url: "${TOKEN_URL}"                  # or derived from keycloak_url + keycloak_realm
  keycloak_url: "${KEYCLOAK_URL}"            # alternative to explicit token_url
  keycloak_realm: "${KEYCLOAK_REALM}"        # used with keycloak_url
  default_policy: "passthrough"              # passthrough (default) or exchange
identity:
  type: spiffe                               # spiffe or client-secret
  client_id: "${CLIENT_ID}"                  # or use client_id_file
  client_id_file: "/shared/client-id.txt"    # read from file (waits up to 60s)
  client_secret_file: "/shared/client-secret.txt"
  jwt_svid_path: "/opt/jwt_svid.token"       # for SPIFFE JWT-SVID auth
bypass:
  inbound_paths:                             # defaults: /.well-known/*, /healthz, /readyz, /livez
    - "/.well-known/*"
    - "/healthz"
routes:
  file: "/etc/authproxy/routes.yaml"         # load routes from file
  rules:                                     # or inline
    - host: "target-service.**"
      target_audience: "target"
      token_scopes: "openid target-aud"
```

### waypoint mode

Shared service for Istio ambient mesh. Derives audience from destination hostname automatically.

```yaml
mode: waypoint
inbound:
  issuer: "${ISSUER}"
outbound:
  keycloak_url: "${KEYCLOAK_URL}"
  keycloak_realm: "${KEYCLOAK_REALM}"
  default_policy: "exchange"
identity:
  type: client-secret
  client_id: "token-exchange-service"
  client_secret: "${CLIENT_SECRET}"
```

### proxy-sidecar mode

Sidecar without Envoy. Reverse proxy validates inbound, forward proxy exchanges outbound.

```yaml
mode: proxy-sidecar
listener:
  reverse_proxy_backend: "http://localhost:8081"
inbound:
  issuer: "${ISSUER}"
outbound:
  keycloak_url: "${KEYCLOAK_URL}"
  keycloak_realm: "${KEYCLOAK_REALM}"
identity:
  type: spiffe
  client_id: "${CLIENT_ID}"
  jwt_svid_path: "/opt/jwt_svid.token"
```

## URL Derivation

When explicit URLs are not set, they are derived automatically:

| Missing field | Derived from | Example |
|---|---|---|
| `token_url` | `keycloak_url` + `keycloak_realm` | `http://keycloak:8080/realms/kagenti/protocol/openid-connect/token` |
| `issuer` | `keycloak_url` + `keycloak_realm` | `http://keycloak:8080/realms/kagenti` |
| `jwks_url` | `token_url` | `.../openid-connect/token` becomes `.../openid-connect/certs` |

Explicit values always take precedence over derived values.

## Credential File Waiting

When `client_id_file`, `client_secret_file`, or `jwt_svid_path` are configured, the binary polls for the file to exist (up to 60 seconds) before starting. This handles the startup race with client-registration and spiffe-helper sidecars.

## Logging

AuthBridge uses Go's `slog` structured logger. The log level is configurable at startup and at runtime.

### Set level at startup

Set the `LOG_LEVEL` env var (`debug`, `info`, `warn`, `error`). Default: `info`.

```bash
# In a deployment
kubectl set env deployment/weather-service -n team1 -c authbridge-proxy LOG_LEVEL=debug

# Standalone
LOG_LEVEL=debug authbridge --config /etc/authbridge/config.yaml
```

### Toggle at runtime (no restart)

Send `SIGUSR1` to toggle between `info` and `debug`:

```bash
kubectl exec deploy/weather-service -n team1 -c authbridge-proxy -- kill -USR1 1
```

Send again to toggle back. The current level is logged on each toggle.

## Architecture

```
cmd/authbridge/
├── main.go              # --mode + --config, starts listeners, graceful shutdown
├── entrypoint.sh        # Envoy + authbridge process supervision (wait -n)
├── Dockerfile           # Combined Envoy + authbridge image (ubi-minimal)
└── listener/
    ├── extproc/         # Envoy ext_proc gRPC streaming (envoy-sidecar mode)
    ├── extauthz/        # Envoy ext_authz gRPC unary (waypoint mode)
    ├── forwardproxy/    # HTTP forward proxy (waypoint + proxy-sidecar)
    └── reverseproxy/    # HTTP reverse proxy (proxy-sidecar mode)
```

Listeners are thin protocol translators (~50-175 lines each). All auth logic lives in `authlib/`.
