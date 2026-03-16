# Local Testing Guide for JWT-SVID Authentication

This guide walks you through testing JWT-SVID authentication using local images (no push to ghcr.io).

## ⚠️ Important: Use the Build Script

**CRITICAL**: You MUST run the `./local-build-and-test.sh` script to build all required images. Do NOT build images individually with `docker build` or `podman build` commands, as this will miss critical images like `spiffe-idp-setup:local` (located in the kagenti repo).

The script:
- Builds images from **both** kagenti and kagenti-extensions repositories
- Automatically detects Docker vs Podman
- Loads all images into your Kind cluster
- Ensures consistent image tags and pull policies

## Prerequisites

- Docker or Podman running
- Kind CLI installed
- Both `kagenti` and `kagenti-extensions` repositories cloned

## Step 0: Create Kind Cluster

The Kagenti ansible installer can create a Kind cluster automatically, but for local image testing, it's better to create it manually first:

```bash
# Create a Kind cluster with the correct name
kind create cluster --name kagenti-dev --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 30080
    hostPort: 8080
    protocol: TCP
  - containerPort: 30443
    hostPort: 8443
    protocol: TCP
EOF

# Verify cluster is running
kubectl cluster-info --context kind-kagenti-dev
```

## Step 1: Build and Load Local Images

**⚠️ REQUIRED: Run the automated build script**

The `local-build-and-test.sh` script is the **only supported way** to build local images for testing. It builds images from both repositories and ensures everything is loaded correctly.

```bash
cd kagenti-extensions

# Make the script executable (first time only)
chmod +x local-build-and-test.sh

# Build all images and load into Kind cluster
# For Podman users: set KIND_EXPERIMENTAL_PROVIDER
export KIND_EXPERIMENTAL_PROVIDER=podman  # Only needed for Podman
./local-build-and-test.sh

# If using a different cluster name:
# CLUSTER_NAME=my-cluster ./local-build-and-test.sh
```

### What the script builds and loads:

**From kagenti repo** (critical - often missed!):
- `ghcr.io/kagenti/kagenti/spiffe-idp-setup:local` - Configures SPIFFE Identity Provider in Keycloak

**From kagenti-extensions repo**:
- `ghcr.io/kagenti/kagenti-extensions/client-registration:local` - Registers agents as Keycloak clients
- `ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local` - Admission webhook for sidecar injection
- `ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:local` - Envoy proxy with token exchange
- `ghcr.io/kagenti/kagenti-extensions/proxy-init:local` - iptables initialization

### Common Mistakes to Avoid:

❌ **DON'T** run individual `docker build` or `podman build` commands
❌ **DON'T** skip building images from the kagenti repo
❌ **DON'T** forget to set `KIND_EXPERIMENTAL_PROVIDER=podman` if using Podman

✅ **DO** run `./local-build-and-test.sh` every time you need to rebuild
✅ **DO** verify all images are loaded: `kind get images --name kagenti-dev | grep :local`

**Note for Podman users:** The script automatically detects Podman and uses tar archives to load images into Kind, since `kind load docker-image` doesn't work with Podman's image store.

## Step 2: Update Hardcoded Image Tags

The webhook's default image tags are hardcoded in [`internal/webhook/config/defaults.go`](kagenti-webhook/internal/webhook/config/defaults.go:12-15). Update them to use `:local`:

```bash
cd kagenti-extensions/kagenti-webhook

# Replace image tags (these sed commands modify defaults.go in-place)
sed -i '' 's|envoy-with-processor:latest|envoy-with-processor:local|g' internal/webhook/config/defaults.go
sed -i '' 's|proxy-init:latest|proxy-init:local|g' internal/webhook/config/defaults.go
sed -i '' 's|client-registration:latest|client-registration:local|g' internal/webhook/config/defaults.go

# Also update PullPolicy to Never (don't pull from registry)
sed -i '' 's|PullPolicy:.*corev1.PullIfNotPresent|PullPolicy:         corev1.PullNever|g' internal/webhook/config/defaults.go

# Verify changes
grep -E "envoy-with-processor|proxy-init|client-registration|PullPolicy" internal/webhook/config/defaults.go

# Rebuild the webhook image with updated tags
make docker-build IMG=ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local

# Load into Kind cluster
# For Docker users:
kind load docker-image ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local --name kagenti-dev

# For Podman users (use tar archive method):
# podman save ghcr.io/kagenti/kagenti-extensions/kagenti-webhook:local -o /tmp/webhook-local.tar
# kind load image-archive /tmp/webhook-local.tar --name kagenti-dev
# rm /tmp/webhook-local.tar
```

