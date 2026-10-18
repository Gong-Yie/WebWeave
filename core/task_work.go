package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"webweave/workspace"
)

var errTaskStalled = errors.New("重复读取已有材料，未取得新证据，已停止重复执行")

type taskCheck struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	Evidence        string `json:"evidence"`
	CallID          string `json:"call_id"`
	TaskID          string `json:"task_id,omitempty"`
	Revision        string `json:"revision,omitempty"`
	RequestRevision string `json:"request_revision,omitempty"`
}

type taskOpenItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type taskResolution struct {
	ID       string `json:"id"`
	Evidence string `json:"evidence"`
}

type taskWorkState struct {
	Phase     string         `json:"phase"`
	Step      string         `json:"step"`
	Completed []string       `json:"completed"`
	Pending   []string       `json:"pending"`
	Findings  []string       `json:"findings"`
	Notes     []string       `json:"notes,omitempty"`
	Checks    []taskCheck    `json:"checks"`
	Next      string         `json:"next"`
	Revision  string         `json:"revision,omitempty"`
	OpenItems []taskOpenItem `json:"open_items,omitempty"`
}

func taskWorkTool() responses.ToolUnionParam {
	return responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
		Name: "update_task_state", Description: openai.String("保存阶段、步骤、完成项、待办、问题证据与验收结论。遗漏旧待办或问题不会删除它；解决或撤销时用 resolved 引用 open_items 中的 ID 并说明证据。验收项按 name 更新，遗漏时保留；需求变更使旧验收不再适用时，可在 resolved 中引用 checks 的 ID 并说明撤销原因。不授予用户确认、不修改项目文件"),
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"phase":     map[string]any{"type": "string", "enum": []string{"requirements", "design", "implementation", "verification", "review", "delivery", "deployment"}},
			"step":      map[string]any{"type": "string", "description": "本步明确目标及验收边界"},
			"completed": stringArraySchema("已完成的工作结论，最多 12 条，不逐条记录读取日志"),
			"pending":   stringArraySchema("尚未完成的验收项或待办，最多 12 条"),
			"findings":  stringArraySchema("阻碍本次验收的未解决问题、位置和证据；不得因压缩而删除，最多 12 条"),
			"notes":     stringArraySchema("已披露的非阻塞限制、建议或用户明确接受的取舍，最多 12 条；不表示问题已修复或运行验证通过。旧问题转备注时须同时用 resolved 引用原 ID 说明依据"),
			"checks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"name": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"reviewed", "passed", "failed", "unverified"}},
				"evidence": map[string]any{"type": "string", "description": "源码位置或工具结果，不把源码阅读写成运行验证"},
				"call_id":  map[string]any{"type": "string", "description": "passed 必须引用真实成功的 terminal_run 调用 ID；源码审阅填空字符串"},
				"task_id":  map[string]any{"type": "string", "description": "引用其他任务的验证证据时填写其任务 ID；本任务可省略"},
			}, "required": []string{"name", "status", "evidence", "call_id"}, "additionalProperties": false}},
			"resolved": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string", "description": "解决证据或撤销原因；只更新描述时须解释新条目如何替代旧条目"},
			}, "required": []string{"id", "evidence"}, "additionalProperties": false}},
			"next":    map[string]any{"type": "string", "description": "下一步动作；已达到验收条件则交回结果，不重复读取"},
			"timeout": map[string]any{"type": "number", "minimum": 0.1},
		}, "required": []string{"phase", "step", "completed", "pending", "findings", "checks", "next"}, "additionalProperties": false},
	}}
}

