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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"webweave/workspace"
)

func TestAgentRoleUpgrade(t *testing.T) {
	legacy := legacyDefaultAgents()
	legacy[1].Prompt = "用户自己的设计指令"
	legacy[2].ModelID = "child-model"
	legacy = append(legacy, AgentConfig{ID: "custom", Name: "自定义", Prompt: "用户自己的角色"})
	data, _ := json.Marshal(map[string]any{"agents": legacy, "llm": map[string]any{"main_model_id": "child-model", "models": []ModelConfig{{ID: "child-model", ProviderConfig: ProviderConfig{Model: "child", BaseURL: "https://example.invalid", APIKey: "test"}}}}})
	cfg, err := parseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents[0] != defaultAgents()[0] || cfg.Agents[1] != legacy[1] || cfg.Agents[2].Name != "前端工程Agent" || cfg.Agents[2].ModelID != "child-model" || cfg.Agents[3].Name != "质量检查Agent" || cfg.Agents[5] != legacy[5] {
		t.Fatalf("role migration discarded edits or failed to upgrade defaults: %+v", cfg.Agents)
	}
}

func TestAgentProtocolContract(t *testing.T) {
	valid := agentTaskContract{Phase: "实现", UserRequest: "调整商品列表", Scope: []string{"只修改商品列表"}, Acceptance: []string{"手机无横向溢出"}, Deliverables: []string{"修改文件和预览"}}
	for _, test := range []struct {
		name   string
		change func(*agentTaskContract)
	}{
		{"valid", func(c *agentTaskContract) {}},
		{"missing phase", func(c *agentTaskContract) { c.Phase = " " }},
		{"missing request", func(c *agentTaskContract) { c.UserRequest = "" }},
		{"missing scope", func(c *agentTaskContract) { c.Scope = nil }},
		{"missing acceptance", func(c *agentTaskContract) { c.Acceptance = nil }},
		{"missing deliverables", func(c *agentTaskContract) { c.Deliverables = nil }},
		{"blank acceptance", func(c *agentTaskContract) { c.Acceptance = []string{" "} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := valid
			test.change(&contract)
			if err := contract.validate(); (err == nil) != (test.name == "valid") {
				t.Fatalf("unexpected contract validation: %v", err)
			}
		})
	}
}

func TestAgentProtocolResult(t *testing.T) {
	for _, test := range []struct {
		name, status, wantStatus, reason string
		pending, questions               []string
		raw                              string
		budget                           bool
		runErr, contextErr               error
	}{
		{name: "completed", status: "completed", wantStatus: "completed", reason: "normal"},
		{name: "partial", status: "partial", pending: []string{"接口未连接"}, wantStatus: "partial", reason: "normal"},
		{name: "blocked", status: "blocked", questions: []string{"请确认布局"}, wantStatus: "blocked", reason: "normal"},
		{name: "blocked dependency", status: "blocked", pending: []string{"缺少接口资料"}, wantStatus: "blocked", reason: "normal"},
		{name: "business failure", status: "failed", wantStatus: "failed", reason: "normal"},
		{name: "legacy text", raw: "已有产物，未检查", wantStatus: "partial", reason: "invalid_result"},
		{name: "missing fields", raw: `{"status":"completed","summary":"完成"}`, wantStatus: "partial", reason: "invalid_result"},
		{name: "completed with pending", status: "completed", pending: []string{"仍需修改"}, wantStatus: "partial", reason: "invalid_result"},
		{name: "partial without pending", status: "partial", wantStatus: "partial", reason: "invalid_result"},
		{name: "blocked without explanation", status: "blocked", wantStatus: "partial", reason: "invalid_result"},
		{name: "unknown status", status: "unknown", wantStatus: "partial", reason: "invalid_result"},
		{name: "budget", status: "completed", budget: true, wantStatus: "partial", reason: "budget_reached"},
		{name: "budget failure", budget: true, runErr: errors.New("预算耗尽"), wantStatus: "failed", reason: "budget_reached"},
		{name: "timeout", runErr: context.DeadlineExceeded, contextErr: context.DeadlineExceeded, wantStatus: "failed", reason: "timeout"},
		{name: "cancelled", runErr: context.Canceled, contextErr: context.Canceled, wantStatus: "failed", reason: "cancelled"},
		{name: "runtime failure", runErr: errors.New("模型调用失败"), wantStatus: "failed", reason: "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := test.raw
			if test.status != "" {
				pending, questions := test.pending, test.questions
				if pending == nil {
					pending = []string{}
				}
				if questions == nil {
					questions = []string{}
				}
				data, _ := json.Marshal(agentResult{Status: test.status, Summary: "交付说明", Artifacts: []string{}, Verification: []string{"未验证：只进行了静态阅读"}, Pending: pending, Questions: questions, Previews: []agentPreview{}})
				text = string(data)
			}
			result := finishAgentCall(defaultAgents()[2], "task-1", text, test.runErr, test.contextErr, test.budget)
			if result.Result.Status != test.wantStatus || result.ExecutionReason != test.reason || result.OK != (test.runErr == nil) || result.SubagentID != "task-1" || result.Output != text {
				t.Fatalf("unexpected handoff: %+v", result)
			}
			if test.runErr == nil && result.ExecutionStatus != "completed" || test.runErr != nil && result.Error == "" {
				t.Fatal("execution and business states were conflated")
			}
		})
	}
}

