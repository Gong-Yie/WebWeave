import argparse
import http.client
import json
import logging
import mimetypes
import os
import re
import shutil
from collections.abc import Mapping
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, BinaryIO
from urllib.parse import parse_qs, quote, unquote, urlsplit
from uuid import uuid4

from .config import (
    RUN_ROOT,
    WORKSPACE_ROOT,
    runtime_config_payload,
    save_runtime_config,
)
from .instance import INSTANCE_MANAGER, InstanceOperationError
from .paths import prepare_run_directories, run_directories
from .preview import PreviewRoute, proxy_preview_request, resolve_preview_route
from .run_store import RunStore
from skills.loader import SkillLoader
from .tools import resolve_tool_timeout
from .web_jobs import RUN_JOB_MANAGER, RunJobConflictError


logger = logging.getLogger(__name__)
WEBUI_ROOT = (WORKSPACE_ROOT / "webui").resolve()
BACKGROUND_ROOT = (WORKSPACE_ROOT / "data" / "background").resolve()
MAX_JSON_BODY_BYTES = 1024 * 1024
MAX_UPLOAD_BYTES = 50 * 1024 * 1024
MAX_BACKGROUND_BYTES = 2 * 1024 * 1024
MAX_TEXT_PREVIEW_BYTES = 2 * 1024 * 1024
MAX_FILE_ITEMS = 5000
RUN_ID_PATTERN = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
BACKGROUND_CONTENT_TYPES = {
    ".jpeg": "image/jpeg",
    ".jpg": "image/jpeg",
    ".png": "image/png",
    ".webp": "image/webp",
}
TEXT_SUFFIXES = {
    ".css",
    ".csv",
    ".html",
    ".ini",
    ".js",
    ".json",
    ".jsx",
    ".md",
    ".py",
    ".scss",
    ".svg",
    ".toml",
    ".ts",
    ".tsx",
    ".txt",
    ".xml",
    ".yaml",
    ".yml",
}


@dataclass(frozen=True)
class ApiError(RuntimeError):
    status: int
    code: str
    message: str

    def __str__(self) -> str:
        return self.message


class WebWeaveHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class WebWeaveRequestHandler(BaseHTTPRequestHandler):
    server_version = "WebWeave/1.0"

    def do_GET(self) -> None:
        self._handle_request()

    def do_HEAD(self) -> None:
        self._handle_request()

    def do_POST(self) -> None:
        self._handle_request()

    def do_PUT(self) -> None:
        self._handle_request()

    def do_PATCH(self) -> None:
        self._handle_request()

    def do_DELETE(self) -> None:
        self._handle_request()

    def do_OPTIONS(self) -> None:
        self.send_response(HTTPStatus.NO_CONTENT)
        self.send_header("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
        self.end_headers()

    def log_message(self, format: str, *args: Any) -> None:
        logger.info("%s - %s", self.client_address[0], format % args)

    def _handle_request(self) -> None:
        try:
            route = resolve_preview_route(
                self.path,
                self.headers.get("Referer"),
                self.headers.get("Cookie"),
            )
            if route is not None and route.source != "cookie":
                self._proxy_preview(route)
                return
            if self._dispatch_webui_request():
                return
            if route is not None:
                self._proxy_preview(route)
                return
            raise ApiError(404, "NOT_FOUND", "请求的资源不存在。")
        except ApiError as exc:
            self._send_json(
                exc.status,
                {"ok": False, "error": {"code": exc.code, "message": exc.message}},
            )
        except RunJobConflictError as exc:
            self._send_json(
                409,
                {
                    "ok": False,
                    "error": {"code": "RUN_ALREADY_ACTIVE", "message": str(exc)},
                },
            )
        except InstanceOperationError as exc:
            status = 404 if exc.code == "INSTANCE_NOT_FOUND" else 409
            self._send_json(status, exc.to_result())
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as exc:
            self._send_json(
                400,
                {
                    "ok": False,
                    "error": {"code": "INVALID_REQUEST", "message": str(exc)},
                },
            )
        except Exception as exc:
            logger.exception("WebUI 请求处理失败")
            self._send_json(
                500,
                {
                    "ok": False,
                    "error": {"code": "INTERNAL_ERROR", "message": str(exc)},
                },
            )

    def _proxy_preview(self, route: PreviewRoute) -> None:
        instance = RunStore(route.run_id).read_state().get("instance")
        if not isinstance(instance, Mapping) or not instance.get("app_url"):
            raise ApiError(404, "INSTANCE_NOT_FOUND", "实例不存在。")
        try:
            proxy_preview_request(self, route, str(instance["app_url"]))
        except (OSError, http.client.HTTPException) as exc:
            raise ApiError(502, "PROXY_FAILED", str(exc)) from exc

    def _dispatch_webui_request(self) -> bool:
        parsed = urlsplit(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)
        if self.command in {"GET", "HEAD"} and path in {
            "/",
            "/index.html",
            "/app.js",
            "/styles.css",
        }:
            name = "index.html" if path == "/" else path.lstrip("/")
            self._send_static(WEBUI_ROOT / name)
            return True
        if self.command in {"GET", "HEAD"} and path == "/favicon.ico":
            self.send_response(HTTPStatus.NO_CONTENT)
            self.end_headers()
            return True
        if path == "/api/health" and self.command == "GET":
            self._send_json(200, {"ok": True, "service": "webweave"})
            return True
        if path == "/api/skills" and self.command == "GET":
            self._send_json(200, {"ok": True, **_skills_payload()})
            return True
        if path == "/api/config":
            if self.command == "GET":
                self._send_json(200, {"ok": True, "config": runtime_config_payload()})
                return True
            if self.command == "PUT":
                config = save_runtime_config(self._read_json_body())
                self._send_json(200, {"ok": True, "config": config})
                return True
        if path == "/api/appearance" and self.command == "GET":
            self._send_json(200, {"ok": True, "appearance": _appearance_payload()})
            return True
        if path == "/api/appearance/background":
            if self.command in {"GET", "HEAD"}:
                background = _current_background()
                if background is None:
                    raise ApiError(404, "BACKGROUND_NOT_FOUND", "尚未设置背景图。")
                self._send_file(background, cache_control="no-store")
                return True
            if self.command == "PUT":
                filename = query.get("name", [""])[0]
                background = self._save_background(filename)
                self._send_json(
                    200,
                    {"ok": True, "appearance": _appearance_payload(background)},
                )
                return True
            if self.command == "DELETE":
                _remove_background()
                self._send_json(200, {"ok": True, "appearance": _appearance_payload()})
                return True
        if path == "/api/runs":
            if self.command == "GET":
                self._send_json(200, {"ok": True, "runs": _list_runs()})
                return True
            if self.command == "POST":
                run_id = uuid4().hex
                prepare_run_directories(run_id)
                RunStore(run_id).set_status("waiting", "新建对话")
                self._send_json(201, {"ok": True, "run": _run_detail(run_id)})
                return True

        parts = [unquote(item) for item in path.strip("/").split("/")]
        if len(parts) < 3 or parts[:2] != ["api", "runs"]:
            return False
        run_id = _validate_run_id(parts[2])
        if len(parts) == 3 and self.command == "GET":
            self._send_json(200, {"ok": True, "run": _run_detail(run_id)})
            return True
        if len(parts) == 3 and self.command == "DELETE":
            _delete_run(run_id)
            self._send_json(200, {"ok": True, "run_id": run_id})
            return True
        if len(parts) != 4:
            return False

        operation = parts[3]
        if operation == "messages" and self.command == "POST":
            payload = self._read_json_body()
            content = payload.get("content")
            attachments = payload.get("attachments", [])
            if not isinstance(content, str) or not isinstance(attachments, list):
                raise ApiError(400, "INVALID_MESSAGE", "消息或附件格式无效。")
            if not all(isinstance(item, str) for item in attachments):
                raise ApiError(400, "INVALID_MESSAGE", "附件名称必须是字符串。")
            RUN_JOB_MANAGER.submit_message(
                run_id,
                content,
                attachments,
                payload.get("instance_context"),
            )
            self._send_json(202, {"ok": True, "run": _run_detail(run_id)})
            return True
        if operation == "continue" and self.command == "POST":
            RUN_JOB_MANAGER.continue_run(run_id)
            self._send_json(202, {"ok": True, "run": _run_detail(run_id)})
            return True
        if operation == "stop" and self.command == "POST":
            requested = RUN_JOB_MANAGER.stop_run(run_id)
            self._send_json(
                202,
                {"ok": True, "stop_requested": requested, "run": _run_detail(run_id)},
            )
            return True
        if operation == "attachments" and self.command == "POST":
            filename = query.get("name", [""])[0]
            saved = self._save_attachment(run_id, filename)
            self._send_json(201, {"ok": True, "file": saved})
            return True
        if operation == "files" and self.command == "GET":
            self._send_json(200, {"ok": True, "files": _list_files(run_id)})
            return True
        if operation == "file" and self.command in {"GET", "HEAD"}:
            scope = query.get("scope", [""])[0]
            relative_path = query.get("path", [""])[0]
            target = _resolve_run_file(run_id, scope, relative_path)
            if query.get("raw", [""])[0] == "1":
                self._send_file(target)
            else:
                self._send_json(200, {"ok": True, "file": _read_file_preview(target)})
            return True
        if operation == "instance" and self.command == "POST":
            payload = self._read_json_body()
            action = payload.get("action", "status")
            timeout = resolve_tool_timeout(payload.get("timeout"))
            if action == "status":
                result = INSTANCE_MANAGER.status(run_id)
            elif action == "stop":
                result = INSTANCE_MANAGER.stop(run_id, timeout)
            elif action == "restart":
                result = INSTANCE_MANAGER.restart(run_id, timeout)
            else:
                raise ApiError(400, "INVALID_ACTION", "不支持的实例操作。")
            result["preview_url"] = _preview_url(run_id)
            self._send_json(200, result)
            return True
        return False

    def _read_json_body(self) -> dict[str, Any]:
        body = self._read_body(MAX_JSON_BODY_BYTES)
        payload = json.loads(body.decode("utf-8"))
        if not isinstance(payload, dict):
            raise ApiError(400, "INVALID_JSON", "JSON 请求体必须是对象。")
        return payload

    def _read_body(self, maximum: int) -> bytes:
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            raise ApiError(411, "LENGTH_REQUIRED", "请求缺少 Content-Length。")
        try:
            length = int(raw_length)
        except ValueError as exc:
            raise ApiError(400, "INVALID_LENGTH", "Content-Length 无效。") from exc
        if length < 0 or length > maximum:
            raise ApiError(413, "BODY_TOO_LARGE", "请求体超过大小限制。")
        return self.rfile.read(length)

    def _save_attachment(self, run_id: str, raw_name: str) -> dict[str, Any]:
        filename = Path(unquote(raw_name)).name.strip()
        if not filename or filename in {".", ".."} or "\x00" in filename:
            raise ApiError(400, "INVALID_FILENAME", "附件文件名无效。")
        content = self._read_body(MAX_UPLOAD_BYTES)
        resources_root = run_directories(run_id)["resources_dir"].resolve()
        resources_root.mkdir(parents=True, exist_ok=True)
        target = _available_upload_path(resources_root, filename)
        temporary = target.with_name(f".{target.name}.{uuid4().hex}.tmp")
        try:
            temporary.write_bytes(content)
            os.replace(temporary, target)
        finally:
            if temporary.exists():
                temporary.unlink()
        return _file_metadata(target, "resources", resources_root)

    def _save_background(self, raw_name: str) -> Path:
        filename = Path(unquote(raw_name)).name.strip()
        suffix = Path(filename).suffix.lower()
        if not filename or suffix not in BACKGROUND_CONTENT_TYPES:
            raise ApiError(400, "INVALID_BACKGROUND", "背景图必须是 PNG、JPG 或 WebP。")
        content = self._read_body(MAX_BACKGROUND_BYTES)
        if not _matches_image_signature(content, suffix):
            raise ApiError(400, "INVALID_BACKGROUND", "背景图内容与文件格式不匹配。")
        BACKGROUND_ROOT.mkdir(parents=True, exist_ok=True)
        target = BACKGROUND_ROOT / f"background{suffix}"
        temporary = BACKGROUND_ROOT / f".background.{uuid4().hex}.tmp"
        try:
            temporary.write_bytes(content)
            os.replace(temporary, target)
            for candidate in _background_candidates():
                if candidate != target:
                    candidate.unlink(missing_ok=True)
        finally:
            temporary.unlink(missing_ok=True)
        return target

    def _send_static(self, path: Path) -> None:
        target = path.resolve()
        if WEBUI_ROOT not in target.parents or not target.is_file():
            raise ApiError(404, "NOT_FOUND", "静态文件不存在。")
        self._send_file(target, cache_control="no-store")

    def _send_file(self, path: Path, cache_control: str | None = None) -> None:
        content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        size = path.stat().st_size
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(size))
        if cache_control:
            self.send_header("Cache-Control", cache_control)
        self.end_headers()
        if self.command == "HEAD":
            return
        with path.open("rb") as stream:
            _copy_stream(stream, self.wfile)

    def _send_json(self, status: int, payload: Mapping[str, Any]) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)


