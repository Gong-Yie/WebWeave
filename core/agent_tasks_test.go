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

func TestTaskNestedCheckpointRecovery(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "nested-recovery")
	file := filepath.Join(app.runRoot("result", run.store.id), "index.html")
	if err := workspace.WriteAtomic(file, []byte("已写入"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	writeArgs := `{"file_path":"index.html","content":"已写入","timeout":1}`
	delegateArgs := `{"agent_id":"code","task":"完成页面","timeout":1}`
	child := &taskCheckpoint{ID: "child-task", AgentID: "code", Status: "stopped", Items: []inputItem{{"role": "user", "content": "完成页面"}, {"type": "function_call", "call_id": "write", "name": "create_file", "arguments": writeArgs}}, Tools: map[string]*taskTool{"write": {Name: "create_file", Arguments: writeArgs, Status: "running"}}}
	parent := &taskCheckpoint{ID: "parent-task", AgentID: "main", Status: "stopped", Items: []inputItem{{"role": "user", "content": "制作页面"}, {"type": "function_call", "call_id": "delegate", "name": "call_agent", "arguments": delegateArgs}}, Tools: map[string]*taskTool{"delegate": {Name: "call_agent", Arguments: delegateArgs, Status: "running", ChildID: child.ID}}}
	for _, task := range []*taskCheckpoint{child, parent} {
		if err := run.store.saveTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.store.selectTask(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := run.store.saveContext(parent.Items, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.store.dir, "context.json")); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input        []inputItem `json:"input"`
			Instructions string      `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			if !strings.Contains(request.Instructions, "（ID：code）") || len(request.Input) != 3 || !strings.Contains(itemString(request.Input[2], "output"), `"recovered":true`) {
				t.Error("child checkpoint was not recovered with the existing write result")
			}
			result := agentResult{Status: "completed", Summary: "恢复完成", Artifacts: []string{"index.html"}, Verification: []string{"已核对文件内容，未运行浏览器"}, Pending: []string{}, Questions: []string{}, Previews: []agentPreview{}}
			data, _ := json.Marshal(result)
			serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
		} else {
			if !strings.Contains(request.Instructions, "（ID：main）") || len(request.Input) != 3 || itemString(request.Input[2], "call_id") != "delegate" {
				t.Error("parent pending delegation was lost")
			}
			serveModelResponse(w, false, []inputItem{taskMessageOutput("恢复并交付")})
		}
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
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
	awaitJob(t, jobDone(t, freshRun))
	detail, err := fresh.detail(run.store.id)
	after, statErr := os.Stat(file)
	if err != nil || statErr != nil || detail.Status != "completed" || calls.Load() != 2 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("recovery duplicated work or failed: %+v %v %v", detail.State, err, statErr)
	}
	for _, id := range []string{parent.ID, child.ID} {
		saved, err := freshRun.store.loadTask(id)
		if err != nil || saved.Status != "completed" || len(pendingTaskCalls(saved.Items)) != 0 {
			t.Fatalf("unpaired task after recovery: %+v %v", saved, err)
		}
	}
}

func TestTaskUnknownOperationRequiresUser(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "unknown-operation")
	raw := `{"command":"Set-Content -LiteralPath marker.txt -Value replayed","timeout":1}`
	task := &taskCheckpoint{ID: "unknown-task", AgentID: "main", Status: "running", Items: []inputItem{{"role": "user", "content": "继续"}, {"type": "function_call", "call_id": "unknown", "name": "terminal_run", "arguments": raw}}, Tools: map[string]*taskTool{"unknown": {Name: "terminal_run", Arguments: raw, Status: "running"}}}
	if err := run.store.saveTask(task); err != nil {
		t.Fatal(err)
	}
	if err := run.store.selectTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := run.store.saveContext(task.Items, nil); err != nil {
		t.Fatal(err)
	}
	if err := app.startMessage(run.store.id, "", "", nil, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, jobDone(t, run))
	detail, err := app.detail(run.store.id)
	if err != nil || detail.Status != "awaiting_user" || detail.State.PendingDecision == nil || detail.State.PendingDecision.RecoveryCall != "unknown" {
		t.Fatalf("unknown operation was not held for reconciliation: %+v %v", detail.State, err)
	}
	if _, err := os.Stat(filepath.Join(app.runRoot("result", run.store.id), "marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("unknown operation was replayed: %v", err)
	}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/unknown-operation/continue", nil), 409)
	text, err := app.answerDecision(run.store.id, detail.State.PendingDecision.ID, "approve")
	if err != nil || !strings.Contains(text, "确认") {
		t.Fatal("explicit retry could not be recorded", err)
	}
	saved, err := run.store.loadTask(task.ID)
	if err != nil || saved.Tools["unknown"].Status != "pending" {
		t.Fatal("user retry did not release precisely the interrupted operation", err)
	}
}

func TestTaskInstanceRecovery(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "instance-recovery")
	config := &launchConfig{Command: "test-server {host} {port}", Cwd: ".", HealthPath: "/"}
	instance := &instanceRecord{InstanceID: "existing", Status: "stopped", Command: config.Command, Cwd: app.runRoot("result", run.store.id), HealthPath: "/"}
	if err := run.store.saveInstance(instance); err != nil {
		t.Fatal(err)
	}
	record := &taskTool{Name: "web_instance", Arguments: `{"action":"start"}`, Status: "running", Launch: config}
	output, recovered, err := app.recoverTaskTool(context.Background(), run.store, record)
	if err != nil || !recovered || !strings.Contains(output, `"instance_id":"existing"`) {
		t.Fatalf("registered instance was not reconciled: %s %v", output, err)
	}
	record.Launch = &launchConfig{Command: "different {host} {port}", Cwd: ".", HealthPath: "/"}
	_, recovered, err = app.recoverTaskTool(context.Background(), run.store, record)
	var pause *taskPause
	if recovered || !errors.As(err, &pause) || pause.Status != "awaiting_user" {
		t.Fatal("unrelated instance was mistaken for the original operation", err)
	}
}

func TestTaskPlanConfirmation(t *testing.T) {
	app := testApp(t)
	response := apiRequest(t, app, "POST", "/api/runs", nil)
	assertStatus(t, response, 201)
	var created struct {
		Run runDetail `json:"run"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Run.RunID
	run, err := app.runtime(id)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []inputItem `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "premature", "name": "create_file", "arguments": `{"file_path":"index.html","content":"提前实现","timeout":1}`}})
		case 2, 3:
			if calls.Load() == 2 && !strings.Contains(itemString(request.Input[len(request.Input)-1], "output"), "PLAN_CONFIRMATION_REQUIRED") {
				t.Error("implementation bypassed the plan gate")
			}
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": fmt.Sprintf("plan-%d", calls.Load()), "name": "request_user_decision", "arguments": `{"kind":"plan","question":"制作中文单页科技博客，深色青蓝风格，先用演示文章，不接后端。确认后开始实现。","timeout":1}`}})
		case 4:
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "approved-write", "name": "create_file", "arguments": `{"file_path":"index.html","content":"确认后实现","timeout":1}`}})
		default:
			serveModelResponse(w, false, []inputItem{taskMessageOutput("首版已完成")})
		}
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/messages", map[string]any{"content": "科技风博客"}), 202)
	awaitJob(t, jobDone(t, run))
	state, err := run.store.readState()
	if err != nil || state.Status != "awaiting_user" || state.PendingDecision == nil || len(state.Approvals) != 0 {
		t.Fatalf("plan was self-approved: %+v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(app.runRoot("result", id), "index.html")); !os.IsNotExist(err) {
		t.Fatal("website was produced before approval")
	}
	oldID := state.PendingDecision.ID
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/continue", nil), 409)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/messages", map[string]any{"content": "先调整一下布局"}), 202)
	awaitJob(t, jobDone(t, run))
	state, err = run.store.readState()
	if err != nil || state.PendingDecision == nil || state.PendingDecision.ID == oldID || len(state.Approvals) != 0 {
		t.Fatal("free-text correction granted approval", err)
	}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/decisions", map[string]any{"decision_id": oldID, "choice": "approve"}), 409)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/decisions", map[string]any{"decision_id": state.PendingDecision.ID, "choice": "approve"}), 202)
	awaitJob(t, jobDone(t, run))
	state, err = run.store.readState()
	data, readErr := os.ReadFile(filepath.Join(app.runRoot("result", id), "index.html"))
	if err != nil || readErr != nil || state.Status != "completed" || state.Approvals["plan"].AnsweredAt == "" || string(data) != "确认后实现" {
		t.Fatalf("approved plan did not resume: %+v %v %v", state, err, readErr)
	}
}

func TestTaskVerificationReuse(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "verification-reuse")
	task := &taskCheckpoint{ID: "check-task", AgentID: "main", Items: []inputItem{}, Tools: map[string]*taskTool{}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{agent: defaultAgents()[0], checkpoint: task})
	revision, err := app.projectRevision(run.store.id)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"command":"Write-Output checked","intent":"verify","timeout":5}`
	key := verificationKey("terminal_run", raw, revision)
	evidence := `{"returncode":0,"stdout":"saved evidence"}`
	if _, err := run.store.verification(key, &verificationRecord{Output: evidence, Success: true}); err != nil {
		t.Fatal(err)
	}
	output, err := app.runTaskTool(ctx, run.store, defaultAgents()[0], &taskTool{Name: "terminal_run", Arguments: raw, Status: "pending"})
	if err != nil || output != evidence {
		t.Fatalf("existing verification was not reused: %s %v", output, err)
	}
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "_check.js"), []byte("new checker"), 0644); err != nil {
		t.Fatal(err)
	}
	progressRevision, err := app.projectRevision(run.store.id)
	if err != nil || progressRevision != revision {
		t.Fatal("temporary checker was counted as product progress", err)
	}
	probeRevision, err := app.fileRevision(run.store.id, true)
	if err != nil || probeRevision == revision {
		t.Fatal("checker change did not invalidate verification", err)
	}
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "index.html"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	newRevision, err := app.fileRevision(run.store.id, true)
	if err != nil || newRevision == revision {
		t.Fatal("file write did not invalidate checks", err)
	}
	newKey := verificationKey("terminal_run", raw, newRevision)
	if _, err := run.store.verification(newKey, &verificationRecord{Failures: 2}); err != nil {
		t.Fatal(err)
	}
	_, err = app.runTaskTool(ctx, run.store, defaultAgents()[0], &taskTool{Name: "terminal_run", Arguments: raw, Status: "pending"})
	var pause *taskPause
	if !errors.As(err, &pause) || pause.Status != "blocked" {
		t.Fatal("repeated failing verification was not stopped", err)
	}
	state, err := run.store.readState()
	if err != nil {
		t.Fatal(err)
	}
	state.Approvals["deploy"] = userDecision{ID: "release", Kind: "deploy", Revision: revision}
	if err := workspace.WriteJSONAtomic(filepath.Join(run.store.dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, agentExecutionKey{}, agentExecution{interactive: true})
	denied, err := app.taskToolPermission(ctx, run.store, "terminal_run", `{"intent":"deploy"}`)
	if err != nil || !strings.Contains(denied, "DEPLOY_CONFIRMATION_REQUIRED") {
		t.Fatal("changed files reused stale release approval", err)
	}
}

func TestTaskProtocolTextIsNotCompletion(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "protocol-text")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveModelResponse(w, false, []inputItem{messageOutput("<｜｜DSML｜｜ calls>not a tool call")})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	if err := app.startMessage(run.store.id, "继续任务", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, jobDone(t, run))
	state, err := run.store.readState()
	if err != nil || state.Status != "blocked" {
		t.Fatalf("protocol text was reported as completion: %+v %v", state, err)
	}
}

