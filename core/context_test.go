package core

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"webweave/workspace"
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
	for _, mode := range []string{"valid", "invalid JSON", "HTML response", "incomplete", "failed", "empty summary", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "compression-task")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input        string `json:"input"`
					Instructions string `json:"instructions"`
					MaxOutput    int    `json:"max_output_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if utf8.RuneCountInString(request.Input) > 8192*2 {
					t.Error("summary request was not bounded")
				}
				if request.Instructions != compressionInstructions || strings.Contains(request.Input, "你是上下文压缩器") || request.MaxOutput != 4096 {
					t.Error("compression instructions or output budget mismatch")
				}
				if mode == "timeout" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if mode == "HTML response" {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<html>upstream unavailable</html>")
					return
				}
				if mode == "incomplete" || mode == "failed" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "resp_summary", "object": "response", "status": mode, "output": []any{},
						"incomplete_details": map[string]any{"reason": "max_output_tokens"},
						"error":              map[string]any{"code": "server_error", "message": "fixture failure"},
						"usage":              map[string]any{"output_tokens": 2000, "output_tokens_details": map[string]any{"reasoning_tokens": 1800}},
					})
					return
				}
				text := "<html>this is not a summary</html>"
				if mode == "valid" {
					text = "```json\n{\"goal\":\"校园网站\",\"pending\":[\"补全联系信息\"]}\n```"
				} else if mode == "empty summary" {
					text = "{}"
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
			items = append(items, inputItem{"role": "assistant", "content": "新增决定：所有表单字段都必须有标签；已验证首页导航。"})
			for i := 0; i < 80; i++ {
				items = append(items, inputItem{"role": "assistant", "content": strings.Repeat("历史记录", 600)})
			}
			items = append(items, inputItem{"role": "user", "content": "保持蓝色配色，添加联系信息"}, inputItem{"type": "function_call", "call_id": "file", "name": "create_file", "arguments": `{"file_path":"contact.html","content":"联系信息"}`}, inputItem{"type": "function_call_output", "call_id": "file", "output": `{"path":"result/contact.html","size":12}`})
			client, err := newClient(cfg.LLM.Main)
			if err != nil {
				t.Fatal(err)
			}
			compressed, err := app.compressContext(context.Background(), client, run.store, items, cfg)
			if mode == "valid" && err != nil || mode != "valid" && !errors.Is(err, errContextCompression) || len(repairItems(compressed)) != len(compressed) {
				t.Fatalf("compression broke history: %v", err)
			}
			if mode != "valid" {
				if !reflect.DeepEqual(compressed, items) {
					t.Fatal("failed compression discarded or altered history")
				}
				var saved savedContext
				if err := readJSONFile(filepath.Join(run.store.dir, "context.json"), &saved); err != nil || !reflect.DeepEqual(saved.InputItems, items) || saved.Summary == nil {
					t.Fatal("failed compression did not preserve the saved history and summary", err)
				}
				_, events, err := run.store.snapshot()
				if err != nil || len(events) == 0 {
					t.Fatal("failure event missing", err)
				}
				last := events[len(events)-1]
				stage := "request"
				if mode == "invalid JSON" || mode == "empty summary" {
					stage = "summary_parse"
				}
				if last.Type != "context_compression_failed" || last.Payload["stage"] != stage || last.Payload["removed_items"] != float64(0) || last.Payload["tokens_before"] != last.Payload["tokens_after"] || last.Payload["compression_error"] == "" {
					t.Fatalf("incorrect compression failure event: %+v", last)
				}
				if mode == "incomplete" || mode == "failed" {
					if last.Payload["response_status"] != mode || last.Payload["incomplete_reason"] != "max_output_tokens" || last.Payload["error_code"] != "server_error" || last.Payload["output_tokens"] != float64(2000) || last.Payload["reasoning_tokens"] != float64(1800) {
						t.Fatal("response failure diagnostics missing")
					}
				}
				return
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
			if last.Type != "context_compressed" || last.Payload["compression_error"] != "" {
				t.Fatal("compression failure reason was not recorded correctly")
			}
		})
	}
}

