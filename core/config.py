import json
import math
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from threading import RLock
from types import MappingProxyType
from typing import Any

from openai import OpenAI


WORKSPACE_ROOT = Path(__file__).resolve().parent.parent
DATA_ROOT = WORKSPACE_ROOT / "data"
RUN_ROOT = DATA_ROOT / "run"
ALLOWED_DIRS = {
    "resources_dir": DATA_ROOT / "resources",
    "result_dir": DATA_ROOT / "result",
    "download_dir": DATA_ROOT / "download",
}

CONFIG_PATH = WORKSPACE_ROOT / "config.json"
TOOL_NAME_PATTERN = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
SECRET_MASK = "********"


@dataclass(frozen=True)
class ToolTimeoutConfig:
    default_seconds: float = 120.0
    max_seconds: float = 1800.0


@dataclass(frozen=True)
class ModelConfig:
    stream: bool = True


@dataclass(frozen=True)
class ProviderConfig:
    model: str = "deepseek-v4-flash"
    base_url: str = "https://api.deepseek.com"
    api_key: str = ""


@dataclass(frozen=True)
class VisionProviderConfig:
    enabled: bool = False
    model: str = ""
    base_url: str = ""
    api_key: str = ""


@dataclass(frozen=True)
class LLMConfig:
    main: ProviderConfig = ProviderConfig()
    vision: VisionProviderConfig = VisionProviderConfig()


@dataclass(frozen=True)
class ContextConfig:
    enabled: bool = True
    max_input_tokens: int = 32_768
    compression_trigger_ratio: float = 0.8
    keep_recent_items: int = 12
    summary_max_tokens: int = 2_000
    compression_timeout_seconds: float = 120.0


@dataclass(frozen=True)
class ToolConfig:
    enabled: bool = True
    run_scoped: bool = False


DEFAULT_TOOLS: Mapping[str, ToolConfig] = MappingProxyType(
    {
        "list_skills": ToolConfig(),
        "read_skill": ToolConfig(),
        "web_search": ToolConfig(run_scoped=True),
        "create_file": ToolConfig(run_scoped=True),
        "file_read": ToolConfig(run_scoped=True),
        "terminal_run": ToolConfig(run_scoped=True),
        "web_instance": ToolConfig(run_scoped=True),
    }
)


@dataclass(frozen=True)
class RuntimeConfig:
    llm: LLMConfig = LLMConfig()
    model: ModelConfig = ModelConfig()
    tool_timeout: ToolTimeoutConfig = ToolTimeoutConfig()
    context: ContextConfig = ContextConfig()
    tools: Mapping[str, ToolConfig] = DEFAULT_TOOLS


class ConfigManager:
    """按文件修改时间热加载运行配置，配置错误时保留上一份有效配置。"""

    def __init__(self, path: Path = CONFIG_PATH) -> None:
        self._path = path
        self._lock = RLock()
        self._mtime_ns: int | None = None
        self._config = RuntimeConfig()
        self._reload_error = ""

    def get(self) -> RuntimeConfig:
        with self._lock:
            mtime_ns = self._get_mtime_ns()
            if mtime_ns == self._mtime_ns:
                return self._config

            try:
                self._config = self._read_config()
                self._reload_error = ""
            except (OSError, TypeError, ValueError) as exc:
                self._reload_error = f"config.json 热加载失败，继续使用上一份配置: {exc}"
            self._mtime_ns = mtime_ns
            return self._config

    @property
    def reload_error(self) -> str:
        with self._lock:
            return self._reload_error

    def _get_mtime_ns(self) -> int:
        try:
            return self._path.stat().st_mtime_ns
        except FileNotFoundError:
            return -1

    def _read_config(self) -> RuntimeConfig:
        if not self._path.exists():
            return RuntimeConfig()

        payload = json.loads(self._path.read_text(encoding="utf-8"))
        return _runtime_config_from_payload(payload)


