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

	"webweave/workspace"
)

func TestModelContextBudget(t *testing.T) {
	for _, test := range []struct {
		name          string
		provider      ProviderConfig
		input, output int
	}{
		{"official", ProviderConfig{Model: "deepseek-flash", BaseURL: "https://api.deepseek.com/v1"}, 934464, 65536},
		{"pro", ProviderConfig{Model: "deepseek-v4-pro", BaseURL: "https://api.deepseek.com"}, 934464, 65536},
		{"proxy unknown", ProviderConfig{Model: "deepseek-flash", BaseURL: "https://proxy.test"}, 65536, 0},
		{"explicit", ProviderConfig{ContextWindow: 200000, MaxOutputTokens: 20000}, 180000, 20000},
		{"cost cap", ProviderConfig{ContextWindow: 200000, MaxOutputTokens: 20000, InputBudget: 100000}, 100000, 20000},
		{"unknown with cap", ProviderConfig{InputBudget: 12000}, 12000, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, output := modelContextBudget(test.provider, 65536)
			if input != test.input || output != test.output {
				t.Fatalf("budget=%d/%d want %d/%d", input, output, test.input, test.output)
			}
		})
	}
	p := ProviderConfig{Model: "a", BaseURL: "https://example.test"}
	usage := taskTokenUsage{Provider: providerIdentity(p), Estimated: 50000, Actual: 35000}
	if got := calibratedTokens(50000, p, usage); got < 40000 || got > 41000 {
		t.Fatalf("API usage not calibrated with margin: %d", got)
	}
	p.Model = "b"
	if got := calibratedTokens(50000, p, usage); got != 50000 {
		t.Fatal("calibration leaked to another model", got)
	}
	for _, p := range []ProviderConfig{{ContextWindow: -1}, {MaxOutputTokens: -1}, {InputBudget: -1}, {ContextWindow: 100, MaxOutputTokens: 100}} {
		p.BaseURL, p.Model = "https://example.test", "model"
		if err := validateProvider(&p, true); err == nil {
			t.Fatal("invalid capacity accepted", p)
		}
	}
}