func (a *App) updateTaskWork(ctx context.Context, store *runStore, raw string) (string, error) {
	execution := ctx.Value(agentExecutionKey{}).(agentExecution)
	task := execution.checkpoint
	var update struct {
		taskWorkState
		Resolved []taskResolution `json:"resolved"`
	}
	invalid := func(err error) (string, error) {
		data, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		return string(data), nil
	}
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		return invalid(err)
	}
	work := update.taskWorkState
	if err := mergeTaskWork(task.Work, &work, update.Resolved); err != nil {
		return invalid(err)
	}
	if !slices.Contains([]string{"requirements", "design", "implementation", "verification", "review", "delivery", "deployment"}, work.Phase) || strings.TrimSpace(work.Step) == "" || strings.TrimSpace(work.Next) == "" {
		return invalid(errors.New("必须填写有效阶段、步骤和下一步"))
	}
	for _, values := range [][]string{work.Completed, work.Pending, work.Findings, work.Notes, {work.Step, work.Next}} {
		if len(values) > 12 {
			return invalid(errors.New("每类工作记录最多 12 条"))
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 1000 {
				return invalid(errors.New("工作记录必须非空且每条不超过 1000 字符"))
			}
		}
	}
	if len(work.Checks) > 12 {
		return invalid(errors.New("验收记录最多 12 项"))
	}
	revision, err := a.fileRevision(store.id, true)
	if err != nil {
		return "", err
	}
	for i := range work.Checks {
		check := &work.Checks[i]
		check.ID = contentKey("check\x00" + check.Name)[:16]
		if strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Evidence) == "" || len(check.Name)+len(check.Evidence) > 6000 || !slices.Contains([]string{"reviewed", "passed", "failed", "unverified"}, check.Status) {
			return invalid(errors.New("验收项需填写名称、证据及有效状态"))
		}
		if check.Status == "passed" {
			source := task
			if check.TaskID != "" && check.TaskID != task.ID {
				source, err = store.loadTask(check.TaskID)
				if err != nil {
					return invalid(err)
				}
			}
			if source == nil {
				return invalid(errors.New("验证证据的任务不存在"))
			}
			record := source.Tools[check.CallID]
			var output struct {
				Returncode *int `json:"returncode"`
			}
			if record == nil || record.Name != "terminal_run" || record.Status != "completed" || record.Revision != revision || json.Unmarshal([]byte(record.Output), &output) != nil || output.Returncode == nil || *output.Returncode != 0 {
				return invalid(errors.New("passed 必须引用当前文件版本真实执行成功的 terminal_run 结果；仅阅读源码用 reviewed，缺少证据用 unverified"))
			}
			check.TaskID = source.ID
		}
		check.Revision = revision
		check.RequestRevision = task.RequestRevision
	}
	if task.Work != nil {
		for _, old := range task.Work.Checks {
			id := contentKey("check\x00" + old.Name)[:16]
			if !slices.ContainsFunc(update.Resolved, func(resolution taskResolution) bool { return resolution.ID == id }) && !slices.ContainsFunc(work.Checks, func(check taskCheck) bool { return check.Name == old.Name }) {
				work.Checks = append(work.Checks, old)
			}
		}
	}
	if len(work.Checks) > 12 {
		return invalid(errors.New("保留旧验收项后超过 12 项，请按原名称更新已有验收项"))
	}
	work.Revision = revision
	for _, check := range work.Checks {
		if check.Status != "reviewed" && check.Status != "passed" {
			continue
		}
		key := contentKey("check\x00" + check.ID + "\x00" + check.Status + "\x00" + check.Revision + "\x00" + check.RequestRevision)
		if task.Observations == nil {
			task.Observations = map[string]bool{}
		}
		if !task.Observations[key] {
			task.Observations[key] = true
			task.Discoveries++
		}
	}
	task.Work, task.ReadsSinceNote = &work, 0
	if err := store.saveTask(task); err != nil {
		return "", err
	}
	if err := appendAgentEvent(ctx, store, "task_work_updated", map[string]any{"task_id": task.ID, "work": work, "resolved": update.Resolved}); err != nil {
		return "", err
	}
	return `{"ok":true,"message":"工作记录已保存；继续 next，已完成的检查不重复执行"}`, nil
}

