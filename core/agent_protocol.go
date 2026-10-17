package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

const collaborationInstructions = `协作协议：
WebWeave 面向弱代码用户，支持从零制作网页、优化复杂前端、局部样式美化及为已有后端生成前端，逐步交付可预览、可导出、可部署的项目。
按理解需求、确认方向、分步实现、检查交付推进，允许跨多轮对话和返回前阶段；不要求每个任务经过所有角色。
需求范围、视觉方向、重大改动和正式上线前，必须向用户展示具体方案并取得确认；阶段内常规实现自主完成。重大改动指明显改变已确认体验、业务流程、接口契约，或替换技术栈、广泛重构。文件数量多不自动代表重大改动。确认只适用于当时展示的范围，实质变化需重新确认。
区分用户原话、已确认决定、Agent 推断和待确认事项；不得把推断或用户沉默当成确认。维护当前阶段和这些共识，沿用已有对话及任务摘要，不重复询问已有答案。
由主 Agent 汇总用户沟通：先读取现有资料，再用通俗语言提出少量关键问题，给出易懂选项和影响，不要求用户审批常规技术细节。
提供多个可预览方向是可选能力，先询问用户是否需要；有明确方向或仅局部微调时直接推进。需要样例时仅制作能支持选择的代表页面或局部，不完整开发每个候选方案。保存方案名称、文件路径和 instance_id，说明是视觉示意、演示数据还是真实接口；用户选定后沿用该方案。
主 Agent 每次委派只推进当前阶段的明确任务，填写 task 与 contract：用户原始需求、当前阶段、必要背景、已确认决定、推断、输入路径、操作范围、验收条件和交付要求。子 Agent 上下文独立，按路径读取实际文件，不传递重复完整源码。
子 Agent 只完成委派范围，不自行改变用户已确认的决定。遇到冲突、关键资料缺失或需要用户确认时返回 blocked，列明具体问题，由主 Agent 先检查能否自行解决，再向用户提问；不得假装已取得确认。
主 Agent 核对交付文件、预览和验收条件后再推进；单项委派完成不等于项目完成。向用户说明完成内容、可查看的预览和下一项决定，不暴露内部协议字段。
简单修改由执行者检查；关键交互、接口对接、较大修改和上线准备按风险安排独立质量检查，不固定执行完整设计、编程、检查流水线。复用仍对应当前文件版本的检查结果，只重查受影响部分。
失败、超时或预算耗尽后先检查已有产物，再安排续做；修复必须有证据和明确目标，不无条件重跑，不反复审计。`

const delegatedResultInstructions = `委派交付格式：最后回复必须是符合 agent_result Schema 的 JSON，不使用 Markdown 围栏。
status 为 completed（已达到本次验收条件）、partial（已有成果但未完成）、blocked（需要决策或必要信息）、failed（业务执行失败）。
summary 简述实际结果；artifacts 列出真实交付文件路径；verification 说明实际执行的命令、结果及对应文件，未验证必须明确写出；pending 列出剩余事项；questions 列出需要主 Agent 处理的决定或问题；previews 列出方案名称、instance_id、用途和数据来源。
无内容的数组返回 []，不得编造文件、预览、验证或用户确认。completed 不得有 pending 或 questions；partial 必须说明 pending；blocked 必须说明 pending 或 questions。预算耗尽不能自称 completed。
样例和正式实现应明确区分，预览不能代替接口验证；源码以文件为准。`

type agentTaskContract struct {
	Phase        string   `json:"phase"`
	UserRequest  string   `json:"user_request"`
	Context      string   `json:"context"`
	Confirmed    []string `json:"confirmed"`
	Assumptions  []string `json:"assumptions"`
	Inputs       []string `json:"inputs"`
	Scope        []string `json:"scope"`
	Acceptance   []string `json:"acceptance"`
	Deliverables []string `json:"deliverables"`
	ReadOnly     bool     `json:"read_only"`
}

