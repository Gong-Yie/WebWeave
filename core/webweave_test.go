package core

import (
	"bytes"
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
	"time"

	"github.com/coder/websocket"

	"webweave/tools"
	"webweave/workspace"
)

func testApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "webui"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
		data, err := os.ReadFile(filepath.Join("..", "webui", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "webui", name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	app, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func testRun(t *testing.T, app *App, id string) *runRuntime {
	t.Helper()
	if err := app.prepareDirectories(id); err != nil {
		t.Fatal(err)
	}
	run, err := app.runtime(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.store.setStatus("waiting", "新建对话"); err != nil {
		t.Fatal(err)
	}
	// Execution fixtures begin after the user's plan confirmation. The initial
	// confirmation gate is exercised separately through newly created API runs.
	state, err := run.store.readState()
	if err != nil {
		t.Fatal(err)
	}
	state.Approvals = map[string]userDecision{"plan": {ID: "fixture-plan", Kind: "plan", Question: "已确认的测试任务范围", AnsweredAt: workspace.Timestamp()}}
	if err := workspace.WriteJSONAtomic(filepath.Join(run.store.dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	return run
}

func apiRequest(t *testing.T, app *App, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if raw, ok := value.([]byte); ok {
		data = raw
	} else if value != nil {
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	return response
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, expected, response.Body.String())
	}
}

func useProvider(t *testing.T, app *App, address string, streaming bool) RuntimeConfig {
	t.Helper()
	cfg := defaultConfig()
	cfg.LLM.Main = ProviderConfig{Model: "test-model", BaseURL: address + "/v1", APIKey: "test-key"}
	cfg.Model.Stream = streaming
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func messageOutput(text string) inputItem {
	return inputItem{"type": "message", "id": "msg_test", "role": "assistant", "status": "completed", "content": []inputItem{{"type": "output_text", "text": text, "annotations": []any{}}}}
}

// Interactive and delegated fixtures must honor the requested result schema.
// Raw messageOutput remains available for summary and malformed-result tests.
func taskMessageOutput(summary string) inputItem {
	data, err := json.Marshal(agentResult{Status: "completed", Summary: summary, Artifacts: []string{}, Verification: []string{"未验证：此响应仅用于隔离的执行流程测试"}, Pending: []string{}, Questions: []string{}, Previews: []agentPreview{}})
	if err != nil {
		panic(err)
	}
	return messageOutput(string(data))
}

func serveModelResponse(w http.ResponseWriter, stream bool, output []inputItem) {
	response := map[string]any{"id": "resp_test", "object": "response", "status": "completed", "model": "test-model", "output": output}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, item := range output {
		if itemString(item, "type") == "message" {
			content := item["content"].([]inputItem)
			data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": itemString(content[0], "text"), "sequence_number": 1})
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
	}
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response, "sequence_number": 2})
	fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func jobDone(t *testing.T, run *runRuntime) <-chan struct{} {
	t.Helper()
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.job == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return run.job.done
}

func awaitJob(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * second):
		t.Fatal("job did not finish")
	}
}

func TestConfigValidationSecretsAndReload(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `{"model":{"stream":"yes"}}`, `{"model":{"stream":null}}`, `{"tools":{"bad/name":{}}}`, `{"tool_timeout":{"default_seconds":2000,"max_seconds":1}}`, `{"context":{"compression_trigger_ratio":1}}`, `{"context":{"keep_recent_items":0}}`, `{"llm":{"vision":{"enabled":true}}}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	app := testApp(t)
	cfg := defaultConfig()
	cfg.LLM.Main.APIKey = "keep-secret"
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	response := apiRequest(t, app, "GET", "/api/config", nil)
	assertStatus(t, response, 200)
	if strings.Contains(response.Body.String(), "keep-secret") || !strings.Contains(response.Body.String(), "********") {
		t.Fatal("secret not masked")
	}
	for _, key := range []string{"********", ""} {
		cfg.LLM.Main.APIKey = key
		response = apiRequest(t, app, "PUT", "/api/config", cfg)
		assertStatus(t, response, 200)
		if app.config.get().LLM.Main.APIKey != "keep-secret" {
			t.Fatal("masked or blank key did not preserve secret")
		}
	}
	if err := os.WriteFile(app.config.path, []byte(`{"model":{"stream":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if app.config.get().LLM.Main.APIKey != "keep-secret" {
		t.Fatal("invalid reload replaced last valid config")
	}
	cfg.LLM.Main.APIKey, cfg.LLM.Main.Model = "new-key", "new-model"
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	if app.config.get().LLM.Main.Model != "new-model" {
		t.Fatal("valid change not hot loaded")
	}
}

func TestAPIAndFileCompatibility(t *testing.T) {
	app := testApp(t)
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/", 200}, {"GET", "/app.js", 200}, {"HEAD", "/styles.css", 200}, {"GET", "/api/health", 200}, {"GET", "/api/runs", 200}, {"GET", "/api/skills", 200}, {"GET", "/api/appearance", 200}, {"GET", "/unknown", 404}, {"GET", "/api/runs/missing", 404}, {"GET", "/api/runs/bad!", 400}, {"OPTIONS", "/api/runs", 204}} {
		t.Run(test.method+test.path, func(t *testing.T) {
			response := apiRequest(t, app, test.method, test.path, nil)
			assertStatus(t, response, test.status)
			if test.method == "HEAD" && response.Body.Len() != 0 {
				t.Fatal("HEAD returned body")
			}
		})
	}
	created := apiRequest(t, app, "POST", "/api/runs", nil)
	assertStatus(t, created, 201)
	var payload struct {
		Run runDetail `json:"run"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	id := payload.Run.RunID
	if len(id) != 32 || payload.Run.Status != "waiting" || payload.Run.Events == nil {
		t.Fatal("new run does not match frontend contract")
	}
	for index, expectedName := range []string{"需求.txt", "需求 (1).txt"} {
		response := apiRequest(t, app, "POST", "/api/runs/"+id+"/attachments?name=%E9%9C%80%E6%B1%82.txt", []byte("中文需求"))
		assertStatus(t, response, 201)
		var file struct {
			File fileMetadata `json:"file"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &file); err != nil {
			t.Fatal(err)
		}
		if file.File.Name != expectedName || file.File.Scope != "resources" || file.File.Size != int64(len([]byte("中文需求"))) {
			t.Fatalf("upload %d contract mismatch: %+v", index, file)
		}
	}
	response := apiRequest(t, app, "GET", "/api/runs/"+id+"/file?scope=resources&path=%E9%9C%80%E6%B1%82.txt", nil)
	assertStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "中文需求") {
		t.Fatal("UTF-8 preview missing")
	}
	response = apiRequest(t, app, "GET", "/api/runs/"+id+"/file?scope=resources&path=%E9%9C%80%E6%B1%82.txt&raw=1", nil)
	assertStatus(t, response, 200)
	if response.Body.String() != "中文需求" {
		t.Fatal("raw file mismatch")
	}
	for _, query := range []string{"scope=resources&path=../../config.json", "scope=invalid&path=a", "scope=resources&path=/config.json"} {
		assertStatus(t, apiRequest(t, app, "GET", "/api/runs/"+id+"/file?"+query, nil), 400)
	}
	assertStatus(t, apiRequest(t, app, "PUT", "/api/config", []byte(`[]`)), 400)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/messages", []byte(`{"content":1}`)), 400)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/continue", nil), 400)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/"+id+"/instance", map[string]any{"action": "status"}), 404)
	files := apiRequest(t, app, "GET", "/api/runs/"+id+"/files", nil)
	var listed struct {
		Files []fileMetadata `json:"files"`
	}
	if err := json.Unmarshal(files.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Files) != 2 {
		t.Fatalf("files: %s", files.Body.String())
	}
	request := httptest.NewRequest("POST", "/api/runs/"+id+"/attachments?name=large.txt", strings.NewReader("x"))
	request.ContentLength = 50*1024*1024 + 1
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	assertStatus(t, recorder, 413)
	assertStatus(t, apiRequest(t, app, "DELETE", "/api/runs/"+id, nil), 200)
	assertStatus(t, apiRequest(t, app, "GET", "/api/runs/"+id, nil), 404)
}

