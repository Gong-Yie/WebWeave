package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"webweave/tools"
	"webweave/workspace"
)

var errTaskYield = errors.New("执行分段已保存，等待调度")

type taskPause struct {
	Status string
	Reason string
}

func (p *taskPause) Error() string { return p.Reason }

type taskTool struct {
	Name          string        `json:"name"`
	Arguments     string        `json:"arguments"`
	Status        string        `json:"status"`
	Output        string        `json:"output,omitempty"`
	ChildID       string        `json:"child_id,omitempty"`
	InstanceIDs   []string      `json:"instance_ids,omitempty"`
	Launch        *launchConfig `json:"launch,omitempty"`
	Revision      string        `json:"revision,omitempty"`
	RetryApproved bool          `json:"retry_approved,omitempty"`
}

type taskCheckpoint struct {
	ID                string                 `json:"id"`
	AgentID           string                 `json:"agent_id"`
	ParentID          string                 `json:"parent_id,omitempty"`
	Status            string                 `json:"status"`
	Reason            string                 `json:"reason,omitempty"`
	Items             []inputItem            `json:"input_items"`
	QueuedInput       []inputItem            `json:"queued_input,omitempty"`
	ReconcileFeedback bool                   `json:"reconcile_feedback,omitempty"`
	Tools             map[string]*taskTool   `json:"tools"`
	Slice             int                    `json:"slice"`
	Round             int                    `json:"round"`
	Stalled           int                    `json:"stalled_slices"`
	Revision          string                 `json:"revision"`
	Discoveries       int                    `json:"discoveries"`
	Observations      map[string]bool        `json:"observations,omitempty"`
	Output            string                 `json:"output,omitempty"`
	Feedback          string                 `json:"feedback,omitempty"`
	Contract          *agentTaskContract     `json:"contract,omitempty"`
	Work              *taskWorkState         `json:"work,omitempty"`
	TokenUsage        taskTokenUsage         `json:"token_usage,omitempty"`
	ReadRanges        map[string][]readRange `json:"read_ranges,omitempty"`
	ReadRepeats       int                    `json:"read_repeats,omitempty"`
	ReadsSinceNote    int                    `json:"reads_since_note,omitempty"`
	ReadRevision      string                 `json:"read_revision,omitempty"`
	RequestRevision   string                 `json:"request_revision,omitempty"`
	Compression       *summaryProgress       `json:"compression,omitempty"`
}

func (s *runStore) loadTask(id string) (*taskCheckpoint, error) {
	if !workspace.ValidName(id) {
		return nil, errors.New("任务 ID 无效")
	}
	var task taskCheckpoint
	if err := readJSONFile(filepath.Join(s.dir, "tasks", id+".json"), &task); err != nil {
		return nil, err
	}
	if task.ID == "" {
		return nil, nil
	}
	return &task, nil
}

func (s *runStore) saveTask(task *taskCheckpoint) error {
	if !workspace.ValidName(task.ID) {
		return errors.New("任务 ID 无效")
	}
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "tasks", task.ID+".json"), task)
}

func (s *runStore) selectTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	state.TaskID = id
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