func (c agentTaskContract) validate() error {
	if strings.TrimSpace(c.Phase) == "" || strings.TrimSpace(c.UserRequest) == "" {
		return errors.New("任务契约必须说明当前阶段和用户原始需求")
	}
	for _, field := range []struct {
		name   string
		values []string
	}{{"操作范围", c.Scope}, {"验收条件", c.Acceptance}, {"交付要求", c.Deliverables}} {
		if len(field.values) == 0 {
			return fmt.Errorf("任务契约缺少%s", field.name)
		}
		for _, value := range field.values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("任务契约的%s不能包含空项", field.name)
			}
		}
	}
	return nil
}

type agentPreview struct {
	Name       string `json:"name"`
	InstanceID string `json:"instance_id"`
	Purpose    string `json:"purpose"`
	DataSource string `json:"data_source"`
}

type agentResult struct {
	Status       string         `json:"status"`
	Summary      string         `json:"summary"`
	Artifacts    []string       `json:"artifacts"`
	Verification []string       `json:"verification"`
	Pending      []string       `json:"pending"`
	Questions    []string       `json:"questions"`
	Previews     []agentPreview `json:"previews"`
}

func parseAgentResult(text string) (agentResult, error) {
	var result agentResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return result, fmt.Errorf("交付结果不是有效 JSON: %w", err)
	}
	if strings.TrimSpace(result.Summary) == "" || result.Artifacts == nil || len(result.Verification) == 0 || result.Pending == nil || result.Questions == nil || result.Previews == nil {
		return result, errors.New("交付结果缺少摘要、验证说明或交付数组")
	}
	switch result.Status {
	case "completed":
		if len(result.Pending) != 0 || len(result.Questions) != 0 {
			return result, errors.New("已完成结果仍有待办或待决策问题")
		}
	case "partial":
		if len(result.Pending) == 0 {
			return result, errors.New("部分完成必须说明剩余事项")
		}
	case "blocked":
		if len(result.Pending) == 0 && len(result.Questions) == 0 {
			return result, errors.New("受阻结果必须说明所需信息或决定")
		}
	case "failed":
	default:
		return result, errors.New("交付结果状态无效")
	}
	return result, nil
}

func stringArraySchema(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "description": description}
}

func agentContractSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"phase":        map[string]any{"type": "string", "description": "当前阶段，例如需求确认、视觉方案、实现、检查或部署准备"},
		"user_request": map[string]any{"type": "string", "description": "与当前任务有关的用户原始需求，不混入 Agent 推断"},
		"context":      map[string]any{"type": "string", "description": "必要背景、已有产物和待确认事项"},
		"confirmed":    stringArraySchema("用户已确认的决定及范围，没有则 []"),
		"assumptions":  stringArraySchema("尚未经用户确认的推断，没有则 []"),
		"inputs":       stringArraySchema("资料、源码、选中组件和预览的引用或路径"),
		"scope":        stringArraySchema("允许的操作范围、可修改文件及禁止事项"),
		"acceptance":   stringArraySchema("可判断的完成条件"),
		"deliverables": stringArraySchema("交付物及路径要求"),
		"read_only":    map[string]any{"type": "boolean", "description": "是否只允许读取；质量检查 Agent 始终只读"},
	}, "required": []string{"phase", "user_request", "context", "confirmed", "assumptions", "inputs", "scope", "acceptance", "deliverables", "read_only"}, "additionalProperties": false}
}

func agentResultFormat() *responses.ResponseFormatTextJSONSchemaConfigParam {
	return &responses.ResponseFormatTextJSONSchemaConfigParam{Name: "agent_result", Strict: openai.Bool(true), Schema: map[string]any{
		"type": "object", "properties": map[string]any{
			"status":       map[string]any{"type": "string", "enum": []string{"completed", "partial", "blocked", "failed"}},
			"summary":      map[string]any{"type": "string"},
			"artifacts":    stringArraySchema("真实交付文件路径"),
			"verification": stringArraySchema("实际验证命令、结果及适用文件；未验证须明确说明"),
			"pending":      stringArraySchema("尚未完成的事项"),
			"questions":    stringArraySchema("需要主 Agent 处理的决定或问题"),
			"previews": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"}, "instance_id": map[string]any{"type": "string"},
				"purpose":     map[string]any{"type": "string", "description": "视觉示意、候选方案或正式实现"},
				"data_source": map[string]any{"type": "string", "description": "无数据、演示数据或真实接口"},
			}, "required": []string{"name", "instance_id", "purpose", "data_source"}, "additionalProperties": false}},
		}, "required": []string{"status", "summary", "artifacts", "verification", "pending", "questions", "previews"}, "additionalProperties": false,
	}}
}

