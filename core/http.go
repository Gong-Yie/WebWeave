package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"webweave/tools"
	"webweave/workspace"
)

type apiError struct {
	status        int
	code, message string
}

func (e apiError) Error() string { return e.message }

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type errorResponse struct {
	OK    bool      `json:"ok"`
	Error errorBody `json:"error"`
}

type runSummary struct {
	RunID       string `json:"run_id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at"`
	EventCount  int    `json:"event_count"`
	HasInstance bool   `json:"has_instance"`
}

type runDetail struct {
	runSummary
	State        runState        `json:"state"`
	Events       []runEvent      `json:"events"`
	Instance     *instanceRecord `json:"instance"`
	JobActive    bool            `json:"job_active"`
	ContextUsage contextUsage    `json:"context_usage"`
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		w.WriteHeader(204)
		return
	}
	if route, ok := resolvePreviewRoute(r); ok && route.source != "cookie" {
		a.servePreview(w, r, route)
		return
	}
	if (r.Method == "GET" || r.Method == "HEAD") && (r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/app.js" || r.URL.Path == "/styles.css") {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		path, err := workspace.ResolveWithin(filepath.Join(a.root, "webui"), name, false)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, path)
		return
	}
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(204)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		payload, status, err := a.handleAPI(w, r)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if status > 0 {
			writeJSON(w, r, status, payload)
		}
		return
	}
	if route, ok := resolvePreviewRoute(r); ok {
		a.servePreview(w, r, route)
		return
	}
	a.writeError(w, r, apiError{404, "NOT_FOUND", "请求的资源不存在"})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		slog.Error("序列化 HTTP 响应失败", "error", err)
		status = 500
		data = []byte(`{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"响应序列化失败"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		if _, err := w.Write(data); err != nil {
			slog.Debug("客户端已断开连接", "error", err)
		}
	}
}

func (a *App) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeJSON(w, r, apiErr.status, errorResponse{false, errorBody{apiErr.code, apiErr.message}})
		return
	}
	var instanceErr instanceError
	if errors.As(err, &instanceErr) {
		status := 409
		if instanceErr.code == "INSTANCE_NOT_FOUND" {
			status = 404
		}
		writeJSON(w, r, status, instanceResult{instanceRecord: instanceErr.record, ProtocolVersion: "1.0", Error: &errorBody{instanceErr.code, instanceErr.message}})
		return
	}
	slog.Error("HTTP 请求处理失败", "path", r.URL.Path, "error", err)
	writeJSON(w, r, 500, errorResponse{false, errorBody{"INTERNAL_ERROR", "请求处理失败，请检查服务日志"}})
}

func readBody(r *http.Request, maximum int64) ([]byte, error) {
	if r.ContentLength < 0 {
		return nil, apiError{411, "LENGTH_REQUIRED", "请求缺少 Content-Length"}
	}
	if r.ContentLength > maximum {
		return nil, apiError{413, "BODY_TOO_LARGE", "请求体超过大小限制"}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil {
		return nil, apiError{400, "INVALID_BODY", "读取请求体失败"}
	}
	if int64(len(data)) > maximum {
		return nil, apiError{413, "BODY_TOO_LARGE", "请求体超过大小限制"}
	}
	return data, nil
}

func readJSONBody(r *http.Request, target any) error {
	data, err := readBody(r, 1024*1024)
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return apiError{400, "INVALID_JSON", "JSON 请求体必须是有效对象"}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return apiError{400, "INVALID_REQUEST", err.Error()}
	}
	return nil
}

func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) (any, int, error) {
	path, method := r.URL.Path, r.Method
	if path == "/api/agents" || strings.HasPrefix(path, "/api/agents/") {
		return a.handleAgents(r)
	}
	if path == "/api/templates" && method == "GET" {
		templates, err := a.listTemplates()
		return map[string]any{"ok": true, "templates": templates}, 200, err
	}
	if path == "/api/models/discover" && method == "POST" {
		return a.discoverModels(r)
	}
	switch path {
	case "/api/health":
		if method == "GET" {
			return map[string]any{"ok": true, "service": "webweave"}, 200, nil
		}
	case "/api/config":
		if method == "GET" {
			return map[string]any{"ok": true, "config": redactedConfig(a.config.get())}, 200, nil
		}
		if method == "PUT" {
			var raw json.RawMessage
			if err := readJSONBody(r, &raw); err != nil {
				return nil, 0, err
			}
			cfg, err := a.config.save(raw)
			if err != nil {
				return nil, 0, apiError{400, "INVALID_REQUEST", err.Error()}
			}
			return map[string]any{"ok": true, "config": cfg}, 200, nil
		}
	case "/api/skills":
		if method == "GET" {
			listing, err := a.skills.List()
			return map[string]any{"ok": true, "skills": listing.Skills, "errors": listing.Errors}, 200, err
		}
	case "/api/appearance":
		if method == "GET" {
			appearance, err := a.appearance()
			return map[string]any{"ok": true, "appearance": appearance}, 200, err
		}
	case "/api/appearance/background":
		return a.handleBackground(w, r)
	case "/api/runs":
		if method == "GET" {
			runs, err := a.listRuns()
			return map[string]any{"ok": true, "runs": runs}, 200, err
		}
		if method == "POST" {
			var request struct {
				AgentID string `json:"agent_id"`
			}
			if r.ContentLength != 0 {
				if err := readJSONBody(r, &request); err != nil {
					return nil, 0, err
				}
			}
			agent, err := findAgent(a.config.get().Agents, request.AgentID)
			if err != nil {
				return nil, 0, err
			}
			id := workspace.NewID()
			if err := a.prepareDirectories(id); err != nil {
				return nil, 0, err
			}
			run, err := a.runtime(id)
			if err != nil {
				return nil, 0, err
			}
			if err := run.store.setStatus("waiting", "新建对话"); err != nil {
				return nil, 0, err
			}
			if err := run.store.selectAgent(agent); err != nil {
				return nil, 0, err
			}
			detail, err := a.detail(id)
			return map[string]any{"ok": true, "run": detail}, 201, err
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "runs" {
		return nil, 0, apiError{404, "NOT_FOUND", "接口不存在"}
	}
	id := parts[2]
	if _, err := a.requireRun(id); err != nil {
		return nil, 0, err
	}
	if len(parts) == 3 {
		if method == "GET" {
			detail, err := a.detail(id)
			return map[string]any{"ok": true, "run": detail}, 200, err
		}
		if method == "DELETE" {
			err := a.deleteRun(id)
			return map[string]any{"ok": true, "run_id": id}, 200, err
		}
	}
	if len(parts) != 4 {
		return nil, 0, apiError{404, "NOT_FOUND", "接口不存在"}
	}
	switch parts[3] {
	case "template":
		if method == "POST" {
			var request templateSaveRequest
			if err := readJSONBody(r, &request); err != nil {
				return nil, 0, err
			}
			template, err := a.saveTemplate(id, request)
			return map[string]any{"ok": true, "template": template}, 201, err
		}
	case "messages":
		if method == "POST" {
			var message struct {
				Content         string   `json:"content"`
				AgentID         string   `json:"agent_id"`
				ReasoningEffort *string  `json:"reasoning_effort"`
				Attachments     []string `json:"attachments"`
				InstanceContext any      `json:"instance_context"`
			}
			if err := readJSONBody(r, &message); err != nil {
				return nil, 0, err
			}
			if strings.TrimSpace(message.Content) == "" {
				return nil, 0, apiError{400, "INVALID_MESSAGE", "消息内容不能为空"}
			}
			for _, name := range message.Attachments {
				if !workspace.ValidFileName(name) {
					return nil, 0, apiError{400, "INVALID_MESSAGE", "附件名称无效"}
				}
				target, err := workspace.ResolveWithin(a.runRoot("resources", id), name, false)
				if err != nil {
					return nil, 0, apiError{400, "INVALID_MESSAGE", err.Error()}
				}
				if info, err := os.Stat(target); err != nil || info.IsDir() {
					return nil, 0, apiError{400, "INVALID_MESSAGE", "附件文件不存在"}
				}
			}
			if err := a.startMessage(id, message.Content, message.AgentID, message.Attachments, message.InstanceContext, false, message.ReasoningEffort); err != nil {
				return nil, 0, err
			}
			detail, err := a.detail(id)
			return map[string]any{"ok": true, "run": detail}, 202, err
		}
	case "continue":
		if method == "POST" {
			var message struct {
				AgentID         string  `json:"agent_id"`
				ReasoningEffort *string `json:"reasoning_effort"`
			}
			if r.ContentLength != 0 {
				if err := readJSONBody(r, &message); err != nil {
					return nil, 0, err
				}
			}
			if err := a.startMessage(id, "", message.AgentID, nil, nil, true, message.ReasoningEffort); err != nil {
				return nil, 0, err
			}
			detail, err := a.detail(id)
			return map[string]any{"ok": true, "run": detail}, 202, err
		}
	case "stop":
		if method == "POST" {
			requested, err := a.stopRun(id)
			if err != nil {
				return nil, 0, err
			}
			detail, err := a.detail(id)
			return map[string]any{"ok": true, "stop_requested": requested, "run": detail}, 202, err
		}
	case "attachments":
		if method == "POST" {
			file, err := a.saveAttachment(r, id)
			return map[string]any{"ok": true, "file": file}, 201, err
		}
	case "files":
		if method == "GET" {
			files, err := a.listFiles(id)
			return map[string]any{"ok": true, "files": files}, 200, err
		}
	case "file":
		if method == "GET" || method == "HEAD" {
			scope, raw := r.URL.Query().Get("scope"), r.URL.Query().Get("path")
			if scope != "resources" && scope != "result" && scope != "download" {
				return nil, 0, apiError{400, "INVALID_SCOPE", "文件目录范围无效"}
			}
			target, err := workspace.ResolveWithin(a.runRoot(scope, id), raw, false)
			if err != nil {
				return nil, 0, apiError{400, "INVALID_PATH", err.Error()}
			}
			info, err := os.Stat(target)
			if err != nil || info.IsDir() {
				return nil, 0, apiError{404, "FILE_NOT_FOUND", "文件不存在"}
			}
			if r.URL.Query().Get("raw") == "1" {
				http.ServeFile(w, r, target)
				return nil, 0, nil
			}
			file := filePreview{Name: info.Name(), Size: info.Size(), MIME: workspace.MIMEType(target), IsText: isTextFile(target)}
			if file.IsText {
				if info.Size() > 2*1024*1024 {
					return nil, 0, apiError{413, "FILE_TOO_LARGE", "文本文件超过预览大小限制"}
				}
				data, err := os.ReadFile(target)
				if err != nil {
					return nil, 0, err
				}
				text := strings.ToValidUTF8(string(data), "�")
				file.Content = &text
			}
			return map[string]any{"ok": true, "file": file}, 200, nil
		}
	case "instance":
		if method == "POST" {
			var args tools.Args
			if err := readJSONBody(r, &args); err != nil {
				return nil, 0, err
			}
			if args.Action == "start" {
				return nil, 0, apiError{400, "INVALID_ACTION", "WebUI 不支持启动操作，请由 Agent 启动实例"}
			}
			timeout, err := tools.ResolveTimeout(tools.Config{DefaultSeconds: a.config.get().ToolTimeout.DefaultSeconds, MaxSeconds: a.config.get().ToolTimeout.MaxSeconds}, args.Timeout)
			if err != nil {
				return nil, 0, apiError{400, "INVALID_REQUEST", err.Error()}
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			result, err := a.instanceAction(ctx, id, args)
			return result, 200, err
		}
	}
	return nil, 0, apiError{404, "NOT_FOUND", "接口不存在"}
}

func (a *App) detail(id string) (runDetail, error) {
	run, err := a.runtime(id)
	if err != nil {
		return runDetail{}, err
	}
	state, events, err := run.store.snapshot()
	if err != nil {
		return runDetail{}, err
	}
	active := a.active(id)
	status := state.Status
	if active && status != "stopping" {
		status = "running"
	} else if !active && (status == "running" || status == "stopping") {
		status = "stopped"
	}
	title := "新对话"
	for _, event := range events {
		if event.Type != "user_message" {
			continue
		}
		text, _ := event.Payload["display_text"].(string)
		if text == "" {
			if item, ok := event.Payload["item"].(map[string]any); ok {
				text, _ = item["content"].(string)
			}
		}
		if text = strings.TrimSpace(text); text != "" {
			line := strings.SplitN(text, "\n", 2)[0]
			runes := []rune(line)
			title = string(runes[:min(len(runes), 120)])
			break
		}
	}
	instance := state.Instance
	if instance != nil {
		copy := *instance
		copy.PreviewURL = previewURL(id)
		instance = &copy
	}
	return runDetail{runSummary{id, title, status, state.UpdatedAt, state.EventCount, instance != nil}, state, events, instance, active, a.runContextUsage(events)}, nil
}

func (a *App) listRuns() ([]runSummary, error) {
	entries, err := os.ReadDir(filepath.Join(a.root, "data", "run"))
	runs := []runSummary{}
	if errors.Is(err, os.ErrNotExist) {
		return runs, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && workspace.ValidName(entry.Name()) {
			detail, err := a.detail(entry.Name())
			if err != nil {
				return nil, err
			}
			runs = append(runs, detail.runSummary)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].UpdatedAt > runs[j].UpdatedAt })
	return runs, nil
}

func (a *App) deleteRun(id string) error {
	run, err := a.runtime(id)
	if err != nil {
		return err
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.job != nil {
		return apiError{409, "RUN_ACTIVE", "对话正在执行，停止后才能删除"}
	}
	run.instanceMu.Lock()
	defer run.instanceMu.Unlock()
	state, err := run.store.readState()
	if err != nil {
		return err
	}
	if state.Instance != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*second)
		err = a.stopInstance(ctx, run, state.Instance)
		cancel()
		if err != nil {
			return err
		}
	}
	target, err := workspace.ResolveWithin(filepath.Join(a.root, "data", "run"), id, false)
	if err != nil {
		return err
	}
	// Keep resources/result/download as the previous API did; only remove run records.
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return nil
}

type fileMetadata struct {
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
	IsText bool   `json:"is_text"`
}

type filePreview struct {
	Name    string  `json:"name"`
	Size    int64   `json:"size"`
	MIME    string  `json:"mime"`
	IsText  bool    `json:"is_text"`
	Content *string `json:"content"`
}

func isTextFile(path string) bool {
	if strings.HasPrefix(workspace.MIMEType(path), "text/") {
		return true
	}
	return strings.Contains(" .css .csv .go .html .ini .js .json .jsx .md .py .scss .svg .toml .ts .tsx .txt .xml .yaml .yml ", " "+strings.ToLower(filepath.Ext(path))+" ")
}

func metadata(path, scope, root string) (fileMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileMetadata{}, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fileMetadata{}, err
	}
	return fileMetadata{scope, filepath.ToSlash(rel), info.Name(), info.Size(), workspace.MIMEType(path), isTextFile(path)}, nil
}

func (a *App) listFiles(id string) ([]fileMetadata, error) {
	files := []fileMetadata{}
	for _, scope := range []string{"resources", "result", "download"} {
		root, err := workspace.CanonicalPath(a.runRoot(scope, id))
		if err != nil {
			return nil, err
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(files) >= 5000 {
				return filepath.SkipAll
			}
			if entry.IsDir() {
				return nil
			}
			resolved, err := workspace.CanonicalPath(path)
			if err != nil {
				return err
			}
			if !workspace.Within(root, resolved) {
				return nil
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			file, err := metadata(path, scope, root)
			if err != nil {
				return err
			}
			files = append(files, file)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (a *App) saveAttachment(r *http.Request, id string) (fileMetadata, error) {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(r.URL.Query().Get("name")), `\`, "/"))
	if !workspace.ValidFileName(name) {
		return fileMetadata{}, apiError{400, "INVALID_FILENAME", "附件文件名无效"}
	}
	data, err := readBody(r, 50*1024*1024)
	if err != nil {
		return fileMetadata{}, err
	}
	run, err := a.runtime(id)
	if err != nil {
		return fileMetadata{}, err
	}
	run.store.mu.Lock()
	defer run.store.mu.Unlock()
	root, err := workspace.CanonicalPath(a.runRoot("resources", id))
	if err != nil {
		return fileMetadata{}, err
	}
	for index := 0; index < 10000; index++ {
		candidate := name
		if index > 0 {
			extension := filepath.Ext(name)
			candidate = strings.TrimSuffix(name, extension) + fmt.Sprintf(" (%d)", index) + extension
		}
		target, err := workspace.ResolveWithin(root, candidate, false)
		if err != nil {
			return fileMetadata{}, err
		}
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fileMetadata{}, err
		}
		if err := workspace.WriteAtomic(target, data, 0644); err != nil {
			return fileMetadata{}, err
		}
		return metadata(target, "resources", root)
	}
	return fileMetadata{}, apiError{409, "FILE_CONFLICT", "无法生成不冲突的附件文件名"}
}

var backgroundExtensions = []string{".jpeg", ".jpg", ".png", ".webp"}

func (a *App) currentBackground() (string, error) {
	for _, extension := range backgroundExtensions {
		path, err := workspace.ResolveWithin(filepath.Join(a.root, "data", "background"), "background"+extension, false)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

func (a *App) appearance() (map[string]any, error) {
	path, err := a.currentBackground()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return map[string]any{"background_url": nil}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"background_url": "/api/appearance/background?v=" + strconv.FormatInt(info.ModTime().UnixNano(), 10)}, nil
}

func (a *App) handleBackground(w http.ResponseWriter, r *http.Request) (any, int, error) {
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if r.Method == "GET" || r.Method == "HEAD" {
		path, err := a.currentBackground()
		if err != nil {
			return nil, 0, err
		}
		if path == "" {
			return nil, 0, apiError{404, "BACKGROUND_NOT_FOUND", "尚未设置背景图"}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, path)
		return nil, 0, nil
	}
	if r.Method != "PUT" && r.Method != "DELETE" {
		return nil, 0, apiError{404, "NOT_FOUND", "接口不存在"}
	}
	var target string
	if r.Method == "PUT" {
		extension := strings.ToLower(filepath.Ext(r.URL.Query().Get("name")))
		data, err := readBody(r, 2*1024*1024)
		if err != nil {
			return nil, 0, err
		}
		valid := extension == ".png" && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) || (extension == ".jpg" || extension == ".jpeg") && bytes.HasPrefix(data, []byte("\xff\xd8\xff")) || extension == ".webp" && len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
		if !valid {
			return nil, 0, apiError{400, "INVALID_BACKGROUND", "背景图必须是内容与格式匹配的 PNG、JPG 或 WebP"}
		}
		target, err = workspace.ResolveWithin(filepath.Join(a.root, "data", "background"), "background"+extension, false)
		if err != nil {
			return nil, 0, err
		}
		if err := workspace.WriteAtomic(target, data, 0644); err != nil {
			return nil, 0, err
		}
	}
	for _, extension := range backgroundExtensions {
		path, err := workspace.ResolveWithin(filepath.Join(a.root, "data", "background"), "background"+extension, false)
		if err != nil {
			return nil, 0, err
		}
		if path != target {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, 0, err
			}
		}
	}
	appearance, err := a.appearance()
	return map[string]any{"ok": true, "appearance": appearance}, 200, err
}
