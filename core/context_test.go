package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestContextToolBoundaries(t *testing.T) {
	call := func(id string) inputItem { return inputItem{"type": "function_call", "call_id": id} }
	output := func(id string) inputItem {
		return inputItem{"type": "function_call_output", "call_id": id, "output": "ok"}
	}
	user := inputItem{"role": "user", "content": "生成网站"}
	for _, test := range []struct {
		name  string
		items []inputItem
		keep  int
		start int
	}{
		{"consecutive tool turns", []inputItem{user, call("a"), output("a"), call("b"), output("b")}, 2, 3},
		{"batch outputs", []inputItem{user, call("a"), call("b"), output("a"), output("b")}, 1, 0},
		{"reasoning with tool", []inputItem{user, call("a"), output("a"), {"type": "reasoning"}, call("b"), output("b")}, 1, 3},
		{"single user", []inputItem{user}, 12, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := recentStart(test.items, test.keep)
			if start != test.start || !reflect.DeepEqual(repairItems(test.items[start:]), test.items[start:]) {
				t.Fatalf("unsafe or ineffective context cut: %d", start)
			}
		})
	}
}

func TestContextCompaction(t *testing.T) {
	content := strings.Repeat("网站源码", 6000)
	args, _ := json.Marshal(map[string]any{"file_path": "index.html", "content": content, "timeout": 5})
	terminal, _ := json.Marshal(map[string]any{"returncode": 1, "stdout": strings.Repeat("大段输出", 90000), "stderr": "真实错误"})
	items := []inputItem{
		{"type": "function_call", "call_id": "written", "name": "create_file", "arguments": string(args)},
		{"type": "function_call_output", "call_id": "written", "output": `{"path":"result/index.html","size":72000}`},
		{"type": "function_call", "call_id": "failed", "name": "create_file", "arguments": string(args)},
		{"type": "function_call_output", "call_id": "failed", "output": "工具执行失败: 权限不足"},
		{"type": "function_call", "call_id": "terminal", "name": "terminal_run", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "terminal", "output": string(terminal)},
		{"type": "function_call", "call_id": "pending", "name": "create_file", "arguments": string(args)},
	}
	compacted := compactModelContext(items)
	if itemString(items[0], "arguments") != string(args) || len(itemString(compacted[0], "arguments")) > 1000 || !strings.Contains(itemString(compacted[0], "arguments"), "result/index.html") {
		t.Fatal("successful file write was not shortened without mutating the record")
	}
	for _, index := range []int{2, 6} {
		if itemString(compacted[index], "arguments") != string(args) {
			t.Fatal("failed or pending write lost the content required for execution")
		}
	}
	var preview struct {
		ReturnCode int    `json:"returncode"`
		Preview    string `json:"preview"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(itemString(compacted[5], "output")), &preview); err != nil || preview.ReturnCode != 1 || !preview.Truncated || utf8.RuneCountInString(preview.Preview) > maxToolContextChars || !strings.Contains(preview.Preview, "真实错误") {
		t.Fatalf("large output lost its error status or exceeded the preview limit: %+v %v", preview, err)
	}
	if !reflect.DeepEqual(compacted, compactModelContext(compacted)) {
		t.Fatal("repeated context compaction wrapped or modified an existing preview")
	}
	if estimateTokens(compacted) >= estimateTokens(items)/2 {
		t.Fatal("large terminal output still dominates context")
	}
}

func TestContextCompressionPreservesTask(t *testing.T) {
	for _, mode := range []string{"valid", "invalid JSON", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "compression-task")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if utf8.RuneCountInString(request.Input) > maxSummaryChars+8500 {
					t.Error("summary request was not bounded")
				}
				if mode == "timeout" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				text := "not JSON"
				if mode == "valid" {
					text = "```json\n{\"goal\":\"校园网站\",\"pending\":[\"补全联系信息\"]}\n```"
				}
				serveModelResponse(w, false, []inputItem{messageOutput(text)})
			}))
			t.Cleanup(server.Close)
			cfg := useProvider(t, app, server.URL, false)
			cfg.Context.KeepRecentItems = 2
			if mode == "timeout" {
				cfg.Context.TimeoutSeconds = 0.05
			}
			previous, _ := json.Marshal(contextSummary{Goal: "校园网站", Decisions: []string{"单页实现"}, Artifacts: []string{"result/index.html"}})
			items := []inputItem{{"role": "assistant", "content": summaryPrefix + string(previous)}, {"role": "user", "content": "做一个校园介绍网站"}}
			for i := 0; i < 80; i++ {
				items = append(items, inputItem{"role": "assistant", "content": strings.Repeat("历史记录", 600)})
			}
			items = append(items, inputItem{"role": "user", "content": "保持蓝色配色，添加联系信息"}, inputItem{"type": "function_call", "call_id": "file", "name": "create_file", "arguments": `{"file_path":"contact.html","content":"联系信息"}`}, inputItem{"type": "function_call_output", "call_id": "file", "output": `{"path":"result/contact.html","size":12}`})
			client, err := newClient(cfg.LLM.Main)
			if err != nil {
				t.Fatal(err)
			}
			compressed, err := app.compressContext(context.Background(), client, run.store, items, cfg)
			if err != nil || len(repairItems(compressed)) != len(compressed) {
				t.Fatalf("compression broke history: %v", err)
			}
			for _, required := range []string{"做一个校园介绍网站", "保持蓝色配色，添加联系信息"} {
				found := false
				for _, item := range compressed {
					found = found || itemString(item, "role") == "user" && userText(item) == required
				}
				if !found {
					t.Fatalf("compression discarded user instruction: %s", required)
				}
			}
			var summary contextSummary
			if err := json.Unmarshal([]byte(strings.TrimPrefix(itemString(compressed[0], "content"), summaryPrefix)), &summary); err != nil {
				t.Fatal(err)
			}
			if len(summary.Artifacts) != 2 || summary.Goal == "" || summary.LatestUserIntent != "保持蓝色配色，添加联系信息" {
				t.Fatalf("task or artifact references were lost: %+v", summary)
			}
			if mode != "valid" && (len(summary.Errors) == 0 || !reflect.DeepEqual(summary.Decisions, []string{"单页实现"})) {
				t.Fatal("failed compression discarded prior decisions or concealed the failure")
			}
			compressed = append(compressed, inputItem{"role": "assistant", "content": "继续执行"})
			if err := run.store.saveContext(compressed, nil); err != nil {
				t.Fatal(err)
			}
			var saved savedContext
			if err := readJSONFile(filepath.Join(run.store.dir, "context.json"), &saved); err != nil || saved.Summary == nil {
				t.Fatal("next context save erased the summary metadata")
			}
			_, events, err := run.store.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]
			if last.Type != "context_compressed" || (last.Payload["compression_error"] != "") != (mode != "valid") {
				t.Fatal("compression failure reason was not recorded correctly")
			}
		})
	}
}

