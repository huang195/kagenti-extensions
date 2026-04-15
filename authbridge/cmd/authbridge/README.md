# AuthBridge Unified Binary

A single binary that replaces three separate codebases (go-processor, waypoint, klaviger) with a unified auth proxy supporting three deployment modes.

## Modes

| Mode | Interception | Listeners | Deployment |
|------|-------------|-----------|------------|
| `envoy-sidecar` | Envoy iptables + ext_proc | gRPC ext_proc on :9090 | Sidecar per agent pod |
| `waypoint` | Istio ambient + ext_authz | gRPC ext_authz + HTTP forward proxy | Shared service in kagenti-system |
| `proxy-sidecar` | Reverse proxy + forward proxy | HTTP reverse proxy + forward proxy | Sidecar without Envoy |

## Building

```bash
# From authbridge/ directory (build context)
podman build -f cmd/authbridge/Dockerfile -t authbridge-unified:local .

# Load into Kind
kind load docker-image authbridge-unified:local --name kagenti
```

The image contains both Envoy and the authbridge binary. The entrypoint starts both processes with `wait -n` supervision (if either dies, the container restarts).

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
