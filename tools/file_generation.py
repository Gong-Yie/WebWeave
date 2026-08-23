import json
import re
from pathlib import Path
from typing import Any


WORKSPACE_ROOT = Path(__file__).resolve().parent.parent
RESULT_ROOT = (WORKSPACE_ROOT / "data" / "result").resolve()

FILE_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "create_file",
    "description": "在当前运行 ID 的 data/result/{run_id} 目录中创建或覆盖一个文件，并返回文件信息。",
    "parameters": {
        "type": "object",
        "properties": {
            "file_path": {
                "type": "string",
                "description": "相对于当前运行 ID 的结果目录的文件路径，例如 static/index.html。",
            },
            "content": {
                "type": "string",
                "description": "要写入文件的完整文本内容。",
            },
        },
        "required": ["file_path", "content"],
        "additionalProperties": False,
    },
    "strict": False,
}


def create_file(file_path: str, content: str, run_id: str = "") -> str:
    """在当前运行 ID 的结果目录内创建文件，拒绝绝对路径和目录穿越。"""
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", run_id):
        raise ValueError("run_id 无效，文件必须绑定到当前运行目录。")

    relative_path = Path(file_path)
    if relative_path.is_absolute() or ".." in relative_path.parts:
        raise ValueError("file_path 必须是当前运行结果目录内的相对路径。")
    if not file_path.strip() or file_path.endswith(("/", "\\")):
        raise ValueError("file_path 必须指向具体文件。")

    run_result_root = (RESULT_ROOT / run_id).resolve()
    target = (run_result_root / relative_path).resolve()
    if target != run_result_root and run_result_root not in target.parents:
        raise ValueError("file_path 越权，目标必须位于当前运行结果目录内。")

    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")
    return json.dumps(
        {
            "path": str(target),
            "size": target.stat().st_size,
        },
        ensure_ascii=False,
    )


TOOL_DEFINITIONS = [FILE_TOOL_DEFINITION]
TOOL_HANDLERS = {"create_file": create_file}
