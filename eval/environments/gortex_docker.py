"""Gortex Docker Environment for SWE-bench Evaluation.

Manages the full container lifecycle for a single eval instance:
  1. Launch container with target repo at specified commit
  2. Copy gortex binary and tool bridge scripts (native/native_augment modes)
  3. Start eval-server inside container, health-check with configurable timeout
  4. Extract patch (git diff) before teardown
  5. Mount/copy cached indexes when available
  6. Record setup failures gracefully — never raise, return failure result

Architecture:
  Agent bash cmd → /usr/local/bin/gortex-search → curl localhost:4747/tool/search_symbols
    → eval-server → in-memory graph
  Fallback: → gortex CLI (cold path)
"""

from __future__ import annotations

import io
import logging
import tarfile
import time
from pathlib import Path
from typing import Any

import docker

logger = logging.getLogger("gortex_docker")

DEFAULT_EVAL_SERVER_PORT = 4747
DEFAULT_GORTEX_TIMEOUT = 120
DEFAULT_CACHE_DIR = Path.home() / ".gortex-eval-cache"
HEALTH_CHECK_INTERVAL = 2.0
CONTAINER_WORKDIR = "/testbed"
GORTEX_BINARY_CONTAINER_PATH = "/usr/local/bin/gortex"
BRIDGE_SCRIPTS_CONTAINER_DIR = "/usr/local/bin"

# Bridge script names (files in eval/bridge/ without __init__.py and __pycache__)
_BRIDGE_SCRIPTS = [
    "gortex-search",
    "gortex-context",
    "gortex-impact",
    "gortex-overview",
    "gortex-usages",
    "gortex-augment",
]


def _make_cache_key(repo_name: str, commit_hash: str) -> str:
    """Build a deterministic cache directory name from repo and commit."""
    safe_repo = repo_name.replace("/", "__")
    return f"{safe_repo}_{commit_hash}"


