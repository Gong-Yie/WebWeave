import json
import math
from dataclasses import dataclass
from collections.abc import Mapping, Sequence
from typing import Any

from openai import OpenAI

from .config import ContextConfig
from .run_store import to_jsonable
from .tools import invoke_with_timeout


COMPRESSION_INSTRUCTIONS = """
你是 Agent 的上下文压缩器，不是任务执行者。
请把给定的历史事件压缩成可供另一个 Agent 继续工作的结构化 JSON。
必须保留：任务目标、用户已确认的决定、已完成工作、未完成工作、生成文件路径、关键错误、运行 ID 和最近用户意图。
不要编造历史中没有的信息；不确定的内容放入 uncertain 数组。
只返回 JSON 对象，不要使用 Markdown 代码块。
JSON 字段必须包含：goal、decisions、completed、pending、artifacts、errors、
uncertain、latest_user_intent。
""".strip()


@dataclass(frozen=True)
class CompressionResult:
    input_items: list[Any]
    summary: dict[str, Any]
    removed_items: int
    tokens_before: int
    tokens_after: int


def estimate_tokens(items: Sequence[Any]) -> int:
    serialized = json.dumps(to_jsonable(list(items)), ensure_ascii=False)
    return max(1, math.ceil(len(serialized) / 2))


def should_compress(items: Sequence[Any], config: ContextConfig) -> bool:
    if not config.enabled:
        return False
    threshold = int(config.max_input_tokens * config.compression_trigger_ratio)
    return estimate_tokens(items) >= max(1, threshold)


def compress_context(
    client: OpenAI,
    input_items: Sequence[Any],
    config: ContextConfig,
    model_name: str,
) -> CompressionResult | None:
    if not should_compress(input_items, config):
        return None

    recent_start = _recent_start(input_items, config.keep_recent_items)
    old_items = list(input_items[:recent_start])
    recent_items = list(input_items[recent_start:])
    if not old_items:
        return _fallback_trim(input_items, config)

    history = _compact_history(old_items)
    prompt = (
        f"{COMPRESSION_INSTRUCTIONS}\n\n"
        "以下是需要压缩的旧上下文事件：\n"
        f"{history}"
    )

    response = invoke_with_timeout(
        lambda: client.responses.create(
            model=model_name,
            temperature=0.1,
            input=prompt,
            max_output_tokens=config.summary_max_tokens,
        ),
        {},
        config.compression_timeout_seconds,
        operation_name="上下文压缩",
    )
    summary = _parse_summary(getattr(response, "output_text", ""))
    summary_item = {
        "role": "assistant",
        "content": "[历史上下文摘要]\n"
        + json.dumps(summary, ensure_ascii=False),
    }
    new_items = [summary_item, *recent_items]
    return CompressionResult(
        input_items=new_items,
        summary=summary,
        removed_items=len(old_items),
        tokens_before=estimate_tokens(input_items),
        tokens_after=estimate_tokens(new_items),
    )


def _recent_start(items: Sequence[Any], keep_recent_items: int) -> int:
    start = max(0, len(items) - keep_recent_items)
    while start > 0 and _item_type(items[start]) in {
        "function_call",
        "function_call_output",
    }:
        start -= 1
    return start


def _item_type(item: Any) -> str:
    if isinstance(item, Mapping):
        return str(item.get("type", ""))
    return str(getattr(item, "type", ""))


def _compact_history(items: Sequence[Any], max_chars_per_item: int = 6_000) -> str:
    compact_items: list[dict[str, Any]] = []
    for index, item in enumerate(items):
        serialized = json.dumps(to_jsonable(item), ensure_ascii=False)
        if len(serialized) > max_chars_per_item:
            half = max_chars_per_item // 2
            serialized = (
                serialized[:half]
                + "...[中间内容已截断]..."
                + serialized[-half:]
            )
        compact_items.append({"index": index, "item": serialized})
    return json.dumps(compact_items, ensure_ascii=False)


def _parse_summary(raw_text: str) -> dict[str, Any]:
    text = (raw_text or "").strip()
    if text.startswith("```"):
        first_newline = text.find("\n")
        if first_newline >= 0:
            text = text[first_newline + 1 :]
        if text.endswith("```"):
            text = text[:-3].strip()

    try:
        payload = json.loads(text)
    except (json.JSONDecodeError, TypeError) as exc:
        raise ValueError(f"上下文压缩结果不是有效 JSON: {exc}") from exc
    if not isinstance(payload, dict):
        raise ValueError("上下文压缩结果必须是 JSON 对象。")

    fields = (
        "goal",
        "decisions",
        "completed",
        "pending",
        "artifacts",
        "errors",
        "uncertain",
        "latest_user_intent",
    )
    return {
        field: payload.get(field, [] if field != "goal" else "") for field in fields
    }


def _fallback_trim(
    input_items: Sequence[Any],
    config: ContextConfig,
) -> CompressionResult:
    start = _recent_start(input_items, max(1, config.keep_recent_items // 2))
    recent_items = list(input_items[start:])
    summary = {
        "goal": "历史上下文压缩失败，需以最近上下文和运行文件为准。",
        "decisions": [],
        "completed": [],
        "pending": ["检查运行记录中的未完成任务"],
        "artifacts": [],
        "errors": ["模型上下文压缩失败，已执行保守截断"],
        "uncertain": [],
        "latest_user_intent": "",
    }
    summary_item = {
        "role": "assistant",
        "content": "[历史上下文摘要]\n"
        + json.dumps(summary, ensure_ascii=False),
    }
    new_items = [summary_item, *recent_items]
    return CompressionResult(
        input_items=new_items,
        summary=summary,
        removed_items=len(input_items) - len(recent_items),
        tokens_before=estimate_tokens(input_items),
        tokens_after=estimate_tokens(new_items),
    )
