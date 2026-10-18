package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"webweave/tools"
	"webweave/workspace"
)

func TestContextSegmentsAndRecall(t *testing.T) {
	for _, extension := range []string{".md", ".html"} {
		t.Run(extension, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "segment-run")
			testRun(t, app, "other-run")
			cfg := defaultConfig()
			cfg.Context.KeepRecentItems = 2
			cfg.Context.LocalCompression.Enabled = true
			calls := 0
			app.compressText = func(_ context.Context, local LocalCompressionConfig, text string) (localCompressionResult, error) {
				calls++
				if !strings.Contains(text, "资料正文") || local.Rate != 0.7 {
					t.Fatal("incorrect prose or rate")
				}
				return localCompressionResult{OK: true, Text: "压缩后的资料正文", InputTokens: 10000, OutputTokens: 7000}, nil
			}
			raw, _ := json.Marshal(map[string]any{"path": "resources/notes" + extension, "content": strings.Repeat("资料正文和背景介绍。", 2000), "truncated": false})
			items := []inputItem{{"role": "user", "content": "制作网页"}, {"type": "function_call", "call_id": "old", "name": "file_read", "arguments": "{}"}, {"type": "function_call_output", "call_id": "old", "output": string(raw)}, {"type": "function_call", "call_id": "recent", "name": "file_read", "arguments": "{}"}, {"type": "function_call_output", "call_id": "recent", "output": string(raw)}}
			compacted, err := app.compactAgentContext(context.Background(), run.store, items, cfg)
			if err != nil || itemString(compacted[4], "output") != string(raw) {
				t.Fatal("recent observation changed", err)
			}
			var preview struct {
				SegmentID string `json:"segment_id"`
				Preview   string `json:"preview"`
			}
			if err := json.Unmarshal([]byte(itemString(compacted[2], "output")), &preview); err != nil || preview.SegmentID == "" {
				t.Fatal("original reference missing", err)
			}
			if extension == ".md" && (calls != 1 || preview.Preview != "压缩后的资料正文") || extension == ".html" && calls != 0 {
				t.Fatal("source code entered token compression or prose was not compressed")
			}
			if _, err := app.compactAgentContext(context.Background(), run.store, items, cfg); err != nil || extension == ".md" && calls != 1 {
				t.Fatal("cached observation was recompressed", err)
			}
			segment, err := workspace.ReadContextSegment(app.root, run.store.id, preview.SegmentID)
			if err != nil || segment.Original != string(raw) {
				t.Fatal("original bytes changed", err)
			}
			arguments, _ := json.Marshal(map[string]any{"segment_id": preview.SegmentID, "offset": 5, "max_chars": 20})
			config := tools.Config{Tools: cfg.Tools, DefaultSeconds: 5, MaxSeconds: 10}
			result := app.tools.Execute(context.Background(), run.store.id, "read_context_segment", string(arguments), config)
			var output struct {
				Content    string `json:"content"`
				NextOffset int    `json:"next_offset"`
			}
			if err := json.Unmarshal([]byte(result), &output); err != nil || output.Content != string([]rune(string(raw))[5:25]) || output.NextOffset != 25 {
				t.Fatal("exact paged original read failed", result)
			}
			for _, id := range []string{"other-run", run.store.id} {
				args := string(arguments)
				if id == run.store.id {
					args = `{"segment_id":"../escape"}`
				}
				if result := app.tools.Execute(context.Background(), id, "read_context_segment", args, config); !strings.Contains(result, "失败") {
					t.Fatal("cross-run or invalid segment read accepted", result)
				}
			}
			history, err := app.summaryHistory(run.store, compacted[:3])
			joined := strings.Join(history, "")
			if err != nil || !strings.Contains(joined, preview.SegmentID) || extension == ".md" && !strings.Contains(joined, "资料正文和背景介绍") || extension == ".html" && strings.Contains(joined, "资料正文和背景介绍") || strings.Contains(joined, "压缩后的资料正文") {
				t.Fatal("summary consumed lossy observations instead of originals", err)
			}
		})
	}
}

