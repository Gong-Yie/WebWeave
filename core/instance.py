import ctypes
import locale
import logging
import os
import shutil
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from ctypes import wintypes
from datetime import datetime, timezone
from pathlib import Path
from threading import RLock
from typing import Any, NoReturn
from uuid import uuid4

from .paths import run_directories
from .run_store import RunStore


PROTOCOL_VERSION = "1.0"
INSTANCE_HOST = "127.0.0.1"
ACTIVE_STATUSES = {"starting", "running", "unhealthy", "stopping"}
MAX_COMMAND_CHARS = 20_000
WEB_INSTANCE_ERROR_CODES = frozenset(
    {
        "INSTANCE_ALREADY_RUNNING",
        "INSTANCE_NOT_FOUND",
        "PORT_IN_USE",
        "STARTUP_TIMEOUT",
        "PROCESS_EXITED",
        "HEALTH_CHECK_FAILED",
        "PROXY_FAILED",
        "STOP_FAILED",
        "INVALID_ARGUMENT",
        "INTERNAL_ERROR",
    }
)
logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class InstanceStartSpec:
    command: str
    cwd: str
    requested_port: int
    health_path: str


@dataclass
class InstanceRecord:
    instance_id: str
    revision: int
    status: str
    pid: int | None
    process_identity: str | None
    host: str
    port: int
    app_url: str
    preview_url: str
    cwd: str
    health_path: str
    log_path: str
    started_at: str
    ready_at: str | None
    updated_at: str
    command: str
    requested_port: int
    error_code: str | None = None
    error_message: str | None = None

    @classmethod
    def from_state(cls, payload: Any) -> "InstanceRecord | None":
        if not isinstance(payload, dict):
            return None
        try:
            return cls(
                instance_id=str(payload["instance_id"]),
                revision=int(payload["revision"]),
                status=str(payload["status"]),
                pid=(int(payload["pid"]) if payload.get("pid") is not None else None),
                process_identity=(
                    str(payload["process_identity"])
                    if payload.get("process_identity") is not None
                    else None
                ),
                host=str(payload["host"]),
                port=int(payload["port"]),
                app_url=str(payload["app_url"]),
                preview_url=str(payload["preview_url"]),
                cwd=str(payload["cwd"]),
                health_path=str(payload["health_path"]),
                log_path=str(payload["log_path"]),
                started_at=str(payload["started_at"]),
                ready_at=(
                    str(payload["ready_at"])
                    if payload.get("ready_at") is not None
                    else None
                ),
                updated_at=str(payload["updated_at"]),
                command=str(payload["command"]),
                requested_port=int(payload["requested_port"]),
                error_code=(
                    str(payload["error_code"])
                    if payload.get("error_code") is not None
                    else None
                ),
                error_message=(
                    str(payload["error_message"])
                    if payload.get("error_message") is not None
                    else None
                ),
            )
        except (KeyError, TypeError, ValueError):
            return None

    def to_state(self) -> dict[str, Any]:
        return asdict(self)

    def to_result(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "protocol_version": PROTOCOL_VERSION,
            "ok": True,
            "instance_id": self.instance_id,
            "revision": self.revision,
            "status": self.status,
            "pid": self.pid,
            "host": self.host,
            "port": self.port,
            "app_url": self.app_url,
            "preview_url": self.preview_url,
            "cwd": self.cwd,
            "health_path": self.health_path,
            "log_path": self.log_path,
            "started_at": self.started_at,
            "ready_at": self.ready_at,
        }
        if self.error_code is not None:
            result["error"] = {
                "code": self.error_code,
                "message": self.error_message or "",
            }
        return result


