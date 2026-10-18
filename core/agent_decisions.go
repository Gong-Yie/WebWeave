package core

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"webweave/workspace"
)

type userDecision struct {
	ID           string   `json:"id"`
	TaskID       string   `json:"task_id"`
	Kind         string   `json:"kind"`
	Question     string   `json:"question"`
	Options      []string `json:"options,omitempty"`
	Revision     string   `json:"revision,omitempty"`
	RecoveryCall string   `json:"recovery_call,omitempty"`
	AnsweredAt   string   `json:"answered_at,omitempty"`
	Selection    string   `json:"selection,omitempty"`
}

func decisionTool() responses.ToolUnionParam {
	return responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
		Name: "request_user_decision", Description: openai.String("向用户展示具体方案或通俗问题并暂停，等待真实用户回复。plan 同时确认需求范围与视觉方向，change 确认重大改动，deploy 确认发布，question 只收集信息，不授予实现权限"),
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []string{"plan", "change", "deploy", "question"}},
			"question": map[string]any{"type": "string", "description": "用户可理解的具体问题或方案，列明范围、视觉方向、影响及需要决定的内容"},
			"options":  stringArraySchema("信息问题或 plan 的具体候选方案；每个方案说明范围与方向，选择 plan 方案同时确认该方案。需要先看样例时先用 question 收集偏好"),
			"timeout":  map[string]any{"type": "number", "minimum": 0.1},
		}, "required": []string{"kind", "question", "timeout"}, "additionalProperties": false},
	}}
}

