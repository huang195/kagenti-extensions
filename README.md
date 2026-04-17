# Kagenti Extensions

Kubernetes security extensions for the [Kagenti](https://github.com/kagenti/kagenti) ecosystem, providing **zero-trust authentication** for workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

## AuthBridge

[AuthBridge](./authbridge/) provides end-to-end authentication for Kubernetes workloads with [SPIFFE/SPIRE](https://spiffe.io) integration. It consists of:

- **[AuthProxy](./authbridge/authproxy/)** — Envoy proxy with a gRPC external processor for inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693). Enables secure service-to-service communication by transparently intercepting traffic.
- **[Client Registration](./authbridge/client-registration/)** — Automatically registers Kubernetes workloads as Keycloak OAuth2 clients using their SPIFFE identity, eliminating manual client configuration and static credentials.
- **[Keycloak Sync](./authbridge/keycloak_sync.py)** — Declarative tool for synchronizing Keycloak configuration.

See the [AuthBridge README](./authbridge/README.md) for architecture details and the [demos index](./authbridge/demos/README.md) for getting started.

## Container Images

All images are published to `ghcr.io/kagenti/kagenti-extensions/`:

| Image | Description |
|-------|-------------|
| `authbridge-unified` | Unified Envoy + authbridge binary (recommended) |
| `authbridge` | Combined sidecar (Envoy + authbridge + spiffe-helper + client-registration) |
| `proxy-init` | Alpine + iptables init container |
| `client-registration` | Python Keycloak client registrar |
| `spiffe-helper` | Fetches SPIFFE credentials from SPIRE |
| `auth-proxy` | Example pass-through proxy (for demos) |
| `demo-app` | Demo target service |

## Development

```bash
# Install pre-commit hooks
make pre-commit

# Run formatters
make fmt

# Build AuthProxy Docker images
make build-images

# Run local testing (requires Kind cluster)
./local-build-and-test.sh
```

See [LOCAL_TESTING_GUIDE.md](./LOCAL_TESTING_GUIDE.md) for the full local development setup.

## Related Repositories

- [kagenti](https://github.com/kagenti/kagenti) — Core Kagenti platform
- [kagenti-operator](https://github.com/kagenti/kagenti-operator) — Kubernetes operator for sidecar injection (includes the admission webhook)

## License

[Apache 2.0](./LICENSE)