// The scheduler re-enters the same task with the same paired tool history. A
// budget is a checkpoint boundary, never an instruction to fabricate a final answer.
func (a *App) runAgent(ctx context.Context, store *runStore, items []inputItem, agent AgentConfig, delegated bool) (string, error) {
	execution, _ := ctx.Value(agentExecutionKey{}).(agentExecution)
	task := execution.checkpoint
	if task == nil {
		id := execution.taskID
		if id == "" {
			id = workspace.NewID()
		}
		task = &taskCheckpoint{ID: id, AgentID: agent.ID, ParentID: execution.parentTaskID, Items: items, Tools: map[string]*taskTool{}}
	}
	execution.checkpoint, execution.agent, execution.delegated = task, agent, delegated
	ctx = context.WithValue(ctx, agentExecutionKey{}, execution)
	if task.Status == "completed" {
		return task.Output, nil
	}
	if task.Tools == nil {
		task.Tools = map[string]*taskTool{}
	}
	for {
		task.Status, task.Reason = "running", ""
		before, err := a.projectRevision(store.id)
		if err != nil {
			return "", err
		}
		if task.Revision == "" {
			task.Revision = before
		}
		discoveries := task.Discoveries
		text, runErr := a.runAgentSegment(ctx, store, task.Items, agent, delegated)
		if errors.Is(runErr, errTaskYield) {
			after, err := a.projectRevision(store.id)
			if err != nil {
				return "", err
			}
			if after == task.Revision && discoveries == task.Discoveries {
				task.Stalled++
			} else {
				task.Stalled = 0
			}
			task.Revision, task.Round = after, 0
			task.Slice++
			if task.Stalled >= 2 {
				runErr = errTaskStalled
			} else {
				task.Status = "yielded"
				if err := store.saveTask(task); err != nil {
					return "", err
				}
				if err := appendAgentEvent(ctx, store, "task_yielded", map[string]any{"task_id": task.ID, "slice": task.Slice, "tool_rounds": agentRoundLimit(agent.ID)}); err != nil {
					return "", err
				}
				continue
			}
		}
		if errors.Is(runErr, errTaskStalled) {
			if delegated {
				text, runErr = task.stalledResult(), nil
			} else {
				runErr = &taskPause{"blocked", "当前步骤未取得新证据，已保留工作记录与产物；需要调整执行步骤后继续"}
			}
		}
		if runErr == nil && (strings.Contains(text, "<｜｜DSML｜｜") || strings.Contains(text, "<tool_call>")) {
			runErr = &taskPause{"blocked", "模型返回了调用标记文本而非有效工具调用或交付，已保存现场，未执行这些文本"}
		}
		if runErr == nil && (delegated || execution.interactive) {
			result, parseErr := parseAgentResult(text)
			declaredPending := append(append([]string{}, result.Pending...), result.Questions...)
			if parseErr == nil && result.Status == "completed" {
				pending, err := a.completionPending(store, task)
				if err != nil {
					return "", err
				}
				if len(pending) > 0 {
					result.Status, result.Pending = "partial", pending
					data, err := json.Marshal(result)
					if err != nil {
						return "", err
					}
					text = string(data)
				}
			}
			if parseErr == nil {
				if task.Work == nil {
					task.Work = &taskWorkState{Phase: "delivery", Step: "交付当前阶段"}
				}
				if result.Status == "completed" {
					task.Work.Completed = appendUnique(task.Work.Completed, result.Summary)
					task.Work.Completed = task.Work.Completed[max(0, len(task.Work.Completed)-12):]
				}
				// Gate feedback is derived from the original work records. Persisting
				// it as a new pending item duplicates findings under different IDs.
				task.Work.Pending = appendUnique(task.Work.Pending, declaredPending...)
				task.Work.Next = "向用户交付当前阶段结果"
				if len(result.Pending) > 0 {
					task.Work.Next = result.Pending[0]
				}
			}
			if !delegated && parseErr == nil && (result.Status == "blocked" || result.Status == "failed") {
				runErr = &taskPause{result.Status, result.Summary + "\n" + strings.Join(append(result.Pending, result.Questions...), "\n")}
			}
			if parseErr != nil {
				if delegated {
					result = agentResult{Status: "blocked", Summary: "子任务交付结构无效，未判为完成", Artifacts: []string{}, Verification: []string{"未验证：需核对已有产物与执行记录"}, Pending: []string{parseErr.Error()}, Questions: []string{}, Previews: []agentPreview{}}
					data, err := json.Marshal(result)
					if err != nil {
						return "", err
					}
					text = string(data)
				} else {
					runErr = &taskPause{"blocked", "主对话交付结构无效，已保留原文与检查点，未判为完成：" + parseErr.Error()}
				}
			}
			if parseErr == nil && result.Status == "partial" {
				after, err := a.projectRevision(store.id)
				if err != nil {
					return "", err
				}
				if after == task.Revision && discoveries == task.Discoveries {
					task.Stalled++
				} else {
					task.Stalled = 0
				}
				task.Revision = after
				if task.Stalled < 2 {
					task.Items = append(task.Items, inputItem{"role": "developer", "content": "本任务仍有未完成事项：\n" + strings.Join(result.Pending, "\n") + "\n保留现有文件和验证证据，从 pending 继续；解决旧待办时先用 update_task_state.resolved 明确记录。需要用户决定时返回 blocked，不重复完成已有工作。"})
					task.Round = 0
					if err := store.saveTask(task); err != nil {
						return "", err
					}
					continue
				}
				if delegated {
					result.Status = "blocked"
					result.Pending = append(result.Pending, "连续两次交付部分结果且没有产物进展；请主 Agent 根据已有证据调整下一步，不原样重跑")
					data, _ := json.Marshal(result)
					text = string(data)
				} else {
					runErr = &taskPause{"blocked", "连续两次仅交付部分结果，任务没有完成；已保存上下文等待调整"}
				}
			}
		}
		if runErr == nil && execution.interactive && !delegated {
			state, err := store.readState()
			if err != nil {
				return "", err
			}
			if _, ok := state.Approvals["plan"]; !ok {
				runErr = &taskPause{"awaiting_user", "请先确认本次需求范围与视觉方案，确认后将自动继续"}
			}
		}
		task.Output = text
		task.Status = "completed"
		var pause *taskPause
		if ctx.Err() != nil {
			task.Status, task.Reason = "stopped", ctx.Err().Error()
		} else if errors.As(runErr, &pause) {
			task.Status, task.Reason = pause.Status, pause.Reason
		} else if runErr != nil {
			task.Status, task.Reason = "failed", runErr.Error()
		}
		if err := store.saveTask(task); err != nil {
			return text, err
		}
		return text, runErr
	}
}