// Omissions are not resolutions. Old checkpoints acquire deterministic IDs on
// their first update, without migrating or replacing the saved conversation.
func mergeTaskWork(previous *taskWorkState, work *taskWorkState, resolved []taskResolution) error {
	items := []taskOpenItem{}
	if previous != nil {
		for _, group := range []struct {
			kind   string
			values []string
		}{{"pending", previous.Pending}, {"finding", previous.Findings}} {
			for _, value := range group.values {
				items = append(items, taskOpenItem{contentKey(group.kind + "\x00" + value)[:16], group.kind, value})
			}
		}
		for _, check := range previous.Checks {
			items = append(items, taskOpenItem{contentKey("check\x00" + check.Name)[:16], "check", check.Name})
		}
	}
	removed := map[string]bool{}
	for _, resolution := range resolved {
		if strings.TrimSpace(resolution.Evidence) == "" || utf8.RuneCountInString(resolution.Evidence) > 1000 || !slices.ContainsFunc(items, func(item taskOpenItem) bool { return item.ID == resolution.ID }) {
			return errors.New("resolved 必须引用现有事项 ID，并提供不超过 1000 字符的解决证据或撤销原因")
		}
		removed[resolution.ID] = true
	}
	work.OpenItems = nil
	for _, item := range items {
		if removed[item.ID] || item.Kind == "check" {
			continue
		}
		values := &work.Pending
		if item.Kind == "finding" {
			values = &work.Findings
		}
		if !slices.Contains(*values, item.Text) {
			*values = append(*values, item.Text)
		}
	}
	for _, group := range []struct {
		kind   string
		values []string
	}{{"pending", work.Pending}, {"finding", work.Findings}} {
		seen := map[string]bool{}
		for _, value := range group.values {
			if !seen[value] {
				work.OpenItems = append(work.OpenItems, taskOpenItem{contentKey(group.kind + "\x00" + value)[:16], group.kind, value})
				seen[value] = true
			}
		}
	}
	if previous != nil {
		work.Completed = appendUnique(previous.Completed, work.Completed...)
		work.Completed = work.Completed[max(0, len(work.Completed)-12):]
		work.Notes = appendUnique(previous.Notes, work.Notes...)
		work.Notes = work.Notes[max(0, len(work.Notes)-12):]
	}
	return nil
}