func TestContextSummaryOutputBudget(t *testing.T) {
	for _, maximum := range []int{1024, 4096, 8192} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			params := summaryParams(ProviderConfig{Model: "deepseek-v4-pro"}, maximum)
			properties := params.Text.Format.OfJSONSchema.Schema["properties"].(map[string]any)
			largest := map[string]any{}
			for name, raw := range properties {
				property := raw.(map[string]any)
				pattern := property
				if property["type"] == "array" {
					pattern = property["items"].(map[string]any)
				}
				var length int
				if _, err := fmt.Sscanf(pattern["pattern"].(string), `^[\s\S]{0,%d}$`, &length); err != nil {
					t.Fatal(err)
				}
				bounded := strings.Repeat("字", length)
				matcher := regexp.MustCompile(pattern["pattern"].(string))
				if !matcher.MatchString(bounded) || matcher.MatchString(bounded+"字") {
					t.Fatal("summary string pattern does not constrain length")
				}
				if property["type"] == "string" {
					largest[name] = bounded
					continue
				}
				var count int
				if _, err := fmt.Sscanf(property["description"].(string), "最多 %d 条", &count); err != nil {
					t.Fatal(err)
				}
				values := []string{}
				for i := 0; i < count; i++ {
					values = append(values, bounded)
				}
				largest[name] = values
			}
			data, err := json.Marshal(largest)
			if err != nil || utf8.RuneCount(data) >= maximum || params.MaxOutputTokens.Value != int64(maximum) || params.Reasoning.Effort != "none" {
				t.Fatalf("summary has no output headroom or changed the configured limit: chars=%d max=%d err=%v", utf8.RuneCount(data), maximum, err)
			}
			schema, err := json.Marshal(params.Text.Format.OfJSONSchema.Schema)
			if err != nil || strings.Contains(string(schema), `"maxLength"`) || strings.Contains(string(schema), `"maxItems"`) {
				t.Fatal("summary schema uses unsupported DeepSeek constraints", err)
			}
		})
	}
}

