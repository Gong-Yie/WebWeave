package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"webweave/workspace"
)

func TestTaskWorkResolutionAndSharedEvidence(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "durable-evidence")
	revision, err := app.fileRevision(run.store.id, true)
	if err != nil {
		t.Fatal(err)
	}
	source := &taskCheckpoint{ID: "engineer", Tools: map[string]*taskTool{"check": {Name: "terminal_run", Arguments: `{"command":"node verify.js","intent":"verify"}`, Status: "completed", Revision: revision, Output: `{"returncode":0,"stdout":"keyboard passed"}`}}}
	if err := run.store.saveTask(source); err != nil {
		t.Fatal(err)
	}
	segment, err := workspace.SaveContextSegment(app.root, run.store.id, "terminal_run", "keyboard evidence", source.Tools["check"].Output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.store.verification("keyboard", &verificationRecord{Success: true, Output: source.Tools["check"].Output, TaskID: source.ID, CallID: "check", Command: "node verify.js", Revision: revision, SegmentID: segment.ID}); err != nil {
		t.Fatal(err)
	}
	task := &taskCheckpoint{ID: "reviewer", Tools: map[string]*taskTool{}, Work: &taskWorkState{Completed: []string{"已检查导航源码"}, Pending: []string{"验证键盘"}, Findings: []string{"E_NAV"}}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task})
	memory, err := app.taskMemory(ctx, run.store)
	if err != nil || !strings.Contains(memory, `"verification_evidence"`) || !strings.Contains(memory, segment.ID) || !strings.Contains(memory, "node verify.js") {
		t.Fatal("shared evidence index was not visible to the reader", memory, err)
	}
	work := taskWorkState{Phase: "review", Step: "键盘验收", Next: "交付结果", Checks: []taskCheck{{Name: "键盘操作", Status: "passed", Evidence: "engineer/check：keyboard passed", TaskID: "engineer", CallID: "check"}}}
	data, _ := json.Marshal(work)
	output, err := app.updateTaskWork(ctx, run.store, string(data))
	if err != nil || !strings.Contains(output, `"ok":true`) || len(task.Work.OpenItems) != 2 || len(task.Work.Completed) != 1 || task.Work.Checks[0].TaskID != source.ID {
		t.Fatal("omission erased work or shared evidence was rejected", output, err)
	}
	before, _ := json.Marshal(task.Work)
	output, err = app.updateTaskWork(ctx, run.store, `{"phase":"review","step":"键盘验收","next":"交付","resolved":[{"id":"unknown","evidence":"声称完成"}]}`)
	after, _ := json.Marshal(task.Work)
	if err != nil || !strings.Contains(output, `"ok":false`) || string(before) != string(after) {
		t.Fatal("invalid resolution changed persistent work", output, err)
	}
	resolved := []taskResolution{}
	for _, item := range task.Work.OpenItems {
		resolved = append(resolved, taskResolution{item.ID, "engineer/check 验证已通过"})
	}
	data, _ = json.Marshal(struct {
		taskWorkState
		Resolved []taskResolution `json:"resolved"`
	}{work, resolved})
	output, err = app.updateTaskWork(ctx, run.store, string(data))
	if err != nil || !strings.Contains(output, `"ok":true`) || len(task.Work.Pending)+len(task.Work.Findings)+len(task.Work.OpenItems) != 0 {
		t.Fatal("explicit resolution did not close old work", output, err)
	}
	if task.Discoveries != 1 {
		t.Fatal("repeated work note manufactured progress", task.Discoveries)
	}
	task.Contract = &agentTaskContract{Acceptance: []string{"键盘操作"}}
	pending, err := app.completionPending(run.store, task)
	if err != nil || len(pending) != 0 {
		t.Fatal("valid acceptance blocked", pending, err)
	}
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "index.html"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	pending, err = app.completionPending(run.store, task)
	if err != nil || len(pending) == 0 {
		t.Fatal("stale cross-task evidence satisfied acceptance", err)
	}
}