func TestContextCompressionFailureBudget(t *testing.T) {
	for _, mode := range []string{"within budget", "failure over budget", "summary over budget"} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "compression-budget")
			var summaries, tasks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Instructions string `json:"instructions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Instructions == compressionInstructions {
					summaries.Add(1)
					text := "not JSON"
					if mode == "summary over budget" {
						text = `{"goal":"保留任务","decisions":["新增决定必须保留"]}`
					}
					serveModelResponse(w, false, []inputItem{messageOutput(text)})
					return
				}
				if tasks.Add(1) == 1 {
					serveModelResponse(w, false, []inputItem{{"type": "function_call", "call_id": "write", "name": "create_file", "arguments": `{"file_path":"index.html","content":"完成","timeout":5}`}})
					return
				}
				serveModelResponse(w, false, []inputItem{messageOutput("完成")})
			}))
			t.Cleanup(server.Close)
			cfg := useProvider(t, app, server.URL, false)
			agent, err := findAgent(cfg.Agents, "main")
			if err != nil {
				t.Fatal(err)
			}
			items := []inputItem{{"role": "user", "content": strings.Repeat("任务事实", 2000)}, {"role": "assistant", "content": "新增决定必须保留"}, {"role": "user", "content": "继续"}}
			params := responses.ResponseNewParams{Instructions: openai.String(agentInstructions(agent, cfg, run.store.id)), Tools: agentTools(agent, cfg)}
			baseline := estimateRequestTokens(params, items)
			cfg.Context.MaxInputTokens = baseline + 1000
			if mode != "within budget" {
				cfg.Context.MaxInputTokens = baseline - 1
			}
			cfg.Context.TriggerRatio, cfg.Context.KeepRecentItems = 0.5, 1
			if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
				t.Fatal(err)
			}
			text, err := app.runAgent(context.Background(), run.store, items, agent, false)
			if mode != "summary over budget" && summaries.Load() != 1 || mode == "summary over budget" && summaries.Load() < 1 {
				t.Fatalf("failed compression was repeated: %d", summaries.Load())
			}
			if mode == "failure over budget" {
				if !errors.Is(err, errContextCompression) || !strings.Contains(err.Error(), "输入预算") || tasks.Load() != 0 {
					t.Fatalf("oversized input was sent or failure was hidden: %v, requests=%d", err, tasks.Load())
				}
			} else if mode == "summary over budget" {
				if err == nil || !strings.Contains(err.Error(), "压缩后上下文") || tasks.Load() != 0 {
					t.Fatalf("oversized summarized input was sent: %v, requests=%d", err, tasks.Load())
				}
			} else if err != nil || text != "完成" || tasks.Load() != 2 {
				t.Fatalf("task within budget did not continue: %q %v, requests=%d", text, err, tasks.Load())
			}
			saved, err := run.store.loadItems()
			if mode == "summary over budget" {
				if err != nil || len(saved) < 2 || userText(saved[1]) != userText(items[0]) {
					t.Fatal("original user request was lost", err)
				}
			} else if err != nil || len(saved) < len(items) || !reflect.DeepEqual(saved[:len(items)], items) {
				t.Fatal("task history was lost after failed compression", err)
			}
		})
	}
}

func TestModelResponseFailureDetails(t *testing.T) {
	if response, err := validateResponse(nil); response != nil || err == nil || !strings.Contains(err.Error(), "空响应") {
		t.Fatal("empty response was not identified", err)
	}
	for _, streaming := range []bool{false, true} {
		for _, status := range []string{"incomplete", "failed"} {
			t.Run(fmt.Sprintf("stream=%v/status=%s", streaming, status), func(t *testing.T) {
				app := testApp(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					response := map[string]any{
						"id": "resp_failure", "object": "response", "status": status, "output": []any{},
						"incomplete_details": map[string]any{"reason": "max_output_tokens"},
						"error":              map[string]any{"code": "server_error", "message": "fixture detail"},
						"usage":              map[string]any{"output_tokens": 2000, "output_tokens_details": map[string]any{"reasoning_tokens": 1800}},
					}
					if !streaming {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					event, _ := json.Marshal(map[string]any{"type": "response." + status, "response": response})
					fmt.Fprintf(w, "event: response.%s\ndata: %s\n\n", status, event)
				}))
				t.Cleanup(server.Close)
				cfg := useProvider(t, app, server.URL, streaming)
				client, err := newClient(cfg.LLM.Main)
				if err != nil {
					t.Fatal(err)
				}
				response, err := modelResponse(context.Background(), client, responses.ResponseNewParams{Model: cfg.LLM.Main.Model}, "总结历史", streaming, nil)
				if err == nil || response == nil || string(response.Status) != status {
					t.Fatalf("failure response metadata was lost: %+v %v", response, err)
				}
				for _, detail := range []string{"status=" + status, "reason=max_output_tokens", "error_code=server_error", "output_tokens=2000", "reasoning_tokens=1800"} {
					if !strings.Contains(err.Error(), detail) {
						t.Fatalf("failure diagnostic missing %q: %v", detail, err)
					}
				}
			})
		}
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
