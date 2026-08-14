"""Unit tests for eval/environments/gortex_docker.py.

Tests focus on pure logic (failure recording, properties) and mock
Docker interactions to avoid requiring a running Docker daemon.
"""

from __future__ import annotations

import io
import tarfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from environments.gortex_docker import (
    DEFAULT_EVAL_SERVER_PORT,
    DEFAULT_GORTEX_TIMEOUT,
    GortexDockerEnvironment,
)


# -- GortexDockerEnvironment init -------------------------------------------

class TestInit:
    def test_defaults(self):
        env = GortexDockerEnvironment(image="test:latest")
        assert env.image == "test:latest"
        assert env.enable_gortex is True
        assert env.gortex_timeout == DEFAULT_GORTEX_TIMEOUT
        assert env.eval_server_port == DEFAULT_EVAL_SERVER_PORT
        assert env._container is None
        assert env._gortex_ready is False

    def test_gortex_disabled(self):
        env = GortexDockerEnvironment(image="test:latest", enable_gortex=False)
        assert env.enable_gortex is False

    def test_custom_params(self):
        env = GortexDockerEnvironment(
            image="swe:v1",
            gortex_binary="/tmp/gortex",
            gortex_timeout=60,
            eval_server_port=9999,
            instance_id="django__django-1234",
        )
        assert env.gortex_binary == Path("/tmp/gortex")
        assert env.gortex_timeout == 60
        assert env.eval_server_port == 9999
        assert env.instance_id == "django__django-1234"


# -- setup / failure recording -----------------------------------------------

class TestRecordFailure:
    def test_returns_failure_dict(self):
        env = GortexDockerEnvironment(image="test:latest", instance_id="test-123")
        result = env._record_failure("something broke")
        assert result["exit_status"] == "setup_failure"
        assert result["instance_id"] == "test-123"
        assert "something broke" in result["setup_error"]
        assert result["submission"] == ""

    def test_sets_setup_error(self):
        env = GortexDockerEnvironment(image="test:latest")
        env._record_failure("timeout")
        assert env.setup_error == "timeout"


# -- is_ready property -------------------------------------------------------

class TestIsReady:
    def test_not_ready_no_container(self):
        env = GortexDockerEnvironment(image="test:latest")
        assert env.is_ready is False

    def test_ready_when_gortex_disabled(self):
        env = GortexDockerEnvironment(image="test:latest", enable_gortex=False)
        env._container = MagicMock()  # simulate running container
        assert env.is_ready is True

    def test_not_ready_gortex_enabled_but_not_setup(self):
        env = GortexDockerEnvironment(image="test:latest", enable_gortex=True)
        env._container = MagicMock()
        assert env.is_ready is False

    def test_ready_gortex_enabled_and_setup(self):
        env = GortexDockerEnvironment(image="test:latest", enable_gortex=True)
        env._container = MagicMock()
        env._gortex_ready = True
        assert env.is_ready is True


# -- extract_patch -----------------------------------------------------------

class TestExtractPatch:
    def test_no_container(self):
        env = GortexDockerEnvironment(image="test:latest")
        assert env.extract_patch() == ""

    def test_successful_diff(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_container.exec_run.return_value = (0, b"diff --git a/foo.py b/foo.py\n+hello\n")
        env._container = mock_container
        patch = env.extract_patch()
        assert "diff --git" in patch
        assert "+hello" in patch

    def test_failed_diff(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_container.exec_run.return_value = (1, b"error")
        env._container = mock_container
        assert env.extract_patch() == ""

    def test_exception_returns_empty(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_container.exec_run.side_effect = RuntimeError("boom")
        env._container = mock_container
        assert env.extract_patch() == ""


# -- teardown ----------------------------------------------------------------

class TestTeardown:
    def test_teardown_stops_and_removes(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_client = MagicMock()
        env._container = mock_container
        env._client = mock_client
        env.teardown()
        mock_container.stop.assert_called_once()
        mock_container.remove.assert_called_once()
        mock_client.close.assert_called_once()
        assert env._container is None
        assert env._client is None

    def test_teardown_no_container(self):
        env = GortexDockerEnvironment(image="test:latest")
        env.teardown()  # should not raise


# -- exec_run ----------------------------------------------------------------

class TestExecRun:
    def test_no_container(self):
        env = GortexDockerEnvironment(image="test:latest")
        code, output = env.exec_run("echo hello")
        assert code == 1
        assert "not running" in output.lower()

    def test_string_command_wrapped_in_bash(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_container.exec_run.return_value = (0, b"hello\n")
        env._container = mock_container
        code, output = env.exec_run("echo hello")
        assert code == 0
        assert output == "hello\n"
        # Verify it was wrapped in bash -c
        call_args = mock_container.exec_run.call_args
        assert call_args[0][0] == ["bash", "-c", "echo hello"]

    def test_list_command_passed_directly(self):
        env = GortexDockerEnvironment(image="test:latest")
        mock_container = MagicMock()
        mock_container.exec_run.return_value = (0, b"ok")
        env._container = mock_container
        code, output = env.exec_run(["ls", "-la"])
        assert code == 0
        call_args = mock_container.exec_run.call_args
        assert call_args[0][0] == ["ls", "-la"]


# -- setup with gortex disabled ---------------------------------------------

class TestSetupGortexDisabled:
    @patch("environments.gortex_docker.docker")
    def test_setup_skips_gortex(self, mock_docker):
        mock_client = MagicMock()
        mock_container = MagicMock()
        mock_container.short_id = "abc123"
        mock_client.containers.run.return_value = mock_container
        mock_docker.from_env.return_value = mock_client

        env = GortexDockerEnvironment(image="test:latest", enable_gortex=False)
        result = env.setup()
        assert result is None
        assert env._container is mock_container


# -- setup container launch failure ------------------------------------------

class TestSetupContainerFailure:
    @patch("environments.gortex_docker.docker")
    def test_container_launch_failure(self, mock_docker):
        mock_client = MagicMock()
        mock_client.containers.run.side_effect = RuntimeError("Docker not running")
        mock_docker.from_env.return_value = mock_client

        env = GortexDockerEnvironment(
            image="test:latest",
            instance_id="fail-instance",
        )
        result = env.setup()
        assert result is not None
        assert result["exit_status"] == "setup_failure"
        assert "fail-instance" in result["instance_id"]
