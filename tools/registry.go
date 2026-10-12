package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"webweave/skills"
	"webweave/workspace"
)

type Settings struct {
	Enabled   bool `json:"enabled"`
	RunScoped bool `json:"run_scoped"`
}

func (t *Settings) UnmarshalJSON(data []byte) error {
	type plain Settings
	value := plain{Enabled: true}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = Settings(value)
	return nil
}

type Config struct {
	Tools                      map[string]Settings
	DefaultSeconds, MaxSeconds float64
}

type Executor struct {
	root     string
	skills   *skills.Loader
	instance func(context.Context, string, Args) (any, error)
}

func NewExecutor(root string, loader *skills.Loader, instance func(context.Context, string, Args) (any, error)) *Executor {
	return &Executor{root: root, skills: loader, instance: instance}
}

func (e *Executor) runRoot(scope, id string) string { return workspace.ScopeRoot(e.root, scope, id) }

type toolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func Definitions(settings map[string]Settings) []responses.ToolUnionParam {
	specs := []toolSpec{
		{"list_skills", "发现当前 Agent 可用的本地 Skill，返回名称、描述和路径", map[string]any{}, []string{}},
		{"read_skill", "读取指定 Skill 的完整指令，先使用 list_skills 获取名称", map[string]any{"name": stringProperty("Skill 名称")}, []string{"name"}},
		{"create_file", "在当前运行的 result 目录创建或覆盖文本文件", map[string]any{"file_path": stringProperty("相对于当前结果目录的文件路径"), "content": stringProperty("完整文本内容")}, []string{"file_path", "content"}},
		{"file_read", "读取当前运行 resources、result 或 download 内的文本文件", map[string]any{"file_path": stringProperty("项目相对路径、允许的绝对路径或 result/resources/download 下的路径"), "max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}}, []string{"file_path"}},
		{"web_search", "搜索公开网页，或将指定 URL 下载到当前运行 download 目录", map[string]any{"query": stringProperty("搜索关键词"), "download_url": stringProperty("http 或 https 下载地址"), "file_name": stringProperty("单个文件名，不含目录")}, []string{}},
		{"terminal_run", "在当前运行的隔离工作目录执行 PowerShell 命令；Web 服务必须用 web_instance", map[string]any{"command": stringProperty("PowerShell 命令"), "cwd": stringProperty("工作目录，默认当前 result 目录"), "background": map[string]any{"type": "boolean"}}, []string{"command"}},
		{"web_instance", "管理当前运行唯一的 Web 实例，支持 start、status、stop、restart", map[string]any{"action": map[string]any{"type": "string", "enum": []string{"start", "status", "stop", "restart"}}, "command": stringProperty("start 使用的命令，必须包含 {host} 和 {port}"), "cwd": stringProperty("相对于当前 result 目录的工作目录"), "port": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535}, "health_path": stringProperty("站内健康检查路径，默认 /")}, []string{"action"}},
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	definitions := []responses.ToolUnionParam{}
	for _, spec := range specs {
		settings, ok := settings[spec.Name]
		if !ok || !settings.Enabled {
			continue
		}
		spec.Properties["timeout"] = map[string]any{"type": "number", "minimum": 0.1, "description": "本次调用的最长时间（秒）"}
		required := append(spec.Required, "timeout")
		definitions = append(definitions, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: spec.Name, Description: openai.String(spec.Description), Strict: openai.Bool(false), Parameters: map[string]any{"type": "object", "properties": spec.Properties, "required": required, "additionalProperties": false}}})
	}
	return definitions
}

type Args struct {
	Timeout     *float64 `json:"timeout"`
	FilePath    string   `json:"file_path"`
	Content     *string  `json:"content"`
	MaxChars    *int     `json:"max_chars"`
	Name        string   `json:"name"`
	Query       string   `json:"query"`
	DownloadURL string   `json:"download_url"`
	FileName    string   `json:"file_name"`
	Command     string   `json:"command"`
	Cwd         string   `json:"cwd"`
	Background  bool     `json:"background"`
	Action      string   `json:"action"`
	Port        *int     `json:"port"`
	HealthPath  string   `json:"health_path"`
}

func ResolveTimeout(cfg Config, requested *float64) (time.Duration, error) {
	seconds := cfg.DefaultSeconds
	if requested != nil {
		seconds = *requested
	}
	if seconds <= 0 {
		return 0, errors.New("timeout 必须为正数")
	}
	seconds = min(seconds, cfg.MaxSeconds)
	return time.Duration(seconds * float64(time.Second)), nil
}

func (e *Executor) Execute(ctx context.Context, id, name, raw string, cfg Config) string {
	settings, ok := cfg.Tools[name]
	if !ok || !settings.Enabled {
		return "工具已禁用或不存在: " + name
	}
	var args Args
	if raw == "" {
		raw = "{}"
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return "工具参数必须是 JSON 对象"
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "工具参数解析失败(" + name + "): " + err.Error()
	}
	if !settings.RunScoped && (name == "create_file" || name == "file_read" || name == "terminal_run" || name == "web_instance" || name == "web_search" && args.DownloadURL != "") {
		return "工具执行失败(" + name + "): 工具必须启用运行隔离才能访问当前运行目录"
	}
	timeout, err := ResolveTimeout(cfg, args.Timeout)
	if err != nil {
		return "工具执行失败(" + name + "): " + err.Error()
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var value any
	switch name {
	case "list_skills":
		value, err = e.skills.List()
	case "read_skill":
		value, err = e.skills.Read(args.Name)
	case "create_file":
		value, err = e.createFile(toolCtx, id, args)
	case "file_read":
		value, err = e.readFile(id, args)
	case "web_search":
		value, err = e.webSearch(toolCtx, id, args)
	case "terminal_run":
		value, err = e.terminalRun(toolCtx, id, args)
	case "web_instance":
		value, err = e.instance(toolCtx, id, args)
	default:
		err = errors.New("工具不存在")
	}
	if err != nil {
		return "工具执行失败(" + name + "): " + err.Error()
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "工具结果序列化失败: " + err.Error()
	}
	return string(data)
}