func TestBackgroundAndSkills(t *testing.T) {
	app := testApp(t)
	assertStatus(t, apiRequest(t, app, "PUT", "/api/appearance/background?name=bad.png", []byte("not a png")), 400)
	assertStatus(t, apiRequest(t, app, "PUT", "/api/appearance/background?name=picture.png", []byte("\x89PNG\r\n\x1a\nfixture")), 200)
	response := apiRequest(t, app, "GET", "/api/appearance", nil)
	if !strings.Contains(response.Body.String(), "/api/appearance/background?v=") {
		t.Fatal("background URL missing")
	}
	assertStatus(t, apiRequest(t, app, "GET", "/api/appearance/background", nil), 200)
	assertStatus(t, apiRequest(t, app, "DELETE", "/api/appearance/background", nil), 200)
	assertStatus(t, apiRequest(t, app, "GET", "/api/appearance/background", nil), 404)
	for _, test := range []struct{ name, text string }{{"valid", "---\nname: web_project\ndescription: >\n  中文能力\n  支持多行描述\n---\n# 指令\n实际内容"}, {"duplicate", "---\nname: web_project\ndescription: 重复名称\n---\n重复"}, {"invalid", "no metadata"}} {
		path := filepath.Join(app.root, "data", "skills", test.name, "SKILL.md")
		if err := workspace.WriteAtomic(path, []byte(test.text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	listing, err := app.skills.List()
	if err != nil || len(listing.Skills) != 1 || len(listing.Errors) != 2 {
		t.Fatalf("skills: %+v, %v", listing, err)
	}
	document, err := app.skills.Read("web_project")
	if err != nil || document.Instructions == "" {
		t.Fatalf("skill body missing: %+v, %v", document, err)
	}
	for _, name := range []string{"../escape", "missing"} {
		if _, err := app.skills.Read(name); err == nil {
			t.Fatal("invalid skill accepted")
		}
	}
}

func TestRunRecoveryAndPathIsolation(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "old-python-run")
	testRun(t, app, "other-run")
	items := []inputItem{{"role": "user", "content": "继续原任务"}, {"type": "function_call", "call_id": "paired", "name": "file_read"}, {"type": "function_call_output", "call_id": "paired", "output": "ok"}, {"type": "function_call", "call_id": "orphan"}, {"type": "function_call_output", "call_id": "unknown"}}
	if err := run.store.saveContext(items, nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := run.store.loadItems()
	if err != nil || len(recovered) != 3 {
		t.Fatalf("unpaired calls not repaired: %+v %v", recovered, err)
	}
	if err := run.store.append("user_message", map[string]any{"item": items[0]}); err != nil {
		t.Fatal(err)
	}
	if err := run.store.append("model_output", map[string]any{"items": []inputItem{items[1]}}); err != nil {
		t.Fatal(err)
	}
	if err := run.store.append("tool_output", map[string]any{"item": items[2]}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.store.dir, "context.json")); err != nil {
		t.Fatal(err)
	}
	recovered, err = run.store.loadItems()
	if err != nil || !reflect.DeepEqual(recovered, items[:3]) {
		t.Fatalf("event reconstruction mismatch: %+v %v", recovered, err)
	}
}

func TestAgentSDKToolLoop(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", streaming), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "agent-loop")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("SDK endpoint or authorization mismatch")
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request["model"] != "test-model" {
					t.Error("configured model ignored")
				}
				if calls.Add(1) == 1 {
					tools := request["tools"].([]any)
					if len(tools) != 11 {
						t.Errorf("tool count %d", len(tools))
					}
					for _, tool := range tools {
						encoded, _ := json.Marshal(tool)
						if !bytes.Contains(encoded, []byte(`"timeout"`)) {
							t.Error("tool timeout schema missing")
						}
					}
					serveModelResponse(w, streaming, []inputItem{{"type": "function_call", "id": "fc_create", "call_id": "call_create", "name": "create_file", "arguments": `{"file_path":"index.html","content":"<h1>中文页面</h1>","timeout":5}`}, {"type": "function_call", "id": "fc_read", "call_id": "call_read", "name": "file_read", "arguments": `{"file_path":"result/index.html","timeout":5}`}})
				} else {
					input := request["input"].([]any)
					if len(input) != 5 {
						t.Errorf("input item count %d, want 5", len(input))
					}
					for _, value := range input[3:] {
						item := value.(map[string]any)
						if item["type"] != "function_call_output" {
							t.Error("tool output missing")
						}
					}
					serveModelResponse(w, streaming, []inputItem{taskMessageOutput("网页已生成")})
				}
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, streaming)
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/agent-loop/messages", map[string]any{"content": "创建中文网页"}), 202)
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			if err != nil || detail.Status != "completed" || detail.Title != "创建中文网页" || calls.Load() != 2 {
				t.Fatalf("agent did not finish: %+v %v", detail, err)
			}
			data, err := os.ReadFile(filepath.Join(app.runRoot("result", run.store.id), "index.html"))
			if err != nil || string(data) != "<h1>中文页面</h1>" {
				t.Fatal("tool did not create UTF-8 file")
			}
			deltas, tools := 0, 0
			for _, event := range detail.Events {
				if event.Type == "model_output_delta" {
					deltas++
				}
				if event.Type == "tool_output" {
					tools++
				}
			}
			if tools != 2 || streaming && deltas == 0 || !streaming && deltas != 0 {
				t.Fatal("event contract mismatch")
			}
		})
	}
}

