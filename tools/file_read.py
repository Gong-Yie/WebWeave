import json
import re
from pathlib import Path
from typing import Any


WORKSPACE_ROOT = Path(__file__).resolve().parent.parent
DATA_ROOT = WORKSPACE_ROOT / "data"
ALLOWED_ROOTS = tuple(
    (DATA_ROOT / directory).resolve()
    for directory in ("resources", "result", "download")
)
DEFAULT_MAX_CHARS = 20_000

FILE_READ_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "file_read",
    "description": "读取当前运行 ID 允许目录中的文本文件内容。",
    "parameters": {
        "type": "object",
        "properties": {
            "file_path": {
                "type": "string",
                "description": "要读取的文件路径，可使用项目相对路径或允许目录内的绝对路径。",
            },
            "max_chars": {
                "type": "integer",
                "description": "最多返回的字符数，默认 20000。",
                "minimum": 1,
                "maximum": 100000,
            },
        },
        "required": ["file_path"],
        "additionalProperties": False,
    },
    "strict": False,
}


def _run_roots(run_id: str) -> tuple[Path, Path, Path]:
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", run_id):
        raise ValueError("run_id 无效，文件读取必须绑定到当前运行目录。")
    roots = tuple((root / run_id).resolve() for root in ALLOWED_ROOTS)
    return roots[0], roots[1], roots[2]


def _resolve_target(file_path: str, run_id: str) -> Path:
    resources_root, result_root, download_root = _run_roots(run_id)
    raw_path = Path(file_path)
    if raw_path.is_absolute():
        return raw_path.resolve()

    if raw_path.parts and raw_path.parts[0] in {"resources", "result", "download"}:
        roots = {
            "resources": resources_root,
            "result": result_root,
            "download": download_root,
        }
        return (roots[raw_path.parts[0]] / Path(*raw_path.parts[1:])).resolve()

    return (WORKSPACE_ROOT / raw_path).resolve()


def file_read(
    file_path: str,
    max_chars: int = DEFAULT_MAX_CHARS,
    run_id: str = "",
) -> str:
    """读取当前运行 ID 的 resources、result 或 download 内的文本文件。"""
    if not file_path.strip():
        raise ValueError("file_path 不能为空。")
    if max_chars < 1 or max_chars > 100_000:
        raise ValueError("max_chars 必须在 1 到 100000 之间。")

    target = _resolve_target(file_path, run_id)
    run_roots = _run_roots(run_id)
    if not any(root == target or root in target.parents for root in run_roots):
        raise ValueError(
            "file_path 越权，只允许读取当前运行 ID 的 data/resources、data/result 或 data/download。"
        )
    if not target.exists():
        raise FileNotFoundError(f"文件不存在: {target}")
    if not target.is_file():
        raise ValueError(f"目标不是文件: {target}")

    with target.open("r", encoding="utf-8", errors="replace") as stream:
        content = stream.read(max_chars + 1)
    truncated = len(content) > max_chars
    if truncated:
        content = content[:max_chars]

    return json.dumps(
        {
            "path": str(target),
            "content": content,
            "truncated": truncated,
            "max_chars": max_chars,
        },
        ensure_ascii=False,
    )


TOOL_DEFINITIONS = [FILE_READ_TOOL_DEFINITION]
TOOL_HANDLERS = {"file_read": file_read}
