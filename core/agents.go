package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"webweave/tools"
	"webweave/workspace"
)

type AgentConfig struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

func defaultAgents() []AgentConfig {
	return []AgentConfig{
		{"main", "主Agent", "统筹任务，调用其他 Agent 并整合结果。", "你是主 Agent，负责理解完整需求、规划任务、协调所有专业 Agent 并交付最终结果。按需使用 call_agent 委派设计、编程、审计或自定义任务，明确目标、文件路径和验收要求，读取委派结果后继续推进。"},
		{"design", "网站灵感设计Agent", "探索网站灵感、视觉风格与交互设计。", "你是网站灵感设计 Agent，负责网站参考资料、视觉方向、页面结构、排版、配色与交互设计。先读取相关资源，按需搜索灵感，产出具体且可实施的设计方案。被委派时只完成委派范围，并给出方案、依据和交付文件路径。"},
		{"code", "代码编程Agent", "编写和修改代码，完成网站功能。", "你是代码编程 Agent，负责阅读已有代码、实现网站功能与修复问题。沿用项目已有技术栈，以最小改动满足需求，按需运行相关验证，报告实际修改的文件和验证结果。被委派时只完成委派范围。"},
		{"audit", "代码审计Agent", "检查代码正确性、安全边界与实现质量。", "你是代码审计 Agent，负责审查代码正确性、安全性、需求符合度和关键边界。先阅读实际源码，报告有证据的问题、严重程度、文件位置和修复建议。默认只审查；只有任务明确要求修复时才修改文件。不得伪造测试或审查结果。"},
	}
}

func builtInAgent(id string) bool {
	return id == "main" || id == "design" || id == "code" || id == "audit"
}

func validateAgents(agents []AgentConfig) error {
	seen := make(map[string]bool, len(agents))
	for i := range agents {
		agent := &agents[i]
		agent.ID = strings.TrimSpace(agent.ID)
		agent.Name = strings.TrimSpace(agent.Name)
		agent.Description = strings.TrimSpace(agent.Description)
		agent.Prompt = strings.TrimSpace(agent.Prompt)
		if !workspace.ValidName(agent.ID) || agent.Name == "" || agent.Prompt == "" {
			return errors.New("Agent ID 必须是有效名称，名称和角色指令不能为空")
		}
		if seen[agent.ID] {
			return fmt.Errorf("Agent ID 重复: %s", agent.ID)
		}
		seen[agent.ID] = true
	}
	for _, agent := range defaultAgents() {
		if !seen[agent.ID] {
			return fmt.Errorf("不能移除默认 Agent: %s", agent.Name)
		}
	}
	return nil
}

func findAgent(agents []AgentConfig, id string) (AgentConfig, error) {
	if id == "" {
		id = "main"
	}
	for _, agent := range agents {
		if agent.ID == id {
			return agent, nil
		}
	}
	return AgentConfig{}, apiError{400, "INVALID_AGENT", "Agent 不存在，请重新选择"}
}

// Agent changes share the configuration lock so saving an agent preserves secrets
// and unrelated settings, including changes made through the configuration page.
func (m *configManager) changeAgent(agent AgentConfig, method string) ([]AgentConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadLocked()
	cfg := m.config
	cfg.Agents = append([]AgentConfig(nil), cfg.Agents...)
	index := -1
	for i, existing := range cfg.Agents {
		if existing.ID == agent.ID {
			index = i
			break
		}
	}
	if method == "POST" {
		if agent.ID == "" {
			agent.ID = workspace.NewID()
		}
		if index >= 0 {
			return nil, apiError{409, "AGENT_EXISTS", "Agent ID 已存在"}
		}
		cfg.Agents = append(cfg.Agents, agent)
	} else {
		if index < 0 {
			return nil, apiError{404, "AGENT_NOT_FOUND", "Agent 不存在"}
		}
		if method == "DELETE" {
			if builtInAgent(agent.ID) {
				return nil, apiError{400, "BUILTIN_AGENT", "默认 Agent 可以编辑，但不能删除"}
			}
			cfg.Agents = append(cfg.Agents[:index], cfg.Agents[index+1:]...)
		} else {
			cfg.Agents[index] = agent
		}
	}
	if err := validateAgents(cfg.Agents); err != nil {
		return nil, apiError{400, "INVALID_AGENT", err.Error()}
	}
	if err := workspace.WriteJSONAtomic(m.path, cfg); err != nil {
		return nil, err
	}
	m.config, m.loaded = cfg, false
	return append([]AgentConfig(nil), cfg.Agents...), nil
}