def _validate_run_id(run_id: str) -> str:
    if not RUN_ID_PATTERN.fullmatch(run_id):
        raise ApiError(400, "INVALID_RUN_ID", "运行 ID 无效。")
    if not (RUN_ROOT / run_id).exists():
        raise ApiError(404, "RUN_NOT_FOUND", "对话不存在。")
    return run_id


def _background_candidates() -> list[Path]:
    return [BACKGROUND_ROOT / f"background{suffix}" for suffix in BACKGROUND_CONTENT_TYPES]


def _current_background() -> Path | None:
    return next((path for path in _background_candidates() if path.is_file()), None)


def _appearance_payload(background: Path | None = None) -> dict[str, str | None]:
    current = background or _current_background()
    if current is None:
        return {"background_url": None}
    return {
        "background_url": (
            f"/api/appearance/background?v={current.stat().st_mtime_ns}"
        )
    }


def _remove_background() -> None:
    for path in _background_candidates():
        path.unlink(missing_ok=True)


def _matches_image_signature(content: bytes, suffix: str) -> bool:
    if suffix == ".png":
        return content.startswith(b"\x89PNG\r\n\x1a\n")
    if suffix in {".jpg", ".jpeg"}:
        return content.startswith(b"\xff\xd8\xff")
    if suffix == ".webp":
        return len(content) >= 12 and content[:4] == b"RIFF" and content[8:12] == b"WEBP"
    return False