class InstanceOperationError(RuntimeError):
    def __init__(
        self,
        code: str,
        message: str,
        record: InstanceRecord | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.record = record

    def to_result(self) -> dict[str, Any]:
        result = _empty_result()
        if self.record is not None:
            result.update(self.record.to_result())
        result["ok"] = False
        result["error"] = {"code": self.code, "message": str(self)}
        return result


class WebInstanceManager:
    """管理每个运行 ID 唯一的本地 Web 实例。"""

    def __init__(self) -> None:
        self._locks: dict[str, RLock] = {}
        self._locks_guard = RLock()
        self._processes: dict[tuple[str, int], subprocess.Popen[bytes]] = {}

    def start(
        self,
        run_id: str,
        command: str,
        cwd: str,
        port: int,
        health_path: str,
        timeout: float,
    ) -> dict[str, Any]:
        spec = self._validate_start_spec(command, cwd, port, health_path)
        with self._run_lock(run_id):
            record = self._start_locked(run_id, spec, timeout)
            return record.to_result()

    def status(self, run_id: str) -> dict[str, Any]:
        with self._run_lock(run_id):
            store = RunStore(run_id)
            record = self._require_record(store)
            changed = self._refresh_record(run_id, record)
            if changed:
                self._persist(store, record)
            return record.to_result()

    def stop(self, run_id: str, timeout: float) -> dict[str, Any]:
        with self._run_lock(run_id):
            store = RunStore(run_id)
            record = self._require_record(store)
            return self._stop_locked(store, record, timeout).to_result()

    def restart(self, run_id: str, timeout: float) -> dict[str, Any]:
        with self._run_lock(run_id):
            _validate_timeout(timeout)
            deadline = time.monotonic() + timeout
            store = RunStore(run_id)
            record = self._require_record(store)
            spec = InstanceStartSpec(
                command=record.command,
                cwd=record.cwd,
                requested_port=record.requested_port,
                health_path=record.health_path,
            )
            self._stop_locked(store, record, timeout)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise InstanceOperationError(
                    "STARTUP_TIMEOUT",
                    f"restart 未能在 {timeout:g} 秒内完成。",
                    record,
                )
            return self._start_locked(run_id, spec, remaining).to_result()

    def _start_locked(
        self,
        run_id: str,
        spec: InstanceStartSpec,
        timeout: float,
    ) -> InstanceRecord:
        _validate_timeout(timeout)
        store = RunStore(run_id)
        previous = self._load_record(store)
        if previous is not None and previous.status in ACTIVE_STATUSES:
            if not self._refresh_record(run_id, previous):
                raise InstanceOperationError(
                    "INSTANCE_ALREADY_RUNNING",
                    "当前运行 ID 已有活动 Web 实例。",
                    previous,
                )
            self._persist(store, previous)
            if previous.status in ACTIVE_STATUSES:
                raise InstanceOperationError(
                    "INSTANCE_ALREADY_RUNNING",
                    "当前运行 ID 已有活动 Web 实例。",
                    previous,
                )

        working_directory = _resolve_result_directory(spec.cwd, run_id)
        selected_port = _select_port(spec.requested_port)
        revision = (previous.revision if previous is not None else 0) + 1
        instance_id = previous.instance_id if previous is not None else uuid4().hex
        started_at = _timestamp()
        app_url = f"http://{INSTANCE_HOST}:{selected_port}"
        log_directory = store.run_dir / "instance"
        log_directory.mkdir(parents=True, exist_ok=True)
        record = InstanceRecord(
            instance_id=instance_id,
            revision=revision,
            status="starting",
            pid=None,
            process_identity=None,
            host=INSTANCE_HOST,
            port=selected_port,
            app_url=app_url,
            preview_url=app_url,
            cwd=str(working_directory),
            health_path=spec.health_path,
            log_path=str(log_directory / f"{revision}.log"),
            started_at=started_at,
            ready_at=None,
            updated_at=started_at,
            command=spec.command,
            requested_port=spec.requested_port,
        )
        self._persist(store, record)

        try:
            process = _spawn_process(record)
            record.pid = process.pid
            record.process_identity = _process_identity(process.pid)
            if record.process_identity is None:
                raise InstanceOperationError(
                    "PROCESS_EXITED",
                    "无法确认 Web 实例进程身份。",
                    record,
                )
            self._processes[(run_id, revision)] = process
            self._persist(store, record)
            _wait_until_ready(record, process, timeout)
        except InstanceOperationError as exc:
            self._raise_failed_start(store, run_id, record, exc)
        except OSError as exc:
            start_error = InstanceOperationError(
                "PROCESS_EXITED",
                f"Web 实例进程启动失败: {exc}",
                record,
            )
            self._raise_failed_start(store, run_id, record, start_error)

        record.status = "running"
        record.ready_at = _timestamp()
        record.error_code = None
        record.error_message = None
        self._persist(store, record)
        return record

    def _stop_locked(
        self,
        store: RunStore,
        record: InstanceRecord,
        timeout: float,
    ) -> InstanceRecord:
        _validate_timeout(timeout)
        if record.status == "stopped":
            return record

        deadline = time.monotonic() + timeout
        record.status = "stopping"
        record.error_code = None
        record.error_message = None
        self._persist(store, record)
        try:
            if self._record_process_is_alive(store.run_id, record):
                _terminate_process_tree(
                    record.pid,
                    max(0.1, deadline - time.monotonic()),
                )
            if not self._wait_for_process_exit(
                store.run_id,
                record,
                max(0.0, deadline - time.monotonic()),
            ):
                raise InstanceOperationError(
                    "STOP_FAILED",
                    "Web 实例进程树未能在超时前停止。",
                    record,
                )
        except InstanceOperationError as exc:
            record.error_code = exc.code
            record.error_message = str(exc)
            self._persist(store, record)
            raise
        except (OSError, subprocess.SubprocessError) as exc:
            record.error_code = "STOP_FAILED"
            record.error_message = f"停止 Web 实例失败: {exc}"
            self._persist(store, record)
            raise InstanceOperationError(
                record.error_code,
                record.error_message,
                record,
            ) from exc

        self._processes.pop((store.run_id, record.revision), None)
        record.status = "stopped"
        record.pid = None
        record.process_identity = None
        self._persist(store, record)
        return record

    def _raise_failed_start(
        self,
        store: RunStore,
        run_id: str,
        record: InstanceRecord,
        start_error: InstanceOperationError,
    ) -> NoReturn:
        error = start_error
        try:
            self._cleanup_failed_start(run_id, record)
        except (OSError, subprocess.SubprocessError) as cleanup_error:
            logger.exception("Web 实例启动失败后的进程清理也失败")
            error = InstanceOperationError(
                "STOP_FAILED",
                f"{start_error} 清理失败: {cleanup_error}",
                record,
            )
        record.status = "failed"
        record.error_code = error.code
        record.error_message = str(error)
        self._persist(store, record)
        raise InstanceOperationError(error.code, str(error), record) from start_error

    def _cleanup_failed_start(self, run_id: str, record: InstanceRecord) -> None:
        deadline = time.monotonic() + 5.0
        if self._record_process_is_alive(run_id, record):
            _terminate_process_tree(record.pid, 5.0)
        if not self._wait_for_process_exit(
            run_id,
            record,
            max(0.0, deadline - time.monotonic()),
        ):
            raise OSError("失败的 Web 实例进程树仍在运行")
        self._processes.pop((run_id, record.revision), None)

    @staticmethod
    def _load_record(store: RunStore) -> InstanceRecord | None:
        return InstanceRecord.from_state(store.read_state().get("instance"))

    def _require_record(self, store: RunStore) -> InstanceRecord:
        record = self._load_record(store)
        if record is None:
            raise InstanceOperationError(
                "INSTANCE_NOT_FOUND",
                "当前运行 ID 没有 Web 实例记录。",
            )
        return record

    def _refresh_record(self, run_id: str, record: InstanceRecord) -> bool:
        if record.status not in ACTIVE_STATUSES:
            return False
        if not self._record_process_is_alive(run_id, record):
            record.status = "exited"
            record.error_code = "PROCESS_EXITED"
            record.error_message = "Web 实例进程已退出。"
            return True

        healthy, _, _ = _health_check(record, 1.0)
        if healthy and record.status != "running":
            record.status = "running"
            record.ready_at = record.ready_at or _timestamp()
            record.error_code = None
            record.error_message = None
            return True
        if not healthy and record.status in {"running", "unhealthy"}:
            changed = record.status != "unhealthy"
            record.status = "unhealthy"
            record.error_code = "HEALTH_CHECK_FAILED"
            record.error_message = "Web 实例健康检查失败。"
            return changed
        return False

    def _record_process_is_alive(
        self,
        run_id: str,
        record: InstanceRecord,
    ) -> bool:
        process = self._processes.get((run_id, record.revision))
        if process is not None:
            return process.poll() is None
        return _record_process_is_alive(record)

    def _wait_for_process_exit(
        self,
        run_id: str,
        record: InstanceRecord,
        timeout: float,
    ) -> bool:
        deadline = time.monotonic() + timeout
        while self._record_process_is_alive(run_id, record):
            if time.monotonic() >= deadline:
                return False
            time.sleep(0.05)
        return True

    @staticmethod
    def _persist(store: RunStore, record: InstanceRecord) -> None:
        record.updated_at = _timestamp()
        store.save_instance(record.to_state())

    def _run_lock(self, run_id: str) -> RLock:
        with self._locks_guard:
            return self._locks.setdefault(run_id, RLock())

    @staticmethod
    def _validate_start_spec(
        command: str,
        cwd: str,
        port: int,
        health_path: str,
    ) -> InstanceStartSpec:
        if not isinstance(command, str) or not command.strip():
            raise InstanceOperationError("INVALID_ARGUMENT", "command 不能为空。")
        if len(command) > MAX_COMMAND_CHARS:
            raise InstanceOperationError(
                "INVALID_ARGUMENT",
                f"command 不能超过 {MAX_COMMAND_CHARS} 个字符。",
            )
        if "{host}" not in command or "{port}" not in command:
            raise InstanceOperationError(
                "INVALID_ARGUMENT",
                "command 必须同时包含 {host} 和 {port} 占位符。",
            )
        if not isinstance(cwd, str):
            raise InstanceOperationError("INVALID_ARGUMENT", "cwd 必须是字符串。")
        if (
            isinstance(port, bool)
            or not isinstance(port, int)
            or not 0 <= port <= 65535
        ):
            raise InstanceOperationError(
                "INVALID_ARGUMENT",
                "port 必须是 0 到 65535 之间的整数。",
            )
        if (
            not isinstance(health_path, str)
            or not health_path.startswith("/")
            or health_path.startswith("//")
        ):
            raise InstanceOperationError(
                "INVALID_ARGUMENT",
                "health_path 必须是以单个 / 开头的站内路径。",
            )
        return InstanceStartSpec(command, cwd or ".", port, health_path)


def _empty_result() -> dict[str, Any]:
    return {
        "protocol_version": PROTOCOL_VERSION,
        "ok": False,
        "instance_id": None,
        "revision": None,
        "status": None,
        "pid": None,
        "host": INSTANCE_HOST,
        "port": None,
        "app_url": None,
        "preview_url": None,
        "cwd": None,
        "health_path": None,
        "log_path": None,
        "started_at": None,
        "ready_at": None,
    }


def _timestamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def _validate_timeout(timeout: float) -> None:
    if (
        isinstance(timeout, bool)
        or not isinstance(timeout, (int, float))
        or timeout <= 0
    ):
        raise InstanceOperationError("INVALID_ARGUMENT", "timeout 必须大于 0。")


def _resolve_result_directory(cwd: str, run_id: str) -> Path:
    result_root = run_directories(run_id)["result_dir"].resolve()
    result_root.mkdir(parents=True, exist_ok=True)
    raw_path = Path(cwd)
    target = (
        raw_path.resolve()
        if raw_path.is_absolute()
        else (result_root / raw_path).resolve()
    )
    if target != result_root and result_root not in target.parents:
        raise InstanceOperationError(
            "INVALID_ARGUMENT",
            "cwd 越权，只允许 data/result/{run_id} 及其子目录。",
        )
    if not target.exists():
        raise InstanceOperationError("INVALID_ARGUMENT", f"工作目录不存在: {target}")
    if not target.is_dir():
        raise InstanceOperationError("INVALID_ARGUMENT", f"cwd 不是目录: {target}")
    return target


def _select_port(requested_port: int) -> int:
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            if os.name == "nt":
                probe.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
            probe.bind((INSTANCE_HOST, requested_port))
            return int(probe.getsockname()[1])
    except OSError as exc:
        raise InstanceOperationError(
            "PORT_IN_USE",
            f"端口 {requested_port} 不可用。",
        ) from exc


def _spawn_process(record: InstanceRecord) -> subprocess.Popen[bytes]:
    shell = shutil.which("powershell.exe") or shutil.which("pwsh.exe")
    if shell is None:
        raise OSError("未找到 PowerShell。")
    expanded_command = record.command.replace("{host}", record.host).replace(
        "{port}", str(record.port)
    )
    wrapped_command = (
        f"$env:WEBWEAVE_INSTANCE_ID='{record.instance_id}'; {expanded_command}"
    )
    command_line = [
        shell,
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-Command",
        wrapped_command,
    ]
    popen_kwargs: dict[str, Any] = {
        "cwd": Path(record.cwd),
        "stdin": subprocess.DEVNULL,
        "shell": False,
    }
    if os.name == "nt":
        popen_kwargs["creationflags"] = (
            subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.CREATE_NO_WINDOW
        )
    else:
        popen_kwargs["start_new_session"] = True

    with Path(record.log_path).open("ab") as log_stream:
        return subprocess.Popen(
            command_line,
            stdout=log_stream,
            stderr=subprocess.STDOUT,
            **popen_kwargs,
        )


def _wait_until_ready(
    record: InstanceRecord,
    process: subprocess.Popen[bytes],
    timeout: float,
) -> None:
    deadline = time.monotonic() + timeout
    last_error = ""
    received_http_response = False
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise InstanceOperationError(
                "PROCESS_EXITED",
                f"Web 实例在就绪前退出，退出码: {process.returncode}。",
                record,
            )
        remaining = deadline - time.monotonic()
        healthy, error, responded = _health_check(record, min(1.0, remaining))
        if healthy:
            return
        last_error = error
        received_http_response = received_http_response or responded
        time.sleep(min(0.2, max(0.0, deadline - time.monotonic())))

    code = "HEALTH_CHECK_FAILED" if received_http_response else "STARTUP_TIMEOUT"
    message = f"Web 实例未在 {timeout:g} 秒内通过健康检查。"
    if last_error:
        message += f" 最后错误: {last_error}"
    raise InstanceOperationError(code, message, record)


def _health_check(record: InstanceRecord, timeout: float) -> tuple[bool, str, bool]:
    url = f"{record.app_url}{record.health_path}"
    request = urllib.request.Request(url, headers={"User-Agent": "WebWeave/1.0"})
    try:
        with urllib.request.urlopen(request, timeout=max(timeout, 0.05)) as response:
            status = int(response.status)
            return 200 <= status < 400, f"HTTP {status}", True
    except urllib.error.HTTPError as exc:
        return False, f"HTTP {exc.code}", True
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return False, str(exc), False


def _record_process_is_alive(record: InstanceRecord) -> bool:
    if record.pid is None or record.process_identity is None:
        return False
    return _process_identity(record.pid) == record.process_identity


def _process_identity(pid: int) -> str | None:
    if os.name == "nt":
        return _windows_process_identity(pid)
    stat_path = Path(f"/proc/{pid}/stat")
    try:
        value = stat_path.read_text(encoding="utf-8")
    except OSError:
        return None
    closing_parenthesis = value.rfind(")")
    fields = value[closing_parenthesis + 2 :].split()
    return fields[19] if closing_parenthesis >= 0 and len(fields) > 19 else None


def _windows_process_identity(pid: int) -> str | None:
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel32.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel32.OpenProcess.restype = wintypes.HANDLE
    kernel32.GetProcessTimes.argtypes = [
        wintypes.HANDLE,
        ctypes.POINTER(wintypes.FILETIME),
        ctypes.POINTER(wintypes.FILETIME),
        ctypes.POINTER(wintypes.FILETIME),
        ctypes.POINTER(wintypes.FILETIME),
    ]
    kernel32.GetProcessTimes.restype = wintypes.BOOL
    kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel32.CloseHandle.restype = wintypes.BOOL
    process_handle = kernel32.OpenProcess(0x1000, False, pid)
    if not process_handle:
        return None
    creation = wintypes.FILETIME()
    exit_time = wintypes.FILETIME()
    kernel_time = wintypes.FILETIME()
    user_time = wintypes.FILETIME()
    try:
        success = kernel32.GetProcessTimes(
            process_handle,
            ctypes.byref(creation),
            ctypes.byref(exit_time),
            ctypes.byref(kernel_time),
            ctypes.byref(user_time),
        )
        if not success:
            return None
        value = (creation.dwHighDateTime << 32) | creation.dwLowDateTime
        return str(value)
    finally:
        kernel32.CloseHandle(process_handle)


def _terminate_process_tree(pid: int | None, timeout: float) -> None:
    if pid is None:
        return
    if os.name == "nt":
        completed = subprocess.run(
            ["taskkill", "/PID", str(pid), "/T", "/F"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            check=False,
            timeout=max(timeout, 0.1),
            text=True,
            encoding=locale.getpreferredencoding(False),
            errors="replace",
        )
        if completed.returncode not in {0, 128}:
            detail = completed.stdout.strip()
            message = f"taskkill 返回退出码 {completed.returncode}"
            raise OSError(f"{message}: {detail}" if detail else message)
        return

    os.killpg(pid, signal.SIGTERM)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return
        time.sleep(0.1)
    os.killpg(pid, signal.SIGKILL)


INSTANCE_MANAGER = WebInstanceManager()
