"""Tests for AuthBridge/client-registration/client_registration.py.

The module has top-level executable code, so we import individual functions
by loading the module source without executing it as __main__.
"""

import importlib
import os
import sys
import types
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Helper: load the module's functions without running top-level code
# ---------------------------------------------------------------------------

MODULE_PATH = (
    Path(__file__).resolve().parents[1]
    / "AuthBridge"
    / "client-registration"
    / "client_registration.py"
)


def _load_functions():
    """Import individual functions from client_registration.py.

    We read the source, compile it, and exec only the function definitions
    so that the module-level side effects (env var reads, Keycloak calls)
    are skipped.
    """
    source = MODULE_PATH.read_text()

    # Build a module object with the required globals
    mod = types.ModuleType("client_registration")
    mod.__file__ = str(MODULE_PATH)

    # Provide the imports the functions need
    import jwt
    from keycloak import KeycloakAdmin, KeycloakPostError

    mod.os = os
    mod.jwt = jwt
    mod.KeycloakAdmin = KeycloakAdmin
    mod.KeycloakPostError = KeycloakPostError

    # Compile and exec only function/class defs + imports
    code = compile(source, str(MODULE_PATH), "exec")
    # We exec the full code but in a controlled namespace; the module-level
    # statements will fail because env vars are missing.  We catch that and
    # still get the function definitions.
    try:
        exec(code, mod.__dict__)
    except (ValueError, Exception):
        pass  # Top-level code fails — that's expected

    return mod


_mod = _load_functions()
get_env_var = _mod.get_env_var
write_client_secret = _mod.write_client_secret
register_client = _mod.register_client
get_or_create_audience_scope = _mod.get_or_create_audience_scope
add_scope_to_platform_clients = _mod.add_scope_to_platform_clients


# ---------------------------------------------------------------------------
# Tests: get_env_var
# ---------------------------------------------------------------------------


class TestGetEnvVar:
    def test_returns_value_when_set(self, monkeypatch):
        monkeypatch.setenv("TEST_VAR_XYZ", "hello")
        assert get_env_var("TEST_VAR_XYZ") == "hello"

    def test_returns_default_when_missing(self):
        assert get_env_var("NONEXISTENT_VAR_12345", "fallback") == "fallback"

    def test_raises_when_missing_no_default(self):
        with pytest.raises(ValueError, match="Missing required environment variable"):
            get_env_var("NONEXISTENT_VAR_12345")

    def test_empty_string_uses_default(self, monkeypatch):
        monkeypatch.setenv("TEST_EMPTY_VAR", "")
        assert get_env_var("TEST_EMPTY_VAR", "default") == "default"

    def test_empty_string_raises_without_default(self, monkeypatch):
        monkeypatch.setenv("TEST_EMPTY_VAR", "")
        with pytest.raises(ValueError):
            get_env_var("TEST_EMPTY_VAR")


# ---------------------------------------------------------------------------
# Tests: write_client_secret
# ---------------------------------------------------------------------------


