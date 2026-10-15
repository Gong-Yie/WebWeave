package core

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"webweave/workspace"
)

//go:embed preview_bridge.js
var bridgeScript string

//go:embed preview_bridge.css
var bridgeStyle string

type previewRoute struct{ id, instanceID, path, source string }

func routeFromPath(raw string) (previewRoute, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return previewRoute{}, false
	}
	parts := strings.SplitN(parsed.Path, "/", 4)
	if len(parts) < 3 || parts[1] != "preview" || !workspace.ValidName(parts[2]) {
		return previewRoute{}, false
	}
	path := "/"
	instanceID := ""
	if len(parts) == 4 {
		tail := parts[3]
		if strings.HasPrefix(tail, "_instances/") {
			segments := strings.SplitN(tail, "/", 3)
			if len(segments) < 2 || !workspace.ValidName(segments[1]) {
				return previewRoute{}, false
			}
			instanceID, tail = segments[1], ""
			if len(segments) == 3 {
				tail = segments[2]
			}
		}
		path += tail
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return previewRoute{id: parts[2], instanceID: instanceID, path: path, source: "direct"}, true
}

func resolvePreviewRoute(r *http.Request) (previewRoute, bool) {
	if route, ok := routeFromPath(r.URL.RequestURI()); ok {
		return route, true
	}
	if route, ok := routeFromPath(r.Referer()); ok {
		route.path, route.source = r.URL.RequestURI(), "referer"
		return route, true
	}
	if cookie, err := r.Cookie("ww_preview_run"); err == nil {
		parts := strings.SplitN(cookie.Value, ":", 2)
		if workspace.ValidName(parts[0]) && (len(parts) == 1 || workspace.ValidName(parts[1])) {
			route := previewRoute{id: parts[0], path: r.URL.RequestURI(), source: "cookie"}
			if len(parts) == 2 {
				route.instanceID = parts[1]
			}
			return route, true
		}
	}
	return previewRoute{}, false
}

func injectBridge(body []byte, baseURL string, scoped bool) []byte {
	html := strings.ToValidUTF8(string(body), "�")
	base := `<base href="` + baseURL + `">`
	if scoped {
		encoded, _ := json.Marshal(baseURL)
		// WebSocket requests may omit Referer; bind same-origin connections before app scripts run.
		base += `<script>(()=>{const base=` + string(encoded) + `,Native=window.WebSocket;if(!Native)return;window.WebSocket=new Proxy(Native,{construct(Target,args){const endpoint=new URL(args[0],document.baseURI.replace(/^http/,"ws"));if(endpoint.host===location.host&&/^wss?:$/.test(endpoint.protocol)&&!endpoint.pathname.startsWith(base)){endpoint.pathname=base+endpoint.pathname.replace(/^\/+/,"");args[0]=endpoint.href}return Reflect.construct(Target,args)}})})();</script>`
	}
	if head := strings.Index(strings.ToLower(html), "<head"); head >= 0 {
		if end := strings.IndexByte(html[head:], '>'); end >= 0 {
			index := head + end + 1
			html = html[:index] + base + html[index:]
		} else {
			html = base + html
		}
	} else {
		html = base + html
	}
	bridge := "<style>" + bridgeStyle + "</style><script>" + bridgeScript + "</script>"
	if end := strings.LastIndex(strings.ToLower(html), "</body>"); end >= 0 {
		html = html[:end] + bridge + html[end:]
	} else {
		html += bridge
	}
	return []byte(html)
}

func (a *App) servePreview(w http.ResponseWriter, r *http.Request, route previewRoute) {
	run, err := a.requireRun(route.id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	state, err := run.store.readState()
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	instance := findInstance(state, route.instanceID)
	if instance == nil {
		a.writeError(w, r, apiError{404, "INSTANCE_NOT_FOUND", "实例不存在"})
		return
	}
	baseURL := previewURL(route.id)
	cookieValue := route.id
	if route.instanceID != "" {
		baseURL = instancePreviewURL(route.id, route.instanceID)
		cookieValue += ":" + route.instanceID
	}
	upstream, err := url.Parse(instance.AppURL)
	if err != nil || upstream.Scheme != "http" || upstream.Hostname() != "127.0.0.1" || upstream.Port() == "" {
		a.writeError(w, r, apiError{400, "INVALID_INSTANCE", "实例地址不是受支持的本地地址"})
		return
	}
	if port, err := strconv.Atoi(upstream.Port()); err != nil || port < 1 || port > 65535 {
		a.writeError(w, r, apiError{400, "INVALID_INSTANCE", "实例端口无效"})
		return
	}
	if r.ContentLength > 50*1024*1024 {
		a.writeError(w, r, apiError{413, "BODY_TOO_LARGE", "代理请求体过大"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			path, _ := url.Parse(route.path)
			request.Out.URL.Path, request.Out.URL.RawPath, request.Out.URL.RawQuery = path.Path, path.RawPath, path.RawQuery
			request.Out.Host = upstream.Host
			if request.In.Header.Get("Origin") != "" {
				request.Out.Header.Set("Origin", upstream.Scheme+"://"+upstream.Host)
			}
			if request.In.Referer() != "" {
				request.Out.Header.Set("Referer", upstream.String()+"/")
			}
			request.Out.Header.Set("Accept-Encoding", "identity")
			var cookies []string
			for _, cookie := range request.In.Cookies() {
				if cookie.Name != "ww_preview_run" {
					cookies = append(cookies, cookie.String())
				}
			}
			request.Out.Header.Del("Cookie")
			if len(cookies) > 0 {
				request.Out.Header.Set("Cookie", strings.Join(cookies, "; "))
			}
		},
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode == http.StatusSwitchingProtocols {
				return nil
			}
			response.Header.Del("Content-Security-Policy")
			response.Header.Del("X-Frame-Options")
			response.Header.Add("Set-Cookie", "ww_preview_run="+cookieValue+"; Path=/; SameSite=Lax")
			if location := response.Header.Get("Location"); location != "" {
				if strings.HasPrefix(location, upstream.String()) {
					location = strings.TrimPrefix(location, upstream.String())
					if location == "" {
						location = "/"
					}
				}
				if strings.HasPrefix(location, "/") {
					response.Header.Set("Location", baseURL+strings.TrimLeft(location, "/"))
				}
			}
			if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") && r.Method != "HEAD" {
				if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
					return errors.New("实例未提供未压缩的 HTML")
				}
				data, err := io.ReadAll(io.LimitReader(response.Body, 50*1024*1024+1))
				response.Body.Close()
				if err != nil {
					return err
				}
				if len(data) > 50*1024*1024 {
					return errors.New("实例 HTML 超过大小限制")
				}
				data = injectBridge(data, baseURL, route.instanceID != "")
				response.Body = io.NopCloser(bytes.NewReader(data))
				response.ContentLength = int64(len(data))
				response.Header.Set("Content-Length", strconv.Itoa(len(data)))
				response.Header.Del("Content-Encoding")
				response.Header.Del("ETag")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a.writeError(w, r, apiError{502, "PROXY_FAILED", "实例代理请求失败"})
		},
	}
	proxy.ServeHTTP(w, r)
}