## Step 3: Install Kagenti with Ansible

**IMPORTANT:** For federated-JWT testing with local images, use the unified `federated-jwt-values.yaml` overlay file from kagenti-extensions.

The ansible installer will detect the existing `kagenti-dev` cluster and install into it:

```bash
# Go to kagenti repo
cd kagenti

# Install with dev base values + TWO overlays (deps local images + extensions federated-jwt)
# --env dev                                → Loads dev_values.yaml (base Kind development config)
# --env-file deployments/envs/...         → Local images for kagenti-deps (SPIRE, Keycloak, etc.)
# --env-file ../kagenti-extensions/...    → Federated-jwt + local images for kagenti-extensions
deployments/ansible/run-install.sh --env dev \
  --env-file deployments/envs/dev_values_local_images.yaml \
  --env-file deployments/envs/dev_values_federated-jwt.yaml
```

**About the values files:**
- `dev_values.yaml`: Base Kind development configuration (components, Keycloak, domain, basic SPIRE config, openshift: false)
- `dev_values_local_images.yaml`: **Testing-only** local image overrides for kagenti-deps (tag: local, pullPolicy: Never)
- `federated-jwt-values.yaml`: **Testing + federated-jwt overlay** for kagenti-extensions:
  - Cluster name: `kagenti-dev`
  - SPIRE enabled with correct namespace (`zero-trust-workload-identity-manager`)
  - JWT-SVID authentication (`authBridge.clientAuthType: federated-jwt`)
  - Local image tags (`:local`) for kagenti-extensions components
  - Keycloak `kagenti` realm configuration
  - Agent namespace (`team1`)
- The installer **merges all three files** in order, each overlay adding/overriding specific values