func TestAgentStopContinueAndConflict(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "cancel-run")
	started := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			return
		}
		serveModelResponse(w, true, []inputItem{taskMessageOutput("继续完成")})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, true)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/cancel-run/messages", map[string]any{"content": "等待停止"}), 202)
	select {
	case <-started:
	case <-time.After(5 * second):
		t.Fatal("request never started")
	}
	done := jobDone(t, run)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/cancel-run/messages", map[string]any{"content": "重复提交"}), 409)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/cancel-run/continue", nil), 409)
	assertStatus(t, apiRequest(t, app, "DELETE", "/api/runs/cancel-run", nil), 409)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/cancel-run/stop", nil), 202)
	awaitJob(t, done)
	detail, err := app.detail(run.store.id)
	if err != nil || detail.Status != "stopped" {
		t.Fatal("run did not stop")
	}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/cancel-run/continue", nil), 202)
	awaitJob(t, jobDone(t, run))
	detail, err = app.detail(run.store.id)
	if err != nil || detail.Status != "completed" || calls.Load() != 2 {
		t.Fatal("run did not resume")
	}
}

func TestModelRetryAndPermanentFailure(t *testing.T) {
	for _, status := range []int{429, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "retry-run")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"message":"fixture error","type":"invalid_request_error","code":"fixture"}}`)
					return
				}
				serveModelResponse(w, false, []inputItem{taskMessageOutput("重试完成")})
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, false)
			if err := app.startMessage(run.store.id, "重试任务", "", nil, nil, false, nil); err != nil {
				t.Fatal(err)
			}
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			if err != nil {
				t.Fatal(err)
			}
			if status == 429 {
				if calls.Load() != 2 || detail.Status != "completed" {
					t.Fatal("retryable failure not retried")
				}
			} else if calls.Load() != 1 || detail.Status != "failed" {
				t.Fatal("permanent failure retried or hidden")
			}
		})
	}
}

func TestVisionAndCompression(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint("fallback=", fallback), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "multimodal-run")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request["model"] == "vision-model" {
					if fallback {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"message":"vision unavailable"}}`)
					} else {
						serveModelResponse(w, false, []inputItem{messageOutput("图片中的布局说明")})
					}
					return
				}
				if fallback {
					serveModelResponse(w, false, []inputItem{messageOutput("not json")})
				} else {
					serveModelResponse(w, false, []inputItem{messageOutput(`{"goal":"保留任务","pending":["继续工作"],"artifacts":["index.html"]}`)})
				}
			}))
			t.Cleanup(server.Close)
			cfg := useProvider(t, app, server.URL, false)
			cfg.LLM.Vision = VisionConfig{ProviderConfig{Model: "vision-model", BaseURL: server.URL + "/v1", APIKey: "vision-key"}, true}
			cfg.Context.KeepRecentItems = 2
			if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
				t.Fatal(err)
			}
			if err := workspace.WriteAtomic(filepath.Join(app.runRoot("resources", run.store.id), "参考.png"), []byte("\x89PNG\r\n\x1a\nfixture"), 0644); err != nil {
				t.Fatal(err)
			}
			item, err := app.prepareUserItem(context.Background(), run.store.id, "读取参考", []string{"参考.png"})
			if err != nil {
				t.Fatal(err)
			}
			if fallback {
				parts, ok := item["content"].([]inputItem)
				if !ok || len(parts) != 2 || itemString(parts[1], "type") != "input_image" {
					t.Fatal("multimodal fallback missing")
				}
			} else if !strings.Contains(itemString(item, "content"), "图片中的布局说明") {
				t.Fatal("vision text missing")
			}
			client, err := newClient(cfg.LLM.Main)
			if err != nil {
				t.Fatal(err)
			}
			items := []inputItem{{"role": "user", "content": "旧任务"}, {"role": "assistant", "content": "旧答复"}, {"role": "user", "content": "最近任务"}, {"type": "function_call", "call_id": "pair"}, {"type": "function_call_output", "call_id": "pair", "output": "ok"}}
			compressed, err := app.compressContext(context.Background(), client, run.store, items, cfg)
			if fallback {
				if !errors.Is(err, errContextCompression) || !reflect.DeepEqual(compressed, items) {
					t.Fatal("failed compression did not preserve the original context", err)
				}
				return
			}
			if err != nil || len(compressed) != 5 || itemString(compressed[1], "content") != "旧任务" || itemString(compressed[3], "call_id") != "pair" || len(repairItems(compressed)) != len(compressed) {
				t.Fatalf("compression broke tool pairing: %+v %v", compressed, err)
			}
			if !strings.Contains(itemString(compressed[0], "content"), "历史上下文摘要") {
				t.Fatal("summary missing")
			}
		})
	}
}

