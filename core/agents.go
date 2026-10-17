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
	ModelID     string `json:"model_id,omitempty"`
}

func defaultAgents() []AgentConfig {
	agents := legacyDefaultAgents()
	agents[0].Description = "引导需求、维护项目共识、协调阶段任务并完成交付。"
	agents[0].Prompt = "你是主 Agent，是用户沟通和决策汇总入口。面向弱代码用户，用通俗问题和实例预览逐步确定需求，维护用户原话、已确认决定、推断和待确认事项。按任务复杂度委派设计、前端工程和质量检查；核对交付后继续推进，常规实现自主完成，关键节点等待用户确认。"
	agents[1].Name = "前端设计Agent"
	agents[1].Description = "设计整页或局部视觉与交互，按需制作可预览候选样例。"
	agents[1].Prompt = "你是前端设计 Agent，负责页面结构、视觉方向、排版、配色与交互方案，也负责复杂前端美化和局部组件设计。先理解已有项目和约束，用户需要比较方向时制作代表性页面或局部预览，不完整开发每个候选方案。交接方案、文件路径、instance_id，并说明哪些代码可复用、哪些只是示意，正式实现交由前端工程 Agent。"
	agents[2].Name = "前端工程Agent"
	agents[2].Description = "理解现有项目，生成或优化前端、微调样式、对接接口并准备交付。"
	agents[2].Prompt = "你是前端工程 Agent，负责从零制作网页、优化复杂前端、局部样式调整、为已有后端生成前端和对接接口。先读取源码与接口资料，沿用现有技术栈，在已确认范围内以最小改动实现。复用选定方案和已有实例，按风险验证，交付实际文件、预览和运行或部署配置；接口未知时明确说明，不伪装已完成真实对接。"
	agents[3].Name = "质量检查Agent"
	agents[3].Description = "只读检查需求符合度、关键功能、接口及安全边界，报告证据与结论。"
	agents[3].Prompt = "你是质量检查 Agent，保持只读。根据委派验收条件检查关键交互、接口对接、运行问题和安全边界，报告有证据的问题、影响程度、文件位置及修复建议，修复交由前端工程 Agent。区分源码检查和真实运行验证，不把静态阅读说成测试通过；复用仍适用于当前文件的检查结论，不反复检查轻微风格问题。"
	return agents
}

// Exact legacy defaults may be upgraded; user-edited roles and model choices stay intact.
func upgradeAgentRoles(agents []AgentConfig) {
	legacy, current := legacyDefaultAgents(), defaultAgents()
	for i := range agents {
		for j, old := range legacy {
			if agents[i].ID == old.ID && agents[i].Name == old.Name && agents[i].Description == old.Description && agents[i].Prompt == old.Prompt {
				modelID := agents[i].ModelID
				agents[i] = current[j]
				agents[i].ModelID = modelID
				break
			}
		}
	}
}

func legacyDefaultAgents() []AgentConfig {
	return []AgentConfig{
		{"main", "主Agent", "统筹任务，调用其他 Agent 并整合结果。", "你是主 Agent，负责理解完整需求、规划任务、协调所有专业 Agent 并交付最终结果。按需使用 call_agent 委派设计、编程、审计或自定义任务，明确目标、文件路径和验收要求，读取委派结果后继续推进。", ""},
		{"design", "网站灵感设计Agent", "探索网站灵感、视觉风格与交互设计。", "你是网站灵感设计 Agent，负责网站参考资料、视觉方向、页面结构、排版、配色与交互设计。先读取相关资源，按需搜索灵感，产出具体且可实施的设计方案。被委派时只完成委派范围，并给出方案、依据和交付文件路径。", ""},
		{"code", "代码编程Agent", "编写和修改代码，完成网站功能。", "你是代码编程 Agent，负责阅读已有代码、实现网站功能与修复问题。沿用项目已有技术栈，以最小改动满足需求，按需运行相关验证，报告实际修改的文件和验证结果。被委派时只完成委派范围。", ""},
		{"audit", "代码审计Agent", "检查代码正确性、安全边界与实现质量。", "你是代码审计 Agent，负责审查代码正确性、安全性、需求符合度和关键边界。先阅读实际源码，报告有证据的问题、严重程度、文件位置和修复建议。默认只审查；只有任务明确要求修复时才修改文件。不得伪造测试或审查结果。", ""},
		{"template", "前端模板制作Agent", "制作突出效果、可复用的三文件前端模板，预览后由用户确认入库。", "你是前端模板制作 Agent，默认使用系统加载的 template-maker Skill。在以模板名命名的文件夹中制作 index.html、style.css、script.js 三个文件，标题文字统一为标题+id，正文统一为文本+id，其他命名组件统一为组件+id，重点突出视觉效果和可复用性。源码写入当前运行 result 目录，启动预览。交付后提示用户检查预览和文件，在界面点击“保存到模板库”确认入库；不得直接写入模板库或替用户同意。", ""},
	}
}

func builtInAgent(id string) bool {
	return id == "main" || id == "design" || id == "code" || id == "audit" || id == "template"
}

