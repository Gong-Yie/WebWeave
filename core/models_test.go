package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"webweave/workspace"
)

func TestModelRegistrySecretsAndReferences(t *testing.T) {
	app := testApp(t)
	cfg := defaultConfig()
	cfg.LLM.Models = []ModelConfig{
		{ID: "first", ProviderConfig: ProviderConfig{Model: "model-a", BaseURL: "https://a.invalid/v1", APIKey: "secret-a", Headers: map[string]string{"X-Project": "header-secret"}}},
		{ID: "second", ProviderConfig: ProviderConfig{Model: "model-b", BaseURL: "https://b.invalid/v1", APIKey: "secret-b", Headers: map[string]string{"X-Account": "second-header"}, ReasoningEfforts: []string{"low", "high"}, ReasoningEffort: "high"}},
	}
	cfg.LLM.MainModelID = "first"
	cfg.Agents[2].ModelID = "second"
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	response := apiRequest(t, app, "GET", "/api/config", nil)
	assertStatus(t, response, 200)
	for _, secret := range []string{"secret-a", "secret-b", "header-secret", "second-header"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("configuration leaked credential")
		}
	}
	var payload struct {
		Config RuntimeConfig `json:"config"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	masked := payload.Config
	masked.LLM.Models[0], masked.LLM.Models[1] = masked.LLM.Models[1], masked.LLM.Models[0]
	masked.LLM.Models = append(masked.LLM.Models, ModelConfig{ID: "imported", CredentialSourceID: "first", ProviderConfig: ProviderConfig{Model: "model-c", BaseURL: "https://a.invalid/v1", APIKey: "********", Headers: map[string]string{"X-Project": "********"}}})
	assertStatus(t, apiRequest(t, app, "PUT", "/api/config", masked), 200)
	saved := app.config.get()
	first, _ := findModel(saved.LLM.Models, "first")
	second, _ := findModel(saved.LLM.Models, "second")
	imported, _ := findModel(saved.LLM.Models, "imported")
	if first.APIKey != "secret-a" || second.APIKey != "secret-b" || imported.APIKey != "secret-a" || imported.Headers["X-Project"] != "header-secret" || imported.CredentialSourceID != "" || saved.LLM.Main.Model != "model-a" {
		t.Fatal("model identity, import credentials, or main role lost")
	}
	for _, invalid := range []string{"dangling-model", "unsupported-effort", "header-injection", "missing-header-secret", "credential-host-change"} {
		t.Run(invalid, func(t *testing.T) {
			data, _ := json.Marshal(redactedConfig(saved))
			var bad RuntimeConfig
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "dangling-model":
				bad.LLM.Models = []ModelConfig{bad.LLM.Models[1]}
			case "unsupported-effort":
				bad.LLM.Models[0].ReasoningEffort = "medium"
			case "header-injection":
				bad.LLM.Models[1].Headers = map[string]string{"X-Project": "value\r\nInjected: yes"}
			case "missing-header-secret":
				bad.LLM.Models[1].Headers = map[string]string{"X-Unknown": "********"}
			case "credential-host-change":
				bad.LLM.Models = append(bad.LLM.Models, ModelConfig{ID: "invalid-import", CredentialSourceID: "first", ProviderConfig: ProviderConfig{Model: "model-x", BaseURL: "https://other.invalid", APIKey: "********"}})
			}
			assertStatus(t, apiRequest(t, app, "PUT", "/api/config", bad), 400)
			if !reflect.DeepEqual(app.config.get(), saved) {
				t.Fatal("rejected configuration changed saved state")
			}
		})
	}
	assertStatus(t, apiRequest(t, app, "PUT", "/api/agents/code", AgentConfig{ID: "code", Name: "代码", Prompt: "执行", ModelID: "missing"}), 400)
	assertStatus(t, apiRequest(t, app, "PUT", "/api/config", map[string]any{"llm": map[string]any{"main": map[string]any{"model": "renamed-model", "base_url": first.BaseURL, "api_key": "********"}}}), 200)
	legacySaved := app.config.get()
	if legacySaved.LLM.Main.Model != "renamed-model" || len(legacySaved.LLM.Models) != 3 || legacySaved.LLM.Main.Headers["X-Project"] != "header-secret" || legacySaved.Agents[2].ModelID != "second" {
		t.Fatal("older client discarded models or custom headers")
	}
	switched := redactedConfig(legacySaved)
	switched.LLM.MainModelID = "second"
	switched.LLM.Main = redactProvider(second.ProviderConfig)
	switched.LLM.VisionModelID = "first"
	switched.LLM.Vision = VisionConfig{redactProvider(first.ProviderConfig), true}
	assertStatus(t, apiRequest(t, app, "PUT", "/api/config", switched), 200)
	routed := app.config.get()
	if routed.LLM.Main.APIKey != "secret-b" || routed.LLM.Main.Headers["X-Account"] != "second-header" || routed.LLM.Vision.APIKey != "secret-a" || routed.LLM.Vision.Headers["X-Project"] != "header-secret" {
		t.Fatal("switching model roles did not preserve selected credentials")
	}
}

func TestModelDiscoveryHeadersAndErrors(t *testing.T) {
	for _, status := range []int{200, 401, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			app := testApp(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret-key" || r.Header.Get("X-Project") != "private-header" {
					t.Error("discovery connection or headers mismatch")
				}
				w.Header().Set("Content-Type", "application/json")
				if status == 302 {
					w.Header().Set("Location", "/redirect-target")
				}
				w.WriteHeader(status)
				if status != 200 {
					_, _ = w.Write([]byte(`{"error":{"message":"secret-key private-header"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-z"},{"id":"model-a"},{"id":"model-a"}]}`))
			}))
			t.Cleanup(server.Close)
			cfg := defaultConfig()
			cfg.LLM.Models = []ModelConfig{{ID: "saved", ProviderConfig: ProviderConfig{Model: "model-a", BaseURL: server.URL + "/v1", APIKey: "secret-key", Headers: map[string]string{"X-Project": "private-header"}}}}
			cfg.LLM.MainModelID = "saved"
			if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
				t.Fatal(err)
			}
			request := ModelConfig{ID: "saved", ProviderConfig: ProviderConfig{BaseURL: server.URL + "/v1", APIKey: "********", Headers: map[string]string{"X-Project": "********"}}}
			response := apiRequest(t, app, "POST", "/api/models/discover", request)
			if status == 200 {
				assertStatus(t, response, 200)
				var payload struct {
					Models []string `json:"models"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || !reflect.DeepEqual(payload.Models, []string{"model-a", "model-z"}) {
					t.Fatal("discovery did not normalize IDs", err)
				}
			} else {
				assertStatus(t, response, 502)
				if strings.Contains(response.Body.String(), "secret-key") || strings.Contains(response.Body.String(), "private-header") {
					t.Fatal("provider error leaked credentials")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("discovery retried or followed redirect")
			}
			request.BaseURL = "https://changed.invalid/v1"
			assertStatus(t, apiRequest(t, app, "POST", "/api/models/discover", request), 400)
			request.BaseURL = "not-a-url"
			assertStatus(t, apiRequest(t, app, "POST", "/api/models/discover", request), 400)
			if calls.Load() != 1 {
				t.Fatal("invalid connection reached provider")
			}
		})
	}
}

func TestModelRedirectIsNotRetried(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "redirect-response")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/redirect-target")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	useProvider(t, app, server.URL, false)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/redirect-response/messages", map[string]any{"content": "测试重定向"}), 202)
	awaitJob(t, jobDone(t, run))
	detail, err := app.detail(run.store.id)
	if err != nil || detail.Status != "failed" || calls.Load() != 1 {
		t.Fatalf("redirect was followed, retried or treated as success: %+v %d %v", detail.State, calls.Load(), err)
	}
}

func TestModelRoutingAndConversationEffort(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherit-main", true: "independent-child"}[independent], func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "model-routing")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model       string   `json:"model"`
					Temperature *float64 `json:"temperature"`
					Reasoning   struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				call := calls.Add(1)
				model, key, effort := "main-model", "main-key", "high"
				if call == 2 {
					effort = "low"
					if independent {
						model, key, effort = "child-model", "child-key", "medium"
					}
				}
				if request.Model != model || request.Reasoning.Effort != effort || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("X-Model") != model || request.Temperature != nil {
					t.Error("model, credentials, effort or temperature routing mismatch")
				}
				if call == 1 {
					serveModelResponse(w, false, []inputItem{{"type": "function_call", "id": "fc_delegate", "call_id": "delegate", "name": "call_agent", "arguments": `{"agent_id":"code","task":"制作页面","timeout":5}`}})
				} else {
					serveModelResponse(w, false, []inputItem{taskMessageOutput("完成")})
				}
			}))
			t.Cleanup(server.Close)
			cfg := defaultConfig()
			cfg.Model.Stream = false
			cfg.LLM.Models = []ModelConfig{
				{ID: "main-model-config", ProviderConfig: ProviderConfig{Model: "main-model", BaseURL: server.URL + "/v1", APIKey: "main-key", Headers: map[string]string{"X-Model": "main-model"}, ReasoningEfforts: []string{"low", "high"}, ReasoningEffort: "low"}},
				{ID: "child-model-config", ProviderConfig: ProviderConfig{Model: "child-model", BaseURL: server.URL + "/v1", APIKey: "child-key", Headers: map[string]string{"X-Model": "child-model"}, ReasoningEfforts: []string{"medium", "high"}, ReasoningEffort: "medium"}},
			}
			cfg.LLM.MainModelID = "main-model-config"
			if independent {
				cfg.Agents[2].ModelID = "child-model-config"
			}
			if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
				t.Fatal(err)
			}
			before, _ := run.store.readState()
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/model-routing/messages", map[string]any{"content": "任务", "reasoning_effort": "medium"}), 400)
			after, _ := run.store.readState()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("invalid effort changed run state")
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/model-routing/messages", map[string]any{"content": "任务", "reasoning_effort": "high"}), 202)
			awaitJob(t, jobDone(t, run))
			detail, err := app.detail(run.store.id)
			if err != nil || detail.Status != "completed" || detail.State.ReasoningEffort != "high" || calls.Load() != 3 {
				t.Fatalf("model task failed: %+v %v", detail.State, err)
			}
			// A continuation without an override preserves the conversation setting.
			calls.Store(2)
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/model-routing/continue", nil), 202)
			awaitJob(t, jobDone(t, run))
			if calls.Load() != 3 {
				t.Fatal("conversation effort was not used on continuation")
			}
		})
	}
}