func (a *App) completionPending(store *runStore, task *taskCheckpoint) ([]string, error) {
	pending := []string{}
	revision, err := a.fileRevision(store.id, true)
	if err != nil {
		return nil, err
	}
	if task.Work != nil {
		pending = appendUnique(pending, task.Work.Pending...)
		pending = appendUnique(pending, task.Work.Findings...)
		for _, check := range task.Work.Checks {
			if check.Status != "reviewed" && check.Status != "passed" || check.Revision != revision || check.RequestRevision != task.RequestRevision {
				pending = appendUnique(pending, "验收尚未完成或证据已过期："+check.Name)
			}
		}
	}
	if task.Contract != nil {
		for _, acceptance := range task.Contract.Acceptance {
			found := false
			if task.Work != nil {
				for _, check := range task.Work.Checks {
					found = found || check.Name == acceptance && (check.Status == "reviewed" || check.Status == "passed") && check.Revision == revision && check.RequestRevision == task.RequestRevision
				}
			}
			if !found {
				pending = appendUnique(pending, "缺少验收证据："+acceptance)
			}
		}
	}
	return pending, nil
}

// Metadata is sufficient for invalidating cached checks after ordinary writes;
// generated probes are not evidence that the product itself advanced.
func (a *App) projectRevision(id string) (string, error) {
	return a.fileRevision(id, false)
}

func (a *App) fileRevision(id string, includeProbes bool) (string, error) {
	root := a.runRoot("result", id)
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeProbes && (strings.HasPrefix(name, "_probe") || strings.HasPrefix(name, "_check") || strings.HasPrefix(name, "_dom") || strings.HasPrefix(name, "_parse") || strings.HasPrefix(name, "_shot") || strings.HasPrefix(name, "_mkprobe")) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", relative, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func pendingTaskCalls(items []inputItem) []inputItem {
	finished := map[string]bool{}
	for _, item := range items {
		if itemString(item, "type") == "function_call_output" {
			finished[callID(item)] = true
		}
	}
	calls := []inputItem{}
	for _, item := range items {
		if itemString(item, "type") == "function_call" && !finished[callID(item)] {
			calls = append(calls, item)
		}
	}
	return calls
}

