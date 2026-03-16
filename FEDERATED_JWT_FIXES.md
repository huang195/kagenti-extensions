# Federated JWT Setup - Issues Found and Fixes Applied

## Overview
This document details issues found during federated-jwt (JWT-SVID) authentication testing and the fixes that need to be reflected in the charts and install scripts.

## 1. ErrImageNeverPull for spiffe-idp-setup:local

### Issue
The `kagenti-spiffe-idp-setup-job` pod failed with `ErrImageNeverPull` because the image `ghcr.io/kagenti/kagenti/spiffe-idp-setup:local` wasn't built or loaded into Kind.

### Root Cause
The `local-build-and-test.sh` script in kagenti-extensions was **not executed**. Instead, individual `docker build` commands were run, which didn't include the spiffe-idp-setup image (located in the kagenti repo, not kagenti-extensions).

### Fix Applied
Manually built and loaded the image:
```bash
cd /Users/alan/Documents/Work/kagenti/kagenti/auth/spiffe-idp-setup
podman build -t ghcr.io/kagenti/kagenti/spiffe-idp-setup:local .
# Load into Kind (for Podman)
tar_file="/tmp/ghcr.io-kagenti-kagenti-spiffe-idp-setup-local.tar"
podman save ghcr.io/kagenti/kagenti/spiffe-idp-setup:local -o "$tar_file"
kind load image-archive "$tar_file" --name kagenti-dev
rm -f "$tar_file"
```

### Required Changes
✅ **DONE** - The `local-build-and-test.sh` script already includes building spiffe-idp-setup (lines 55-63).

**Action Required**: Document in LOCAL_TESTING_GUIDE.md that users MUST run `./local-build-and-test.sh` instead of building images individually.

## 2. Missing RBAC Permissions for SPIFFE IdP Setup Job

### Issue
The spiffe-idp-setup job's init container failed with:
```
Error from server (Forbidden): pods is forbidden: User "system:serviceaccount:kagenti-system:kagenti-spiffe-idp-setup"
cannot list resource "pods" in API group "" in the namespace "keycloak"
```

### Root Cause
The ServiceAccount `kagenti-spiffe-idp-setup` needs permission to list pods in the `keycloak` namespace to wait for Keycloak to be ready.

### Fix Applied
Created Role and RoleBinding:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kagenti-spiffe-idp-pod-reader
  namespace: keycloak
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kagenti-spiffe-idp-pod-reader
  namespace: keycloak
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kagenti-spiffe-idp-pod-reader
subjects:
- kind: ServiceAccount
  name: kagenti-spiffe-idp-setup
  namespace: kagenti-system
```

### Required Changes
❌ **MISSING** - This RBAC configuration needs to be added to the Kagenti Helm chart.

**Action Required**: Add Role and RoleBinding to `charts/kagenti/templates/spiffe-idp-job.yaml` or create a new template file for the RBAC resources.

## 3. clientAuthenticatorType Not Set to federated-jwt

### Issue
When manually creating a test client, it had `clientAuthenticatorType: "client-spiffe"` instead of `"federated-jwt"`, and was missing required attributes:
- `jwt.credential.issuer`
- `jwt.credential.sub`

### Root Cause
This was a **testing artifact**, not a real issue with the installation:

1. The test deployment was manually created in the `agent1` namespace, which was also manually created
2. The Kagenti chart creates `team1` and `team2` namespaces by default (not `agent1`)
3. The current branch has a new ConfigMap `kagenti-webhook-config` with CLIENT_AUTH_TYPE and SPIFFE_IDP_ALIAS (commit 62108e73)
4. However, the Kagenti chart was installed **before** these changes were merged, so the installed chart doesn't have this ConfigMap

### Expected Behavior (with current branch)
When the chart is installed with `clientAuthType: "federated-jwt"`, it should:
1. Create `kagenti-webhook-config` ConfigMap in each agent namespace with:
   - `CLIENT_AUTH_TYPE: "federated-jwt"`
   - `SPIFFE_IDP_ALIAS: "spire-spiffe"`
   - `JWT_AUDIENCE: "..."`
   - `KEYCLOAK_NAMESPACE: "keycloak"`
2. The webhook reads from `kagenti-webhook-config` and passes CLIENT_AUTH_TYPE to the client-registration sidecar
3. The client-registration script creates clients with proper federated-jwt configuration

### Fix Applied (for testing)
Manually updated the test client:
```bash
kubectl exec -n keycloak keycloak-0 -- sh -c '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin > /dev/null 2>&1 && /opt/keycloak/bin/kcadm.sh update clients/e8bd70e4-b37d-4127-8eeb-1764ac36767f -r kagenti -s clientAuthenticatorType=federated-jwt -s '\''attributes."jwt.credential.issuer"=spire-spiffe'\'' -s '\''attributes."jwt.credential.sub"=spiffe://localtest.me/ns/agent1/sa/default'\'''
```

### Required Changes
✅ **DONE** - Commit 62108e73 "Consolidate kagenti-webhook-config with environments configmap" adds the kagenti-webhook-config ConfigMap.

**Action Required**:
1. Merge the current branch changes to main
2. Update Helm chart version/tag
3. Verify the webhook correctly reads CLIENT_AUTH_TYPE from kagenti-webhook-config

## 4. Wrong client_assertion_type Used

### Issue
Initial testing used `client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer` which failed with "invalid_client_credentials".

### Root Cause
Keycloak's SPIFFE authenticator requires the specific assertion type: `urn:ietf:params:oauth:client-assertion-type:jwt-spiffe`

### Fix Applied
Used correct assertion type in token exchange:
```bash
curl -X POST \
  -d "grant_type=client_credentials" \
  -d "client_id=spiffe://localtest.me/ns/agent1/sa/default" \
  -d "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" \
  -d "client_assertion=$JWT_SVID" \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token"