def _list_runs() -> list[dict[str, Any]]:
    if not RUN_ROOT.exists():
        return []
    runs = [
        _run_summary(path.name)
        for path in RUN_ROOT.iterdir()
        if path.is_dir() and RUN_ID_PATTERN.fullmatch(path.name)
    ]
    return sorted(runs, key=lambda item: item.get("updated_at", ""), reverse=True)


def _skills_payload() -> dict[str, Any]:
    summaries, errors = SkillLoader().list_skills()
    return {
        "skills": [
            {
                "name": summary.name,
                "description": summary.description,
                "path": summary.path,
            }
            for summary in summaries
        ],
        "errors": errors,
    }


def _delete_run(run_id: str) -> None:
    if RUN_JOB_MANAGER.is_running(run_id):
        raise ApiError(409, "RUN_ACTIVE", "对话正在执行，停止后才能删除。")

    root = RUN_ROOT.resolve()
    target = (root / run_id).resolve()
    if target == root or root not in target.parents:
        raise ApiError(400, "INVALID_RUN_PATH", "对话路径无效。")
    try:
        shutil.rmtree(target)
    except FileNotFoundError as exc:
        raise ApiError(404, "RUN_NOT_FOUND", "对话不存在。") from exc


def _run_summary(run_id: str) -> dict[str, Any]:
    store = RunStore(run_id)
    state = store.read_state()
    events = _read_events(store.events_path)
    title = "新对话"
    for event in events:
        if event.get("type") != "user_message":
            continue
        payload = event.get("payload", {})
        if not isinstance(payload, Mapping):
            continue
        text = payload.get("display_text")
        if not isinstance(text, str):
            item = payload.get("item", {})
            text = item.get("content") if isinstance(item, Mapping) else None
        if isinstance(text, str) and text.strip():
            title = text.strip().splitlines()[0][:120]
            break
    status = str(state.get("status", "waiting"))
    job_active = RUN_JOB_MANAGER.is_running(run_id)
    if job_active and status != "stopping":
        status = "running"
    elif not job_active and status in {"running", "stopping"}:
        status = "stopped"
    return {
        "run_id": run_id,
        "title": title,
        "status": status,
        "updated_at": state.get("updated_at"),
        "event_count": int(state.get("event_count", len(events))),
        "has_instance": isinstance(state.get("instance"), Mapping),
    }