// Only the delegated execution owns this outcome; it must not inherit its parent's budget.
type agentOutcome struct {
	budgetReached bool
}

type agentCallResult struct {
	OK              bool        `json:"ok"`
	AgentID         string      `json:"agent_id,omitempty"`
	AgentName       string      `json:"agent_name,omitempty"`
	SubagentID      string      `json:"subagent_id,omitempty"`
	ExecutionStatus string      `json:"execution_status"`
	ExecutionReason string      `json:"execution_reason"`
	Output          string      `json:"output"`
	Result          agentResult `json:"result"`
	Error           string      `json:"error,omitempty"`
}

func finishAgentCall(agent AgentConfig, taskID, text string, runErr, contextErr error, budgetReached bool) agentCallResult {
	result := agentCallResult{OK: runErr == nil, AgentID: agent.ID, AgentName: agent.Name, SubagentID: taskID, ExecutionStatus: "completed", ExecutionReason: "normal", Output: text}
	parsed, parseErr := parseAgentResult(text)
	if parseErr != nil {
		parsed = agentResult{Status: "partial", Summary: text, Artifacts: []string{}, Verification: []string{"未验证：主 Agent 需核对已有文件和执行记录"}, Pending: []string{"未取得有效交付结果：" + parseErr.Error()}, Questions: []string{}, Previews: []agentPreview{}}
		if strings.HasPrefix(strings.TrimSpace(text), "{") || strings.TrimSpace(text) == "" {
			parsed.Summary = "子 Agent 未提供有效交付结果，需核对已有产物。"
		}
		result.ExecutionReason = "invalid_result"
	}
	if budgetReached {
		result.ExecutionReason = "budget_reached"
		if parsed.Status == "completed" {
			parsed.Status = "partial"
		}
		parsed.Pending = append(parsed.Pending, "工具轮次预算已耗尽，主 Agent 需核对验收条件和已有产物后决定续做")
	}
	if runErr != nil {
		result.ExecutionStatus, result.ExecutionReason = "failed", "error"
		if budgetReached {
			result.ExecutionReason = "budget_reached"
		}
		if contextErr != nil {
			result.ExecutionStatus, result.ExecutionReason = "stopped", "cancelled"
			if errors.Is(contextErr, context.DeadlineExceeded) {
				result.ExecutionReason = "timeout"
			}
		}
		result.Error = fmt.Sprintf("%s 执行失败: %v", agent.Name, runErr)
		parsed.Status, parsed.Summary = "failed", result.Error
	}
	result.Result = parsed
	return result
}

func readOnlyAgentTools(definitions []responses.ToolUnionParam) []responses.ToolUnionParam {
	result := []responses.ToolUnionParam{}
	for _, definition := range definitions {
		tool := definition.OfFunction
		if tool == nil {
			continue
		}
		switch tool.Name {
		case "list_skills", "read_skill", "file_read", "read_context_segment":
		case "web_search", "web_instance":
			parameters := tool.Parameters
			properties := parameters["properties"].(map[string]any)
			for name := range properties {
				if name != "timeout" && !(tool.Name == "web_search" && name == "query") && !(tool.Name == "web_instance" && (name == "action" || name == "instance_id")) {
					delete(properties, name)
				}
			}
			if tool.Name == "web_instance" {
				properties["action"] = map[string]any{"type": "string", "enum": []string{"status"}}
				tool.Description = openai.String("只读查看已有实例状态，不启动、停止或重启实例")
			} else {
				tool.Description = openai.String("只读搜索公开网页，不下载文件")
			}
		default:
			continue
		}
		result = append(result, definition)
	}
	return result
}