```

### Required Changes
✅ **DONE** - The go-processor code already uses the correct assertion type (AuthBridge/AuthProxy/go-processor/main.go:363).

**Action Required**: None - already implemented correctly.

## 5. SPIFFE Identity Provider Not Created in Correct Realm

### Issue
The SPIFFE IdP setup job was initially configured with `KEYCLOAK_REALM: demo` instead of `KEYCLOAK_REALM: kagenti`, causing the identity provider to be created in the wrong realm.

### Root Cause
The job manifest had a hardcoded default that didn't match the actual realm being used.

### Fix Applied
Recreated the job with correct configuration:
```yaml
- name: KEYCLOAK_REALM
  value: "kagenti"  # Fixed from "demo"
```

### Required Changes
❓ **TO VERIFY** - Check if the Ansible/Helm chart correctly sets KEYCLOAK_REALM for the spiffe-idp-setup job.

**Action Required**:
1. Verify the job template in kagenti repo uses the correct realm from values
2. Ensure it reads from `{{ .Values.keycloak.realm }}` not a hardcoded default

## Summary of Required Code Changes

### 1. kagenti-extensions Repository (kagenti-webhook)

**File**: `kagenti-webhook/internal/webhook/injector/container_builder.go`

**Current Issue**: Reads CLIENT_AUTH_TYPE from `kagenti-webhook-config` ConfigMap (line 182).

**Status**: ✅ **CORRECT** - This aligns with the new ConfigMap structure in the kagenti chart.

### 2. kagenti Repository (Helm Charts)

**File**: `charts/kagenti/templates/agent-namespaces.yaml`

**Changes on current branch (spiffe_keycloak2)**:
- ✅ Added `kagenti-webhook-config` ConfigMap with CLIENT_AUTH_TYPE, SPIFFE_IDP_ALIAS (commit 62108e73)
- ✅ Added keycloak-admin-secret for client-registration
- ✅ Added SPIRE_ENABLED, KEYCLOAK_URL, KEYCLOAK_REALM to environments ConfigMap

**Missing**:
- ❌ RBAC Role/RoleBinding for keycloak namespace access

**File**: `charts/kagenti/templates/spiffe-idp-job.yaml` (or wherever the job is defined)

**To Verify**:
- ❓ Ensure KEYCLOAK_REALM uses `{{ .Values.keycloak.realm }}` not hardcoded default

**File**: `charts/kagenti-deps/templates/keycloak-k8s.yaml`

**Changes made**:
- ✅ Bumped Keycloak to 26.5.2 (commit 5f86dbc1)
- ✅ Added KC_FEATURES: "client-auth-federated:v1,spiffe:v1"

### 3. Documentation Updates

**File**: `kagenti-extensions/LOCAL_TESTING_GUIDE.md`

**Required Updates**:
- ❌ Emphasize that `./local-build-and-test.sh` MUST be run (not individual docker build commands)
- ❌ Document that it builds images from BOTH kagenti and kagenti-extensions repos
- ❌ Add troubleshooting section for ErrImageNeverPull

## Testing Verification

### Successful End-to-End Flow

1. ✅ Keycloak 26.5.2 running with KC_FEATURES enabled
2. ✅ SPIFFE Identity Provider created in kagenti realm:
   - Alias: `spire-spiffe`
   - Trust domain: `spiffe://localtest.me`
   - Bundle endpoint: `http://spire-spiffe-oidc-discovery-provider.zero-trust-workload-identity-manager.svc.cluster.local/keys`
3. ✅ Client created with proper federated-jwt configuration:
   - `clientAuthenticatorType: "federated-jwt"`
   - `attributes.jwt.credential.issuer: "spire-spiffe"`
   - `attributes.jwt.credential.sub: "spiffe://localtest.me/ns/agent1/sa/default"`
4. ✅ Token exchange working:
   - JWT-SVID → Keycloak access token
   - Access token includes SPIFFE ID as client_id and azp claims

### Working Token Exchange Command

```bash
JWT=$(cat /opt/jwt_svid.token)
curl -s -X POST \
    -d "grant_type=client_credentials" \
    -d "client_id=spiffe://localtest.me/ns/agent1/sa/default" \
    -d "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" \
    -d "client_assertion=$JWT" \
    "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" | jq .
```

## Next Steps

1. **Merge current branch changes** - The spiffe_keycloak2 branch has critical fixes that need to be merged to main
2. **Add RBAC resources** - Create Role/RoleBinding for spiffe-idp-setup job to access keycloak namespace
3. **Update documentation** - Emphasize using local-build-and-test.sh in LOCAL_TESTING_GUIDE.md
4. **Verify job configuration** - Ensure spiffe-idp-setup job uses correct realm from values
5. **Test clean installation** - Run full installation from scratch with federated-jwt values to verify all components work together