func appendUnique(values []string, additions ...string) []string {
	result := append([]string(nil), values...)
	for _, value := range additions {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func (a *App) taskMemory(ctx context.Context, store *runStore) (string, error) {
	execution, _ := ctx.Value(agentExecutionKey{}).(agentExecution)
	task := execution.checkpoint
	if task == nil {
		return "", nil
	}
	revision, err := a.fileRevision(store.id, true)
	if err != nil {
		return "", err
	}
	if task.Work != nil {
		if err := mergeTaskWork(nil, task.Work, nil); err != nil {
			return "", err
		}
		for i := range task.Work.Checks {
			check := &task.Work.Checks[i]
			if check.Revision != revision || check.RequestRevision != task.RequestRevision {
				check.Status = "unverified"
			}
		}
	}
	state, err := store.readState()
	if err != nil {
		return "", err
	}
	evidence, err := store.currentEvidence(revision)
	if err != nil {
		return "", err
	}
	memory := struct {
		Contract  *agentTaskContract      `json:"contract,omitempty"`
		Work      *taskWorkState          `json:"work,omitempty"`
		Approvals map[string]userDecision `json:"user_approvals,omitempty"`
		Pending   *userDecision           `json:"pending_decision,omitempty"`
		Revision  string                  `json:"current_revision"`
		Evidence  []verificationRecord    `json:"verification_evidence,omitempty"`
	}{task.Contract, task.Work, state.Approvals, state.PendingDecision, revision, evidence}
	data, err := json.Marshal(memory)
	if err != nil {
		return "", err
	}
	text := "\n[持久任务记录]\n" + string(data) + "\n以真实 user_approvals 为确认依据，contract.confirmed 和 work 是 Agent 记录。沿用 work 中的审阅结论与未解决问题；revision 不一致的验收证据仅供参考，不可宣称当前版本已通过。verification_evidence 是同一运行当前版本的成功验证，可用 segment_id 回读结果，再用 task_id/call_id 引用，无需重跑。解决待办或问题时用 update_task_state.resolved 引用 open_items 的 ID 并说明证据。验收项 name 使用 contract.acceptance 原文；静态检查用 reviewed，运行验证用 passed。完成本步验收后交回结果，不为摘要没有展开源码而重读。"
	text += "\npending/findings 只记录阻碍本次任务验收的事项；非阻塞建议、可选补测和用户明确接受的限制放 notes，并在交付中如实披露。旧问题转为 notes 时必须同时用 resolved 引用其 open_items ID，保留未运行验证等事实；不能通过只写 notes 删除旧问题，不能以用户拒绝可选补测为由反复要求检查。仍属于当前契约的必需验收项不能因此豁免。"
	if task.ReadsSinceNote >= 6 {
		text += "\n已连续读取多次，下一步先用 update_task_state 保存已形成的结论、具体问题和下一步，再继续。"
	}
	if task.ReadRepeats >= 3 {
		text += "\n多次读取没有新内容。请依据已有证据结束本步审阅；缺少运行证据时返回具体待验证项交主 Agent 安排。"
	}
	return text, nil
}

type readRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (t *taskCheckpoint) observeFile(raw string) bool {
	var output struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Hash    string `json:"content_sha256"`
		Offset  int    `json:"offset"`
	}
	if json.Unmarshal([]byte(raw), &output) != nil || output.Path == "" || output.Hash == "" || output.Content == "" {
		return false
	}
	key := output.Path + "\x00" + output.Hash
	start, end := output.Offset, output.Offset+utf8.RuneCountInString(output.Content)
	return t.observeRange(key, start, end)
}

func (t *taskCheckpoint) observeRecall(root, id, raw string) bool {
	var output struct {
		SegmentID string `json:"segment_id"`
		Offset    int    `json:"offset"`
		Next      int    `json:"next_offset"`
	}
	if json.Unmarshal([]byte(raw), &output) != nil || output.SegmentID == "" {
		return false
	}
	segment, err := workspace.ReadContextSegment(root, id, output.SegmentID)
	if err != nil {
		return false
	}
	// A historical copy of a file is the same observation as its original read.
	if segment.Kind == "file_read" {
		return t.observeFile(segment.Original)
	}
	return t.observeRange("segment:"+segment.ID, output.Offset, output.Next)
}

func (t *taskCheckpoint) observeRange(key string, start, end int) bool {
	if end <= start {
		return false
	}
	if t.ReadRanges == nil {
		t.ReadRanges = map[string][]readRange{}
	}
	ranges := append([]readRange(nil), t.ReadRanges[key]...)
	covered := start
	for _, r := range ranges {
		if r.Start > covered {
			break
		}
		if r.End > covered {
			covered = r.End
		}
	}
	if covered >= end {
		return false
	}
	ranges = append(ranges, readRange{start, end})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	merged := []readRange{}
	for _, r := range ranges {
		if len(merged) > 0 && r.Start <= merged[len(merged)-1].End {
			merged[len(merged)-1].End = max(merged[len(merged)-1].End, r.End)
		} else {
			merged = append(merged, r)
		}
	}
	t.ReadRanges[key] = merged
	return true
}

func (t *taskCheckpoint) stalledResult() string {
	result := agentResult{Status: "blocked", Summary: errTaskStalled.Error(), Artifacts: []string{}, Verification: []string{"本步骤尚未完成验收，不能标记为通过"}, Pending: []string{"主 Agent 请根据已保存结论核对已有产物，安排尚缺的具体检查或向用户交付当前状态；不要原样重新委派"}, Questions: []string{}, Previews: []agentPreview{}}
	if t.Work != nil {
		result.Summary += "；当前步骤：" + t.Work.Step
		result.Pending = append(result.Pending, t.Work.Pending...)
		result.Verification = append(result.Verification, t.Work.Findings...)
		for _, check := range t.Work.Checks {
			result.Verification = append(result.Verification, fmt.Sprintf("%s [%s] %s", check.Name, check.Status, check.Evidence))
		}
	}
	data, _ := json.Marshal(result)
	return string(data)
}

func contentKey(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
