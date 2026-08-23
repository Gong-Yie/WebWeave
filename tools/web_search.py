import json
import re
import shutil
import subprocess
from html.parser import HTMLParser
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlsplit


WORKSPACE_ROOT = Path(__file__).resolve().parent.parent
DOWNLOAD_ROOT = WORKSPACE_ROOT / "data" / "download"


class _SearchResultParser(HTMLParser):
    """提取 DuckDuckGo HTML 页面中的搜索结果。"""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.results: list[dict[str, str]] = []
        self._current: dict[str, str] | None = None
        self._capture: str | None = None
        self._capture_tag: str | None = None
        self._buffer: list[str] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        attributes = dict(attrs)
        classes = set((attributes.get("class") or "").split())
        if tag == "a" and "result__a" in classes:
            self._current = {
                "title": "",
                "url": attributes.get("href") or "",
                "snippet": "",
            }
            self._start_capture("title", tag)
        elif self._current is not None and "result__snippet" in classes:
            self._start_capture("snippet", tag)

    def handle_endtag(self, tag: str) -> None:
        if self._capture_tag != tag:
            return
        if self._current is not None and self._capture is not None:
            self._current[self._capture] = " ".join("".join(self._buffer).split())
        if self._capture == "snippet" and self._current is not None:
            if self._current["title"] and self._current["url"]:
                self.results.append(self._current)
            self._current = None
        self._capture = None
        self._capture_tag = None
        self._buffer = []

    def handle_data(self, data: str) -> None:
        if self._capture is not None:
            self._buffer.append(data)

    def _start_capture(self, field: str, tag: str) -> None:
        self._capture = field
        self._capture_tag = tag
        self._buffer = []


WEB_SEARCH_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "web_search",
    "description": "使用 curl 搜索公开网页，或把指定 URL 下载到当前运行目录。",
    "parameters": {
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "要搜索的编程、模板或技术文档问题。",
            },
            "download_url": {
                "type": "string",
                "description": "需要下载的 http 或 https 文件 URL；提供后执行下载。",
            },
            "file_name": {
                "type": "string",
                "description": "下载文件名，只能是单个文件名，不得包含目录。",
            },
        },
        "required": [],
        "additionalProperties": False,
    },
    "strict": False,
}


def web_search(
    query: str = "",
    download_url: str = "",
    file_name: str = "",
    run_id: str = "",
) -> str:
    """使用系统 curl 搜索网页或下载文件。"""
    curl_command = shutil.which("curl.exe") or shutil.which("curl")
    if not curl_command:
        raise RuntimeError("未找到 curl，无法进行联网搜索。")

    if download_url.strip():
        return _download_file(curl_command, download_url, file_name, run_id)
    if not query.strip():
        return "搜索关键词不能为空，或请提供 download_url。"

    return _search_web(curl_command, query)


def _search_web(curl_command: str, query: str) -> str:
    try:
        result = subprocess.run(
            [
                curl_command,
                "--silent",
                "--show-error",
                "--fail-with-body",
                "--max-time",
                "30",
                "--get",
                "https://html.duckduckgo.com/html/",
                "--data-urlencode",
                f"q={query}",
                "--user-agent",
                "WebWeave/1.0",
            ],
            capture_output=True,
            timeout=35,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError("网页搜索请求超时。") from exc
    except OSError as exc:
        raise RuntimeError(f"启动 curl 失败: {exc}") from exc

    stdout = result.stdout.decode("utf-8", errors="replace")
    stderr = result.stderr.decode("utf-8", errors="replace").strip()
    if result.returncode != 0:
        detail = stderr or stdout or f"curl 返回码 {result.returncode}"
        raise RuntimeError(f"网页搜索失败: {detail[:500]}")

    parser = _SearchResultParser()
    parser.feed(stdout)
    results = parser.results[:5]
    return json.dumps(
        {
            "query": query,
            "engine": "DuckDuckGo HTML",
            "results": results,
            "raw": "" if results else stdout[:4000],
        },
        ensure_ascii=False,
        indent=2,
    )


def _download_file(
    curl_command: str,
    download_url: str,
    file_name: str,
    run_id: str,
) -> str:
    parsed_url = urlsplit(download_url)
    if parsed_url.scheme not in {"http", "https"} or not parsed_url.netloc:
        raise ValueError("download_url 必须是有效的 http 或 https URL。")
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", run_id):
        raise ValueError("run_id 无效，下载必须绑定到当前运行目录。")

    default_name = Path(unquote(parsed_url.path)).name or "download"
    target_name = file_name.strip() or default_name
    if (
        not target_name
        or target_name in {".", ".."}
        or "/" in target_name
        or "\\" in target_name
    ):
        raise ValueError("file_name 必须是单个文件名。")

    run_directory = (DOWNLOAD_ROOT / run_id).resolve()
    run_directory.mkdir(parents=True, exist_ok=True)
    target = (run_directory / target_name).resolve()
    if run_directory not in target.parents:
        raise ValueError("下载目标越权。")

    try:
        result = subprocess.run(
            [
                curl_command,
                "--silent",
                "--show-error",
                "--fail-with-body",
                "--location",
                "--max-time",
                "120",
                "--output",
                str(target),
                download_url,
            ],
            capture_output=True,
            timeout=125,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        target.unlink(missing_ok=True)
        raise RuntimeError("文件下载超时。") from exc
    except OSError as exc:
        target.unlink(missing_ok=True)
        raise RuntimeError(f"启动 curl 下载失败: {exc}") from exc

    stdout = result.stdout.decode("utf-8", errors="replace")
    stderr = result.stderr.decode("utf-8", errors="replace").strip()
    if result.returncode != 0:
        target.unlink(missing_ok=True)
        detail = stderr or stdout or f"curl 返回码 {result.returncode}"
        raise RuntimeError(f"文件下载失败: {detail[:500]}")

    return json.dumps(
        {
            "run_id": run_id,
            "url": download_url,
            "path": str(target),
            "size": target.stat().st_size,
        },
        ensure_ascii=False,
        indent=2,
    )


TOOL_DEFINITIONS = [WEB_SEARCH_TOOL_DEFINITION]
TOOL_HANDLERS = {"web_search": web_search}
