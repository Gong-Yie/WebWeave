import json
import logging
from typing import Any

from core.instance import INSTANCE_MANAGER, InstanceOperationError


logger = logging.getLogger(__name__)

WEB_INSTANCE_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "web_instance",
    "description": (
        "管理当前运行 ID 唯一的 Web 实例，支持 start、status、stop 和 restart。"
        "实例只能从 data/result/{run_id} 启动；restart 复用上次启动参数。"
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "action": {
                "type": "string",
                "enum": ["start", "status", "stop", "restart"],
                "description": "要执行的实例生命周期操作。",
            },
            "command": {
                "type": "string",
                "description": (
                    "start 必填的 PowerShell 命令，必须包含 {host} 和 {port}。"
                ),
            },
            "cwd": {
                "type": "string",
                "description": (
                    "start 使用的工作目录，相对于 data/result/{run_id}，默认是根目录。"
                ),
            },
            "port": {
                "type": "integer",
                "minimum": 0,
                "maximum": 65535,
                "description": "start 使用的监听端口；0 表示自动分配。",
            },
            "health_path": {
                "type": "string",
                "description": "start 使用的健康检查路径，默认 /。",
            },
        },
        "required": ["action"],
        "additionalProperties": False,
    },
    "strict": False,
}


def web_instance(
    action: str,
    command: str | None = None,
    cwd: str | None = None,
    port: int | None = None,
    health_path: str | None = None,
    timeout: float = 120.0,
    run_id: str = "",
) -> str:
    """执行当前运行 ID 的 Web 实例生命周期操作。"""
    try:
        if action == "start":
            result = INSTANCE_MANAGER.start(
                run_id=run_id,
                command=command or "",
                cwd=cwd if cwd is not None else ".",
                port=port if port is not None else 0,
                health_path=health_path if health_path is not None else "/",
                timeout=timeout,
            )
        elif action in {"status", "stop", "restart"}:
            if any(value is not None for value in (command, cwd, port, health_path)):
                raise InstanceOperationError(
                    "INVALID_ARGUMENT",
                    f"{action} 不接受启动参数；restart 会复用原启动参数。",
                )
            if action == "status":
                result = INSTANCE_MANAGER.status(run_id)
            elif action == "stop":
                result = INSTANCE_MANAGER.stop(run_id, timeout)
            else:
                result = INSTANCE_MANAGER.restart(run_id, timeout)
        else:
            raise InstanceOperationError(
                "INVALID_ARGUMENT",
                "action 必须是 start、status、stop 或 restart。",
            )
    except InstanceOperationError as exc:
        result = exc.to_result()
    except Exception as exc:
        logger.exception("web_instance 执行失败")
        result = InstanceOperationError(
            "INTERNAL_ERROR",
            f"web_instance 内部错误: {exc}",
        ).to_result()
    return json.dumps(result, ensure_ascii=False)


TOOL_DEFINITIONS = [WEB_INSTANCE_TOOL_DEFINITION]
TOOL_HANDLERS = {"web_instance": web_instance}