class GortexDockerEnvironment:
    """Docker environment managing the full container lifecycle for Gortex eval.

    Lifecycle: setup() → (agent runs) → extract_patch() → teardown()

    On any setup failure (container launch, binary copy, health-check timeout),
    the environment records the failure and returns a result dict with
    ``exit_status="setup_failure"`` instead of raising.
    """

    def __init__(
        self,
        *,
        image: str,
        repo_path: str = CONTAINER_WORKDIR,
        enable_gortex: bool = True,
        gortex_binary: str | Path | None = None,
        gortex_timeout: int = DEFAULT_GORTEX_TIMEOUT,
        eval_server_port: int = DEFAULT_EVAL_SERVER_PORT,
        cache_dir: str | Path | None = None,
        instance_id: str = "",
    ) -> None:
        self.image = image
        self.repo_path = repo_path
        self.enable_gortex = enable_gortex
        self.gortex_binary = Path(gortex_binary) if gortex_binary else None
        self.gortex_timeout = gortex_timeout
        self.eval_server_port = eval_server_port
        self.cache_dir = Path(cache_dir) if cache_dir else DEFAULT_CACHE_DIR
        self.instance_id = instance_id

        self._client: docker.DockerClient | None = None
        self._container: Any = None  # docker.models.containers.Container
        self._gortex_ready = False
        self._setup_error: str | None = None
        self.index_time: float = 0.0

    # -- public API ----------------------------------------------------------

    def setup(self) -> dict[str, Any] | None:
        """Launch container, copy gortex + bridge scripts, start eval-server.

        Returns ``None`` on success, or a failure result dict on error.
        The caller should check the return value and skip agent execution
        when a failure dict is returned.
        """
        try:
            self._launch_container()
        except Exception as exc:
            return self._record_failure(f"Container launch failed: {exc}")

        if not self.enable_gortex:
            logger.info("Gortex disabled for this instance, skipping tool setup")
            return None

        try:
            start = time.time()
            self._copy_gortex_binary()
            self._copy_bridge_scripts()
            self._restore_or_skip_cache()
            self._start_eval_server()
            self._wait_for_health()
            self.index_time = time.time() - start
            self._gortex_ready = True
            logger.info(
                "Gortex environment ready for %s in %.1fs",
                self.instance_id,
                self.index_time,
            )
        except _SetupTimeout as exc:
            return self._record_failure(str(exc))
        except Exception as exc:
            return self._record_failure(f"Gortex setup failed: {exc}")

        return None

    def extract_patch(self) -> str:
        """Extract the agent's patch via ``git diff`` inside the container.

        Returns the diff string, or an empty string on failure.
        """
        if self._container is None:
            return ""
        try:
            exit_code, output = self._container.exec_run(
                ["git", "diff"],
                workdir=self.repo_path,
            )
            if exit_code == 0:
                return output.decode("utf-8", errors="replace")
            logger.warning("git diff exited %d: %s", exit_code, output[:500])
        except Exception as exc:
            logger.warning("Patch extraction failed: %s", exc)
        return ""

    def teardown(self) -> None:
        """Stop and remove the container."""
        if self._container is not None:
            try:
                self._container.stop(timeout=5)
            except Exception:
                pass
            try:
                self._container.remove(force=True)
            except Exception:
                pass
            self._container = None
        if self._client is not None:
            try:
                self._client.close()
            except Exception:
                pass
            self._client = None

    def exec_run(self, cmd: str | list[str], **kwargs: Any) -> tuple[int, str]:
        """Execute a command inside the container.

        Returns (exit_code, output_string).
        """
        if self._container is None:
            return (1, "Container not running")
        if isinstance(cmd, str):
            cmd = ["bash", "-c", cmd]
        exit_code, output = self._container.exec_run(cmd, **kwargs)
        return exit_code, output.decode("utf-8", errors="replace")

    @property
    def is_ready(self) -> bool:
        """Whether the environment is fully set up and gortex is healthy."""
        return self._container is not None and (
            not self.enable_gortex or self._gortex_ready
        )

    @property
    def setup_error(self) -> str | None:
        """Description of setup failure, if any."""
        return self._setup_error

    # -- private helpers -----------------------------------------------------

    def _launch_container(self) -> None:
        """Create and start a Docker container from the configured image."""
        self._client = docker.from_env()
        logger.info("Launching container from image %s", self.image)
        self._container = self._client.containers.run(
            self.image,
            command="sleep infinity",
            detach=True,
            working_dir=self.repo_path,
        )
        logger.info("Container %s started", self._container.short_id)

    def _copy_gortex_binary(self) -> None:
        """Copy the gortex binary into the container."""
        if self.gortex_binary is None:
            logger.info("No gortex binary path specified, assuming pre-installed")
            return
        if not self.gortex_binary.is_file():
            raise FileNotFoundError(f"Gortex binary not found: {self.gortex_binary}")

        logger.info("Copying gortex binary into container")
        self._put_file_in_container(
            self.gortex_binary,
            GORTEX_BINARY_CONTAINER_PATH,
            executable=True,
        )
        # Verify binary is accessible and executable
        exit_code, output = self._container.exec_run(
            ["ls", "-la", GORTEX_BINARY_CONTAINER_PATH]
        )
        if exit_code != 0:
            raise RuntimeError(
                f"Gortex binary not found in container after copy: {output.decode()}"
            )
        # Check if binary can actually run (catches arch mismatch, missing libs)
        exit_code, output = self._container.exec_run(
            [GORTEX_BINARY_CONTAINER_PATH, "--help"]
        )
        if exit_code != 0:
            # Try to get more info
            _, file_output = self._container.exec_run(
                ["file", GORTEX_BINARY_CONTAINER_PATH]
            )
            _, ldd_output = self._container.exec_run(
                ["bash", "-c", f"ldd {GORTEX_BINARY_CONTAINER_PATH} 2>&1 || true"]
            )
            raise RuntimeError(
                f"Gortex binary cannot execute in container.\n"
                f"  file: {file_output.decode().strip()}\n"
                f"  ldd: {ldd_output.decode().strip()}\n"
                f"  error: {output.decode().strip()}"
            )
        logger.info("Gortex binary verified at %s", GORTEX_BINARY_CONTAINER_PATH)

    def _copy_bridge_scripts(self) -> None:
        """Copy tool bridge bash scripts into /usr/local/bin/ in the container."""
        bridge_dir = Path(__file__).resolve().parent.parent / "bridge"
        copied = 0
        for script_name in _BRIDGE_SCRIPTS:
            script_path = bridge_dir / script_name
            if not script_path.is_file():
                logger.warning("Bridge script not found: %s", script_path)
                continue
            self._put_file_in_container(
                script_path,
                f"{BRIDGE_SCRIPTS_CONTAINER_DIR}/{script_name}",
                executable=True,
            )
            copied += 1
        logger.info("Copied %d bridge scripts into container", copied)

    def _restore_or_skip_cache(self) -> None:
        """Mount/copy a cached index into the container if one exists."""
        repo_name, commit_hash = self._get_repo_identity()
        cache_key = _make_cache_key(repo_name, commit_hash)
        cache_path = self.cache_dir / cache_key

        if not cache_path.is_dir():
            logger.info("No cached index for %s, eval-server will index fresh", cache_key)
            return

        tarball = cache_path / "index.tar.gz"
        if not tarball.is_file():
            logger.info("Cache dir exists but no tarball for %s, skipping", cache_key)
            return

        logger.info("Restoring cached index %s into container", cache_key)
        try:
            cache_dest = "/root/.gortex-cache"
            self._container.exec_run(["mkdir", "-p", cache_dest])
            with open(tarball, "rb") as f:
                self._container.put_archive(cache_dest, f.read())
            logger.info("Cached index restored to %s", cache_dest)
        except Exception as exc:
            logger.warning("Cache restore failed, will index fresh: %s", exc)

    def _start_eval_server(self) -> None:
        """Start ``gortex eval-server`` as a background process in the container."""
        cmd = (
            f"nohup /usr/local/bin/gortex eval-server "
            f"--port {self.eval_server_port} "
            f"--index {self.repo_path} "
            f"> /tmp/gortex-eval-server.log 2>&1 &"
        )
        logger.info("Starting eval-server on port %d", self.eval_server_port)
        self._container.exec_run(["bash", "-c", cmd], detach=True)

    def _wait_for_health(self) -> None:
        """Poll the eval-server health endpoint until ready or timeout.

        Raises ``_SetupTimeout`` if the server doesn't become healthy
        within ``self.gortex_timeout`` seconds.
        """
        deadline = time.time() + self.gortex_timeout
        health_url = f"http://127.0.0.1:{self.eval_server_port}/health"
        attempt = 0

        while time.time() < deadline:
            time.sleep(HEALTH_CHECK_INTERVAL)
            attempt += 1
            try:
                exit_code, output = self._container.exec_run(
                    ["curl", "-sf", health_url],
                )
                if exit_code == 0 and b'"status"' in output and b'"ok"' in output:
                    elapsed = self.gortex_timeout - (deadline - time.time())
                    logger.info(
                        "Eval-server healthy after %d attempts (%.1fs)",
                        attempt,
                        elapsed,
                    )
                    return
            except Exception:
                pass

        # Grab server logs for diagnostics
        try:
            _, log_output = self._container.exec_run(
                ["tail", "-30", "/tmp/gortex-eval-server.log"],
            )
            log_tail = log_output.decode("utf-8", errors="replace")[-1000:]
        except Exception:
            log_tail = "(unavailable)"

        raise _SetupTimeout(
            f"Eval-server health check timed out after {self.gortex_timeout}s "
            f"for instance {self.instance_id}. Server log tail:\n{log_tail}"
        )

    def _get_repo_identity(self) -> tuple[str, str]:
        """Extract (repo_name, commit_hash) from the container's /testbed repo."""
        _, repo_out = self._container.exec_run(
            ["bash", "-c", "basename $(git remote get-url origin 2>/dev/null || basename $(pwd)) .git"],
            workdir=self.repo_path,
        )
        _, commit_out = self._container.exec_run(
            ["bash", "-c", "git rev-parse HEAD 2>/dev/null || echo unknown"],
            workdir=self.repo_path,
        )
        repo_name = repo_out.decode("utf-8", errors="replace").strip() or "unknown"
        commit_hash = commit_out.decode("utf-8", errors="replace").strip() or "unknown"
        return repo_name, commit_hash

    def _put_file_in_container(
        self,
        local_path: Path,
        container_path: str,
        *,
        executable: bool = False,
    ) -> None:
        """Copy a local file into the container using the Docker API.

        Falls back to ``podman cp`` / ``docker cp`` if the API method fails.
        """
        dest_dir = str(Path(container_path).parent)

        # Try Docker SDK put_archive first
        try:
            data = local_path.read_bytes()
            tar_stream = io.BytesIO()
            with tarfile.open(fileobj=tar_stream, mode="w") as tar:
                info = tarfile.TarInfo(name=Path(container_path).name)
                info.size = len(data)
                if executable:
                    info.mode = 0o755
                tar.addfile(info, io.BytesIO(data))
            tar_stream.seek(0)
            self._container.put_archive(dest_dir, tar_stream)

            # Verify the file actually landed
            exit_code, _ = self._container.exec_run(["test", "-f", container_path])
            if exit_code == 0:
                if executable:
                    self._container.exec_run(["chmod", "+x", container_path])
                return
            logger.warning("put_archive succeeded but file not found, trying cp fallback")
        except Exception as exc:
            logger.warning("put_archive failed (%s), trying cp fallback", exc)

        # Fallback: use podman/docker cp via subprocess
        import subprocess
        container_id = self._container.short_id
        try:
            subprocess.run(
                ["podman", "cp", str(local_path), f"{container_id}:{container_path}"],
                check=True, capture_output=True, timeout=30,
            )
        except (subprocess.CalledProcessError, FileNotFoundError):
            # Try docker cp as last resort
            subprocess.run(
                ["docker", "cp", str(local_path), f"{container_id}:{container_path}"],
                check=True, capture_output=True, timeout=30,
            )
        if executable:
            self._container.exec_run(["chmod", "+x", container_path])

    def _record_failure(self, message: str) -> dict[str, Any]:
        """Record a setup failure and return a result dict for the runner."""
        logger.error("Setup failure for %s: %s", self.instance_id, message)
        self._setup_error = message
        return {
            "instance_id": self.instance_id,
            "exit_status": "setup_failure",
            "setup_error": message,
            "submission": "",
        }


class _SetupTimeout(Exception):
    """Raised internally when the eval-server health check times out."""