func TestContextRollingSummaryAndDedicatedModel(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "rolling-summary")
	seen := map[string]bool{}
	batches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input     string `json:"input"`
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Text struct {
				Format struct {
					Type   string         `json:"type"`
					Schema map[string]any `json:"schema"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Model != "deepseek-v4-pro" || r.Header.Get("Authorization") != "Bearer summary-key" || request.Reasoning.Effort != "none" || request.Text.Format.Type != "json_schema" || len(request.Text.Format.Schema) == 0 {
			t.Error("dedicated summary model, non-thinking mode or schema missing")
		}
		var input summaryRequest
		if err := json.Unmarshal([]byte(request.Input), &input); err != nil {
			t.Error(err)
			return
		}
		for i := 0; i < 20; i++ {
			marker := fmt.Sprintf("历史事实-%02d", i)
			if strings.Contains(request.Input, marker) {
				seen[marker] = true
			}
		}
		if batches > 0 && len(input.Previous.Completed) == 0 {
			t.Error("previous batch summary was not carried forward")
		}
		batches++
		summary := input.Previous
		summary.Goal = input.Goal
		summary.Completed = []string{"已完成前一批历史整理"}
		data, _ := json.Marshal(summary)
		serveModelResponse(w, false, []inputItem{messageOutput(string(data))})
	}))
	t.Cleanup(server.Close)
	cfg := useProvider(t, app, server.URL, false)
	cfg.LLM.Models = []ModelConfig{{ID: "summary", ProviderConfig: ProviderConfig{Model: "deepseek-v4-pro", BaseURL: server.URL + "/v1", APIKey: "summary-key"}}}
	cfg.Context.SummaryModelID, cfg.Context.SummaryInputTokens, cfg.Context.KeepRecentItems = "summary", 3000, 1
	items := []inputItem{{"role": "user", "content": "保留所有历史事实"}}
	for i := 0; i < 20; i++ {
		items = append(items, inputItem{"role": "assistant", "content": fmt.Sprintf("历史事实-%02d：", i) + strings.Repeat("背景描述", 200)})
	}
	items = append(items, inputItem{"role": "user", "content": "继续完成网页"})
	client, err := newClient(cfg.LLM.Main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.compressContext(context.Background(), client, run.store, items, cfg); err != nil {
		t.Fatal(err)
	}
	if batches < 2 || len(seen) != 20 {
		t.Fatalf("middle history was discarded: batches=%d seen=%d", batches, len(seen))
	}
}

func TestContextSearchReferencesPreserved(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "search-reference")
	cfg := defaultConfig()
	cfg.Context.KeepRecentItems, cfg.Context.LocalCompression.Enabled = 1, true
	app.compressText = func(_ context.Context, _ LocalCompressionConfig, _ string) (localCompressionResult, error) {
		return localCompressionResult{OK: true, Text: "压缩资料", InputTokens: 10000, OutputTokens: 7000}, nil
	}
	raw, _ := json.Marshal([]map[string]string{{"title": "原文来源", "url": "https://example.test/original", "snippet": strings.Repeat("背景资料", 5000)}})
	items := []inputItem{{"role": "user", "content": "制作网页"}, {"type": "function_call", "call_id": "search", "name": "web_search", "arguments": "{}"}, {"type": "function_call_output", "call_id": "search", "output": string(raw)}, {"role": "user", "content": "继续"}}
	result, err := app.compactAgentContext(context.Background(), run.store, items, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Facts struct {
			Sources []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"sources"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(itemString(result[2], "output")), &preview); err != nil || len(preview.Facts.Sources) != 1 || preview.Facts.Sources[0].Title != "原文来源" || preview.Facts.Sources[0].URL != "https://example.test/original" {
		t.Fatal("compression lost the exact source title or URL", err)
	}
}

func TestContextProtectedObservationNotExpanded(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "protected-prose")
	cfg := defaultConfig()
	cfg.Context.KeepRecentItems, cfg.Context.LocalCompression.Enabled = 1, true
	calls := 0
	app.compressText = func(_ context.Context, _ LocalCompressionConfig, text string) (localCompressionResult, error) {
		calls++
		return localCompressionResult{OK: true, Text: text, InputTokens: 100, OutputTokens: 100}, nil
	}
	raw, _ := json.Marshal(map[string]any{"path": "notes.md", "content": strings.Repeat("必须保留 768 的约束。\n", 2000)})
	items := []inputItem{{"role": "user", "content": "制作网页"}, {"type": "function_call", "call_id": "read", "name": "file_read", "arguments": "{}"}, {"type": "function_call_output", "call_id": "read", "output": string(raw)}, {"role": "user", "content": "继续"}}
	for i := 0; i < 2; i++ {
		result, err := app.compactAgentContext(context.Background(), run.store, items, cfg)
		if err != nil || itemString(result[2], "output") != string(raw) {
			t.Fatal("protected observation was expanded or altered", err)
		}
	}
	if calls != 1 {
		t.Fatal("unchanged protected text was not cached")
	}
	_, events, err := run.store.snapshot()
	if err != nil || events[len(events)-1].Type != "context_segment_compression_skipped" {
		t.Fatal("no-op was reported as successful compression", err)
	}
}

func TestPythonCompressionLifecycle(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("Python unavailable")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compression"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "import json, os, sys, time\nif 'sleep-startup' in sys.argv: time.sleep(30)\nfor line in sys.stdin:\n r=json.loads(line)\n if r['text']=='wait': time.sleep(30)\n print(json.dumps({'ok':True,'text':str(os.getpid()),'input_tokens':100,'output_tokens':10}),flush=True)\n"
	if err := os.WriteFile(filepath.Join(root, "compression", "worker.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := newPythonCompressor(ctx, root)
	t.Cleanup(func() { cancel(); worker.Close() })
	cfg := LocalCompressionConfig{Python: python, Model: "fixture", Device: "cpu", Rate: 0.7}
	first, err := worker.Compress(context.Background(), cfg, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := worker.Compress(context.Background(), cfg, "second")
	if err != nil || first.Text != second.Text {
		t.Fatal("worker was not reused", err)
	}
	timeout, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if _, err := worker.Compress(timeout, cfg, "wait"); err != context.DeadlineExceeded {
		t.Fatal("cancellation did not stop the worker", err)
	}
	third, err := worker.Compress(context.Background(), cfg, "third")
	if err != nil || first.Text == third.Text {
		t.Fatal("cancelled worker was not replaced", err)
	}
	cfg.Model = "sleep-startup"
	startup, stopStartup := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopStartup()
	if _, err := worker.Compress(startup, cfg, strings.Repeat("large request", 100000)); err != context.DeadlineExceeded {
		t.Fatal("blocked write during model loading ignored cancellation", err)
	}
}
