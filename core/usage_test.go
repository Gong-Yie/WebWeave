package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"webweave/workspace"
)

func TestReasoningAndContextUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "reasoning-usage")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Reasoning struct {
						Summary string `json:"summary"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Reasoning.Summary != "auto" {
					t.Error("reasoning summary request missing", err)
				}
				response := map[string]any{
					"id": "resp_reasoning", "object": "response", "status": "completed", "model": "test-model",
					"output": []inputItem{{"id": "rs_test", "type": "reasoning", "summary": []inputItem{{"type": "summary_text", "text": "先检查布局，再制作模板。"}}}, messageOutput("制作完成")},
					"usage":  map[string]any{"input_tokens": 1234, "output_tokens": 42, "total_tokens": 1276},
				}
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []map[string]any{
					{"type": "response.reasoning_summary_text.delta", "item_id": "rs_test", "summary_index": 0, "delta": "先检查布局，"},
					{"type": "response.reasoning_summary_text.delta", "item_id": "rs_test", "summary_index": 0, "delta": "再制作模板。"},
					{"type": "response.reasoning_text.delta", "item_id": "rs_content", "content_index": 0, "delta": "接口返回的思考文本"},
					{"type": "response.output_text.delta", "delta": "制作完成"},
					{"type": "response.completed", "response": response},
				} {
					data, _ := json.Marshal(event)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
					w.(http.Flusher).Flush()
				}
			}))
			t.Cleanup(server.Close)
			cfg := useProvider(t, app, server.URL, streaming)
			cfg.Model.ReasoningSummary = true
			if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
				t.Fatal(err)
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/reasoning-usage/messages", map[string]any{"content": "制作页面"}), 202)
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			if err != nil || detail.Status != "completed" {
				t.Fatalf("request failed: %+v %v", detail.State, err)
			}
			usage := detail.ContextUsage
			if usage.InputTokens != 1234 || usage.Estimated || usage.Threshold != int(float64(cfg.Context.MaxInputTokens)*cfg.Context.TriggerRatio) || !usage.Enabled || usage.AgentName != "主Agent" {
				t.Fatalf("incorrect usage: %+v", usage)
			}
			var reasoning strings.Builder
			estimateFound, outputFound := false, false
			for _, event := range detail.Events {
				if event.Type == "context_usage" && event.Payload["estimated"] == true {
					estimateFound = true
				}
				if event.Type == "model_reasoning_delta" {
					reasoning.WriteString(event.Payload["delta"].(string))
					if event.Payload["agent_id"] != "main" {
						t.Fatal("reasoning attribution missing")
					}
				}
				if event.Type == "model_output" {
					outputFound = strings.Contains(fmt.Sprint(event.Payload["items"]), "先检查布局")
				}
			}
			if !estimateFound || !outputFound {
				t.Fatal("estimate or persisted reasoning missing")
			}
			if streaming && reasoning.String() != "先检查布局，再制作模板。接口返回的思考文本" {
				t.Fatalf("streamed reasoning was lost: %q", reasoning.String())
			}
		})
	}
}

func TestContextUsageEstimateAndReload(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "estimate-usage")
	params := responses.ResponseNewParams{Instructions: openai.String("角色指令"), Tools: agentTools(defaultAgents()[0], defaultConfig())}
	items := []inputItem{{"role": "user", "content": "页面"}}
	tokens := estimateRequestTokens(params, items)
	if tokens <= estimateTokens(items) {
		t.Fatal("request estimate omitted instructions and tools")
	}
	if err := run.store.append("context_usage", map[string]any{"input_tokens": tokens, "estimated": true, "agent_name": "代码编程Agent", "delegated": true}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Context.MaxInputTokens, cfg.Context.TriggerRatio, cfg.Context.Enabled = 10000, 0.9, false
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	detail, err := app.detail(run.store.id)
	if err != nil || detail.ContextUsage.InputTokens != tokens || !detail.ContextUsage.Estimated || detail.ContextUsage.Threshold != 9000 || detail.ContextUsage.Enabled || !detail.ContextUsage.Delegated {
		t.Fatalf("estimate or updated config lost: %+v %v", detail.ContextUsage, err)
	}
}
