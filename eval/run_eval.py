"""Eval runner CLI for Gortex SWE-bench evaluation.

Orchestrates evaluation runs across models and modes:
- ``single`` — run one (model, mode) configuration
- ``matrix`` — run full cross-product of models × modes
- ``debug`` — run a single instance with verbose logging
- ``list-configs`` — show available model/mode configs

Entry point: ``main()`` (referenced in pyproject.toml as ``gortex-eval``).

Heavy lifting lives elsewhere:
- Config: ``eval.config`` (load, merge, validate YAML configs)
- Environment: ``eval.environments.gortex_docker`` (container lifecycle)
- Agent: ``eval.agents.gortex_agent`` (LLM agent wrapper)
- Metrics persistence: ``eval.results`` (task 15, not yet implemented)
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import logging
import sys
import time
from itertools import product
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger("gortex_eval")

# ---------------------------------------------------------------------------
# Dataset name mapping
# ---------------------------------------------------------------------------

DATASET_MAPPING: Dict[str, str] = {
    "lite": "princeton-nlp/SWE-bench_Lite",
    "verified": "princeton-nlp/SWE-bench_Verified",
    "full": "princeton-nlp/SWE-bench",
}

DEFAULT_OUTPUT_DIR = Path("results")
DEFAULT_SUBSET = "lite"
DEFAULT_SPLIT = "test"


# ---------------------------------------------------------------------------
# Pure helper functions (exported for testing)
# ---------------------------------------------------------------------------

def parse_slice(spec: str) -> slice:
    """Parse a slice spec string into a Python ``slice`` object.

    Supports the same semantics as Python's ``list[start:end]``:
    - ``"0:5"``  → ``slice(0, 5)``
    - ``"10:20"`` → ``slice(10, 20)``
    - ``":3"``   → ``slice(None, 3)``
    - ``"5:"``   → ``slice(5, None)``
    - ``"::2"``  → ``slice(None, None, 2)``
    - ``""``     → ``slice(None)`` (no-op, selects everything)

    Raises ``ValueError`` for malformed specs.
    """
    spec = spec.strip()
    if not spec:
        return slice(None)

    parts = spec.split(":")
    if len(parts) > 3:
        raise ValueError(f"Invalid slice spec: {spec!r} (too many colons)")

    def _parse_part(s: str) -> Optional[int]:
        s = s.strip()
        if not s:
            return None
        try:
            return int(s)
        except ValueError:
            raise ValueError(f"Invalid slice component: {s!r} in {spec!r}")

    parsed = [_parse_part(p) for p in parts]

    if len(parsed) == 1:
        # Single value like "5" — treat as "0:5" for convenience
        return slice(None, parsed[0])
    elif len(parsed) == 2:
        return slice(parsed[0], parsed[1])
    else:
        return slice(parsed[0], parsed[1], parsed[2])


def generate_run_id(model_name: str, mode_name: str, timestamp: Optional[float] = None) -> str:
    """Generate a unique run ID from model name, mode, and timestamp.

    Format: ``{model}_{mode}_{timestamp}``
    """
    ts = int(timestamp or time.time())
    return f"{model_name}_{mode_name}_{ts}"


def build_matrix_configs(
    models: List[str], modes: List[str]
) -> List[Tuple[str, str]]:
    """Build the cross-product of (model, mode) pairs.

    Returns a list of unique ``(model_name, mode_name)`` tuples.
    """
    return list(product(models, modes))


# ---------------------------------------------------------------------------
# Instance loading
# ---------------------------------------------------------------------------

def load_instances(
    subset: str = DEFAULT_SUBSET,
    split: str = DEFAULT_SPLIT,
    slice_spec: str = "",
    filter_spec: str = "",
) -> List[Dict[str, Any]]:
    """Load SWE-bench instances from HuggingFace datasets.

    Parameters
    ----------
    subset:
        Dataset subset name (``lite``, ``verified``, ``full``) or a full
        HuggingFace dataset path.
    split:
        Dataset split (e.g., ``test``, ``dev``).
    slice_spec:
        Optional slice spec string (e.g., ``"0:5"``) applied after loading.
    filter_spec:
        Optional regex to filter instance IDs.

    Returns
    -------
    list[dict]
        List of SWE-bench instance dicts.
    """
    try:
        from datasets import load_dataset
    except ImportError:
        logger.error(
            "The 'datasets' package is required for loading SWE-bench instances. "
            "Install it with: pip install datasets"
        )
        sys.exit(1)

    import re

    dataset_path = DATASET_MAPPING.get(subset, subset)
    logger.info("Loading dataset: %s, split: %s", dataset_path, split)
    instances = list(load_dataset(dataset_path, split=split))

    if filter_spec:
        pattern = re.compile(filter_spec)
        instances = [i for i in instances if pattern.match(i["instance_id"])]

    if slice_spec:
        sl = parse_slice(slice_spec)
        instances = instances[sl]

    logger.info("Loaded %d instances", len(instances))
    return instances


# ---------------------------------------------------------------------------
# Run orchestration
# ---------------------------------------------------------------------------

def _build_config(model_name: str, mode_name: str) -> Dict[str, Any]:
    """Load and merge model + mode configs, validate the result."""
    from config import load_model_config, load_mode_config, merge_configs, validate_config

    model_cfg = load_model_config(model_name)
    mode_cfg = load_mode_config(mode_name)
    merged = merge_configs(model_cfg, mode_cfg)
    validate_config(merged)
    return merged


def _get_instance_image(instance: Dict[str, Any], env_cfg: Dict[str, Any]) -> str:
    """Derive the SWE-bench Docker image name for an instance.

    SWE-bench v4 names images as:
      sweb.eval.x86_64.{instance_id}:{tag}
    Falls back to the instance's environment_image_key if present.
    """
    tag = env_cfg.get("swebench_image_tag", "sweb.eval.x86_64")
    instance_id = instance["instance_id"]
    # Check if instance has an explicit image key
    image_key = instance.get("environment_image_key", "")
    if image_key and not image_key.startswith("swebench/"):
        return image_key
    # SWE-bench v4 convention
    return f"sweb.eval.x86_64.{instance_id}:{tag}"


def process_instance(
    instance: Dict[str, Any],
    config: Dict[str, Any],
    output_dir: Path,
    run_id: str,
    model_name: str,
    mode_name: str,
) -> Dict[str, Any]:
    """Process a single SWE-bench instance.

    Lifecycle: launch container → setup → drive agent → extract patch →
    collect metrics → teardown.

    On non-recoverable error: log, record as failed, return result dict.
    """
    instance_id = instance["instance_id"]
    instance_dir = output_dir / run_id / instance_id
    instance_dir.mkdir(parents=True, exist_ok=True)

    agent_cfg = config.get("agent", {})
    cost_limit = float(agent_cfg.get("cost_limit", 3.0))
    step_limit = int(agent_cfg.get("step_limit", 30))

    result: Dict[str, Any] = {
        "instance_id": instance_id,
        "model": model_name,
        "mode": mode_name,
        "exit_status": None,
        "submission": "",
        "cost": 0.0,
        "tokens_input": 0,
        "tokens_output": 0,
        "n_calls": 0,
        "n_steps": 0,
        "duration_seconds": 0.0,
        "gortex_metrics": {},
    }

    env = None
    agent = None
    start_time = time.time()

    try:
        # --- environment setup ---
        from environments.gortex_docker import GortexDockerEnvironment

        env_cfg = config.get("environment", {})
        instance_image = _get_instance_image(instance, env_cfg)
        env = GortexDockerEnvironment(
            image=instance_image,
            enable_gortex=env_cfg.get("enable_gortex", True),
            gortex_binary=env_cfg.get("gortex_binary"),
            gortex_timeout=int(env_cfg.get("gortex_timeout", 120)),
            eval_server_port=int(env_cfg.get("eval_server_port", 4747)),
            instance_id=instance_id,
        )
        env.setup()

        # --- agent setup ---
        from agents.gortex_agent import GortexAgent

        model_cfg = config.get("model", {})
        model_name_full = model_cfg.get("model_name", model_name)

        # Create mini-swe-agent Model and Environment
        try:
            from minisweagent.models import get_model
            from minisweagent.environments.docker import DockerEnvironment

            mswe_model = get_model(model_name_full)

            # Log API key source for debugging
            import os
            api_key = os.environ.get("ANTHROPIC_API_KEY", "")
            if api_key:
                logger.info("ANTHROPIC_API_KEY from env: %s...%s", api_key[:8], api_key[-4:])
            else:
                logger.warning("ANTHROPIC_API_KEY not set in environment")
            # Check mini-swe-agent's config file
            mswe_env_path = Path.home() / "Library" / "Application Support" / "mini-swe-agent" / ".env"
            if mswe_env_path.exists():
                for line in mswe_env_path.read_text().splitlines():
                    if "ANTHROPIC_API_KEY" in line and not line.strip().startswith("#"):
                        val = line.split("=", 1)[-1].strip().strip("'\"")
                        logger.info("mini-swe-agent .env has ANTHROPIC_API_KEY: %s...%s", val[:8], val[-4:])
                        if val != api_key:
                            logger.warning("mini-swe-agent .env key DIFFERS from shell env — this key will be used")

            # Build run_args to mount gortex tools into mini-swe-agent's container
            run_args = ["--rm"]
            gortex_mode = agent_cfg.get("gortex_mode", "baseline")
            if gortex_mode in ("native", "native_augment"):
                gortex_binary = env_cfg.get("gortex_binary")
                if gortex_binary and Path(gortex_binary).is_file():
                    abs_binary = str(Path(gortex_binary).resolve())
                    run_args.append(f"-v={abs_binary}:/usr/local/bin/gortex:ro")

                bridge_dir = Path(__file__).resolve().parent / "bridge"
                if bridge_dir.is_dir():
                    for script in bridge_dir.iterdir():
                        if script.is_file() and not script.name.startswith("__") and not script.name.endswith(".py"):
                            run_args.append(f"-v={script.resolve()}:/usr/local/bin/{script.name}:ro")

            mswe_env = DockerEnvironment(
                image=instance_image,
                executable=env_cfg.get("container_executable", "docker"),
                cwd="/testbed",
                run_args=run_args,
            )

            # For native/native_augment: start eval-server inside mini-swe-agent's container
            # after it launches, before the agent starts running
            _start_eval_server_hook = None
            if gortex_mode in ("native", "native_augment") and gortex_binary and Path(gortex_binary).is_file():
                eval_port = int(env_cfg.get("eval_server_port", 4747))
                _start_eval_server_hook = (
                    f"nohup /usr/local/bin/gortex eval-server "
                    f"--port {eval_port} --index /testbed "
                    f"> /tmp/gortex-eval-server.log 2>&1 & "
                    f"for i in $(seq 1 60); do "
                    f"  curl -sf http://127.0.0.1:{eval_port}/health >/dev/null 2>&1 && break; "
                    f"  sleep 2; "
                    f"done"
                )

            # Load prompt templates for mini-swe-agent
            gortex_agent_tmp = GortexAgent.__new__(GortexAgent)
            gortex_agent_tmp.mode = __import__("agents.gortex_agent", fromlist=["GortexMode"]).GortexMode(
                agent_cfg.get("gortex_mode", "baseline")
            )
            gortex_agent_tmp._system_template = None
            gortex_agent_tmp._instance_template = None
            gortex_agent_tmp._load_prompt_templates()

            system_tpl = gortex_agent_tmp.render_system_prompt()
            instance_tpl = gortex_agent_tmp.render_instance_prompt("{{task}}")

            agent = GortexAgent(
                config=config,
                model=mswe_model,
                env=mswe_env,
                system_template=system_tpl,
                instance_template=instance_tpl,
            )
        except ImportError:
            agent = GortexAgent(config=config)
            _start_eval_server_hook = None

        # --- drive agent ---
        logger.info("[%s] Starting instance %s", run_id, instance_id)

        # Start eval-server inside mini-swe-agent's container if in native mode
        if _start_eval_server_hook:
            try:
                logger.info("[%s] Starting eval-server in agent container (indexing may take 1-2 min)", run_id)
                mswe_env.execute(
                    {"command": _start_eval_server_hook},
                    timeout=180,  # 3 min for indexing large repos
                )
                # Verify it's running
                health = mswe_env.execute(
                    {"command": "curl -sf http://127.0.0.1:4747/health 2>/dev/null || echo 'not ready'"},
                    timeout=10,
                )
                logger.info("[%s] Eval-server health: %s", run_id, health.get("output", "")[:200])
            except Exception as srv_exc:
                logger.warning("[%s] Eval-server startup failed: %s", run_id, srv_exc)

        task = instance.get("problem_statement", "")

        # Run agent (the agent respects its own step/cost limits)
        info = agent.run(task)

        # Extract metrics from mini-swe-agent's return value
        if isinstance(info, dict):
            result["exit_status"] = info.get("exit_status", "submitted")
            result["submission"] = info.get("submission", "")
        else:
            result["exit_status"] = "submitted"

        # Extract cost/call metrics from mini-swe-agent's agent internals
        result["cost"] = getattr(agent, "cost", 0.0)
        result["n_calls"] = getattr(agent, "n_calls", 0)
        result["n_steps"] = getattr(agent, "n_steps", getattr(agent, "_step_count", 0))

        # Try to get token counts from model stats
        model_obj = getattr(agent, "model", None)
        if model_obj:
            result["tokens_input"] = getattr(model_obj, "tokens_input", 0)
            result["tokens_output"] = getattr(model_obj, "tokens_output", 0)

        # Overlay with our GortexAgent metrics
        result["gortex_metrics"] = agent.get_metrics()

        # Also try to get patch from mini-swe-agent's serialized state or env
        if not result["submission"]:
            try:
                serialized = agent.serialize()
                result["submission"] = serialized.get("info", {}).get("submission", "")
            except Exception:
                pass
        if not result["submission"]:
            patch = env.extract_patch()
            result["submission"] = patch or ""

    except KeyboardInterrupt:
        raise
    except Exception as exc:
        logger.error("[%s] Instance %s failed: %s", run_id, instance_id, exc)
        result["exit_status"] = "error"
        result["error"] = str(exc)

    finally:
        result["duration_seconds"] = round(time.time() - start_time, 2)

        # --- save per-instance result JSON ---
        try:
            result_file = instance_dir / f"{instance_id}.json"
            result_file.write_text(json.dumps(result, indent=2, default=str))
            logger.info("[%s] Result saved: %s", run_id, result_file)
        except Exception as save_exc:
            logger.warning("[%s] Failed to save result for %s: %s", run_id, instance_id, save_exc)

        # --- save mini-swe-agent trajectory if available ---
        if agent is not None:
            try:
                trajectory = getattr(agent, "serialize", lambda: None)()
                if trajectory:
                    traj_file = instance_dir / f"{instance_id}_trajectory.json"
                    traj_file.write_text(json.dumps(trajectory, indent=2, default=str))
                    logger.info("[%s] Trajectory saved: %s", run_id, traj_file)
            except Exception as traj_exc:
                logger.debug("[%s] No trajectory for %s: %s", run_id, instance_id, traj_exc)

        # --- log summary line for quick scanning ---
        patch_len = len(result.get("submission", ""))
        logger.info(
            "[%s] %s | status=%s | cost=$%.4f | steps=%s | calls=%s | patch=%d chars | %.1fs",
            run_id, instance_id,
            result.get("exit_status", "unknown"),
            result.get("cost", 0),
            result.get("n_steps", 0),
            result.get("n_calls", 0),
            patch_len,
            result.get("duration_seconds", 0),
        )

        # --- teardown ---
        if env is not None:
            try:
                env.teardown()
            except Exception as teardown_exc:
                logger.warning(
                    "[%s] Teardown failed for %s: %s",
                    run_id, instance_id, teardown_exc,
                )

    return result


def run_configuration(
    model_name: str,
    mode_name: str,
    instances: List[Dict[str, Any]],
    output_dir: Path,
    workers: int = 1,
) -> List[Dict[str, Any]]:
    """Run a single (model, mode) configuration across all instances.

    Supports sequential (workers=1) or parallel execution via
    ``concurrent.futures.ThreadPoolExecutor``.
    """
    config = _build_config(model_name, mode_name)
    run_id = generate_run_id(model_name, mode_name)
    run_dir = output_dir / run_id
    run_dir.mkdir(parents=True, exist_ok=True)

    # Set up file logging for this run
    file_handler = None
    try:
        log_file = run_dir / "run.log"
        file_handler = logging.FileHandler(str(log_file))
        file_handler.setLevel(logging.DEBUG)
        file_handler.setFormatter(logging.Formatter(
            "%(asctime)s %(name)s %(levelname)s %(message)s"
        ))
        logging.getLogger().addHandler(file_handler)
        logger.info("[%s] Log file: %s", run_id, log_file)
    except Exception:
        file_handler = None
    logger.info("[%s] Config: %s", run_id, json.dumps(config, indent=2, default=str))

    logger.info(
        "[%s] Running %d instances with %d worker(s)",
        run_id, len(instances), workers,
    )

    results: List[Dict[str, Any]] = []

    if workers <= 1:
        # Sequential execution
        for instance in instances:
            result = process_instance(
                instance, config, output_dir, run_id, model_name, mode_name,
            )
            results.append(result)
    else:
        # Parallel execution
        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
            futures = {
                executor.submit(
                    process_instance,
                    instance, config, output_dir, run_id, model_name, mode_name,
                ): instance["instance_id"]
                for instance in instances
            }
            for future in concurrent.futures.as_completed(futures):
                iid = futures[future]
                try:
                    results.append(future.result())
                except Exception as exc:
                    logger.error("[%s] Uncaught error for %s: %s", run_id, iid, exc)
                    results.append({
                        "instance_id": iid,
                        "model": model_name,
                        "mode": mode_name,
                        "exit_status": "error",
                        "error": str(exc),
                        "submission": "",
                    })

    # Write run summary with per-instance breakdown
    summary = {
        "run_id": run_id,
        "model": model_name,
        "mode": mode_name,
        "total_instances": len(results),
        "completed": sum(
            1 for r in results if r.get("exit_status") not in (None, "error", "setup_failure")
        ),
        "errors": sum(1 for r in results if r.get("exit_status") == "error"),
        "setup_failures": sum(1 for r in results if r.get("exit_status") == "setup_failure"),
        "patches_produced": sum(1 for r in results if r.get("submission")),
        "total_cost": sum(r.get("cost", 0) for r in results),
        "total_duration": sum(r.get("duration_seconds", 0) for r in results),
        "per_instance": [
            {
                "instance_id": r.get("instance_id"),
                "exit_status": r.get("exit_status"),
                "cost": r.get("cost", 0),
                "n_steps": r.get("n_steps", 0),
                "n_calls": r.get("n_calls", 0),
                "duration_seconds": r.get("duration_seconds", 0),
                "has_patch": bool(r.get("submission")),
                "error": r.get("error", ""),
            }
            for r in results
        ],
    }
    try:
        (run_dir / "summary.json").write_text(
            json.dumps(summary, indent=2, default=str)
        )
        logger.info("[%s] Summary saved: %s", run_id, run_dir / "summary.json")
    except Exception as exc:
        logger.warning("Failed to write summary: %s", exc)

    # Remove file handler to avoid accumulation across runs
    if file_handler is not None:
        logging.getLogger().removeHandler(file_handler)
        file_handler.close()

    return results


# ---------------------------------------------------------------------------
# CLI subcommands
# ---------------------------------------------------------------------------

def cmd_single(args: argparse.Namespace) -> None:
    """Handle the ``single`` subcommand."""
    instances = load_instances(
        subset=args.subset,
        split=args.split,
        slice_spec=args.slice or "",
        filter_spec=args.filter or "",
    )

    print(f"\nRunning evaluation: {args.model} + {args.mode}")
    print(f"  Instances: {len(instances)}")
    print(f"  Output: {args.output}\n")

    results = run_configuration(
        args.model, args.mode, instances, Path(args.output), args.workers,
    )

    _print_summary(results, args.model, args.mode)


def cmd_matrix(args: argparse.Namespace) -> None:
    """Handle the ``matrix`` subcommand."""
    instances = load_instances(
        subset=args.subset,
        split=args.split,
        slice_spec=args.slice or "",
        filter_spec=args.filter or "",
    )

    combos = build_matrix_configs(args.models, args.modes)
    print(f"\nMatrix evaluation: {len(args.models)} models x {len(args.modes)} modes = {len(combos)} configs")
    print(f"  Models: {', '.join(args.models)}")
    print(f"  Modes: {', '.join(args.modes)}")
    print(f"  Instances per config: {len(instances)}")
    print(f"  Output: {args.output}\n")

    all_results: Dict[str, List[Dict[str, Any]]] = {}
    for model_name, mode_name in combos:
        run_key = f"{model_name}_{mode_name}"
        print(f"\n━━━ {run_key} ━━━")
        results = run_configuration(
            model_name, mode_name, instances, Path(args.output), args.workers,
        )
        all_results[run_key] = results

    _print_matrix_summary(all_results)


def cmd_debug(args: argparse.Namespace) -> None:
    """Handle the ``debug`` subcommand — single instance with verbose logging."""
    # Enable verbose logging
    logging.basicConfig(level=logging.DEBUG, format="%(asctime)s %(name)s %(levelname)s %(message)s")

    instances = load_instances(
        subset=args.subset,
        split=args.split,
    )

    # Find the target instance
    target = None
    for inst in instances:
        if inst["instance_id"] == args.instance_id:
            target = inst
            break

    if target is None:
        print(f"Error: instance '{args.instance_id}' not found in {args.subset}/{args.split}")
        sys.exit(1)

    print(f"\nDebug run: {args.model} + {args.mode}")
    print(f"  Instance: {args.instance_id}")
    print(f"  Output: {args.output}\n")

    config = _build_config(args.model, args.mode)
    run_id = generate_run_id(args.model, args.mode)

    result = process_instance(
        target, config, Path(args.output), run_id, args.model, args.mode,
    )

    print(f"\nResult: {json.dumps(result, indent=2, default=str)}")


def cmd_list_configs(args: argparse.Namespace) -> None:
    """Handle the ``list-configs`` subcommand."""
    from config import list_configs

    configs = list_configs()

    print("\nAvailable configurations:")
    print(f"\n  Models ({len(configs.get('models', []))}):")
    for name in configs.get("models", []):
        print(f"    - {name}")

    print(f"\n  Modes ({len(configs.get('modes', []))}):")
    for name in configs.get("modes", []):
        print(f"    - {name}")
    print()


# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

def _print_summary(
    results: List[Dict[str, Any]], model: str, mode: str
) -> None:
    """Print a brief summary of a single run."""
    total = len(results)
    if total == 0:
        print("No results.")
        return

    patches = sum(1 for r in results if r.get("submission"))
    cost = sum(r.get("cost", 0) for r in results)
    errors = sum(1 for r in results if r.get("exit_status") == "error")

    print(f"\n{'─' * 50}")
    print(f"  {model} + {mode}")
    print(f"  Instances: {total}")
    print(f"  Patches:   {patches}/{total} ({patches / total * 100:.0f}%)")
    print(f"  Errors:    {errors}")
    print(f"  Cost:      ${cost:.2f}")
    print(f"{'─' * 50}")


def _print_matrix_summary(all_results: Dict[str, List[Dict[str, Any]]]) -> None:
    """Print a comparative summary across all matrix runs."""
    print(f"\n{'═' * 60}")
    print("  Matrix Summary")
    print(f"{'═' * 60}")

    for run_key, results in all_results.items():
        total = len(results)
        if total == 0:
            print(f"  {run_key}: no results")
            continue
        patches = sum(1 for r in results if r.get("submission"))
        cost = sum(r.get("cost", 0) for r in results)
        print(f"  {run_key}: {patches}/{total} patches, ${cost:.2f} cost")

    print(f"{'═' * 60}")


# ---------------------------------------------------------------------------
# CLI parser
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    """Build the argparse CLI parser with subcommands."""
    parser = argparse.ArgumentParser(
        prog="gortex-eval",
        description="SWE-bench evaluation harness for Gortex code intelligence",
    )
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="Enable verbose logging"
    )
    subparsers = parser.add_subparsers(dest="command", help="Available commands")

    # --- single ---
    p_single = subparsers.add_parser("single", help="Run a single (model, mode) configuration")
    p_single.add_argument("-m", "--model", required=True, help="Model config name")
    p_single.add_argument("--mode", default="baseline", help="Evaluation mode (default: baseline)")
    p_single.add_argument("--subset", default=DEFAULT_SUBSET, help="SWE-bench subset: lite, verified, full")
    p_single.add_argument("--split", default=DEFAULT_SPLIT, help="Dataset split")
    p_single.add_argument("--slice", default="", help="Slice spec (e.g., '0:5', ':3')")
    p_single.add_argument("--filter", default="", help="Filter instance IDs by regex")
    p_single.add_argument("-w", "--workers", type=int, default=1, help="Parallel workers (default: 1)")
    p_single.add_argument("-o", "--output", default=str(DEFAULT_OUTPUT_DIR), help="Output directory")
    p_single.set_defaults(func=cmd_single)

    # --- matrix ---
    p_matrix = subparsers.add_parser("matrix", help="Run full model × mode evaluation matrix")
    p_matrix.add_argument("--models", nargs="+", required=True, help="Model config names")
    p_matrix.add_argument("--modes", nargs="+", required=True, help="Mode config names")
    p_matrix.add_argument("--subset", default=DEFAULT_SUBSET, help="SWE-bench subset")
    p_matrix.add_argument("--split", default=DEFAULT_SPLIT, help="Dataset split")
    p_matrix.add_argument("--slice", default="", help="Slice spec")
    p_matrix.add_argument("--filter", default="", help="Filter instance IDs by regex")
    p_matrix.add_argument("-w", "--workers", type=int, default=1, help="Parallel workers per config")
    p_matrix.add_argument("-o", "--output", default=str(DEFAULT_OUTPUT_DIR), help="Output directory")
    p_matrix.set_defaults(func=cmd_matrix)

    # --- debug ---
    p_debug = subparsers.add_parser("debug", help="Debug a single instance with verbose logging")
    p_debug.add_argument("-m", "--model", required=True, help="Model config name")
    p_debug.add_argument("--mode", default="baseline", help="Evaluation mode")
    p_debug.add_argument("-i", "--instance-id", required=True, help="SWE-bench instance ID")
    p_debug.add_argument("--subset", default=DEFAULT_SUBSET, help="SWE-bench subset")
    p_debug.add_argument("--split", default=DEFAULT_SPLIT, help="Dataset split")
    p_debug.add_argument("-o", "--output", default=str(DEFAULT_OUTPUT_DIR), help="Output directory")
    p_debug.set_defaults(func=cmd_debug)

    # --- list-configs ---
    p_list = subparsers.add_parser("list-configs", help="Show available model/mode configs")
    p_list.set_defaults(func=cmd_list_configs)

    return parser


def main() -> None:
    """CLI entry point."""
    parser = build_parser()
    args = parser.parse_args()

    if args.verbose:
        logging.basicConfig(level=logging.DEBUG, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    else:
        logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")

    if not args.command:
        parser.print_help()
        sys.exit(0)

    args.func(args)


if __name__ == "__main__":
    main()