func TestTaskChangeAndDeploymentDecisions(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "stage-decisions")
	task := &taskCheckpoint{ID: "stage-task", AgentID: "main", Items: []inputItem{}, Tools: map[string]*taskTool{}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{agent: defaultAgents()[0], interactive: true, checkpoint: task})
	for _, kind := range []string{"change", "deploy"} {
		_, err := app.requestDecision(ctx, run.store, fmt.Sprintf(`{"kind":%q,"question":"请确认具体变更与发布范围"}`, kind))
		var pause *taskPause
		if !errors.As(err, &pause) || pause.Status != "awaiting_user" {
			t.Fatalf("%s did not wait: %v", kind, err)
		}
		state, err := run.store.readState()
		if err != nil {
			t.Fatal(err)
		}
		if kind == "change" {
			if _, ok := state.Approvals["plan"]; ok {
				t.Fatal("major change retained the old implementation permission")
			}
		}
		if _, err := app.answerDecision(run.store.id, state.PendingDecision.ID, "approve"); err != nil {
			t.Fatal(err)
		}
	}
	denied, err := app.taskToolPermission(ctx, run.store, "terminal_run", `{"intent":"deploy"}`)
	if err != nil || denied != "" {
		t.Fatalf("approved release was still blocked: %s %v", denied, err)
	}
	if err := run.store.revokeDeployment(""); err != nil {
		t.Fatal(err)
	}
	denied, err = app.taskToolPermission(ctx, run.store, "terminal_run", `{"intent":"deploy"}`)
	if err != nil || !strings.Contains(denied, "DEPLOY_CONFIRMATION_REQUIRED") {
		t.Fatal("new user intent did not revoke the previous release permission", err)
	}
}