func (a *App) recoverTaskTool(ctx context.Context, store *runStore, record *taskTool) (string, bool, error) {
	if record.Name == "call_agent" || readOnlyToolAllowed(record.Name, record.Arguments) {
		return "", false, nil
	}
	var args tools.Args
	if err := json.Unmarshal([]byte(record.Arguments), &args); err != nil {
		return "", false, err
	}
	if record.Name == "create_file" && args.Content != nil {
		path, err := workspace.ResolveWithin(a.runRoot("result", store.id), args.FilePath, false)
		if err != nil {
			return "", false, err
		}
		content, err := os.ReadFile(path)
		if err == nil && string(content) == *args.Content {
			data, _ := json.Marshal(map[string]any{"path": path, "size": len(content), "recovered": true})
			return string(data), true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	if record.Name == "web_instance" && args.Action == "start" {
		state, err := store.readState()
		if err != nil {
			return "", false, err
		}
		known := map[string]bool{}
		for _, id := range record.InstanceIDs {
			known[id] = true
		}
		var found *instanceRecord
		for _, instance := range runInstances(state) {
			if record.Launch == nil {
				continue
			}
			cwd, err := workspace.WorkingDirectory(a.root, store.id, record.Launch.Cwd, true)
			if err != nil {
				return "", false, err
			}
			matches := instance.Command == record.Launch.Command && instance.Cwd == cwd && instance.RequestedPort == record.Launch.Port && instance.HealthPath == record.Launch.HealthPath
			if matches && (!known[instance.InstanceID] || args.InstanceID == instance.InstanceID) {
				if found != nil {
					return "", false, &taskPause{"blocked", "中断期间出现多个新实例，无法确定原启动操作的结果，请核对实例后再继续"}
				}
				found = instance
			}
		}
		if found != nil {
			result, err := a.instanceAction(ctx, store.id, tools.Args{Action: "status", InstanceID: found.InstanceID})
			if err != nil {
				return "", false, err
			}
			data, _ := json.Marshal(result)
			return string(data), true, nil
		}
	}
	return "", false, &taskPause{"awaiting_user", "上次操作的结果尚未确认，已暂停以避免重复执行：" + record.Name}
}

type verificationRecord struct {
	Output    string `json:"output"`
	Failures  int    `json:"failures"`
	Success   bool   `json:"success"`
	TaskID    string `json:"task_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Command   string `json:"command,omitempty"`
	Revision  string `json:"revision,omitempty"`
	SegmentID string `json:"segment_id,omitempty"`
}

func (s *runStore) currentEvidence(revision string) ([]verificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := map[string]verificationRecord{}
	if err := readJSONFile(filepath.Join(s.dir, "verification.json"), &records); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	evidence := []verificationRecord{}
	for _, key := range keys {
		record := records[key]
		if record.Success && record.Revision == revision && record.TaskID != "" && record.CallID != "" {
			task, err := s.loadTask(record.TaskID)
			if err != nil {
				return nil, err
			}
			if task == nil || task.Tools[record.CallID] == nil || task.Tools[record.CallID].Status != "completed" || task.Tools[record.CallID].Revision != revision {
				continue
			}
			record.Output = ""
			evidence = append(evidence, record)
		}
	}
	return evidence[max(0, len(evidence)-12):], nil
}

func verificationKey(name, raw, revision string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(raw), &args)
	delete(args, "timeout")
	data, _ := json.Marshal(args)
	hash := sha256.Sum256([]byte(name + "\x00" + string(data) + "\x00" + revision))
	return hex.EncodeToString(hash[:])
}

func isVerification(name, raw string) bool {
	var args struct {
		Intent string `json:"intent"`
	}
	return name == "terminal_run" && json.Unmarshal([]byte(raw), &args) == nil && args.Intent == "verify"
}

func (s *runStore) verification(key string, update *verificationRecord) (verificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := map[string]verificationRecord{}
	path := filepath.Join(s.dir, "verification.json")
	if err := readJSONFile(path, &records); err != nil {
		return verificationRecord{}, err
	}
	if update != nil {
		records[key] = *update
		if err := workspace.WriteJSONAtomic(path, records); err != nil {
			return verificationRecord{}, err
		}
	}
	return records[key], nil
}