func (s *runStore) setDecision(decision *userDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	state.PendingDecision = decision
	if decision != nil && (decision.Kind == "change" || decision.Kind == "plan") {
		delete(state.Approvals, "plan")
	}
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

func (s *runStore) revokeDeployment(keepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	if approval, ok := state.Approvals["deploy"]; ok && approval.ID != keepID {
		delete(state.Approvals, "deploy")
		return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
	}
	return nil
}

func (a *App) requestDecision(ctx context.Context, store *runStore, raw string) (string, error) {
	var args struct {
		Kind     string   `json:"kind"`
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "决策参数无效：" + err.Error(), nil
	}
	if strings.TrimSpace(args.Question) == "" || args.Kind != "plan" && args.Kind != "change" && args.Kind != "deploy" && args.Kind != "question" {
		return "决策必须包含有效类型和具体问题", nil
	}
	execution := ctx.Value(agentExecutionKey{}).(agentExecution)
	decision := &userDecision{ID: workspace.NewID(), TaskID: execution.checkpoint.ID, Kind: args.Kind, Question: args.Question, Options: args.Options}
	if args.Kind == "deploy" {
		var err error
		decision.Revision, err = a.fileRevision(store.id, true)
		if err != nil {
			return "", err
		}
	}
	if err := store.setDecision(decision); err != nil {
		return "", err
	}
	if execution.checkpoint.Work == nil {
		execution.checkpoint.Work = &taskWorkState{}
	}
	if args.Kind == "plan" {
		execution.checkpoint.Work.Phase = "requirements"
	}
	if args.Kind == "deploy" {
		execution.checkpoint.Work.Phase = "deployment"
	}
	execution.checkpoint.Work.Next = "等待用户回答当前决策"
	if err := appendAgentEvent(ctx, store, "user_decision_requested", map[string]any{"decision": decision}); err != nil {
		return "", err
	}
	data, _ := json.Marshal(map[string]any{"waiting_for_user": true, "decision_id": decision.ID})
	return string(data), &taskPause{"awaiting_user", args.Question}
}

func (a *App) taskToolPermission(ctx context.Context, store *runStore, name, raw string) (string, error) {
	execution, _ := ctx.Value(agentExecutionKey{}).(agentExecution)
	if !execution.interactive || name == "request_user_decision" || readOnlyToolAllowed(name, raw) {
		return "", nil
	}
	state, err := store.readState()
	if err != nil {
		return "", err
	}
	if _, ok := state.Approvals["plan"]; !ok {
		return `{"ok":false,"error":"PLAN_CONFIRMATION_REQUIRED","message":"需求范围与视觉方向尚未由用户确认。主 Agent 必须先用 request_user_decision 展示具体方案并等待用户确认，不得自行把推断当成授权。"}`, nil
	}
	var args struct {
		Intent string `json:"intent"`
	}
	if name == "terminal_run" && json.Unmarshal([]byte(raw), &args) == nil && args.Intent == "deploy" {
		revision, err := a.fileRevision(store.id, true)
		if err != nil {
			return "", err
		}
		approval, ok := state.Approvals["deploy"]
		if !ok || approval.Revision != revision {
			return `{"ok":false,"error":"DEPLOY_CONFIRMATION_REQUIRED","message":"当前文件版本的发布方案尚未由用户确认，请先请求 deploy 决策。"}`, nil
		}
	}
	return "", nil
}

// Only an HTTP user action grants approval. A free-text correction clears the
// question, but does not approve the plan the model proposed.
func (a *App) answerDecision(id, decisionID, choice string) (string, error) {
	run, err := a.runtime(id)
	if err != nil {
		return "", err
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.job != nil {
		return "", apiError{409, "RUN_ALREADY_ACTIVE", "请等待当前操作结束"}
	}
	run.store.mu.Lock()
	defer run.store.mu.Unlock()
	state, err := run.store.stateLocked()
	if err != nil {
		return "", err
	}
	decision := state.PendingDecision
	if decision == nil || decision.ID != decisionID {
		return "", apiError{409, "STALE_DECISION", "该问题已处理或已更新，请刷新后操作"}
	}
	text := ""
	if decision.Kind == "question" {
		for _, option := range decision.Options {
			if option == choice {
				text = option
			}
		}
	} else if choice == "approve" || decision.Kind == "plan" && slices.Contains(decision.Options, choice) {
		if decision.Kind == "plan" && len(decision.Options) > 0 {
			if !slices.Contains(decision.Options, choice) {
				return "", apiError{400, "CHOICE_REQUIRED", "请选择一个具体方案后确认"}
			}
			decision.Selection = choice
		}
		text = "确认并继续：" + decision.Question
		if decision.Selection != "" {
			text = "用户选择并确认：" + decision.Selection + "\n对应方案范围：" + decision.Question
		}
		decision.AnsweredAt = workspace.Timestamp()
		if state.Approvals == nil {
			state.Approvals = map[string]userDecision{}
		}
		state.Approvals[decision.Kind] = *decision
		if decision.Kind == "change" {
			state.Approvals["plan"] = *decision
		}
		if decision.RecoveryCall != "" {
			task, err := run.store.loadTask(decision.TaskID)
			if err != nil {
				return "", err
			}
			if task == nil || task.Tools[decision.RecoveryCall] == nil {
				return "", errors.New("待恢复的工具记录不存在")
			}
			task.Tools[decision.RecoveryCall].Status = "pending"
			task.Tools[decision.RecoveryCall].RetryApproved = true
			task.Stalled, task.Round = 0, 0
			if err := run.store.saveTask(task); err != nil {
				return "", err
			}
		}
	} else if choice == "revise" {
		text = "暂不确认，请先调整方案，再向我确认。"
	}
	if text == "" {
		return "", apiError{400, "INVALID_DECISION", "请选择当前问题中的有效选项"}
	}
	if decision.RecoveryCall != "" && choice == "revise" {
		return "", apiError{400, "RECOVERY_PAUSED", "原操作结果未知，已保持暂停；核对后可选择允许重试"}
	}
	state.PendingDecision = nil
	if err := workspace.WriteJSONAtomic(filepath.Join(run.store.dir, "state.json"), state); err != nil {
		return "", err
	}
	if err := run.store.appendLocked("user_decision_answered", map[string]any{"decision_id": decision.ID, "kind": decision.Kind, "choice": choice}, ""); err != nil {
		return "", err
	}
	return text, nil
}