def _runtime_config_from_payload(payload: Any) -> RuntimeConfig:
    if not isinstance(payload, dict):
        raise ValueError("根配置必须是 JSON 对象。")

    tools = _tool_configs(
        payload.get("tools", DEFAULT_TOOLS),
        "tools",
    )

    llm_payload = payload.get("llm", {})
    if not isinstance(llm_payload, Mapping):
        raise ValueError("llm 必须是 JSON 对象。")
    main_payload = llm_payload.get("main", {})
    if not isinstance(main_payload, Mapping):
        raise ValueError("llm.main 必须是 JSON 对象。")
    main_provider = ProviderConfig(
        model=_required_text(
            main_payload.get("model", ProviderConfig.model),
            "llm.main.model",
        ),
        base_url=_required_text(
            main_payload.get("base_url", ProviderConfig.base_url),
            "llm.main.base_url",
        ),
        api_key=_text(main_payload.get("api_key", ""), "llm.main.api_key"),
    )
    vision_payload = llm_payload.get("vision", {})
    if vision_payload is None:
        vision_payload = {}
    if not isinstance(vision_payload, Mapping):
        raise ValueError("llm.vision 必须是配置对象或 null。")
    vision_enabled = _boolean(
        vision_payload.get("enabled", False),
        "llm.vision.enabled",
    )
    vision_model = _text(vision_payload.get("model", ""), "llm.vision.model")
    vision_base_url = _text(
        vision_payload.get("base_url", ""),
        "llm.vision.base_url",
    )
    vision_api_key = _text(
        vision_payload.get("api_key", ""),
        "llm.vision.api_key",
    )
    if vision_enabled and not all(
        (vision_model, vision_base_url, vision_api_key)
    ):
        raise ValueError(
            "启用视觉模型时，llm.vision.model、base_url 和 api_key 不能为空。"
        )

    model_payload = payload.get("model", {})
    if not isinstance(model_payload, dict):
        raise ValueError("model 必须是 JSON 对象。")
    model = ModelConfig(
        stream=_boolean(model_payload.get("stream", True), "model.stream"),
    )

    timeout_payload = payload.get("tool_timeout", {})
    if not isinstance(timeout_payload, dict):
        raise ValueError("tool_timeout 必须是 JSON 对象。")

    default_seconds = _positive_number(
        timeout_payload.get("default_seconds", 120),
        "tool_timeout.default_seconds",
    )
    max_seconds = _positive_number(
        timeout_payload.get("max_seconds", 1800),
        "tool_timeout.max_seconds",
    )
    if max_seconds < default_seconds:
        raise ValueError("tool_timeout.max_seconds 不能小于 default_seconds。")

    context_payload = payload.get("context", {})
    if not isinstance(context_payload, dict):
        raise ValueError("context 必须是 JSON 对象。")
    context = ContextConfig(
        enabled=_boolean(context_payload.get("enabled", True), "context.enabled"),
        max_input_tokens=_positive_integer(
            context_payload.get("max_input_tokens", 32_768),
            "context.max_input_tokens",
        ),
        compression_trigger_ratio=_ratio(
            context_payload.get("compression_trigger_ratio", 0.8),
            "context.compression_trigger_ratio",
        ),
        keep_recent_items=_positive_integer(
            context_payload.get("keep_recent_items", 12),
            "context.keep_recent_items",
        ),
        summary_max_tokens=_positive_integer(
            context_payload.get("summary_max_tokens", 2_000),
            "context.summary_max_tokens",
        ),
        compression_timeout_seconds=_positive_number(
            context_payload.get("compression_timeout_seconds", 120),
            "context.compression_timeout_seconds",
        ),
    )

    return RuntimeConfig(
        llm=LLMConfig(
            main=main_provider,
            vision=VisionProviderConfig(
                enabled=vision_enabled,
                model=vision_model,
                base_url=vision_base_url,
                api_key=vision_api_key,
            ),
        ),
        model=model,
        tool_timeout=ToolTimeoutConfig(
            default_seconds=default_seconds,
            max_seconds=max_seconds,
        ),
        context=context,
        tools=tools,
    )


