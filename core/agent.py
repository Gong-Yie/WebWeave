import json
import time
from collections.abc import Callable, Sequence
from typing import Any
from uuid import uuid4

from openai import (
    APIConnectionError,
    APITimeoutError,
    APIStatusError,
    OpenAI,
    RateLimitError,
)

from tools.registry import ToolRegistry

from .config import CONFIG_MANAGER, build_openai_client, get_runtime_config
from .context import _fallback_trim, compress_context, should_compress
from .paths import prepare_run_directories
from .prompt import SYSTEM_PROMPT
from .run_store import RunStore, repair_input_items
from .tools import call_value, execute_tool


TOOL_REGISTRY = ToolRegistry()
STREAM_EVENT_FLUSH_SECONDS = 0.2
STREAM_EVENT_FLUSH_CHARS = 256
LLM_MAX_ATTEMPTS = 5


class AgentStopped(RuntimeError):
    """用户请求在安全边界停止当前 Agent 运行。"""


def run_messages(
    client: OpenAI,
    input_items: list[Any],
    verbose: bool,
    registry: ToolRegistry = TOOL_REGISTRY,
    run_id: str = "",
    run_store: RunStore | None = None,
    should_stop: Callable[[], bool] | None = None,
) -> str:
    active_run_id = run_id or uuid4().hex
    prepare_run_directories(active_run_id)
    store = run_store or RunStore(active_run_id)
    store.start()
    repaired_items = repair_input_items(input_items)
    if len(repaired_items) != len(input_items):
        input_items[:] = repaired_items
        store.save_context(input_items)
    store.save_context(input_items)
    iteration = 0
    try:
        while True:
            if should_stop is not None and should_stop():
                raise AgentStopped("用户请求停止")
            iteration += 1
            if verbose:
                print(f"\n[Agent] 第 {iteration} 轮思考中...")

            runtime_config = get_runtime_config()
            if verbose and CONFIG_MANAGER.reload_error:
                print(f"[Config] {CONFIG_MANAGER.reload_error}")

            if should_compress(input_items, runtime_config.context):
                try:
                    compression = compress_context(
                    client,
                    input_items,
                    runtime_config.context,
                    runtime_config.llm.main.model,
                    )
                except Exception as exc:
                    if verbose:
                        print(f"[Context] 压缩失败，执行保守截断: {exc}")
                    compression = _fallback_trim(input_items, runtime_config.context)

                input_items[:] = compression.input_items
                store.append_event(
                    "context_compressed",
                    {
                        "summary": compression.summary,
                        "input_items": input_items,
                        "removed_items": compression.removed_items,
                        "tokens_before": compression.tokens_before,
                        "tokens_after": compression.tokens_after,
                    },
                )
                store.save_context(input_items, compression.summary)

            snapshot = registry.snapshot()
            if verbose and snapshot.reload_errors:
                print(f"[Tool] 热加载警告: {'; '.join(snapshot.reload_errors)}")

            instructions = (
                SYSTEM_PROMPT.replace("{run_id}", active_run_id)
                .replace(
                    "{timeout_default}",
                    f"{runtime_config.tool_timeout.default_seconds:g}",
                )
                .replace(
                    "{timeout_max}",
                    f"{runtime_config.tool_timeout.max_seconds:g}",
                )
            )
            response = _create_model_response(
                client=client,
                input_items=input_items,
                tool_definitions=list(snapshot.definitions),
                instructions=instructions,
                stream_enabled=runtime_config.model.stream,
                model_name=runtime_config.llm.main.model,
                store=store,
                verbose=verbose,
                should_stop=should_stop,
            )
            output_text = getattr(response, "output_text", "") or ""
            output_items = list(getattr(response, "output", None) or [])
            if not output_items and output_text:
                output_items = [{"role": "assistant", "content": output_text}]
            input_items.extend(output_items)
            store.append_event(
                "model_output",
                {"items": output_items, "output_text": output_text},
            )
            store.save_context(input_items)
            function_calls = [
                item
                for item in output_items
                if call_value(item, "type") == "function_call"
            ]
            if not function_calls:
                if verbose and not runtime_config.model.stream:
                    print("[Agent] 无工具调用，准备输出最终答案。")
                    print(f"\nAgent: {output_text}")
                store.set_status("completed")
                return output_text

            for call_index, call in enumerate(function_calls):
                if should_stop is not None and should_stop():
                    _append_cancelled_tool_outputs(
                        function_calls[call_index:],
                        input_items,
                        store,
                    )
                    raise AgentStopped("用户请求停止")
                tool_name = call_value(call, "name") or ""
                raw_args = call_value(call, "arguments") or "{}"
                tool_call_id = (
                    call_value(call, "call_id")
                    or call_value(call, "id")
                    or ""
                )
                tool_started_at = time.monotonic()
                tool_output = execute_tool(
                    tool_name,
                    raw_args,
                    snapshot.handlers,
                    verbose,
                    active_run_id,
                )
                tool_output_item = {
                    "type": "function_call_output",
                    "call_id": tool_call_id,
                    "output": tool_output,
                }
                input_items.append(tool_output_item)
                store.append_event(
                    "tool_output",
                    {
                        "tool_name": tool_name,
                        "arguments": raw_args,
                        "duration_seconds": round(
                            time.monotonic() - tool_started_at,
                            3,
                        ),
                        "item": tool_output_item,
                    },
                )
                store.save_context(input_items)
            continue
    except AgentStopped:
        store.set_status("stopped", "用户请求停止")
        raise
    except KeyboardInterrupt:
        store.set_status("stopped", "用户中断")
        raise
    except Exception as exc:
        store.set_status("failed", str(exc))
        raise


