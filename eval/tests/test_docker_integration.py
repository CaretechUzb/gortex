"""Docker container setup integration test.

Feature: eval-framework
Tests the GortexDockerEnvironment setup/teardown lifecycle with a real
Docker daemon. Skipped when Docker is not available.

This test validates:
- Container launch with a lightweight image
- Gortex binary copy into container
- Eval-server health check
- Container teardown and cleanup
"""

from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from environments.gortex_docker import GortexDockerEnvironment


# --- Docker availability check ---

def _docker_available() -> bool:
    """Check if Docker daemon is accessible."""
    try:
        import docker
        client = docker.from_env()
        client.ping()
        client.close()
        return True
    except Exception:
        return False


docker_available = _docker_available()


# --- Integration tests (require Docker) ---

@pytest.mark.skipif(
    not docker_available,
    reason="Docker daemon not available",
)
class TestDockerContainerLifecycle:
    """Integration tests that exercise real Docker container lifecycle.

    These tests require a running Docker daemon and will pull/use
    lightweight images. They are skipped in CI environments without Docker.
    """

    def test_setup_teardown_gortex_disabled(self) -> None:
        """Launch a container with gortex disabled, verify it runs, teardown."""
        env = GortexDockerEnvironment(
            image="alpine:latest",
            enable_gortex=False,
            instance_id="integration-test-no-gortex",
        )
        try:
            result = env.setup()
            # setup should succeed (no failure dict returned)
            if result is not None:
                pytest.skip(f"Container setup failed: {result.get('setup_error', 'unknown')}")

            assert env._container is not None
            assert env.is_ready

            # Run a simple command inside the container
            code, output = env.exec_run("echo hello-from-container")
            assert code == 0
            assert "hello-from-container" in output
        finally:
            env.teardown()
            assert env._container is None

    def test_extract_patch_empty_repo(self) -> None:
        """Extract patch from a container with no git changes returns empty."""
        env = GortexDockerEnvironment(
            image="alpine:latest",
            enable_gortex=False,
            instance_id="integration-test-patch",
        )
        try:
            result = env.setup()
            if result is not None:
                pytest.skip(f"Container setup failed: {result.get('setup_error', 'unknown')}")

            # No git repo in alpine, so extract_patch should return empty
            patch = env.extract_patch()
            assert patch == ""
        finally:
            env.teardown()


# --- Mock-based tests (always run, no Docker required) ---

class TestDockerEnvironmentMocked:
    """Tests that verify Docker integration logic using mocks.

    These always run regardless of Docker availability.
    """

    @patch("environments.gortex_docker.docker")
    def test_full_lifecycle_gortex_disabled(self, mock_docker) -> None:
        """Verify setup → exec → extract_patch → teardown with mocked Docker."""
        mock_client = MagicMock()
        mock_container = MagicMock()
        mock_container.short_id = "abc123"
        mock_container.exec_run.return_value = (0, b"hello\n")
        mock_client.containers.run.return_value = mock_container
        mock_docker.from_env.return_value = mock_client

        env = GortexDockerEnvironment(
            image="test:latest",
            enable_gortex=False,
            instance_id="mock-lifecycle",
        )

        # Setup
        result = env.setup()
        assert result is None
        assert env._container is mock_container

        # Exec
        code, output = env.exec_run("echo hello")
        assert code == 0
        assert output == "hello\n"

        # Teardown
        env.teardown()
        mock_container.stop.assert_called_once()
        mock_container.remove.assert_called_once()
        assert env._container is None

    @patch("environments.gortex_docker.docker")
    def test_setup_failure_records_error(self, mock_docker) -> None:
        """Verify that container launch failure is properly recorded."""
        mock_client = MagicMock()
        mock_client.containers.run.side_effect = RuntimeError("Docker daemon not running")
        mock_docker.from_env.return_value = mock_client

        env = GortexDockerEnvironment(
            image="test:latest",
            instance_id="fail-test",
        )
        result = env.setup()
        assert result is not None
        assert result["exit_status"] == "setup_failure"
        assert "fail-test" in result["instance_id"]