func TestModelWorkingContextRetained(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "working-material")
	source := "SOURCE_BEGIN\n" + strings.Repeat("functionx();\n", 7000) + "SOURCE_END"
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "app.js"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input     []inputItem `json:"input"`
			MaxOutput int         `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			serveModelResponse(w, false, []inputItem{messageOutput("invalid")})
			return
		}
		if request.MaxOutput != 20000 {
			t.Error("effective model output reserve not sent", request.MaxOutput)
		}
		n := requests.Add(1)
		if n == 8 {
			found := false
			for _, item := range request.Input {
				if itemString(item, "type") != "function_call_output" {
					continue
				}
				var output struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(itemString(item, "output")), &output) == nil && output.Content == source {
					found = true
				}
			}
			if !found {
				t.Error("working source was pruned despite fitting the model budget")
			}
			serveModelResponse(w, false, []inputItem{taskMessageOutput("完成源码审阅")})
			return
		}
		if n > 8 {
			t.Error("unexpected extra request")
			serveModelResponse(w, false, []inputItem{messageOutput("stop")})
			return
		}
		name, args := "list_skills", `{"timeout":1}`
		if n == 1 {
			name, args = "file_read", `{"file_path":"app.js","max_chars":100000,"timeout":1}`
		}
		serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": fmt.Sprintf("working-%d", n), "name": name, "arguments": args}})
	}))
	t.Cleanup(server.Close)
	cfg := useProvider(t, app, server.URL, false)
	cfg.Context.MaxInputTokens = 8192
	cfg.LLM.Main.ContextWindow, cfg.LLM.Main.MaxOutputTokens = 200000, 20000
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.startMessage(run.store.id, "审阅现有源码", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, jobDone(t, run))
	detail, err := app.detail(run.store.id)
	if err != nil || detail.Status != "completed" || requests.Load() != 8 || detail.ContextUsage.Threshold != 144000 {
		t.Fatalf("incorrect budget or premature compaction: %+v %v", detail.ContextUsage, err)
	}
}

func TestTaskReadRangesAndWorkEvidence(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "work-ranges")
	path := filepath.Join(app.runRoot("result", run.store.id), "index.html")
	if err := workspace.WriteAtomic(path, []byte("中文0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	task := &taskCheckpoint{ID: "work-task", AgentID: "audit", Items: []inputItem{}, Tools: map[string]*taskTool{}}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task, agent: defaultAgents()[3], readOnly: true})
	for i, args := range []string{
		`{"file_path":"index.html","max_chars":5,"timeout":1}`,
		`{"file_path":"result/index.html","max_chars":5,"timeout":2}`,
		`{"file_path":"index.html","max_chars":3,"timeout":1}`,
		`{"file_path":"index.html","offset":4,"max_chars":4,"timeout":1}`,
	} {
		if _, err := app.runTaskTool(ctx, run.store, defaultAgents()[3], &taskTool{Name: "file_read", Arguments: args}); err != nil {
			t.Fatal(err)
		}
		want := []int{1, 1, 1, 2}[i]
		if task.Discoveries != want {
			t.Fatalf("read %d discoveries=%d want=%d", i, task.Discoveries, want)
		}
	}
	work := taskWorkState{Phase: "verification", Step: "检查导航", Completed: []string{"已审阅导航事件"}, Pending: []string{"验证键盘操作"}, Findings: []string{"index.html：缺少键盘操作证据"}, Checks: []taskCheck{{Name: "导航", Status: "passed", Evidence: "声称通过", CallID: "invented"}}, Next: "交回缺少的验证要求"}
	raw, _ := json.Marshal(work)
	output, err := app.updateTaskWork(ctx, run.store, string(raw))
	if err != nil || !strings.Contains(output, `"ok":false`) || task.Work != nil {
		t.Fatal("invented runtime evidence accepted", output, err)
	}
	revision, _ := app.fileRevision(run.store.id, true)
	task.Tools["verified"] = &taskTool{Name: "terminal_run", Status: "completed", Revision: revision, Output: `{"returncode":0,"stdout":"keyboard check passed"}`}
	work.Checks[0].CallID = "verified"
	raw, _ = json.Marshal(work)
	output, err = app.updateTaskWork(ctx, run.store, string(raw))
	if err != nil || !strings.Contains(output, `"ok":true`) {
		t.Fatal("valid work not saved", output, err)
	}
	saved, err := run.store.loadTask(task.ID)
	if err != nil || saved.Work == nil || saved.Work.Findings[0] != work.Findings[0] || saved.Discoveries != 3 {
		t.Fatal("work or read coverage was not persisted", err)
	}
	if err := workspace.WriteAtomic(path, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	memory, err := app.taskMemory(ctx, run.store)
	if err != nil || task.Work.Checks[0].Status != "unverified" || !strings.Contains(memory, work.Findings[0]) {
		t.Fatal("old verification survived a file change or finding lost", err)
	}
}

func TestTaskWorkSurvivesCompressionAndRestart(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "work-resume")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input        json.RawMessage `json:"input"`
			Instructions string          `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests.Add(1)
		if len(request.Input) > 0 && request.Input[0] == '"' {
			if !strings.Contains(string(request.Input), "E_NAV") {
				t.Error("summary did not receive persistent findings")
			}
			serveModelResponse(w, false, []inputItem{messageOutput(`{"goal":"检查博客","completed":["已读取文件"],"pending":[],"uncertain":[]}`)})
			return
		}
		for _, value := range []string{"E_NAV", "reviewed", "补充键盘验证", "verification"} {
			if !strings.Contains(request.Instructions, value) {
				t.Error("restored task lost working memory", value)
			}
		}
		data, _ := json.Marshal(agentResult{Status: "blocked", Summary: "保留问题结论，补充验证", Artifacts: []string{}, Verification: []string{"未验证：键盘证据仍缺失"}, Pending: []string{"补充键盘验证"}, Questions: []string{}, Previews: []agentPreview{}})
		serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
	}))
	t.Cleanup(server.Close)
	cfg := useProvider(t, app, server.URL, false)
	cfg.Context.KeepRecentItems = 1
	task := &taskCheckpoint{ID: "persistent-work", AgentID: "main", Status: "stopped", Tools: map[string]*taskTool{}, Work: &taskWorkState{Phase: "verification", Step: "检查博客", Findings: []string{"E_NAV：导航缺少键盘证据"}, Checks: []taskCheck{{Name: "源码审阅", Status: "reviewed", Evidence: "index.html 导航事件"}}, Next: "补充键盘验证"}}
	task.Items = []inputItem{{"role": "user", "content": "检查博客"}, {"role": "assistant", "content": strings.Repeat("历史操作。", 1500)}, {"role": "user", "content": "保留结论继续"}}
	revision, _ := app.fileRevision(run.store.id, true)
	task.Work.Revision, task.Work.Checks[0].Revision = revision, revision
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task, agent: defaultAgents()[0]})
	client, err := newClient(cfg.LLM.Main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.compressContext(ctx, client, run.store, task.Items, cfg); err != nil {
		t.Fatal(err)
	}
	if err := run.store.selectTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.store.dir, "context.json")); err != nil {
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
	awaitJob(t, jobDone(t, freshRun))
	state, err := freshRun.store.readState()
	if err != nil || state.Status != "blocked" || requests.Load() < 2 {
		t.Fatal("task could not resume", state, err)
	}
}

