package core

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"webweave/tools"
	"webweave/workspace"
)

func (a *App) executeTaskCalls(ctx context.Context, store *runStore, items, calls []inputItem, agent AgentConfig) ([]inputItem, error) {
	execution := ctx.Value(agentExecutionKey{}).(agentExecution)
	task := execution.checkpoint
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return items, err
		}
		id, name, raw := callID(call), itemString(call, "name"), itemString(call, "arguments")
		record := task.Tools[id]
		if record == nil {
			record = &taskTool{Name: name, Arguments: raw, Status: "pending"}
			task.Tools[id] = record
		} else if record.Name != name || record.Arguments != raw {
			return items, &taskPause{"blocked", "模型重复使用同一工具调用 ID 却改变了参数，已暂停以保护执行记录"}
		}
		started := time.Now()
		result, toolErr := "", error(nil)
		if task.ReconcileFeedback && !record.RetryApproved && (record.Status == "pending" || record.Status == "running" && readOnlyToolAllowed(name, raw)) {
			// A new user message invalidates unstarted operations. Pair the old
			// calls before delivering feedback, without executing stale side effects.
			record.Status = "completed"
			record.Output = `{"ok":false,"cancelled":true,"error":"USER_FEEDBACK_SUPERSEDED","message":"用户已补充或修改需求；本操作未执行，请根据新反馈重新安排。"}`
		}
		if record.Status == "completed" {
			result = record.Output
		} else {
			if record.Status == "running" {
				var recovered bool
				result, recovered, toolErr = a.recoverTaskTool(ctx, store, record)
				if toolErr != nil {
					var pause *taskPause
					if errors.As(toolErr, &pause) && pause.Status == "awaiting_user" {
						decision := &userDecision{ID: workspace.NewID(), TaskID: task.ID, Kind: "recovery", Question: pause.Reason + "。请先核对实际结果；点击确认将允许重新执行这一步。", RecoveryCall: id}
						if err := store.setDecision(decision); err != nil {
							return items, err
						}
						if err := appendAgentEvent(ctx, store, "user_decision_requested", map[string]any{"decision": decision}); err != nil {
							return items, err
						}
					}
					return items, toolErr
				}
				if recovered {
					record.Status = "completed"
				}
			}
			if record.Status != "completed" {
				if err := appendAgentEvent(ctx, store, "tool_started", map[string]any{"tool_name": name, "arguments": raw}); err != nil {
					return items, err
				}
				result, toolErr = a.runTaskTool(ctx, store, agent, record)
				if ctx.Err() != nil {
					return items, ctx.Err()
				}
				var pause *taskPause
				if errors.Is(toolErr, errTaskStalled) {
					result = `{"ok":false,"error":"TASK_STALLED","message":"重复读取未推进任务，请使用已保存结论交回当前结果"}`
				} else if toolErr != nil && !(name == "request_user_decision" && errors.As(toolErr, &pause)) {
					if !errors.As(toolErr, &pause) || record.Status != "pending" {
						return items, toolErr
					}
					data, _ := json.Marshal(map[string]any{"ok": false, "paused": true, "error": pause.Reason})
					result = string(data)
				}
			}
		}
		record.Status, record.Output = "completed", result
		item := inputItem{"type": "function_call_output", "call_id": id, "output": result}
		items = append(items, item)
		// The result and its paired conversation item share one atomic checkpoint.
		if err := saveAgentContext(ctx, store, items, nil); err != nil {
			return items, err
		}
		if err := appendAgentEvent(ctx, store, "tool_output", map[string]any{"tool_name": name, "arguments": raw, "duration_seconds": time.Since(started).Seconds(), "cancelled": false, "item": item}); err != nil {
			return items, err
		}
		if toolErr != nil {
			return items, toolErr
		}
	}
	return items, nil
}