func TestPreviewHTTPAndWebSocket(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "preview-run")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Security-Policy", "none")
			fmt.Fprint(w, `<html><head></head><body><h1 data-ww-component="test">页面</h1></body></html>`)
		case "/asset.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, "asset")
		case "/redirect":
			w.Header().Set("Location", "/asset.js")
			w.WriteHeader(302)
		case "/socket":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			kind, data, err := conn.Read(r.Context())
			if err == nil {
				_ = conn.Write(r.Context(), kind, data)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(upstream.Close)
	if err := run.store.saveInstance(&instanceRecord{InstanceID: "fixture", Revision: 1, Status: "running", Host: "127.0.0.1", AppURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	response := apiRequest(t, app, "GET", "/preview/preview-run/", nil)
	assertStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), `<base href="/preview/preview-run/">`) || !strings.Contains(response.Body.String(), "webweave:selection") || response.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("preview bridge missing")
	}
	for _, source := range []string{"referer", "cookie"} {
		request := httptest.NewRequest("GET", "/asset.js", nil)
		if source == "referer" {
			request.Header.Set("Referer", "http://127.0.0.1/preview/preview-run/")
		} else {
			request.AddCookie(&http.Cookie{Name: "ww_preview_run", Value: "preview-run"})
		}
		recorder := httptest.NewRecorder()
		app.ServeHTTP(recorder, request)
		if recorder.Body.String() != "asset" {
			t.Fatal("preview asset route failed: " + source)
		}
	}
	redirect := apiRequest(t, app, "GET", "/preview/preview-run/redirect", nil)
	assertStatus(t, redirect, 302)
	if redirect.Header().Get("Location") != "/preview/preview-run/asset.js" {
		t.Fatal("redirect not rewritten")
	}
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/preview/preview-run/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte("preview websocket")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != "preview websocket" {
		t.Fatalf("websocket proxy: %s %v", data, err)
	}
}