func TestAgentRoundBudget(t *testing.T) {
	for _, refuses := range []bool{false, true} {
		t.Run(fmt.Sprint("refuses=", refuses), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "round-budget")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ToolChoice string `json:"tool_choice"`
					Tools      []any  `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				count := calls.Add(1)
				if count == int32(agentRoundLimit("audit")+1) {
					if request.ToolChoice != "none" || len(request.Tools) != 0 {
						t.Error("final response was still permitted to call tools")
					}
					if !refuses {
						serveModelResponse(w, false, []inputItem{messageOutput("已完成必要检查，交付现有结果；剩余事项待处理")})
						return
					}
				}
				id := fmt.Sprintf("call_%d", count)
				serveModelResponse(w, false, []inputItem{{"type": "function_call", "id": id, "call_id": id, "name": "list_skills", "arguments": `{"timeout":1}`}})
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			text, err := app.runAgent(ctx, run.store, []inputItem{{"role": "user", "content": "检查网站"}}, defaultAgents()[3], false)
			if calls.Load() != int32(agentRoundLimit("audit")+1) || refuses && (err == nil || !strings.Contains(err.Error(), "工具预算")) || !refuses && (err != nil || text == "") {
				t.Fatalf("tool loop did not stop at its budget: %d %q %v", calls.Load(), text, err)
			}
			_, events, err := run.store.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			tools := 0
			for _, event := range events {
				if event.Type == "tool_output" {
					tools++
				}
			}
			if tools != agentRoundLimit("audit") {
				t.Fatal("a tool was executed after the budget was exhausted")
			}
		})
	}
}