This installation will:
1. Detect and use the existing `kagenti-dev` Kind cluster
2. Deploy kagenti-deps (Keycloak, SPIRE, etc.) via Helm
3. **Patch SPIRE ConfigMap** with `set_key_use: true` (workaround for SPIRE Helm chart bug)
4. **Create SPIFFE IdP setup job** (configures Keycloak with SPIFFE Identity Provider)
5. Deploy kagenti chart with `authBridge.clientAuthType=federated-jwt`
6. Use `:local` image tags for all components (pulled from the cluster's local image cache)

**How SPIFFE IdP setup works:**
- Ansible creates the job AFTER patching the SPIRE ConfigMap (avoids race condition)
- Job waits for SPIRE server and OIDC discovery provider to be ready
- Job validates JWKS has required "use" field
- Job configures Keycloak with SPIFFE Identity Provider named "spire-spiffe"
- Ansible waits for job completion before proceeding

**Expected behavior:**
- Installation typically completes in 6-8 minutes
- The SPIFFE IdP job should succeed on first attempt (no CrashLoopBackOff)
- All components should be running and ready

## Step 4: Verify SPIRE and Keycloak

```bash
cd kagenti-extensions

chmod +x verify-spire-keycloak.sh
./verify-spire-keycloak.sh
```

Expected output:
- ✅ SPIRE server is running
- ✅ SPIRE OIDC discovery provider is running
- ✅ SPIRE JWKS has 'use' field
- ✅ Keycloak is running
- ✅ Keycloak admin secret exists
- ✅ SPIFFE IdP setup job completed successfully

## Step 5: Test SPIFFE/Keycloak Authentication

This test verifies that workloads can authenticate to Keycloak using JWT-SVIDs from SPIRE.

### Create Test Deployment

```bash
# 1. Create namespace and deploy SPIFFE helper pod
kubectl create namespace agent1

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: spiffe-helper-config
  namespace: agent1
data:
  helper.conf: |
    agent_address = "/spiffe-workload-api/spire-agent.sock"
    cmd = ""
    cmd_args = ""
    svid_file_name = "/opt/svid.pem"
    svid_key_file_name = "/opt/svid_key.pem"
    svid_bundle_file_name = "/opt/svid_bundle.pem"
    jwt_svids = [{jwt_audience="http://keycloak.localtest.me:8080/realms/kagenti", jwt_svid_file_name="/opt/jwt_svid.token"}]
    jwt_svid_file_mode = 0644
    include_federated_domains = true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spiffe-demo
  namespace: agent1
  labels:
    app: spiffe-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: spiffe-demo
  template:
    metadata:
      labels:
        app: spiffe-demo
    spec:
      containers:
      - name: spiffe-helper
        image: ghcr.io/spiffe/spiffe-helper:nightly
        args: ["-config", "/conf/helper.conf"]
        volumeMounts:
        - name: spiffe-workload-api
          mountPath: /spiffe-workload-api
          readOnly: true
        - name: certs
          mountPath: /opt
        - name: config
          mountPath: /conf
      - name: tools
        image: ghcr.io/nicolaka/netshoot:latest
        command: ["sleep", "infinity"]
        volumeMounts:
        - name: certs
          mountPath: /opt
          readOnly: true
      volumes:
      - name: spiffe-workload-api
        csi:
          driver: "csi.spiffe.io"
          readOnly: true
      - name: certs
        emptyDir: {}
      - name: config
        configMap:
          name: spiffe-helper-config
EOF

# Wait for pod to be ready
kubectl wait -n agent1 --for=condition=ready pod -l app=spiffe-demo --timeout=120s
```

### Create Keycloak Client

```bash
# 2. Configure kcadm.sh credentials
kubectl exec -n keycloak keycloak-0 -- /opt/keycloak/bin/kcadm.sh config credentials \
  --server http://localhost:8080 --realm master --user admin --password admin

# 3. Create client with SPIFFE authentication
kubectl exec -n keycloak keycloak-0 -- /opt/keycloak/bin/kcadm.sh create clients -r kagenti \
  -s 'clientId=spiffe://localtest.me/ns/agent1/sa/default' \
  -s 'clientAuthenticatorType=client-spiffe' \
  -s 'serviceAccountsEnabled=true' \
  -s 'directAccessGrantsEnabled=false' \
  -s 'standardFlowEnabled=false' \
  -s 'enabled=true'
```

### Test Token Exchange

```bash
# 4. Get JWT-SVID from the pod
export JWT=$(kubectl exec -n agent1 deploy/spiffe-demo -c tools -- sh -c 'cat /opt/jwt_svid.token')

# 5. Exchange JWT-SVID for access token
curl -s -X POST \
    -d "grant_type=client_credentials" \
    -d "client_id=spiffe://localtest.me/ns/agent1/sa/default" \
    -d "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" \
    -d "client_assertion=$JWT" \
    "http://keycloak.localtest.me:8080/realms/kagenti/protocol/openid-connect/token"
```

**Expected output:** JSON response with `access_token`, `expires_in`, `token_type`, etc.

**What this test verifies:**
- SPIRE agent issues JWT-SVIDs with correct audience
- Keycloak SPIFFE IdP is configured correctly
- Workloads can authenticate using their SPIFFE identity
- Token exchange flow works end-to-end

### Cleanup

```bash
kubectl delete namespace agent1
```

### Optional: Run Full AuthBridge Demo

For testing the complete AuthBridge flow with automatic sidecar injection:

**Manual Demo:** Follow [AuthBridge/demos/github-issue/demo-manual.md](AuthBridge/demos/github-issue/demo-manual.md)

**Webhook Demo:** Follow [AuthBridge/demos/webhook/README.md](AuthBridge/demos/webhook/README.md)

---

## Appendix: Standalone Helm Install (Without Ansible)

If you want to install kagenti-deps directly with Helm instead of using the Ansible installer, you must manually configure SPIFFE IdP support due to a bug in the SPIRE Helm chart that prevents `set_key_use` from being rendered correctly.

### Step 1: Install kagenti-deps

```bash
helm install kagenti-deps ./charts/kagenti-deps/ \
  -n kagenti-system \
  --create-namespace \
  --set spire.enabled=true \
  --set keycloak.enabled=true \
  --wait
```

### Step 2: Patch SPIRE ConfigMap

The SPIRE Helm chart doesn't render `set_key_use: true` to the ConfigMap (even when set in values). This causes the JWKS to be missing the "use" field that Keycloak 26+ requires.

```bash
# Get the SPIRE namespace (may vary)
SPIRE_NAMESPACE=zero-trust-workload-identity-manager

# Patch the ConfigMap
kubectl get configmap spire-spiffe-oidc-discovery-provider \
  -n $SPIRE_NAMESPACE -o json | \
  jq '.data["oidc-discovery-provider.conf"] |= (fromjson | .set_key_use = true | tojson)' | \
  kubectl apply -f -

# Restart OIDC provider to apply changes
kubectl rollout restart deployment/spire-spiffe-oidc-discovery-provider -n $SPIRE_NAMESPACE

# Wait for rollout to complete
kubectl rollout status deployment/spire-spiffe-oidc-discovery-provider -n $SPIRE_NAMESPACE --timeout=2m
```

### Step 3: Create SPIFFE IdP Setup Job

The job is not included in the Helm chart to avoid race conditions. Create it manually after the patch:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kagenti-spiffe-idp-setup
  namespace: kagenti-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kagenti-spiffe-idp-reader
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["keycloak-admin-secret"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kagenti-spiffe-idp-keycloak-reader
  namespace: keycloak
subjects:
  - kind: ServiceAccount
    name: kagenti-spiffe-idp-setup
    namespace: kagenti-system
roleRef:
  kind: ClusterRole
  name: kagenti-spiffe-idp-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: batch/v1
kind: Job
metadata:
  name: kagenti-spiffe-idp-setup-job
  namespace: kagenti-system
spec:
  backoffLimit: 10
  template:
    spec:
      serviceAccountName: kagenti-spiffe-idp-setup
      restartPolicy: OnFailure
      initContainers:
        - name: wait-for-spire
          image: bitnami/kubectl:latest
          command:
            - sh
            - -c
            - |
              echo "Waiting for SPIRE server..."
              kubectl wait --for=condition=ready pod -l app=spire-server \
                -n zero-trust-workload-identity-manager --timeout=300s
              echo "Waiting for SPIRE OIDC provider..."
              kubectl wait --for=condition=ready pod \
                -l app.kubernetes.io/name=spire-spiffe-oidc-discovery-provider \
                -n zero-trust-workload-identity-manager --timeout=300s
      containers:
        - name: setup-spiffe-idp
          image: ghcr.io/kagenti/kagenti/spiffe-idp-setup:latest
          env:
            - name: KEYCLOAK_BASE_URL
              value: "http://keycloak-service.keycloak.svc:8080"
            - name: KEYCLOAK_REALM
              value: "kagenti"
            - name: KEYCLOAK_NAMESPACE
              value: "keycloak"
            - name: KEYCLOAK_ADMIN_SECRET_NAME
              value: "keycloak-admin-secret"
            - name: KEYCLOAK_ADMIN_USERNAME_KEY
              value: "username"
            - name: KEYCLOAK_ADMIN_PASSWORD_KEY
              value: "password"
            - name: SPIFFE_TRUST_DOMAIN
              value: "spiffe://localtest.me"
            - name: SPIFFE_BUNDLE_ENDPOINT
              value: "http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys"
            - name: SPIFFE_IDP_ALIAS
              value: "spire-spiffe"
EOF

# Wait for job to complete
kubectl wait --for=condition=complete job/kagenti-spiffe-idp-setup-job \
  -n kagenti-system --timeout=5m

# Check job logs
kubectl logs job/kagenti-spiffe-idp-setup-job -n kagenti-system
```

### Step 4: Verify

```bash
# Check job status
kubectl get job kagenti-spiffe-idp-setup-job -n kagenti-system

# Verify JWKS has "use" field
kubectl run test-curl --rm -i --image=curlimages/curl --restart=Never -- \
  curl -s http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys | \
  jq '.keys[] | select(.use)'

# Should return keys with "use": "sig"
```

**Why these manual steps are needed:**

1. **SPIRE Helm chart bug**: The chart doesn't render `set_key_use` from values.yaml to the ConfigMap
2. **Keycloak 26+ requirement**: Keycloak requires JWKS keys to have a "use" field for SPIFFE authentication
3. **Race condition avoidance**: The job must run AFTER the ConfigMap is patched, not as a Helm post-install hook

**Recommendation:** Use the Ansible installer (Step 3 in main guide) instead of standalone Helm - it handles all of this automatically!

## Troubleshooting

### Issue: ErrImageNeverPull for spiffe-idp-setup

**Symptom:**
```
kagenti-spiffe-idp-setup-job-xxxxx   0/1   ErrImageNeverPull   0   5m
```

**Root Cause:** The `spiffe-idp-setup:local` image wasn't built or loaded into Kind.

**Solution:**
1. Verify you ran `./local-build-and-test.sh` (not individual docker build commands)
2. Check if the image is loaded:
   ```bash
   kind get images --name kagenti-dev | grep spiffe-idp-setup
   ```
3. If missing, rebuild:
   ```bash
   cd /Users/alan/Documents/Work/kagenti/kagenti/auth/spiffe-idp-setup

   # For Docker:
   docker build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
   kind load docker-image ghcr.io/kagenti/kagenti/spiffe-idp-setup:local --name kagenti-dev

   # For Podman:
   podman build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
   podman save ghcr.io/kagenti/kagenti/spiffe-idp-setup:local -o /tmp/spiffe-idp.tar
   kind load image-archive /tmp/spiffe-idp.tar --name kagenti-dev
   rm /tmp/spiffe-idp.tar
   ```
4. Delete the failing pod:
   ```bash
   kubectl delete pod -n kagenti-system -l job-name=kagenti-spiffe-idp-setup-job
   ```

### Issue: SPIFFE IdP Job Init Container CrashLoopBackOff

**Symptom:**
```
kagenti-spiffe-idp-setup-job-xxxxx   0/1   Init:CrashLoopBackOff   3   2m
```

**Root Cause:** Missing RBAC permissions to list pods in keycloak or SPIRE namespaces.

**Check:**
```bash
kubectl logs -n kagenti-system -l job-name=kagenti-spiffe-idp-setup-job -c wait-for-spire
```

**Expected error:**
```
Error from server (Forbidden): pods is forbidden: User "system:serviceaccount:kagenti-system:kagenti-spiffe-idp-setup"
cannot list resource "pods" in API group "" in the namespace "keycloak"
```

**Solution:** This should be automatically created by the Ansible installer. If missing, manually create RBAC:
```bash
# For keycloak namespace
kubectl create role kagenti-spiffe-idp-pod-reader \
  -n keycloak \
  --verb=get,list,watch \
  --resource=pods

kubectl create rolebinding kagenti-spiffe-idp-pod-reader \
  -n keycloak \
  --role=kagenti-spiffe-idp-pod-reader \
  --serviceaccount=kagenti-system:kagenti-spiffe-idp-setup
```

### Issue: Token Exchange Fails with "invalid_client"

**Symptom:**
```json
{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}
```

**Root Causes & Solutions:**

1. **Wrong client_assertion_type**
   - ❌ DON'T use: `urn:ietf:params:oauth:client-assertion-type:jwt-bearer`
   - ✅ DO use: `urn:ietf:params:oauth:client-assertion-type:jwt-spiffe`

2. **Client not configured for federated-jwt**
   - Check client authenticator type:
     ```bash
     kubectl exec -n keycloak keycloak-0 -- sh -c \
       '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin && \
        /opt/keycloak/bin/kcadm.sh get clients -r kagenti -q clientId="spiffe://localtest.me/ns/agent1/sa/default"' | \
       jq '.[] | {clientAuthenticatorType, attributes}'
     ```
   - Should show:
     ```json
     {
       "clientAuthenticatorType": "federated-jwt",
       "attributes": {
         "jwt.credential.issuer": "spire-spiffe",
         "jwt.credential.sub": "spiffe://localtest.me/ns/agent1/sa/default"
       }
     }
     ```

3. **SPIFFE Identity Provider missing**
   - Check IdP exists:
     ```bash
     kubectl exec -n keycloak keycloak-0 -- sh -c \
       '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin && \
        /opt/keycloak/bin/kcadm.sh get identity-provider/instances -r kagenti'
     ```
   - Should show IdP with alias "spire-spiffe"

### Issue: Images Not Found After Rebuild

**Symptom:** After making code changes and rebuilding, the old images are still used.

**Solution:**
1. Delete pods to force recreation:
   ```bash
   kubectl delete pod -n <namespace> <pod-name>
   ```
2. For webhook changes, restart the webhook deployment:
   ```bash
   kubectl rollout restart deployment -n kagenti-webhook-system kagenti-webhook
   ```
3. Verify new images are loaded:
   ```bash
   kind get images --name kagenti-dev | grep :local
   ```

## Verify Federated-JWT Configuration

Since you installed with `dev_values_federated-jwt.yaml`, the system should already be configured for JWT-SVID authentication:

```bash
# 1. Verify authBridge.clientAuthType is set to federated-jwt
kubectl get configmap kagenti-webhook-config -n team1 -o jsonpath='{.data.CLIENT_AUTH_TYPE}'
# Expected: federated-jwt

# 2. Deploy an agent and check client-registration logs
# (After deploying an agent in Step 6)
kubectl logs -n team1 deployment/<your-agent> -c kagenti-client-registration -f
# Expected: "Configuring client for JWT-SVID authentication (federated-jwt)"

# 3. Verify Keycloak client uses federated-jwt authenticator
# (After agent deployment creates a Keycloak client)
kubectl run test-curl --rm -i --image=curlimages/curl --restart=Never -- sh -c "
  ADMIN_TOKEN=\$(curl -s 'http://keycloak-service.keycloak.svc:8080/realms/master/protocol/openid-connect/token' \
    -d 'grant_type=password' -d 'client_id=admin-cli' -d 'username=admin' -d 'password=admin' | jq -r '.access_token')
  curl -s -H 'Authorization: Bearer \$ADMIN_TOKEN' \
    'http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients' | \
    jq '.[] | select(.clientId | contains(\"spiffe\")) | {clientId, clientAuthenticatorType}'
"
# Expected: "clientAuthenticatorType": "federated-jwt"
```