from copy import deepcopy
from dataclasses import dataclass
from importlib import import_module, reload
from pathlib import Path
from threading import RLock
from types import ModuleType
from typing import Any, Callable, Mapping


ToolHandler = Callable[..., str]


@dataclass(frozen=True)
class ToolSnapshot:
    """某一轮模型请求使用的工具定义和处理器快照。"""

    definitions: tuple[Mapping[str, Any], ...]
    handlers: Mapping[str, ToolHandler]
    version: int
    reload_errors: tuple[str, ...]


@dataclass(frozen=True)
class _ModuleState:
    module: ModuleType
    mtime_ns: int


class ToolRegistry:
    """扫描 tools 目录，并在模块文件变化后重新加载工具。"""

    def __init__(self, tools_dir: Path | None = None) -> None:
        self._tools_dir = (tools_dir or Path(__file__).parent).resolve()
        self._modules: dict[str, _ModuleState] = {}
        self._reload_errors: dict[str, str] = {}
        self._version = 0
        self._lock = RLock()

    def snapshot(self) -> ToolSnapshot:
        """刷新工具模块并返回本轮请求应使用的工具快照。"""
        from core.config import get_runtime_config

        with self._lock:
            self._refresh_locked()
            definitions: list[Mapping[str, Any]] = []
            handlers: dict[str, ToolHandler] = {}
            enabled_tools = get_runtime_config().tools
            for module_name in sorted(self._modules):
                module = self._modules[module_name].module
                module_definitions = getattr(module, "TOOL_DEFINITIONS", ())
                module_handlers = getattr(module, "TOOL_HANDLERS", {})
                for definition in module_definitions:
                    tool_name = self._validate_definition(module_name, definition)
                    tool_config = enabled_tools.get(tool_name)
                    if tool_config is None or not tool_config.enabled:
                        continue
                    if tool_name in handlers:
                        raise RuntimeError(f"工具名称重复: {tool_name}")
                    handler = module_handlers.get(tool_name) or getattr(
                        module, tool_name, None
                    )
                    if not callable(handler):
                        raise RuntimeError(
                            f"工具 {tool_name} 未找到可调用处理器: {module_name}"
                        )
                    definitions.append(self._add_timeout_parameter(definition))
                    handlers[tool_name] = handler

            return ToolSnapshot(
                definitions=tuple(definitions),
                handlers=handlers,
                version=self._version,
                reload_errors=tuple(self._reload_errors.values()),
            )

    def _refresh_locked(self) -> None:
        discovered: dict[str, int] = {}
        changed = False
        for path in self._discover_files():
            try:
                mtime_ns = path.stat().st_mtime_ns
            except OSError:
                continue
            module_name = f"tools.{path.stem}"
            discovered[module_name] = mtime_ns
            previous = self._modules.get(module_name)
            if previous is not None and previous.mtime_ns == mtime_ns:
                continue

            try:
                module = (
                    reload(previous.module)
                    if previous is not None
                    else import_module(module_name)
                )
            except Exception as exc:
                if previous is None:
                    self._reload_errors[module_name] = f"首次加载失败: {exc}"
                else:
                    self._reload_errors[module_name] = f"热加载失败，继续使用旧版本: {exc}"
                continue

            self._modules[module_name] = _ModuleState(module=module, mtime_ns=mtime_ns)
            self._reload_errors.pop(module_name, None)
            changed = True

        for module_name in set(self._modules) - set(discovered):
            del self._modules[module_name]
            self._reload_errors.pop(module_name, None)
            changed = True

        if changed:
            self._version += 1

    def _discover_files(self) -> list[Path]:
        return sorted(
            path
            for path in self._tools_dir.glob("*.py")
            if path.stem not in {"__init__", "registry"}
            and not path.stem.startswith("_")
        )

    @staticmethod
    def _validate_definition(module_name: str, definition: Any) -> str:
        if not isinstance(definition, Mapping):
            raise RuntimeError(f"工具定义必须是映射: {module_name}")
        if definition.get("type") != "function":
            raise RuntimeError(f"工具类型必须为 function: {module_name}")
        tool_name = definition.get("name")
        if not isinstance(tool_name, str) or not tool_name.strip():
            raise RuntimeError(f"工具定义缺少 name: {module_name}")
        return tool_name

    @staticmethod
    def _add_timeout_parameter(definition: Mapping[str, Any]) -> Mapping[str, Any]:
        enriched = deepcopy(definition)
        parameters = enriched.get("parameters")
        if not isinstance(parameters, dict):
            raise RuntimeError("工具定义缺少 parameters 对象。")
        properties = parameters.setdefault("properties", {})
        if not isinstance(properties, dict):
            raise RuntimeError("工具定义的 parameters.properties 必须是对象。")
        properties.setdefault(
            "timeout",
            {
                "type": "number",
                "minimum": 0.1,
                "description": "本次工具调用允许的最长时间（秒），请根据任务复杂度填写。",
            },
        )
        required = parameters.setdefault("required", [])
        if not isinstance(required, list):
            raise RuntimeError("工具定义的 parameters.required 必须是数组。")
        if "timeout" not in required:
            required.append("timeout")
        return enriched