def _create_model_response(
    client: OpenAI,
    input_items: list[Any],
    tool_definitions: Sequence[Any],
    instructions: str,
    stream_enabled: bool,
    model_name: str,
    store: RunStore,
    verbose: bool,
    should_stop: Callable[[], bool] | None,
) -> Any:
    for attempt in range(1, LLM_MAX_ATTEMPTS + 1):
        try:
            if not stream_enabled:
                return client.responses.create(
                    model=model_name,
                    temperature=0.7,
                    instructions=instructions,
                    input=input_items,
                    tools=list(tool_definitions),
                )
            return _create_streaming_model_response(
                client=client,
                input_items=input_items,
                tool_definitions=tool_definitions,
                instructions=instructions,
                model_name=model_name,
                store=store,
                verbose=verbose,
                should_stop=should_stop,
            )
        except Exception as exc:
            if isinstance(exc, AgentStopped) or not _is_retryable_llm_error(exc):
                raise
            if attempt == LLM_MAX_ATTEMPTS:
                raise RuntimeError(
                    f"大模型请求连续失败 {LLM_MAX_ATTEMPTS} 次，最后错误: {exc}"
                ) from exc
            delay = min(2 ** (attempt - 1), 8)
            store.append_event(
                "llm_retry",
                {
                    "attempt": attempt,
                    "next_attempt": attempt + 1,
                    "delay_seconds": delay,
                    "error": str(exc),
                },
            )
            if verbose:
                print(f"[LLM] 第 {attempt} 次请求失败，{delay} 秒后重试: {exc}")
            if should_stop is not None and should_stop():
                raise AgentStopped("用户请求停止") from exc
            time.sleep(delay)
    raise AssertionError("LLM 重试循环未正常结束")


def _is_retryable_llm_error(exc: Exception) -> bool:
    if isinstance(exc, (APIConnectionError, APITimeoutError, RateLimitError)):
        return True
    if isinstance(exc, APIStatusError):
        return exc.status_code in {408, 409, 425, 429} or exc.status_code >= 500
    return exc.__class__.__name__ in {
        "RemoteProtocolError",
        "ReadError",
        "ConnectError",
        "TimeoutException",
    }


def _create_streaming_model_response(
    client: OpenAI,
    input_items: list[Any],
    tool_definitions: Sequence[Any],
    instructions: str,
    model_name: str,
    store: RunStore,
    verbose: bool,
    should_stop: Callable[[], bool] | None,
) -> Any:
    pending_parts: list[str] = []
    pending_length = 0
    last_flush = time.monotonic()
    completed_response: Any = None
    printed_text = False

    def flush_pending() -> None:
        nonlocal pending_length, last_flush
        if not pending_parts:
            return
        store.append_event("model_output_delta", {"delta": "".join(pending_parts)})
        pending_parts.clear()
        pending_length = 0
        last_flush = time.monotonic()

    try:
        with client.responses.create(
            model=model_name,
            temperature=0.7,
            instructions=instructions,
            input=input_items,
            tools=list(tool_definitions),
            stream=True,
        ) as response_stream:
            for event in response_stream:
                if should_stop is not None and should_stop():
                    raise AgentStopped("用户请求停止")
                event_type = call_value(event, "type")
                if event_type == "response.output_text.delta":
                    delta = call_value(event, "delta")
                    if not isinstance(delta, str) or not delta:
                        continue
                    pending_parts.append(delta)
                    pending_length += len(delta)
                    if verbose:
                        if not printed_text:
                            print("\nAgent: ", end="", flush=True)
                            printed_text = True
                        print(delta, end="", flush=True)
                    if (
                        pending_length >= STREAM_EVENT_FLUSH_CHARS
                        or time.monotonic() - last_flush
                        >= STREAM_EVENT_FLUSH_SECONDS
                    ):
                        flush_pending()
                elif event_type == "response.completed":
                    completed_response = call_value(event, "response")
    finally:
        flush_pending()
        if verbose and printed_text:
            print()

    if completed_response is None:
        raise RuntimeError("流式响应结束，但未收到 response.completed 事件。")
    return completed_response


def _append_cancelled_tool_outputs(
    calls: Sequence[Any],
    input_items: list[Any],
    store: RunStore,
) -> None:
    for call in calls:
        tool_name = call_value(call, "name") or ""
        tool_call_id = call_value(call, "call_id") or call_value(call, "id") or ""
        item = {
            "type": "function_call_output",
            "call_id": tool_call_id,
            "output": json.dumps(
                {
                    "ok": False,
                    "error": {
                        "code": "RUN_STOPPED",
                        "message": "运行已由用户停止，工具未执行。",
                    },
                },
                ensure_ascii=False,
            ),
        }
        input_items.append(item)
        store.append_event(
            "tool_output",
            {
                "tool_name": tool_name,
                "arguments": call_value(call, "arguments") or "{}",
                "duration_seconds": 0,
                "cancelled": True,
                "item": item,
            },
        )
    store.save_context(input_items)


def run_agent(
    user_input: str,
    verbose: bool = True,
    run_id: str | None = None,
) -> str:
    if not user_input.strip():
        return "请输入有效问题。"

    active_run_id = run_id or uuid4().hex
    store = RunStore(active_run_id)
    store.start()
    input_items = store.load_input_items()
    user_item = {"role": "user", "content": user_input}
    input_items.append(user_item)
    store.append_event("user_message", {"item": user_item})
    store.save_context(input_items)
    return run_messages(
        build_openai_client(),
        input_items,
        verbose,
        run_id=active_run_id,
        run_store=store,
    )