def _positive_number(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{name} 必须是正数。")
    number = float(value)
    if not math.isfinite(number) or number <= 0:
        raise ValueError(f"{name} 必须是正数。")
    return number


def _positive_integer(value: Any, name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ValueError(f"{name} 必须是正整数。")
    return value


def _boolean(value: Any, name: str) -> bool:
    if not isinstance(value, bool):
        raise ValueError(f"{name} 必须是布尔值。")
    return value


def _text(value: Any, name: str) -> str:
    if not isinstance(value, str):
        raise ValueError(f"{name} 必须是字符串。")
    return value.strip()


def _required_text(value: Any, name: str) -> str:
    text = _text(value, name)
    if not text:
        raise ValueError(f"{name} 不能为空。")
    return text


def _tool_configs(value: Any, name: str) -> Mapping[str, ToolConfig]:
    if not isinstance(value, Mapping):
        raise ValueError(f"{name} 必须是工具配置对象。")

    configs: dict[str, ToolConfig] = {}
    for tool_name, settings in value.items():
        if not isinstance(tool_name, str) or not TOOL_NAME_PATTERN.fullmatch(
            tool_name
        ):
            raise ValueError(f"{name} 包含无效工具名称: {tool_name!r}。")
        if not isinstance(settings, Mapping):
            raise ValueError(f"{name}.{tool_name} 必须是配置对象。")
        configs[tool_name] = ToolConfig(
            enabled=_boolean(
                settings.get("enabled", True), f"{name}.{tool_name}.enabled"
            ),
            run_scoped=_boolean(
                settings.get("run_scoped", False),
                f"{name}.{tool_name}.run_scoped",
            ),
        )
    return MappingProxyType(configs)


def _ratio(value: Any, name: str) -> float:
    number = _positive_number(value, name)
    if number >= 1:
        raise ValueError(f"{name} 必须大于 0 且小于 1。")
    return number


CONFIG_MANAGER = ConfigManager()


def get_runtime_config() -> RuntimeConfig:
    return CONFIG_MANAGER.get()


def runtime_config_payload() -> dict[str, Any]:
    return _runtime_config_to_payload(get_runtime_config(), redact_secrets=True)


def save_runtime_config(payload: Any) -> dict[str, Any]:
    """校验并原子写入 config.json，返回规范化后的配置。"""
    payload = _preserve_existing_secrets(payload, CONFIG_MANAGER.get())
    config = _runtime_config_from_payload(payload)
    normalized = _runtime_config_to_payload(config)
    temporary_path = CONFIG_PATH.with_name(f".{CONFIG_PATH.name}.tmp")
    temporary_path.write_text(
        json.dumps(normalized, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary_path, CONFIG_PATH)
    return _runtime_config_to_payload(config, redact_secrets=True)


def _runtime_config_to_payload(
    config: RuntimeConfig,
    redact_secrets: bool = False,
) -> dict[str, Any]:
    main_api_key = SECRET_MASK if redact_secrets and config.llm.main.api_key else config.llm.main.api_key
    vision_api_key = SECRET_MASK if redact_secrets and config.llm.vision.api_key else config.llm.vision.api_key
    return {
        "llm": {
            "main": {
                "model": config.llm.main.model,
                "base_url": config.llm.main.base_url,
                "api_key": main_api_key,
            },
            "vision": {
                "enabled": config.llm.vision.enabled,
                "model": config.llm.vision.model,
                "base_url": config.llm.vision.base_url,
                "api_key": vision_api_key,
            },
        },
        "tools": {
            name: {
                "enabled": settings.enabled,
                "run_scoped": settings.run_scoped,
            }
            for name, settings in config.tools.items()
        },
        "model": {
            "stream": config.model.stream,
        },
        "tool_timeout": {
            "default_seconds": config.tool_timeout.default_seconds,
            "max_seconds": config.tool_timeout.max_seconds,
        },
        "context": {
            "enabled": config.context.enabled,
            "max_input_tokens": config.context.max_input_tokens,
            "compression_trigger_ratio": config.context.compression_trigger_ratio,
            "keep_recent_items": config.context.keep_recent_items,
            "summary_max_tokens": config.context.summary_max_tokens,
            "compression_timeout_seconds": (
                config.context.compression_timeout_seconds
            ),
        },
    }


def _preserve_existing_secrets(payload: Any, current: RuntimeConfig) -> dict[str, Any]:
    if not isinstance(payload, Mapping):
        raise TypeError("配置必须是 JSON 对象。")
    normalized = dict(payload)
    llm_payload = dict(normalized.get("llm", {}))
    main_payload = dict(llm_payload.get("main", {}))
    vision_payload = dict(llm_payload.get("vision", {}))
    if not main_payload.get("api_key") or main_payload["api_key"] == SECRET_MASK:
        main_payload["api_key"] = current.llm.main.api_key
    if not vision_payload.get("api_key") or vision_payload["api_key"] == SECRET_MASK:
        vision_payload["api_key"] = current.llm.vision.api_key
    llm_payload["main"] = main_payload
    llm_payload["vision"] = vision_payload
    normalized["llm"] = llm_payload
    return normalized


def build_openai_client(provider: ProviderConfig | VisionProviderConfig | None = None) -> OpenAI:
    """根据配置创建 OpenAI 兼容客户端。"""
    selected = provider or get_runtime_config().llm.main
    if not selected.api_key:
        raise RuntimeError("未配置大模型 API Key，请在设置的大模型页面填写。")
    return OpenAI(api_key=selected.api_key, base_url=selected.base_url)