func TestTaskStopAndResumeChild(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "stop-child")
	started := make(chan struct{})
	var parentCalls, childCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(request.Instructions, "（ID：main）") {
			if parentCalls.Add(1) == 1 {
				serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "delegate", "name": "call_agent", "arguments": `{"agent_id":"code","task":"实现页面","timeout":5}`}})
			} else {
				serveModelResponse(w, false, []inputItem{taskMessageOutput("继续并交付")})
			}
			return
		}
		switch childCalls.Add(1) {
		case 1:
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "write", "name": "create_file", "arguments": `{"file_path":"index.html","content":"只写一次","timeout":1}`}})
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		default:
			data, _ := json.Marshal(agentResult{Status: "completed", Summary: "恢复成功", Artifacts: []string{"index.html"}, Verification: []string{"未验证浏览器"}, Pending: []string{}, Questions: []string{}, Previews: []agentPreview{}})
			serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
		}
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	if err := app.startMessage(run.store.id, "开始", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child never reached the cancellable request")
	}
	done := jobDone(t, run)
	if _, err := app.stopRun(run.store.id); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, done)
	state, err := run.store.readState()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := run.store.loadTask(state.TaskID)
	if err != nil || parent.Tools["delegate"].ChildID == "" || len(pendingTaskCalls(parent.Items)) != 1 {
		t.Fatal("stop discarded the parent-child waiting relationship", err)
	}
	childID := parent.Tools["delegate"].ChildID
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
	awaitJob(t, jobDone(t, freshRun))
	state, err = freshRun.store.readState()
	parent, loadErr := freshRun.store.loadTask(state.TaskID)
	if err != nil || loadErr != nil || state.Status != "completed" || parent.Tools["delegate"].ChildID != childID || parentCalls.Load() != 2 || childCalls.Load() != 3 {
		t.Fatalf("resume restarted the delegation: %+v calls=%d/%d %v %v", state, parentCalls.Load(), childCalls.Load(), err, loadErr)
	}
}