func TestInstanceHelper(t *testing.T) {
	if os.Getenv("WEBWEAVE_INSTANCE_ID") == "" {
		return
	}
	args := os.Args
	if len(args) < 2 {
		os.Exit(2)
	}
	address := args[len(args)-2] + ":" + args[len(args)-1]
	if err := http.ListenAndServe(address, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body>fixture instance %s</body></html>", os.Getenv("WEBWEAVE_INSTANCE_ID"))
	})); err != nil {
		os.Exit(2)
	}
}

func TestInstanceLifecycle(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "instance-run")
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "& '" + strings.ReplaceAll(program, "'", "''") + "' '-test.run=TestInstanceHelper' -- {host} {port}"
	ctx, cancel := context.WithTimeout(context.Background(), 10*second)
	defer cancel()
	instance, err := app.instanceAction(ctx, run.store.id, tools.Args{Action: "start", Command: command})
	if err != nil || instance.Status != "running" || instance.PID == nil {
		state, _ := run.store.readState()
		var log []byte
		if state.Instance != nil {
			log, _ = os.ReadFile(state.Instance.LogPath)
		}
		t.Fatalf("instance start: %+v %v\nprocess log:\n%s", instance, err, log)
	}
	firstRevision := instance.Revision
	var launch launchConfig
	if err := readJSONFile(filepath.Join(app.runRoot("result", run.store.id), launchConfigName), &launch); err != nil || launch.Command != command || launch.Cwd != "." {
		t.Fatalf("Agent startup parameters were not saved: %+v %v", launch, err)
	}
	if _, err := app.instanceAction(ctx, run.store.id, tools.Args{Action: "start", InstanceID: instance.InstanceID, Command: command}); err == nil {
		t.Fatal("duplicate instance allowed")
	}
	response := apiRequest(t, app, "GET", previewURL(run.store.id), nil)
	assertStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "fixture instance") {
		t.Fatal("instance preview missing")
	}
	instance, err = app.instanceAction(ctx, run.store.id, tools.Args{Action: "restart"})
	if err != nil || instance.Revision != firstRevision+1 {
		t.Fatalf("restart: %+v %v", instance, err)
	}
	instance, err = app.instanceAction(ctx, run.store.id, tools.Args{Action: "stop"})
	if err != nil || instance.Status != "stopped" || instance.PID != nil {
		t.Fatalf("stop: %+v %v", instance, err)
	}
}

func TestCLIUsesSharedAgent(t *testing.T) {
	app := testApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveModelResponse(w, false, []inputItem{taskMessageOutput("命令行已完成")})
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	var output bytes.Buffer
	if err := app.Chat(context.Background(), strings.NewReader("命令行任务\nq\n"), &output, "cli-run"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "命令行已完成") {
		t.Fatalf("CLI output: %s", output.String())
	}
}