class TestWriteClientSecret:
    def test_writes_secret_to_file(self, mock_keycloak_admin, tmp_secret_file):
        mock_keycloak_admin.get_client_secrets.return_value = {"value": "s3cret"}

        write_client_secret(mock_keycloak_admin, "internal-id", "my-client", tmp_secret_file)

        assert Path(tmp_secret_file).read_text() == "s3cret"
        mock_keycloak_admin.get_client_secrets.assert_called_once_with("internal-id")

    def test_handles_keycloak_error(self, mock_keycloak_admin, tmp_secret_file):
        from keycloak import KeycloakPostError

        mock_keycloak_admin.get_client_secrets.side_effect = KeycloakPostError(
            error_message="not found", response_code=404
        )

        # Should not raise, just print error
        write_client_secret(mock_keycloak_admin, "internal-id", "my-client", tmp_secret_file)

        # File should not be created
        assert not Path(tmp_secret_file).exists()

    def test_handles_file_write_error(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_secrets.return_value = {"value": "s3cret"}

        # Writing to a non-existent directory should fail gracefully
        write_client_secret(
            mock_keycloak_admin, "internal-id", "my-client", "/nonexistent/dir/secret.txt"
        )


# ---------------------------------------------------------------------------
# Tests: register_client
# ---------------------------------------------------------------------------


class TestRegisterClient:
    def test_returns_existing_client(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_id.return_value = "existing-uuid"

        result = register_client(mock_keycloak_admin, "my-client", {"clientId": "my-client"})

        assert result == "existing-uuid"
        mock_keycloak_admin.create_client.assert_not_called()

    def test_creates_new_client(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_id.return_value = None
        mock_keycloak_admin.create_client.return_value = "new-uuid"

        result = register_client(mock_keycloak_admin, "my-client", {"clientId": "my-client"})

        assert result == "new-uuid"
        mock_keycloak_admin.create_client.assert_called_once()

    def test_raises_on_create_failure(self, mock_keycloak_admin):
        from keycloak import KeycloakPostError

        mock_keycloak_admin.get_client_id.return_value = None
        mock_keycloak_admin.create_client.side_effect = KeycloakPostError(
            error_message="conflict", response_code=409
        )

        with pytest.raises(KeycloakPostError):
            register_client(mock_keycloak_admin, "my-client", {"clientId": "my-client"})


# ---------------------------------------------------------------------------
# Tests: get_or_create_audience_scope
# ---------------------------------------------------------------------------


class TestGetOrCreateAudienceScope:
    def test_returns_existing_scope(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_scopes.return_value = [
            {"name": "agent-test-aud", "id": "scope-123"}
        ]

        result = get_or_create_audience_scope(mock_keycloak_admin, "agent-test-aud", "my-audience")

        assert result == "scope-123"
        mock_keycloak_admin.create_client_scope.assert_not_called()

    def test_creates_new_scope(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_scopes.return_value = []
        mock_keycloak_admin.create_client_scope.return_value = "new-scope-id"

        result = get_or_create_audience_scope(mock_keycloak_admin, "agent-test-aud", "my-audience")

        assert result == "new-scope-id"
        mock_keycloak_admin.create_client_scope.assert_called_once()
        mock_keycloak_admin.add_mapper_to_client_scope.assert_called_once()

    def test_returns_none_on_create_failure(self, mock_keycloak_admin):
        from keycloak import KeycloakPostError

        mock_keycloak_admin.get_client_scopes.return_value = []
        mock_keycloak_admin.create_client_scope.side_effect = KeycloakPostError(
            error_message="error", response_code=500
        )

        result = get_or_create_audience_scope(mock_keycloak_admin, "agent-test-aud", "my-audience")

        assert result is None


# ---------------------------------------------------------------------------
# Tests: add_scope_to_platform_clients
# ---------------------------------------------------------------------------


class TestAddScopeToPlatformClients:
    def test_adds_scope_to_existing_client(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_id.return_value = "platform-uuid"

        add_scope_to_platform_clients(
            mock_keycloak_admin, "scope-123", "agent-test-aud", ["kagenti"]
        )

        mock_keycloak_admin.add_client_default_client_scope.assert_called_once_with(
            "platform-uuid", "scope-123", {}
        )

    def test_skips_missing_platform_client(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_id.return_value = None

        add_scope_to_platform_clients(
            mock_keycloak_admin, "scope-123", "agent-test-aud", ["nonexistent"]
        )

        mock_keycloak_admin.add_client_default_client_scope.assert_not_called()

    def test_handles_409_conflict_gracefully(self, mock_keycloak_admin):
        mock_keycloak_admin.get_client_id.return_value = "platform-uuid"
        mock_keycloak_admin.add_client_default_client_scope.side_effect = Exception(
            "409 Conflict"
        )

        # Should not raise
        add_scope_to_platform_clients(
            mock_keycloak_admin, "scope-123", "agent-test-aud", ["kagenti"]
        )
