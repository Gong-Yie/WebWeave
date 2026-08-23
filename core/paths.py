import re
from pathlib import Path

from .config import ALLOWED_DIRS, WORKSPACE_ROOT


def resolve_path(raw: str) -> Path:
    path = Path(raw)
    if not path.is_absolute():
        path = WORKSPACE_ROOT / path
    return path.resolve()


def run_directories(run_id: str) -> dict[str, Path]:
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", run_id):
        raise ValueError("run_id 无效。")
    return {key: root / run_id for key, root in ALLOWED_DIRS.items()}


def prepare_run_directories(run_id: str) -> dict[str, Path]:
    directories = run_directories(run_id)
    for directory in directories.values():
        directory.mkdir(parents=True, exist_ok=True)
    return directories


def is_path_allowed(raw: str, run_id: str) -> bool:
    target = resolve_path(raw)
    directories = run_directories(run_id)
    return any(
        target == root or root in target.parents for root in directories.values()
    )
