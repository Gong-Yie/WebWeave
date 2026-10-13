package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"webweave/workspace"
)

func TestMultiAgentManagement(t *testing.T) {
	app := testApp(t)
	response := apiRequest(t, app, "GET", "/api/agents", nil)
	assertStatus(t, response, 200)
	var listing struct {
		Agents []AgentConfig `json:"agents"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listing.Agents, defaultAgents()) {
		t.Fatal("default agent roster mismatch")
	}
	cfg := defaultConfig()
	cfg.LLM.Main.APIKey = "keep-secret"
	cfg.Model.Stream = false
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	response = apiRequest(t, app, "POST", "/api/agents", AgentConfig{Name: "内容策划", Description: "规划网站文案", Prompt: "编写中文内容"})
	assertStatus(t, response, 201)
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	defaultCount := len(defaultAgents())
	if len(listing.Agents) != defaultCount+1 || listing.Agents[defaultCount].ID == "" || strings.Contains(response.Body.String(), "keep-secret") {
		t.Fatal("custom agent creation or response secrecy failed")
	}
	agent := listing.Agents[defaultCount]
	agent.Prompt = "检查中文内容"
	assertStatus(t, apiRequest(t, app, "PUT", "/api/agents/"+agent.ID, agent), 200)
	// A settings client without the new field must preserve the current roster.
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var olderConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &olderConfig); err != nil {
		t.Fatal(err)
	}
	delete(olderConfig, "agents")
	assertStatus(t, apiRequest(t, app, "PUT", "/api/config", olderConfig), 200)
	saved := app.config.get()
	if saved.LLM.Main.APIKey != "keep-secret" || saved.Model.Stream || len(saved.Agents) != defaultCount+1 || saved.Agents[defaultCount].Prompt != agent.Prompt {
		t.Fatal("agent or configuration save discarded unrelated values")
	}
	fresh := newConfigManager(app.root).get()
	if !reflect.DeepEqual(fresh.Agents, saved.Agents) {
		t.Fatal("custom agents were not persisted")
	}
	for _, test := range []struct {
		name, method, path string
		body               any
		status             int
	}{
		{"empty prompt", "POST", "/api/agents", AgentConfig{Name: "无指令"}, 400},
		{"duplicate id", "POST", "/api/agents", defaultAgents()[0], 409},
		{"builtin deletion", "DELETE", "/api/agents/main", nil, 400},
		{"missing agent", "PUT", "/api/agents/missing", agent, 404},
		{"invalid path", "DELETE", "/api/agents/invalid/path", nil, 400},
		{"null body", "POST", "/api/agents", []byte(`null`), 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertStatus(t, apiRequest(t, app, test.method, test.path, test.body), test.status)
		})
	}
	if len(app.config.get().Agents) != defaultCount+1 {
		t.Fatal("invalid edits changed the roster")
	}
	assertStatus(t, apiRequest(t, app, "DELETE", "/api/agents/"+agent.ID, nil), 200)
	if len(app.config.get().Agents) != defaultCount {
		t.Fatal("custom agent was not deleted")
	}
	for _, agents := range [][]AgentConfig{{}, {defaultAgents()[0]}, append(defaultAgents(), defaultAgents()[0])} {
		encoded, err := json.Marshal(map[string]any{"agents": agents})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseConfig(encoded); err == nil {
			t.Fatal("invalid config roster was accepted")
		}
	}
}

func TestMultiAgentSelectionAndResume(t *testing.T) {
	for _, agentID := range []string{"", "design", "code", "audit", "template", "custom"} {
		t.Run("agent="+agentID, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "selected-agent")
			if agentID == "template" {
				installTemplateSkill(t, app)
			}
			var calls atomic.Int32
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Instructions string           `json:"instructions"`
					Tools        []map[string]any `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				selected := agentID
				if selected == "" {
					selected = "main"
				}
				if !strings.Contains(request.Instructions, "（ID："+selected+"）") {
					t.Error("selected role was not used")
				}
				if selected == "template" && !strings.Contains(request.Instructions, "文件夹内固定且仅有") {
					t.Error("default template skill was not injected")
				}
				delegation := false
				for _, tool := range request.Tools {
					delegation = delegation || tool["name"] == "call_agent"
				}
				if delegation != (selected == "main") {
					t.Error("delegation permission mismatch")
				}
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(started)
					<-r.Context().Done()
					return
				}
				serveModelResponse(w, true, []inputItem{messageOutput("继续完成")})
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, true)
			assertStatus(t, apiRequest(t, app, "POST", "/api/agents", AgentConfig{"custom", "自定义Agent", "自定义任务", "处理自定义任务", ""}), 201)
			before, err := run.store.readState()
			if err != nil {
				t.Fatal(err)
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/selected-agent/messages", map[string]any{"content": "任务", "agent_id": "unknown"}), 400)
			after, err := run.store.readState()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("invalid selection changed run state")
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/selected-agent/messages", map[string]any{"content": "任务", "agent_id": agentID}), 202)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("model request never started")
			}
			done := jobDone(t, run)
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/selected-agent/stop", nil), 202)
			awaitJob(t, done)
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/selected-agent/continue", nil), 202)
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			expected := agentID
			if expected == "" {
				expected = "main"
			}
			if err != nil || detail.Status != "completed" || detail.State.AgentID != expected || calls.Load() != 2 {
				t.Fatalf("selected agent was not preserved on resume: %+v %v", detail.State, err)
			}
			for _, event := range detail.Events {
				if event.Type == "model_output_delta" || event.Type == "model_output" {
					if event.Payload["agent_id"] != expected {
						t.Fatal("model event attribution mismatch")
					}
				}
			}
		})
	}
}