func TestTaskStalledChildHandsBackEvidence(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "audit-handoff")
	if err := workspace.WriteAtomic(filepath.Join(app.runRoot("result", run.store.id), "index.html"), []byte("<html>preview</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	var childRequests, parentRequests atomic.Int32
	work := `{"phase":"verification","step":"检查导航","completed":["已完成源码审阅"],"pending":["补充键盘验证"],"findings":["E_NAV：键盘证据缺失"],"checks":[{"name":"导航源码","status":"reviewed","evidence":"index.html","call_id":""}],"next":"向主 Agent 交回缺少的验证项"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input        []inputItem `json:"input"`
			Instructions string      `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(request.Instructions, "（ID：audit）") {
			n := childRequests.Add(1)
			if n > 12 {
				t.Error("audit failed to converge")
				serveModelResponse(w, false, []inputItem{messageOutput("unexpected")})
				return
			}
			name, args := "file_read", fmt.Sprintf(`{"file_path":"index.html","max_chars":%d,"timeout":1}`, 100+n)
			if n == 1 || n == 8 {
				name, args = "update_task_state", work
			}
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": fmt.Sprintf("audit-%d", n), "name": name, "arguments": args}})
			return
		}
		if parentRequests.Add(1) == 1 {
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "audit", "name": "call_agent", "arguments": `{"agent_id":"audit","task":"检查导航","timeout":3}`}})
			return
		}
		last := itemString(request.Input[len(request.Input)-1], "output")
		if !strings.Contains(last, "E_NAV") || !strings.Contains(last, `"status":"blocked"`) {
			t.Error("coordinator did not receive evidence with blocked business result", last)
		}
		serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "review", "name": "request_user_decision", "arguments": `{"kind":"question","question":"页面可以预览，键盘验证尚未完成；先查看首版还是继续完善？","options":["先查看首版","继续完善"],"timeout":1}`}})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	if err := app.startMessage(run.store.id, "检查已有预览", "", nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, jobDone(t, run))
	state, err := run.store.readState()
	if err != nil || state.Status != "awaiting_user" || state.PendingDecision == nil || parentRequests.Load() != 2 || childRequests.Load() > 12 {
		t.Fatalf("coordinator stayed trapped: %+v %v", state, err)
	}
	parent, err := run.store.loadTask(state.TaskID)
	if err != nil || parent.Tools["audit"].Status != "completed" || len(pendingTaskCalls(parent.Items)) != 0 {
		t.Fatal("handoff left a pending child operation", err)
	}
}

func TestTaskPlanRecordsSpecificSelection(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "plan-choice")
	task := &taskCheckpoint{ID: "choice-task", AgentID: "main"}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{checkpoint: task})
	_, err := app.requestDecision(ctx, run.store, `{"kind":"plan","question":"请选择博客视觉方向，范围均为单页博客","options":["A 深空青蓝","B 赛博霓虹"]}`)
	var pause *taskPause
	if !errors.As(err, &pause) {
		t.Fatal(err)
	}
	state, _ := run.store.readState()
	if _, err := app.answerDecision(run.store.id, state.PendingDecision.ID, "approve"); err == nil {
		t.Fatal("generic approval silently selected a style")
	}
	text, err := app.answerDecision(run.store.id, state.PendingDecision.ID, "B 赛博霓虹")
	if err != nil || !strings.Contains(text, "用户选择并确认：B 赛博霓虹") {
		t.Fatal(text, err)
	}
	state, _ = run.store.readState()
	if state.Approvals["plan"].Selection != "B 赛博霓虹" {
		t.Fatal("selection not persisted")
	}
}
