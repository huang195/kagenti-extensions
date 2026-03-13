"""
E2E test fixtures for kagenti-extensions Kind cluster tests.

Requires:
- KUBECONFIG pointing to a Kind cluster with the kagenti-webhook deployed
- cert-manager installed (for TLS certificate injection)

Environment variables:
- WEBHOOK_NAMESPACE: Namespace where kagenti-webhook is deployed (default: kagenti-webhook-system)
- TEST_NAMESPACE: Namespace to use for injection tests (default: kagenti-webhook-test)
"""

import os
import time

import pytest
from kubernetes import client, config
from kubernetes.client.rest import ApiException


def _load_k8s_config():
    """Load Kubernetes config from kubeconfig or in-cluster."""
    try:
        config.load_kube_config()
    except config.ConfigException:
        try:
            config.load_incluster_config()
        except config.ConfigException as e:
            pytest.skip(f"Could not load Kubernetes config: {e}")


@pytest.fixture(scope="session")
def k8s_client():
    """CoreV1Api client."""
    _load_k8s_config()
    return client.CoreV1Api()


@pytest.fixture(scope="session")
def k8s_apps_client():
    """AppsV1Api client."""
    _load_k8s_config()
    return client.AppsV1Api()


@pytest.fixture(scope="session")
def k8s_admission_client():
    """AdmissionregistrationV1Api client for webhook configuration."""
    _load_k8s_config()
    return client.AdmissionregistrationV1Api()


@pytest.fixture(scope="session")
def webhook_namespace():
    """Namespace where the kagenti-webhook is deployed."""
    return os.getenv("WEBHOOK_NAMESPACE", "kagenti-webhook-system")


@pytest.fixture(scope="session")
def test_namespace(k8s_client):
    """
    Create and yield a test namespace for injection tests.

    The namespace is labeled with kagenti-enabled=true so that the Helm-deployed
    webhook namespaceSelector matches it. When using kustomize (make deploy), the
    MutatingWebhookConfiguration has no namespaceSelector so any namespace works.

    Cleaned up after the test session.
    """
    ns_name = os.getenv("TEST_NAMESPACE", "kagenti-webhook-test")

    # Create namespace if it doesn't exist
    try:
        k8s_client.create_namespace(
            client.V1Namespace(
                metadata=client.V1ObjectMeta(
                    name=ns_name,
                    labels={"kagenti-enabled": "true"},
                )
            )
        )
    except ApiException as e:
        if e.status == 409:
            # Already exists - ensure label is set
            k8s_client.patch_namespace(
                name=ns_name,
                body={"metadata": {"labels": {"kagenti-enabled": "true"}}},
            )
        else:
            pytest.fail(f"Failed to create test namespace {ns_name}: {e}")

    yield ns_name

    # Cleanup: delete the namespace and all resources in it
    try:
        k8s_client.delete_namespace(name=ns_name)
    except ApiException:
        pass  # Best effort cleanup


def wait_for_pod_phase(k8s_client, namespace, name, phases, timeout=30):
    """
    Wait until a pod reaches one of the expected phases.

    Used in injection tests to ensure the pod object is fully stored
    before checking its spec.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            pod = k8s_client.read_namespaced_pod(name=name, namespace=namespace)
            if pod.status.phase in phases:
                return pod
        except ApiException:
            pass
        time.sleep(1)
    return k8s_client.read_namespaced_pod(name=name, namespace=namespace)