func TestAgentProtocolDelegation(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("read-only=%v", readOnly), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "protocol")
			contract := agentTaskContract{Phase: "实现", UserRequest: "调整卡片", Context: "已有页面", Confirmed: []string{"保留配色"}, Assumptions: []string{}, Inputs: []string{"index.html"}, Scope: []string{"只修改卡片"}, Acceptance: []string{"手机无溢出"}, Deliverables: []string{"卡片源码"}, ReadOnly: readOnly}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input []inputItem `json:"input"`
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					Text struct {
						Format struct {
							Name string `json:"name"`
						} `json:"format"`
					} `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Text.Format.Name != "agent_result" {
					t.Error("delegated output schema missing")
				}
				for _, tool := range request.Tools {
					if readOnly && (tool.Name == "terminal_run" || tool.Name == "create_file") {
						t.Error("read-only task advertised a write tool")
					}
				}
				call := calls.Add(1)
				if call == 1 {
					var task struct {
						Task     string            `json:"task"`
						Contract agentTaskContract `json:"contract"`
					}
					if len(request.Input) != 1 || json.Unmarshal([]byte(itemString(request.Input[0], "content")), &task) != nil || task.Task != "调整卡片" || !reflect.DeepEqual(task.Contract, contract) {
						t.Error("contract did not reach isolated child context")
					}
					serveModelResponse(w, false, []inputItem{{"type": "function_call", "id": "fc_write", "call_id": "write", "name": "create_file", "arguments": `{"file_path":"card.html","content":"卡片","timeout":1}`}})
					return
				}
				if !readOnly && call == 2 {
					// A file write alone does not satisfy the delegated acceptance.
					serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "review", "name": "update_task_state", "arguments": `{"phase":"review","step":"检查卡片","completed":["已检查样式"],"pending":[],"findings":[],"checks":[{"name":"手机无溢出","status":"reviewed","evidence":"card.html：静态布局检查；未运行浏览器","call_id":""}],"next":"交付检查结论"}`}})
					return
				}
				status, artifacts, pending := "completed", []string{"card.html"}, []string{}
				if readOnly {
					if !strings.Contains(itemString(request.Input[2], "output"), "AGENT_READ_ONLY") {
						t.Error("unadvertised write tool bypassed runtime restriction")
					}
					status, artifacts, pending = "blocked", []string{}, []string{"需要主 Agent 安排工程修改"}
				}
				data, _ := json.Marshal(agentResult{Status: status, Summary: "卡片处理结果", Artifacts: artifacts, Verification: []string{"未验证：尚未浏览器检查"}, Pending: pending, Questions: []string{}, Previews: []agentPreview{}})
				serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, false)
			args, _ := json.Marshal(map[string]any{"agent_id": "code", "task": "调整卡片", "contract": contract, "timeout": 5})
			var result agentCallResult
			if err := json.Unmarshal([]byte(app.callAgent(context.Background(), run.store, defaultAgents()[0], string(args))), &result); err != nil {
				t.Fatal(err)
			}
			expected := map[bool]string{false: "completed", true: "blocked"}[readOnly]
			expectedCalls := int32(3)
			if readOnly {
				expectedCalls = 2
			}
			if !result.OK || result.Result.Status != expected || result.ExecutionStatus != "completed" || result.ExecutionReason != "normal" || result.SubagentID == "" || calls.Load() != expectedCalls {
				t.Fatalf("invalid delegated result: %+v", result)
			}
			_, err := os.Stat(filepath.Join(app.runRoot("result", run.store.id), "card.html"))
			if readOnly && !os.IsNotExist(err) || !readOnly && err != nil {
				t.Fatalf("file write boundary mismatch: %v", err)
			}
			_, events, err := run.store.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			started := false
			for _, event := range events {
				started = started || event.Type == "subagent_started" && event.Payload["contract"] != nil
			}
			if !started || events[len(events)-1].Payload["result"].(map[string]any)["status"] != expected {
				t.Fatalf("handoff was not persisted: %v", err)
			}
		})
	}
}

func TestAgentProtocolReadOnly(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "readonly")
	path := filepath.Join(app.runRoot("result", run.store.id), "index.html")
	if err := workspace.WriteAtomic(path, []byte("原页面"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), agentExecutionKey{}, agentExecution{agent: defaultAgents()[3]})
	for _, test := range []struct{ tool, args string }{
		{"create_file", `{"file_path":"index.html","content":"被覆盖","timeout":1}`},
		{"terminal_run", `{"command":"Remove-Item index.html","timeout":1}`},
		{"web_search", `{"download_url":"https://example.invalid/file","timeout":1}`},
		{"web_instance", `{"action":"start","timeout":1}`},
		{"web_instance", `{"action":"stop","timeout":1}`},
		{"web_instance", `{"action":"restart","timeout":1}`},
		{"web_search", `not-json`},
	} {
		if output := app.executeTool(ctx, run.store.id, test.tool, test.args); !strings.Contains(output, "AGENT_READ_ONLY") {
			t.Fatalf("write operation was not rejected: %s %s", test.tool, output)
		}
	}
	if output := app.executeTool(ctx, run.store.id, "file_read", `{"file_path":"index.html","timeout":1}`); !strings.Contains(output, "原页面") {
		t.Fatalf("read-only role could not read source: %s", output)
	}
	if !readOnlyToolAllowed("web_search", `{"query":"布局灵感"}`) || !readOnlyToolAllowed("web_instance", `{"action":"status"}`) {
		t.Fatal("safe search and instance inspection were blocked")
	}
	for _, tool := range agentTools(defaultAgents()[3], app.config.get()) {
		if tool.OfFunction.Name == "web_instance" {
			properties := tool.OfFunction.Parameters["properties"].(map[string]any)
			if _, exists := properties["command"]; exists || !reflect.DeepEqual(properties["action"].(map[string]any)["enum"], []string{"status"}) {
				t.Fatal("read-only instance schema still allows mutations")
			}
		}
	}
}

func TestAgentProtocolBudget(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "protocol-budget")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call <= int32(agentRoundLimit("design")) {
			id := fmt.Sprintf("call_%d", call)
			serveModelResponse(w, false, []inputItem{{"type": "function_call", "id": id, "call_id": id, "name": "list_skills", "arguments": `{"timeout":1}`}})
			return
		}
		data, _ := json.Marshal(agentResult{Status: "completed", Summary: "模型自称完成", Artifacts: []string{}, Verification: []string{"未验证"}, Pending: []string{}, Questions: []string{}, Previews: []agentPreview{}})
		serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	var result agentCallResult
	if err := json.Unmarshal([]byte(app.callAgent(context.Background(), run.store, defaultAgents()[0], `{"agent_id":"design","task":"提供设计方向","timeout":5}`)), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Result.Status != "completed" || result.ExecutionReason != "normal" || calls.Load() != int32(agentRoundLimit("design")+1) {
		t.Fatalf("delegated task did not continue after its execution segment: %+v", result)
	}
}
