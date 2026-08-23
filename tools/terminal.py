import json
import os
import shutil
import subprocess
import uuid
from pathlib import Path
from typing import Any

from core.paths import run_directories


MAX_COMMAND_CHARS = 20_000

TERMINAL_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "terminal_run",
    "description": (
        "在当前运行 ID 的隔离工作目录执行 PowerShell 命令。"
        "前台命令返回输出；后台命令返回 PID 和日志，但不会注册为 WebUI 实例。"
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "command": {
                "type": "string",
                "description": "要执行的 PowerShell 命令。",
            },
            "cwd": {
                "type": "string",
                "description": (
                    "工作目录。可使用当前运行的 result/resources/download 子目录，"
                    "默认是 data/result/{run_id}。"
                ),
            },
            "background": {
                "type": "boolean",
                "description": (
                    "是否后台启动并保持进程运行，返回 PID 和日志路径。"
                    "Web 服务必须改用 web_instance。"
                ),
            },
        },
        "required": ["command"],
        "additionalProperties": False,
    },
    "strict": False,
}


def terminal_run(
    command: str,
    cwd: str = "",
    background: bool = False,
    timeout: float = 120.0,
    run_id: str = "",
) -> str:
    """在当前运行目录执行命令，支持前台执行和后台服务进程。"""
    if not command.strip():
        raise ValueError("command 不能为空。")
    if len(command) > MAX_COMMAND_CHARS:
        raise ValueError(f"command 不能超过 {MAX_COMMAND_CHARS} 个字符。")
    if not isinstance(background, bool):
        raise ValueError("background 必须是布尔值。")
    if timeout <= 0:
        raise ValueError("timeout 必须大于 0。")

    shell = _find_powershell()
    working_directory = _resolve_working_directory(cwd, run_id)
    command_line = [
        shell,
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-Command",
        command,
    ]
    if background:
        return _start_background(command_line, working_directory, run_id, command)
    return _run_foreground(command_line, working_directory, timeout, command)


def _find_powershell() -> str:
    command = shutil.which("powershell.exe") or shutil.which("pwsh.exe")
    if not command:
        raise RuntimeError("未找到 PowerShell，无法执行终端命令。")
    return command


def _resolve_working_directory(cwd: str, run_id: str) -> Path:
    directories = run_directories(run_id)
    default_directory = directories["result_dir"]
    if not cwd.strip():
        default_directory.mkdir(parents=True, exist_ok=True)
        return default_directory

    raw_path = Path(cwd)
    target = (
        raw_path.resolve()
        if raw_path.is_absolute()
        else (default_directory / raw_path).resolve()
    )
    allowed = any(
        target == root or root in target.parents for root in directories.values()
    )
    if not allowed:
        raise ValueError(
            "cwd 越权，只允许当前运行 ID 的 data/resources、data/result 或 "
            "data/download 目录。"
        )
    if not target.exists():
        raise FileNotFoundError(f"工作目录不存在: {target}")
    if not target.is_dir():
        raise ValueError(f"cwd 不是目录: {target}")
    return target


def _run_foreground(
    command_line: list[str],
    working_directory: Path,
    timeout: float,
    command: str,
) -> str:
    process = subprocess.Popen(
        command_line,
        cwd=working_directory,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        shell=False,
    )
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        _terminate_process(process)
        raise TimeoutError(f"终端命令执行超过 {timeout:g} 秒。") from exc

    return json.dumps(
        {
            "mode": "foreground",
            "command": command,
            "cwd": str(working_directory),
            "returncode": process.returncode,
            "stdout": _decode(stdout),
            "stderr": _decode(stderr),
        },
        ensure_ascii=False,
    )


def _start_background(
    command_line: list[str],
    working_directory: Path,
    run_id: str,
    command: str,
) -> str:
    run_directory = run_directories(run_id)["result_dir"].parent.parent / "run" / run_id
    log_directory = run_directory / "terminal"
    log_directory.mkdir(parents=True, exist_ok=True)
    log_path = log_directory / f"{uuid.uuid4().hex}.log"

    popen_kwargs: dict[str, Any] = {
        "cwd": working_directory,
        "stdin": subprocess.DEVNULL,
        "stdout": log_path.open("ab"),
        "stderr": subprocess.STDOUT,
        "shell": False,
    }
    if os.name == "nt":
        popen_kwargs["creationflags"] = (
            subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.CREATE_NO_WINDOW
        )
    else:
        popen_kwargs["start_new_session"] = True

    try:
        process = subprocess.Popen(command_line, **popen_kwargs)
    finally:
        popen_kwargs["stdout"].close()

    return json.dumps(
        {
            "mode": "background",
            "command": command,
            "cwd": str(working_directory),
            "pid": process.pid,
            "log_path": str(log_path),
        },
        ensure_ascii=False,
    )


def _terminate_process(process: subprocess.Popen[bytes]) -> None:
    if os.name == "nt":
        subprocess.run(
            ["taskkill", "/PID", str(process.pid), "/T", "/F"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
    else:
        process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def _decode(value: bytes) -> str:
    return value.decode("utf-8", errors="replace")


TOOL_DEFINITIONS = [TERMINAL_TOOL_DEFINITION]
TOOL_HANDLERS = {"terminal_run": terminal_run}
