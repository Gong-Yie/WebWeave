import http.client
import re
import select
import socket
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler
from typing import Final
from urllib.parse import quote, urlsplit


MAX_PROXY_BODY_BYTES: Final = 50 * 1024 * 1024
HOP_BY_HOP_HEADERS: Final = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
}
RUN_ID_PATTERN: Final = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")


@dataclass(frozen=True)
class PreviewRoute:
    run_id: str
    upstream_path: str
    source: str


def resolve_preview_route(
    request_path: str,
    referer: str | None,
    cookie: str | None,
) -> PreviewRoute | None:
    direct = _route_from_path(request_path)
    if direct is not None:
        return direct

    if referer:
        referenced = _route_from_path(urlsplit(referer).path)
        if referenced is not None:
            return PreviewRoute(referenced.run_id, request_path, "referer")

    match = re.search(r"(?:^|;\s*)ww_preview_run=([A-Za-z0-9_-]{1,64})", cookie or "")
    if match:
        return PreviewRoute(match.group(1), request_path, "cookie")
    return None


def proxy_preview_request(
    handler: BaseHTTPRequestHandler,
    route: PreviewRoute,
    app_url: str,
) -> None:
    parsed = urlsplit(app_url)
    if parsed.hostname != "127.0.0.1" or parsed.port is None:
        raise ValueError("实例地址不是受支持的本地地址。")
    if handler.headers.get("Upgrade", "").lower() == "websocket":
        _proxy_websocket(handler, route, parsed.port)
        return
    _proxy_http(handler, route, parsed.port)


def _route_from_path(path: str) -> PreviewRoute | None:
    parsed_path = urlsplit(path).path
    parts = parsed_path.split("/", 3)
    if len(parts) < 3 or parts[1] != "preview":
        return None
    run_id = parts[2]
    if not RUN_ID_PATTERN.fullmatch(run_id):
        return None
    suffix = "/" + parts[3] if len(parts) == 4 else "/"
    query = urlsplit(path).query
    return PreviewRoute(
        run_id,
        f"{suffix}?{query}" if query else suffix,
        "direct",
    )


def _proxy_http(
    handler: BaseHTTPRequestHandler,
    route: PreviewRoute,
    port: int,
) -> None:
    body = _read_request_body(handler)
    headers = _upstream_headers(handler, port)
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        connection.request(
            handler.command,
            route.upstream_path,
            body=body,
            headers=headers,
        )
        response = connection.getresponse()
        response_body = response.read()
        content_type = response.getheader("Content-Type", "")
        if "text/html" in content_type.lower() and response_body:
            response_body = _inject_bridge(response_body, route.run_id)

        handler.send_response(response.status, response.reason)
        for name, value in response.getheaders():
            lowered = name.lower()
            if lowered in HOP_BY_HOP_HEADERS or lowered in {
                "content-length",
                "content-encoding",
                "content-security-policy",
                "x-frame-options",
            }:
                continue
            if lowered == "location":
                value = _rewrite_location(value, route.run_id, port)
            handler.send_header(name, value)
        handler.send_header(
            "Set-Cookie",
            f"ww_preview_run={route.run_id}; Path=/; SameSite=Lax",
        )
        handler.send_header("Content-Length", str(len(response_body)))
        handler.end_headers()
        if handler.command != "HEAD":
            handler.wfile.write(response_body)
    finally:
        connection.close()


def _proxy_websocket(
    handler: BaseHTTPRequestHandler,
    route: PreviewRoute,
    port: int,
) -> None:
    upstream = socket.create_connection(("127.0.0.1", port), timeout=10)
    try:
        request_lines = [f"GET {route.upstream_path} HTTP/1.1"]
        for name, value in handler.headers.items():
            lowered = name.lower()
            if lowered == "host":
                value = f"127.0.0.1:{port}"
            elif lowered == "origin":
                value = f"http://127.0.0.1:{port}"
            elif lowered == "cookie":
                value = _without_preview_cookie(value)
                if not value:
                    continue
            request_lines.append(f"{name}: {value}")
        upstream.sendall(("\r\n".join(request_lines) + "\r\n\r\n").encode("latin-1"))

        response_head = _receive_headers(upstream)
        handler.connection.sendall(response_head)
        if not response_head.startswith(b"HTTP/1.1 101"):
            return

        handler.close_connection = True
        sockets = [handler.connection, upstream]
        while True:
            readable, _, exceptional = select.select(sockets, [], sockets, 30)
            if exceptional or not readable:
                if exceptional:
                    return
                continue
            for source in readable:
                target = (
                    upstream
                    if source is handler.connection
                    else handler.connection
                )
                chunk = source.recv(65536)
                if not chunk:
                    return
                target.sendall(chunk)
    finally:
        upstream.close()


def _read_request_body(handler: BaseHTTPRequestHandler) -> bytes | None:
    raw_length = handler.headers.get("Content-Length")
    if raw_length is None:
        return None
    try:
        length = int(raw_length)
    except ValueError as exc:
        raise ValueError("无效的 Content-Length。") from exc
    if length < 0 or length > MAX_PROXY_BODY_BYTES:
        raise ValueError("代理请求体过大。")
    return handler.rfile.read(length)