func TestMultiAgentDelegation(t *testing.T) {
	for _, target := range []string{"design", "code", "audit", "template", "custom"} {
		t.Run(target, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "delegation")
			if target == "template" {
				installTemplateSkill(t, app)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Instructions string      `json:"instructions"`
					Input        []inputItem `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				switch calls.Add(1) {
				case 1:
					if !strings.Contains(request.Instructions, "（ID：main）") || !strings.Contains(request.Instructions, "custom：") {
						t.Error("main agent does not know the full roster")
					}
					arguments, _ := json.Marshal(map[string]any{"agent_id": target, "task": "完成委派任务", "timeout": 5})
					serveModelResponse(w, true, []inputItem{{"type": "function_call", "id": "fc_delegate", "call_id": "call_delegate", "name": "call_agent", "arguments": string(arguments)}})
				case 2:
					if !strings.Contains(request.Instructions, "（ID："+target+"）") || len(request.Input) != 1 || itemString(request.Input[0], "content") != "完成委派任务" {
						t.Error("delegated role or isolated task context mismatch")
					}
					// Delegation must not overwrite the parent's pending call.
					var saved savedContext
					if err := readJSONFile(filepath.Join(run.store.dir, "context.json"), &saved); err != nil || len(saved.InputItems) != 2 {
						t.Error("delegation overwrote parent context")
					}
					serveModelResponse(w, true, []inputItem{{"type": "function_call", "id": "fc_file", "call_id": "call_file", "name": "create_file", "arguments": `{"file_path":"delegated.txt","content":"共享成果","timeout":5}`}})
				case 3:
					serveModelResponse(w, true, []inputItem{messageOutput("委派结果")})
				case 4:
					if len(request.Input) != 3 || itemString(request.Input[2], "call_id") != "call_delegate" || !strings.Contains(itemString(request.Input[2], "output"), "委派结果") {
						t.Error("delegated result was not returned to the parent")
					}
					serveModelResponse(w, true, []inputItem{messageOutput("整合完成")})
				default:
					t.Error("unexpected extra model call")
					serveModelResponse(w, true, []inputItem{messageOutput("额外调用")})
				}
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, true)
			assertStatus(t, apiRequest(t, app, "POST", "/api/agents", AgentConfig{"custom", "自定义Agent", "自定义任务", "处理委派任务", ""}), 201)
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/delegation/messages", map[string]any{"content": "主任务"}), 202)
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			if err != nil || detail.Status != "completed" || calls.Load() != 4 || detail.State.AgentID != "main" {
				t.Fatalf("delegation failed: %+v %v", detail.State, err)
			}
			data, err := os.ReadFile(filepath.Join(app.runRoot("result", run.store.id), "delegated.txt"))
			if err != nil || string(data) != "共享成果" {
				t.Fatal("delegated agent did not share run files")
			}
			delegatedEvents := 0
			subagentID := ""
			started, finished := false, false
			for _, event := range detail.Events {
				if event.Payload["delegated"] == true {
					delegatedEvents++
					if event.Payload["agent_id"] != target {
						t.Fatal("delegated event attribution mismatch")
					}
					if subagentID == "" {
						subagentID, _ = event.Payload["subagent_id"].(string)
					}
					if subagentID == "" || event.Payload["subagent_id"] != subagentID || event.Payload["parent_agent_id"] != "main" {
						t.Fatal("subagent identity or parent mismatch")
					}
					started = started || event.Type == "subagent_started"
					finished = finished || (event.Type == "subagent_finished" && event.Payload["status"] == "completed")
				}
			}
			if delegatedEvents < 4 {
				t.Fatal("delegated streaming, model and tool events missing")
			}
			if !started || !finished {
				t.Fatal("subagent lifecycle events missing")
			}
			saved, err := run.store.loadItems()
			if err != nil || len(saved) != 4 {
				t.Fatal("parent context contains delegated items")
			}
			if err := os.Remove(filepath.Join(run.store.dir, "context.json")); err != nil {
				t.Fatal(err)
			}
			recovered, err := run.store.loadItems()
			if err != nil || !reflect.DeepEqual(saved, recovered) {
				t.Fatal("event recovery mixed parent and delegated context")
			}
		})
	}
}

func TestMultiAgentDelegationErrors(t *testing.T) {
	for _, test := range []struct {
		name, caller, arguments string
	}{
		{"specialist cannot delegate", "code", `{"agent_id":"design","task":"设计"}`},
		{"cannot call self", "main", `{"agent_id":"main","task":"设计"}`},
		{"missing target", "main", `{"agent_id":"missing","task":"设计"}`},
		{"empty task", "main", `{"agent_id":"design","task":" "}`},
		{"invalid JSON", "main", `[]`},
		{"invalid timeout", "main", `{"agent_id":"design","task":"设计","timeout":-1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "invalid-delegation")
			caller, err := findAgent(app.config.get().Agents, test.caller)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(app.callAgent(context.Background(), run.store, caller, test.arguments)), &result); err != nil || result.OK || result.Error == "" {
				t.Fatal("delegation error was not explicit")
			}
		})
	}
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "delegate timeout", true: "parent cancellation"}[cancelParent], func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "cancel-delegation")
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan string, 1)
			timeout := 0.1
			if cancelParent {
				timeout = 5
			}
			args, _ := json.Marshal(map[string]any{"agent_id": "design", "task": "等待取消", "timeout": timeout})
			go func() { result <- app.callAgent(ctx, run.store, defaultAgents()[0], string(args)) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("delegated request never started")
			}
			if cancelParent {
				cancel()
			}
			select {
			case text := <-result:
				if !strings.Contains(text, `"ok":false`) || !strings.Contains(text, "执行失败") {
					t.Fatal("cancellation did not return a failure")
				}
				_, events, err := run.store.snapshot()
				if err != nil || len(events) == 0 || events[len(events)-1].Type != "subagent_finished" || events[len(events)-1].Payload["status"] != "stopped" {
					t.Fatal("cancelled subagent was not marked stopped")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delegation ignored timeout or cancellation")
			}
			if !cancelParent && ctx.Err() != nil {
				t.Fatal("delegate timeout cancelled parent context")
			}
		})
	}
}