def _run_detail(run_id: str) -> dict[str, Any]:
    summary = _run_summary(run_id)
    store = RunStore(run_id)
    state = store.read_state()
    events = _read_events(store.events_path)
    instance = state.get("instance")
    if isinstance(instance, Mapping):
        instance = dict(instance)
        instance["preview_url"] = _preview_url(run_id)
    else:
        instance = None
    return {
        **summary,
        "state": state,
        "events": events,
        "instance": instance,
        "job_active": RUN_JOB_MANAGER.is_running(run_id),
    }


def _read_events(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    events: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8") as stream:
        for line in stream:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(event, dict):
                events.append(event)
    return events


def _list_files(run_id: str) -> list[dict[str, Any]]:
    files: list[dict[str, Any]] = []
    for key, root in run_directories(run_id).items():
        scope = key.removesuffix("_dir")
        resolved_root = root.resolve()
        if not resolved_root.exists():
            continue
        for path in sorted(resolved_root.rglob("*")):
            if len(files) >= MAX_FILE_ITEMS:
                return files
            try:
                resolved = path.resolve()
            except OSError:
                continue
            if not path.is_file() or resolved_root not in resolved.parents:
                continue
            files.append(_file_metadata(resolved, scope, resolved_root))
    return files


def _resolve_run_file(run_id: str, scope: str, relative_path: str) -> Path:
    directories = run_directories(run_id)
    key = f"{scope}_dir"
    root = directories.get(key)
    if root is None:
        raise ApiError(400, "INVALID_SCOPE", "文件目录范围无效。")
    root = root.resolve()
    raw_path = Path(unquote(relative_path))
    if raw_path.is_absolute():
        raise ApiError(400, "INVALID_PATH", "文件路径必须是相对路径。")
    target = (root / raw_path).resolve()
    if root not in target.parents or not target.is_file():
        raise ApiError(404, "FILE_NOT_FOUND", "文件不存在。")
    return target


def _read_file_preview(path: Path) -> dict[str, Any]:
    size = path.stat().st_size
    content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    is_text = _is_text_file(path, content_type)
    if not is_text:
        return {
            "name": path.name,
            "size": size,
            "mime": content_type,
            "is_text": False,
            "content": None,
        }
    if size > MAX_TEXT_PREVIEW_BYTES:
        raise ApiError(413, "FILE_TOO_LARGE", "文本文件超过预览大小限制。")
    return {
        "name": path.name,
        "size": size,
        "mime": content_type,
        "is_text": True,
        "content": path.read_text(encoding="utf-8", errors="replace"),
    }


def _file_metadata(path: Path, scope: str, root: Path) -> dict[str, Any]:
    content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    return {
        "scope": scope,
        "path": path.relative_to(root).as_posix(),
        "name": path.name,
        "size": path.stat().st_size,
        "mime": content_type,
        "is_text": _is_text_file(path, content_type),
    }


def _is_text_file(path: Path, content_type: str) -> bool:
    return content_type.startswith("text/") or path.suffix.lower() in TEXT_SUFFIXES


def _available_upload_path(root: Path, filename: str) -> Path:
    candidate = (root / filename).resolve()
    if root not in candidate.parents:
        raise ApiError(400, "INVALID_FILENAME", "附件路径越权。")
    if not candidate.exists():
        return candidate
    for index in range(1, 10_000):
        alternative = candidate.with_name(
            f"{candidate.stem} ({index}){candidate.suffix}"
        )
        if not alternative.exists():
            return alternative
    raise ApiError(409, "FILE_CONFLICT", "无法生成不冲突的附件文件名。")


def _preview_url(run_id: str) -> str:
    return f"/preview/{quote(run_id)}/"


def _copy_stream(source: BinaryIO, target: BinaryIO) -> None:
    while chunk := source.read(64 * 1024):
        target.write(chunk)


def serve_webui(host: str = "127.0.0.1", port: int = 8765) -> None:
    if host != "127.0.0.1":
        raise ValueError("WebUI 只允许监听 127.0.0.1。")
    server = WebWeaveHTTPServer((host, port), WebWeaveRequestHandler)
    logger.info("WebWeave WebUI 已启动: http://%s:%s", host, server.server_port)
    try:
        server.serve_forever(poll_interval=0.25)
    except KeyboardInterrupt:
        logger.info("WebWeave WebUI 已停止。")
    finally:
        server.server_close()


def main() -> None:
    parser = argparse.ArgumentParser(description="启动 WebWeave WebUI")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8765)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    serve_webui(args.host, args.port)


if __name__ == "__main__":
    main()