func TestTaskCompletionRejectsUnfinishedResults(t *testing.T) {
	for _, test := range []struct {
		name, text string
		contract   bool
	}{
		{"plain text", "已完成", false},
		{"malformed JSON", "{}", false},
		{"missing acceptance", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "completion-gate")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if test.contract {
					serveModelResponse(w, false, []inputItem{taskMessageOutput("自称完成")})
					return
				}
				serveModelResponse(w, false, []inputItem{messageOutput(test.text)})
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, false)
			task := &taskCheckpoint{ID: "unfinished", Items: []inputItem{{"role": "user", "content": "完成键盘验收"}}, Tools: map[string]*taskTool{}}
			if test.contract {
				task.Contract = &agentTaskContract{Acceptance: []string{"键盘操作"}}
			}
			ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task, interactive: true})
			_, err := app.runAgent(ctx, run.store, task.Items, defaultAgents()[0], false)
			if err == nil || task.Status != "blocked" || calls.Load() > 2 {
				t.Fatal("unfinished result became completed", task.Status, calls.Load(), err)
			}
		})
	}
}

func TestTaskWorkExplicitlyWithdrawsSupersededCheck(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "superseded-check")
	task := &taskCheckpoint{ID: "changed-scope", RequestRevision: "new-scope", Tools: map[string]*taskTool{}, Work: &taskWorkState{Checks: []taskCheck{{Name: "旧版导航验收", Status: "failed", Evidence: "旧界面缺陷", RequestRevision: "old-scope"}}}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task})
	work := taskWorkState{Phase: "review", Step: "按新范围交付", Next: "交付结果"}
	data, _ := json.Marshal(work)
	output, err := app.updateTaskWork(ctx, run.store, string(data))
	if err != nil || !strings.Contains(output, `"ok":true`) || len(task.Work.Checks) != 1 {
		t.Fatal("omission silently erased an old check", output, err)
	}
	data, _ = json.Marshal(struct {
		taskWorkState
		Resolved []taskResolution `json:"resolved"`
	}{work, []taskResolution{{contentKey("check\x00旧版导航验收")[:16], "用户已撤销旧界面范围，该验收不适用于本次任务"}}})
	output, err = app.updateTaskWork(ctx, run.store, string(data))
	pending, checkErr := app.completionPending(run.store, task)
	if err != nil || checkErr != nil || !strings.Contains(output, `"ok":true`) || len(task.Work.Checks) != 0 || len(pending) != 0 {
		t.Fatal("explicit withdrawal left the new scope blocked", output, pending, err, checkErr)
	}
}

