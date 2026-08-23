import json
from collections.abc import Mapping
from concurrent.futures import ThreadPoolExecutor, TimeoutError as FutureTimeoutError
from typing import Any, Callable, TypeVar

from .config import CONFIG_MANAGER, get_runtime_config
from .paths import is_path_allowed


ToolHandler = Callable[..., str]
TIMEOUT_AWARE_TOOLS = {"terminal_run", "web_instance"}
ResultType = TypeVar("ResultType")


def is_run_scoped_tool(tool_name: str) -> bool:
    tool_config = get_runtime_config().tools.get(tool_name)
    return tool_config is not None and tool_config.run_scoped


def resolve_tool_timeout(requested: Any) -> float:
    timeout_config = get_runtime_config().tool_timeout
    if requested is None:
        return timeout_config.default_seconds
    if isinstance(requested, bool) or not isinstance(requested, (int, float)):
        raise ValueError("timeout 必须是正数秒数。")

    timeout = float(requested)
    if timeout <= 0:
        raise ValueError("timeout 必须大于 0。")
    return min(timeout, timeout_config.max_seconds)


def invoke_with_timeout(
    tool: Callable[..., ResultType],
    tool_args: dict[str, Any],
    timeout: float,
    operation_name: str = "工具",
) -> ResultType:
    executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="webweave-tool")
    future = executor.submit(tool, **tool_args)
    try:
        return future.result(timeout=timeout)
    except FutureTimeoutError as exc:
        future.cancel()
        raise TimeoutError(
            f"{operation_name}执行超过 {timeout:g} 秒，已将超时结果返回给 Agent。"
        ) from exc
    finally:
        executor.shutdown(wait=False, cancel_futures=True)


def sanitize_tool_args(
    tool_name: str,
    tool_args: dict[str, Any],
    run_id: str,
) -> dict[str, Any]:
    args = dict(tool_args or {})

    for key, value in args.items():
        if (
            key.endswith("_dir")
            and isinstance(value, str)
            and not is_path_allowed(value, run_id)
        ):
            raise ValueError(
                f"参数 {key} 越权: {value}。仅允许当前运行 ID 的 "
                "data/resources、data/result 或 data/download 目录。"
            )

    if is_run_scoped_tool(tool_name):
        for unsafe_key in ("path", "file_path", "resources_dir", "result_dir"):
            if tool_name == "web_search" or unsafe_key != "file_path":
                args.pop(unsafe_key, None)
        args.pop("run_id", None)

    return args


def short_text(text: str, limit: int = 260) -> str:
    compact = (text or "").replace("\n", " ").strip()
    return compact if len(compact) <= limit else compact[:limit] + "..."


def call_value(call: Any, key: str) -> Any:
    if isinstance(call, dict):
        return call.get(key)
    return getattr(call, key, None)


def execute_tool(
    tool_name: str,
    raw_args: str,
    handlers: Mapping[str, ToolHandler],
    verbose: bool,
    run_id: str,
) -> str:
    try:
        tool_args = json.loads(raw_args) if raw_args else {}
        if not isinstance(tool_args, dict):
            raise ValueError("工具参数必须是 JSON 对象。")
    except (json.JSONDecodeError, TypeError, ValueError) as exc:
        return f"工具参数解析失败({tool_name}): {exc}"

    tool = handlers.get(tool_name)
    if tool is None:
        return f"工具不存在: {tool_name}"

    tool_config = get_runtime_config().tools.get(tool_name)
    if tool_config is None or not tool_config.enabled:
        return f"工具已禁用: {tool_name}"

    try:
        requested_timeout = tool_args.pop("timeout", None)
        timeout = resolve_tool_timeout(requested_timeout)
        safe_args = sanitize_tool_args(tool_name, tool_args, run_id)
        if is_run_scoped_tool(tool_name):
            safe_args["run_id"] = run_id
        if tool_name in TIMEOUT_AWARE_TOOLS:
            safe_args["timeout"] = timeout
        if verbose:
            print(f"[Tool] 调用 {tool_name}")
            print(f"[Tool] 超时 {timeout:g} 秒")
            if CONFIG_MANAGER.reload_error:
                print(f"[Config] {CONFIG_MANAGER.reload_error}")
            print(f"[Tool] 参数 {json.dumps(safe_args, ensure_ascii=False)}")
        watchdog_timeout = (
            timeout + 5.0 if tool_name in TIMEOUT_AWARE_TOOLS else timeout
        )
        output = invoke_with_timeout(tool, safe_args, watchdog_timeout)
        if verbose:
            print(f"[Tool] 结果 {short_text(str(output))}")
        return str(output)
    except Exception as exc:
        output = f"工具执行失败({tool_name}): {exc}"
        if verbose:
            print(f"[Tool] 异常 {output}")
        return output