def _upstream_headers(
    handler: BaseHTTPRequestHandler,
    port: int,
) -> dict[str, str]:
    headers: dict[str, str] = {}
    for name, value in handler.headers.items():
        lowered = name.lower()
        if lowered in HOP_BY_HOP_HEADERS or lowered in {
            "host",
            "content-length",
            "accept-encoding",
        }:
            continue
        if lowered == "origin":
            value = f"http://127.0.0.1:{port}"
        elif lowered == "referer":
            value = f"http://127.0.0.1:{port}/"
        elif lowered == "cookie":
            value = _without_preview_cookie(value)
            if not value:
                continue
        headers[name] = value
    headers["Host"] = f"127.0.0.1:{port}"
    headers["Accept-Encoding"] = "identity"
    return headers


def _without_preview_cookie(value: str) -> str:
    return "; ".join(
        item.strip()
        for item in value.split(";")
        if item.strip() and not item.strip().startswith("ww_preview_run=")
    )


def _receive_headers(connection: socket.socket) -> bytes:
    chunks = bytearray()
    while b"\r\n\r\n" not in chunks:
        chunk = connection.recv(4096)
        if not chunk:
            break
        chunks.extend(chunk)
        if len(chunks) > 65536:
            raise OSError("WebSocket 响应头过大。")
    return bytes(chunks)


def _rewrite_location(value: str, run_id: str, port: int) -> str:
    local_origin = f"http://127.0.0.1:{port}"
    if value.startswith(local_origin):
        value = value[len(local_origin) :] or "/"
    if value.startswith("/"):
        return f"/preview/{quote(run_id)}/{value.lstrip('/')}"
    return value


def _inject_bridge(body: bytes, run_id: str) -> bytes:
    html = body.decode("utf-8", errors="replace")
    base = f'<base href="/preview/{quote(run_id)}/">'
    bridge = f"<style>{_BRIDGE_STYLE}</style><script>{_BRIDGE_SCRIPT}</script>"
    head_index = html.lower().find("<head")
    if head_index >= 0:
        head_end = html.find(">", head_index)
        html = html[: head_end + 1] + base + html[head_end + 1 :]
    else:
        html = base + html
    body_end = html.lower().rfind("</body>")
    if body_end >= 0:
        html = html[:body_end] + bridge + html[body_end:]
    else:
        html += bridge
    return html.encode("utf-8")


_BRIDGE_STYLE = """
#ww-inspect-outline{position:fixed;z-index:2147483646;display:none;border:2px solid #1677ff;background:rgba(22,119,255,.08);pointer-events:none;box-sizing:border-box}#ww-inspect-point{position:absolute;z-index:2147483647;display:none;width:12px;height:12px;border:3px solid #fff;border-radius:50%;background:#c83d35;box-shadow:0 0 0 2px #c83d35;pointer-events:none;transform:translate(-50%,-50%)}html[data-ww-mode="component"] *,html[data-ww-mode="point"] *{cursor:crosshair!important}
""".strip()


_BRIDGE_SCRIPT = """
(()=>{const outline=document.createElement("div"),point=document.createElement("div");outline.id="ww-inspect-outline";point.id="ww-inspect-point";document.documentElement.append(outline,point);let mode="preview";const esc=v=>window.CSS&&CSS.escape?CSS.escape(v):String(v).replace(/[^a-zA-Z0-9_-]/g,"\\$&");const selector=el=>{if(el.id)return`#${esc(el.id)}`;const explicit=el.getAttribute("data-ww-selector")||el.getAttribute("data-selector");if(explicit)return explicit;const parts=[];for(let node=el;node&&node.nodeType===1&&parts.length<5;node=node.parentElement){let part=node.tagName.toLowerCase();if(node.classList.length)part+=`.`+[...node.classList].slice(0,2).map(esc).join(".");if(node.parentElement){const peers=[...node.parentElement.children].filter(item=>item.tagName===node.tagName);if(peers.length>1)part+=`:nth-of-type(${peers.indexOf(node)+1})`}parts.unshift(part)}return parts.join(" > ")};const targetFor=el=>el.closest("[data-ww-component],[data-component]")||el;const detail=el=>{const target=targetFor(el),rect=target.getBoundingClientRect();return{component:target.getAttribute("data-ww-component")||target.getAttribute("data-component")||target.tagName.toLowerCase(),selector:selector(target),source:target.getAttribute("data-ww-source")||null,tag:target.tagName.toLowerCase(),text:(target.innerText||target.getAttribute("aria-label")||"").trim().slice(0,180),rect:{x:Math.round(rect.x),y:Math.round(rect.y),width:Math.round(rect.width),height:Math.round(rect.height)}}};const show=el=>{const rect=targetFor(el).getBoundingClientRect();Object.assign(outline.style,{display:"block",left:`${rect.left}px`,top:`${rect.top}px`,width:`${rect.width}px`,height:`${rect.height}px`})};document.addEventListener("pointermove",event=>{if(mode==="component")show(event.target)},true);document.addEventListener("click",event=>{if(mode==="preview")return;event.preventDefault();event.stopImmediatePropagation();const data=detail(event.target);if(mode==="component")show(event.target);else{const x=Math.round(event.pageX),y=Math.round(event.pageY);Object.assign(point.style,{display:"block",left:`${x}px`,top:`${y}px`});data.point={x,y,clientX:Math.round(event.clientX),clientY:Math.round(event.clientY)}}parent.postMessage({type:"webweave:selection",mode,selection:data},location.origin)},true);addEventListener("message",event=>{if(event.source!==parent||event.data?.type!=="webweave:set-mode")return;mode=["preview","component","point"].includes(event.data.mode)?event.data.mode:"preview";document.documentElement.dataset.wwMode=mode;if(mode==="preview"){outline.style.display="none";point.style.display="none"}},false);parent.postMessage({type:"webweave:preview-ready"},location.origin)})();
""".strip()