func validateAgents(agents []AgentConfig) error {
	seen := make(map[string]bool, len(agents))
	for i := range agents {
		agent := &agents[i]
		agent.ID = strings.TrimSpace(agent.ID)
		agent.Name = strings.TrimSpace(agent.Name)
		agent.Description = strings.TrimSpace(agent.Description)
		agent.Prompt = strings.TrimSpace(agent.Prompt)
		agent.ModelID = strings.TrimSpace(agent.ModelID)
		if agent.ID == "main" && agent.ModelID != "" {
			return errors.New("主 Agent 使用模型配置页选定的主模型")
		}
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
	if err := validateModelConfig(&cfg); err != nil {
		return nil, apiError{400, "INVALID_MODEL", err.Error()}
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
	taskID    string
	parentID  string
	effort    string
	readOnly  bool
	outcome   *agentOutcome
}

type agentExecutionKey struct{}

func appendAgentEvent(ctx context.Context, store *runStore, kind string, payload map[string]any) error {
	if execution, ok := ctx.Value(agentExecutionKey{}).(agentExecution); ok {
		payload["agent_id"] = execution.agent.ID
		payload["agent_name"] = execution.agent.Name
		payload["delegated"] = execution.delegated
		if execution.delegated {
			payload["result_format"] = "agent_result"
		}
		if execution.taskID != "" {
			payload["subagent_id"] = execution.taskID
			payload["parent_agent_id"] = execution.parentID
		}
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
	instructions += "\n\n" + collaborationInstructions
	if agent.ID == "main" {
		instructions += "\n委派按顺序进行，明确每项任务的写入范围。模板制作是独立入口，不加入普通网页任务的默认流程。简单微调可直接交前端工程，视觉方案确认后再正式实现；上线前准备具体发布方案供用户确认。\n"
		instructions += "\n可通过 call_agent 调用以下 Agent。它们共享当前运行文件目录，但有独立的任务上下文；务必提供完整任务、必要背景和文件路径。\n"
		for _, target := range cfg.Agents {
			if target.ID != "main" {
				instructions += target.ID + "：" + target.Name + "；" + target.Description + "\n"
			}
		}
	}
	if agent.ID == "design" {
		instructions += "\n交付要求：除非用户明确要求完整设计文档，否则把方案控制在约 800 字，提供可直接实现的布局、色板和交互要点，不生成上千行规范。不为常规配色、字体栈和缓动曲线搜索网页；外部搜索失败后继续完成方案。\n"
	}
	if agent.ID == "audit" {
		instructions += "\n质量检查保持只读，不能写入文件、执行任意终端命令、下载文件或改变实例。需要运行命令时向主 Agent 提供具体检查要求，由前端工程执行并回传证据；未取得证据时标注未验证。只报告有证据且影响交付的问题，一次检查通过后不重复验证。\n"
	}
	return instructions
}

func agentRoundLimit(id string) int {
	switch id {
	case "design":
		return 8
	case "audit":
		return 12
	case "main":
		return 24
	default:
		return 48
	}
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
			Name: "call_agent", Description: openai.String("按协作协议委派当前阶段任务；填写任务契约，返回业务结果与独立执行状态；共享当前运行目录"), Strict: openai.Bool(false),
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"agent_id": map[string]any{"type": "string", "enum": ids},
				"task":     map[string]any{"type": "string", "description": "本次委派的明确目标"},
				"contract": agentContractSchema(),
				"timeout":  map[string]any{"type": "number", "minimum": 0.1},
			}, "required": []string{"agent_id", "task", "contract", "timeout"}, "additionalProperties": false},
		}})
	}
	if agent.ID == "audit" {
		return readOnlyAgentTools(definitions)
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
		AgentID  string             `json:"agent_id"`
		Task     string             `json:"task"`
		Contract *agentTaskContract `json:"contract"`
		Timeout  *float64           `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return fail(fmt.Errorf("委派参数无效: %w", err))
	}
	if args.AgentID == "" || args.AgentID == "main" || strings.TrimSpace(args.Task) == "" {
		return fail(errors.New("必须选择其他 Agent 并填写非空任务"))
	}
	if args.Contract != nil {
		if err := args.Contract.validate(); err != nil {
			return fail(err)
		}
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
	outcome := &agentOutcome{}
	execution := agentExecution{agent: agent, delegated: true, taskID: workspace.NewID(), parentID: caller.ID, readOnly: agent.ID == "audit" || args.Contract != nil && args.Contract.ReadOnly, outcome: outcome}
	delegateCtx = context.WithValue(delegateCtx, agentExecutionKey{}, execution)
	if err := appendAgentEvent(delegateCtx, store, "subagent_started", map[string]any{"task": args.Task, "contract": args.Contract, "read_only": execution.readOnly}); err != nil {
		return fail(err)
	}
	task := args.Task
	if args.Contract != nil {
		data, _ := json.Marshal(struct {
			Task     string             `json:"task"`
			Contract *agentTaskContract `json:"contract"`
		}{args.Task, args.Contract})
		task = string(data)
	}
	text, runErr := a.runAgent(delegateCtx, store, []inputItem{{"role": "user", "content": task}}, agent, true)
	result := finishAgentCall(agent, execution.taskID, text, runErr, delegateCtx.Err(), outcome.budgetReached)
	if eventErr := appendAgentEvent(delegateCtx, store, "subagent_finished", map[string]any{"status": result.ExecutionStatus, "reason": result.Error, "execution_reason": result.ExecutionReason, "result": result.Result}); eventErr != nil {
		return fail(eventErr)
	}
	data, _ := json.Marshal(result)
	return string(data)
}