func TestTaskFeedbackCancelsUnstartedOperations(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "feedback-order")
	args := `{"file_path":"obsolete.html","content":"obsolete","timeout":1}`
	// Even an earlier approved retry must yield to the user's latest correction.
	task := &taskCheckpoint{ID: "old-task", AgentID: "main", Status: "stopped", Items: []inputItem{{"role": "user", "content": "旧需求"}, {"type": "function_call", "call_id": "old-write", "name": "create_file", "arguments": args}}, Tools: map[string]*taskTool{"old-write": {Name: "create_file", Arguments: args, Status: "pending", RetryApproved: true}}}
	if err := run.store.saveTask(task); err != nil {
		t.Fatal(err)
	}
	if err := run.store.selectTask(task.ID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []inputItem `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Input) != 4 || !strings.Contains(itemString(request.Input[2], "output"), "USER_FEEDBACK_SUPERSEDED") || userText(request.Input[3]) != "取消旧实现，只检查现有文件" {
			t.Error("model did not receive cancellation before new feedback", request.Input)
		}
		serveModelResponse(w, false, []inputItem{taskMessageOutput("已按新需求处理")})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	if err := app.startMessage(run.store.id, "取消旧实现，只检查现有文件", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, jobDone(t, run))
	if _, err := os.Stat(filepath.Join(app.runRoot("result", run.store.id), "obsolete.html")); !os.IsNotExist(err) {
		t.Fatal("stale write executed", err)
	}
	saved, err := run.store.loadTask(task.ID)
	if err != nil || saved.Status != "completed" || len(pendingTaskCalls(saved.Items)) != 0 {
		t.Fatal("feedback left an unpaired checkpoint", err)
	}
}

func TestContextSummaryBoundsKeepExactArchive(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "summary-bounds")
	summary := contextSummary{Goal: "保存事实", Artifacts: []string{}, Errors: []string{"E_NAV: " + strings.Repeat("exact-error", 100)}}
	for i := 0; i < 30; i++ {
		summary.Artifacts = append(summary.Artifacts, fmt.Sprintf("assets/file-%d.js", i))
	}
	bounded, err := app.boundSummary(run.store, summary, 4096)
	if err != nil || len(bounded.Artifacts) != 4 || bounded.Archive == "" {
		t.Fatal("unbounded summary", err)
	}
	archive, err := workspace.ReadContextSegment(app.root, run.store.id, bounded.Archive)
	if err != nil || !strings.Contains(archive.Original, "assets/file-29.js") || !strings.Contains(archive.Original, summary.Errors[0]) {
		t.Fatal("summary bounds lost exact paths or error", err)
	}
	second, err := app.boundSummary(run.store, bounded, 4096)
	if err != nil || second.Archive != bounded.Archive {
		t.Fatal("bounded summary was archived repeatedly", err)
	}
}

func TestLongWorkflowCompletesAcrossCompressionAndRestart(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "long-flow")
	source := strings.Repeat("function section() { return '键盘与导航验收材料'; }\n", 10000)
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "app.js"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	contract := &agentTaskContract{Phase: "review", UserRequest: "制作单页并验收键盘", Scope: []string{"当前页面"}, Acceptance: []string{"导航源码审阅"}, Deliverables: []string{"index.html"}}
	paused := make(chan struct{})
	var childCalls, parentCalls, summaries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input        json.RawMessage `json:"input"`
			Instructions string          `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Input) > 0 && request.Input[0] == '"' {
			summaries.Add(1)
			serveModelResponse(w, false, []inputItem{messageOutput(`{"goal":"制作单页并完成检查","completed":["已整理旧记录"],"pending":[]}`)})
			return
		}
		if strings.Contains(request.Instructions, "（ID：main）") {
			if parentCalls.Add(1) == 1 {
				args, _ := json.Marshal(map[string]any{"agent_id": "code", "task": "完成导航源码审阅", "contract": contract, "timeout": 20})
				serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "delegate", "name": "call_agent", "arguments": string(args)}})
			} else {
				serveModelResponse(w, false, []inputItem{taskMessageOutput("任务已完成并交付")})
			}
			return
		}
		n := childCalls.Add(1)
		if n == 20 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(paused)
			<-r.Context().Done()
			return
		}
		if n > 62 {
			t.Error("workflow did not converge")
			serveModelResponse(w, false, []inputItem{messageOutput("unexpected")})
			return
		}
		if n > 2 && n <= 61 && !strings.Contains(request.Instructions, "E_NAV") {
			t.Error("compression or restart lost unresolved finding")
		}
		if n == 62 {
			serveModelResponse(w, false, []inputItem{taskMessageOutput("导航源码审阅完成")})
			return
		}
		name, args := "file_read", fmt.Sprintf(`{"file_path":"app.js","offset":%d,"max_chars":7000,"timeout":2}`, n*3000)
		if n == 1 {
			name, args = "create_file", `{"file_path":"index.html","content":"只生成一次","timeout":2}`
		}
		if n == 2 || n%6 == 0 || n == 61 {
			name = "update_task_state"
			work := taskWorkState{Phase: "review", Step: "导航源码审阅", Findings: []string{"E_NAV"}, Next: "继续读取尚未审阅的范围"}
			resolved := []taskResolution{}
			if n == 61 {
				work.Findings = nil
				work.Checks = []taskCheck{{Name: "导航源码审阅", Status: "reviewed", Evidence: "app.js：已完成委派范围内源码审阅；未运行浏览器", CallID: ""}}
				work.Next = "交付结论"
				resolved = []taskResolution{{contentKey("finding\x00E_NAV")[:16], "已完成导航源码检查，未发现该项缺陷；运行验证另行报告"}}
			}
			data, _ := json.Marshal(struct {
				taskWorkState
				Resolved []taskResolution `json:"resolved"`
			}{work, resolved})
			args = string(data)
		}
		serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": fmt.Sprintf("step-%d", n), "name": name, "arguments": args}})
	}))
	t.Cleanup(server.Close)
	cfg := useProvider(t, app, server.URL, false)
	cfg.Context.KeepRecentItems = 1
	cfg.Context.MaxInputTokens = 16000
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.startMessage(run.store.id, "制作单页并验收键盘", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		t.Fatal("workflow did not reach restart point")
	}
	done := jobDone(t, run)
	if _, err := app.stopRun(run.store.id); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, done)
	path := filepath.Join(app.runRoot("result", run.store.id), "index.html")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := New(app.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.Close)
	freshRun, err := fresh.runtime(run.store.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.startMessage(run.store.id, "", "", nil, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-jobDone(t, freshRun):
	case <-time.After(30 * time.Second):
		t.Fatal("resumed workflow did not finish")
	}
	state, err := freshRun.store.readState()
	if err != nil || state.Status != "completed" || childCalls.Load() != 62 || parentCalls.Load() != 2 || summaries.Load() < 3 {
		t.Fatalf("long workflow failed: state=%+v child=%d parent=%d summaries=%d err=%v", state, childCalls.Load(), parentCalls.Load(), summaries.Load(), err)
	}
	parent, err := freshRun.store.loadTask(state.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := freshRun.store.loadTask(parent.Tools["delegate"].ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if child == nil || child.Work == nil {
		t.Fatal("child checkpoint or work record missing")
	}
	if child.Status != "completed" || child.Slice < 1 || len(child.Work.OpenItems) != 0 || len(pendingTaskCalls(child.Items)) != 0 {
		t.Fatalf("checkpoint lost task closure: status=%s slices=%d reason=%s", child.Status, child.Slice, child.Reason)
	}
	after, err := os.Stat(path)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("restart replayed a completed write", err)
	}
	_, events, err := freshRun.store.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	compressions := 0
	for _, event := range events {
		if event.Type == "context_compressed" {
			compressions++
		}
	}
	if compressions < 3 {
		t.Fatal("workflow did not cross three compression boundaries", compressions)
	}
	t.Logf("completed %d child requests, %d compressions (%d summary batches), %d segment yields with restart", childCalls.Load(), compressions, summaries.Load(), child.Slice)
}

func TestSummaryProgressResumesAfterBatchFailure(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "summary-progress")
	var requests atomic.Int32
	var expectedRemaining string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		n := requests.Add(1)
		if n == 2 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "limit", "object": "response", "status": "incomplete", "output": []any{}, "incomplete_details": map[string]string{"reason": "max_output_tokens"}})
			return
		}
		if n == 3 {
			var input summaryRequest
			if err := json.Unmarshal([]byte(request.Input), &input); err != nil || len(input.History) == 0 || !strings.HasPrefix(expectedRemaining, strings.TrimPrefix(input.History[0], "[连续历史片段]\n")) {
				t.Error("successful summary batches replayed after restart", err)
			}
		}
		serveModelResponse(w, false, []inputItem{messageOutput(`{"goal":"保留进度","completed":["已处理前批历史"]}`)})
	}))
	t.Cleanup(server.Close)
	cfg := useProvider(t, app, server.URL, false)
	cfg.Context.SummaryInputTokens, cfg.Context.KeepRecentItems = 3000, 1
	task := &taskCheckpoint{ID: "summary-task", AgentID: "main", Tools: map[string]*taskTool{}, Items: []inputItem{{"role": "user", "content": "保留需求"}, {"role": "assistant", "content": strings.Repeat("待整理材料", 2500)}, {"role": "user", "content": "继续"}}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task})
	client, err := newClient(cfg.LLM.Main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.compressContext(ctx, client, run.store, task.Items, cfg); !errors.Is(err, errContextCompression) {
		t.Fatal("expected isolated second-batch failure", err)
	}
	restored, err := run.store.loadTask(task.ID)
	if err != nil || restored.Compression == nil || restored.Compression.Batches != 1 || len(restored.Compression.Remaining) == 0 {
		t.Fatal("completed batch progress was not persisted", err)
	}
	expectedRemaining = restored.Compression.Remaining[0]
	fresh, err := New(app.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.Close)
	freshRun, err := fresh.runtime(run.store.id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: restored})
	if _, err := fresh.compressContext(ctx, client, freshRun.store, restored.Items, cfg); err != nil || restored.Compression != nil {
		t.Fatal("summary did not resume and close its checkpoint", err)
	}
}