func (a *App) runTaskTool(ctx context.Context, store *runStore, agent AgentConfig, record *taskTool) (string, error) {
	execution := ctx.Value(agentExecutionKey{}).(agentExecution)
	if record.Name == "update_task_state" {
		return a.updateTaskWork(ctx, store, record.Arguments)
	}
	denied, err := a.taskToolPermission(ctx, store, record.Name, record.Arguments)
	if err != nil || denied != "" {
		return denied, err
	}
	if record.Name == "request_user_decision" {
		if !execution.interactive || execution.delegated {
			return `{"ok":false,"error":"只有主 Agent 可请求用户决策"}`, nil
		}
		return a.requestDecision(ctx, store, record.Arguments)
	}
	revision, err := a.fileRevision(store.id, true)
	if err != nil {
		return "", err
	}
	if record.Name == "file_read" || record.Name == "read_context_segment" {
		if execution.checkpoint.ReadRevision != revision {
			execution.checkpoint.ReadRepeats = 0
			execution.checkpoint.ReadRevision = revision
		}
		if execution.checkpoint.ReadRepeats >= 6 {
			return "", errTaskStalled
		}
		if execution.checkpoint.ReadsSinceNote >= 6 {
			return `{"ok":false,"error":"WORK_LOG_REQUIRED","message":"请先用 update_task_state 保存已形成的审阅结论、待办与下一步，再继续读取；不是重复记录读过哪些文件。"}`, nil
		}
	}
	record.Revision = revision
	var key string
	var previous verificationRecord
	if isVerification(record.Name, record.Arguments) {
		key = verificationKey(record.Name, record.Arguments, revision)
		previous, err = store.verification(key, nil)
		if err != nil {
			return "", err
		}
		if previous.Success {
			if err := appendAgentEvent(ctx, store, "verification_reused", map[string]any{"revision": revision, "tool_name": record.Name, "arguments": record.Arguments}); err != nil {
				return "", err
			}
			return previous.Output, nil
		}
		if previous.Failures >= 2 {
			return "", &taskPause{"blocked", "同一文件版本的同一验证已连续失败两次，已暂停；请修正检查命令或相关文件后继续"}
		}
	}
	if record.Name == "web_instance" {
		state, err := store.readState()
		if err != nil {
			return "", err
		}
		for _, instance := range runInstances(state) {
			record.InstanceIDs = append(record.InstanceIDs, instance.InstanceID)
		}
		var args tools.Args
		if json.Unmarshal([]byte(record.Arguments), &args) == nil && args.Action == "start" {
			config := launchConfig{Command: args.Command, Cwd: args.Cwd, HealthPath: args.HealthPath}
			if args.Port != nil {
				config.Port = *args.Port
			}
			if args.Command == "" {
				if err := readJSONFile(filepath.Join(a.runRoot("result", store.id), launchConfigName), &config); err != nil {
					return "", err
				}
			}
			if config.Cwd == "" {
				config.Cwd = "."
			}
			if config.HealthPath == "" {
				config.HealthPath = "/"
			}
			if config.Command != "" {
				record.Launch = &config
			}
		}
	}
	record.Status = "running"
	if err := store.saveTask(execution.checkpoint); err != nil {
		return "", err
	}
	if record.Name == "call_agent" {
		return a.executeDelegation(ctx, store, agent, record.Arguments, record)
	}
	result := a.executeTool(ctx, store.id, record.Name, record.Arguments)
	if record.Name == "file_read" || record.Name == "read_context_segment" {
		task := execution.checkpoint
		task.ReadsSinceNote++
		newContent := record.Name == "file_read" && task.observeFile(result)
		if record.Name == "read_context_segment" {
			newContent = task.observeRecall(a.root, store.id, result)
		}
		if newContent {
			task.Discoveries++
			task.ReadRepeats = 0
		} else {
			task.ReadRepeats++
		}
	}
	if record.Name == "web_search" && json.Valid([]byte(result)) && strings.HasPrefix(strings.TrimSpace(result), "[") {
		observation := contentKey(result)
		if execution.checkpoint.Observations == nil {
			execution.checkpoint.Observations = map[string]bool{}
		}
		if !execution.checkpoint.Observations[observation] {
			execution.checkpoint.Observations[observation] = true
			execution.checkpoint.Discoveries++
		}
	}
	if key != "" {
		after, err := a.fileRevision(store.id, true)
		if err != nil {
			return "", err
		}
		var output struct {
			Returncode *int `json:"returncode"`
		}
		success := json.Unmarshal([]byte(result), &output) == nil && output.Returncode != nil && *output.Returncode == 0
		if after == revision {
			previous.Output, previous.Success = result, success
			if !success {
				previous.Failures++
			}
			if success {
				segment, err := workspace.SaveContextSegment(a.root, store.id, "terminal_run", "验证结果", result)
				if err != nil {
					return "", err
				}
				previous.TaskID, previous.Revision, previous.SegmentID = execution.checkpoint.ID, revision, segment.ID
				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(record.Arguments), &args); err != nil {
					return "", err
				}
				previous.Command = args.Command
				for id, candidate := range execution.checkpoint.Tools {
					if candidate == record {
						previous.CallID = id
						break
					}
				}
			}
			if _, err := store.verification(key, &previous); err != nil {
				return "", err
			}
			if success {
				execution.checkpoint.Discoveries++
			}
		}
	}
	return result, nil
}