func (a *App) handleAgents(r *http.Request) (any, int, error) {
	id := strings.TrimPrefix(r.URL.Path, "/api/agents")
	if id == "" && r.Method == "GET" {
		return map[string]any{"ok": true, "agents": a.config.get().Agents}, 200, nil
	}
	if (id == "" && r.Method != "POST") || (id != "" && r.Method != "PUT" && r.Method != "DELETE") {
		return nil, 0, apiError{404, "NOT_FOUND", "接口不存在"}
	}
	var agent AgentConfig
	if r.Method != "DELETE" {
		if err := readJSONBody(r, &agent); err != nil {
			return nil, 0, err
		}
	}
	if id != "" {
		agent.ID = strings.TrimPrefix(id, "/")
		if !workspace.ValidName(agent.ID) {
			return nil, 0, apiError{400, "INVALID_AGENT", "Agent ID 无效"}
		}
	}
	agents, err := a.config.changeAgent(agent, r.Method)
	status := 200
	if r.Method == "POST" {
		status = 201
	}
	return map[string]any{"ok": true, "agents": agents}, status, err
}

type agentExecution struct {
	agent     AgentConfig
	delegated bool
}

type agentExecutionKey struct{}

func appendAgentEvent(ctx context.Context, store *runStore, kind string, payload map[string]any) error {
	if execution, ok := ctx.Value(agentExecutionKey{}).(agentExecution); ok {
		payload["agent_id"] = execution.agent.ID
		payload["agent_name"] = execution.agent.Name
		payload["delegated"] = execution.delegated
	}
	return store.append(kind, payload)
}

func saveAgentContext(ctx context.Context, store *runStore, items []inputItem, summary any) error {
	if execution, ok := ctx.Value(agentExecutionKey{}).(agentExecution); ok && execution.delegated {
		return nil
	}
	return store.saveContext(items, summary)
}

func agentInstructions(agent AgentConfig, cfg RuntimeConfig, id string) string {
	instructions := strings.NewReplacer("{run_id}", id, "{timeout_default}", fmt.Sprintf("%g", cfg.ToolTimeout.DefaultSeconds), "{timeout_max}", fmt.Sprintf("%g", cfg.ToolTimeout.MaxSeconds)).Replace(systemPrompt)
	instructions += "\n\n当前 Agent：" + agent.Name + "（ID：" + agent.ID + "）\n角色指令：\n" + agent.Prompt
	if agent.ID == "main" {
		instructions += "\n可通过 call_agent 调用以下 Agent。它们共享当前运行文件目录，但有独立的任务上下文；务必提供完整任务、必要背景和文件路径。\n"
		for _, target := range cfg.Agents {
			if target.ID != "main" {
				instructions += target.ID + "：" + target.Name + "；" + target.Description + "\n"
			}
		}
	}
	return instructions
}

func agentTools(agent AgentConfig, cfg RuntimeConfig) []responses.ToolUnionParam {
	definitions := tools.Definitions(cfg.Tools)
	if agent.ID == "main" {
		ids := []string{}
		for _, target := range cfg.Agents {
			if target.ID != "main" {
				ids = append(ids, target.ID)
			}
		}
		definitions = append(definitions, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: "call_agent", Description: openai.String("委派任务给专业或自定义 Agent，返回执行结果；共享当前运行目录"), Strict: openai.Bool(false),
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"agent_id": map[string]any{"type": "string", "enum": ids},
				"task":     map[string]any{"type": "string", "description": "完整任务、背景、相关文件路径和预期结果"},
				"timeout":  map[string]any{"type": "number", "minimum": 0.1},
			}, "required": []string{"agent_id", "task", "timeout"}, "additionalProperties": false},
		}})
	}
	return definitions
}

func (a *App) callAgent(ctx context.Context, store *runStore, caller AgentConfig, raw string) string {
	fail := func(err error) string {
		data, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		return string(data)
	}
	if caller.ID != "main" {
		return fail(errors.New("只有主 Agent 可以调用其他 Agent"))
	}
	var args struct {
		AgentID string   `json:"agent_id"`
		Task    string   `json:"task"`
		Timeout *float64 `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return fail(fmt.Errorf("委派参数无效: %w", err))
	}
	if args.AgentID == "" || args.AgentID == "main" || strings.TrimSpace(args.Task) == "" {
		return fail(errors.New("必须选择其他 Agent 并填写非空任务"))
	}
	cfg := a.config.get()
	agent, err := findAgent(cfg.Agents, args.AgentID)
	if err != nil {
		return fail(err)
	}
	timeout, err := tools.ResolveTimeout(tools.Config{DefaultSeconds: cfg.ToolTimeout.DefaultSeconds, MaxSeconds: cfg.ToolTimeout.MaxSeconds}, args.Timeout)
	if err != nil {
		return fail(err)
	}
	delegateCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	text, err := a.runAgent(delegateCtx, store, []inputItem{{"role": "user", "content": args.Task}}, agent, true)
	if err != nil {
		return fail(fmt.Errorf("%s 执行失败: %w", agent.Name, err))
	}
	data, _ := json.Marshal(map[string]any{"ok": true, "agent_id": agent.ID, "agent_name": agent.Name, "output": text})
	return string(data)
}
