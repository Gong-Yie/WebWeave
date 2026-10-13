package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	summaryPrefix       = "[历史上下文摘要]\n"
	maxToolContextChars = 12000
	maxSummaryChars     = 24000
)

type contextSummary struct {
	Goal             string   `json:"goal"`
	Decisions        []string `json:"decisions"`
	Completed        []string `json:"completed"`
	Pending          []string `json:"pending"`
	Artifacts        []string `json:"artifacts"`
	Errors           []string `json:"errors"`
	Uncertain        []string `json:"uncertain"`
	LatestUserIntent string   `json:"latest_user_intent"`
}

func shortenText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const marker = "\n...[内容已省略，需按文件路径读取相关片段]...\n"
	available := max(0, limit-utf8.RuneCountInString(marker))
	head := available * 3 / 4
	return string(runes[:head]) + marker + string(runes[len(runes)-(available-head):])
}

func completedWrites(items []inputItem) map[string]string {
	writes := map[string]string{}
	for _, item := range items {
		if itemString(item, "type") != "function_call_output" {
			continue
		}
		var output struct {
			Path string `json:"path"`
			Size *int   `json:"size"`
		}
		if json.Unmarshal([]byte(itemString(item, "output")), &output) == nil && output.Path != "" && output.Size != nil {
			writes[itemString(item, "call_id")] = output.Path
		}
	}
	return writes
}

// Full results and file contents remain in events.jsonl. Context retains only
// bounded previews and references to files that were successfully written.
func compactModelContext(items []inputItem) []inputItem {
	result := append([]inputItem(nil), items...)
	writes := completedWrites(items)
	for i, item := range items {
		field, replacement := "", ""
		switch itemString(item, "type") {
		case "function_call":
			id := itemString(item, "call_id")
			if id == "" {
				id = itemString(item, "id")
			}
			if itemString(item, "name") != "create_file" || writes[id] == "" {
				continue
			}
			var args map[string]json.RawMessage
			var content string
			if json.Unmarshal([]byte(itemString(item, "arguments")), &args) != nil || json.Unmarshal(args["content"], &content) != nil || utf8.RuneCountInString(content) <= maxToolContextChars {
				continue
			}
			args["content"], _ = json.Marshal("[此内容已成功写入 " + writes[id] + "；修改前读取相关文件片段，原文保留在文件和运行记录中]")
			data, _ := json.Marshal(args)
			field, replacement = "arguments", string(data)
		case "function_call_output":
			output := itemString(item, "output")
			if utf8.RuneCountInString(output) <= maxToolContextChars {
				continue
			}
			var object map[string]json.RawMessage
			parsed := json.Unmarshal([]byte(output), &object) == nil
			if parsed && string(object["context_compacted"]) == "true" {
				continue
			}
			preview := map[string]any{"context_compacted": true, "truncated": true, "preview": shortenText(output, maxToolContextChars), "context_note": "完整结果保留在运行记录中；需要源码时按路径读取相关片段，勿重复输出全文。"}
			if parsed {
				for _, key := range []string{"ok", "returncode", "path", "error"} {
					if value, ok := object[key]; ok && len(value) <= 1000 {
						preview[key] = value
					}
				}
			}
			data, _ := json.Marshal(preview)
			field, replacement = "output", string(data)
		}
		if field != "" {
			copy := make(inputItem, len(item))
			for key, value := range item {
				copy[key] = value
			}
			copy[field] = replacement
			result[i] = copy
		}
	}
	return result
}

// Move the cut only across calls whose outputs cross the boundary. Consecutive
// completed tool turns can be summarized without retaining the whole task.
func recentStart(items []inputItem, keep int) int {
	start := max(0, len(items)-max(1, keep))
	outputs := map[string]int{}
	for i, item := range items {
		if itemString(item, "type") == "function_call_output" {
			outputs[itemString(item, "call_id")] = i
		}
	}
	for start > 0 {
		previous := start
		for i, item := range items[:start] {
			id := itemString(item, "call_id")
			if id == "" {
				id = itemString(item, "id")
			}
			if output, ok := outputs[id]; itemString(item, "type") == "function_call" && ok && output >= start {
				start = i
				break
			}
		}
		if start > 0 && itemString(items[start], "type") == "function_call" {
			before := items[start-1]
			if kind := itemString(before, "type"); kind == "function_call" || kind == "reasoning" || itemString(before, "role") == "user" {
				start--
			}
		}
		if start == previous {
			break
		}
	}
	return start
}

func userText(item inputItem) string {
	if text, ok := item["content"].(string); ok {
		return text
	}
	data, _ := json.Marshal(item["content"])
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(data, &parts) != nil {
		return ""
	}
	texts := []string{}
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func compressionPrompt(items []inputItem, goal, latest string) string {
	history := []string{}
	for _, item := range items {
		if itemString(item, "type") == "reasoning" {
			continue
		}
		data, _ := json.Marshal(item)
		history = append(history, shortenText(string(data), 1200))
	}
	return "你是上下文压缩器，不是任务执行者。只返回 JSON 对象：goal、latest_user_intent 为字符串；decisions、completed、pending、artifacts、errors、uncertain 为字符串数组。保留已确认决定、真实完成和待办、文件路径、错误，不编造信息。原始需求与最近用户消息会原样保留，无需重复源码。\n原始目标：" + shortenText(goal, 4000) + "\n最近用户意图：" + shortenText(latest, 4000) + "\n历史记录（大内容已省略）：\n" + shortenText(strings.Join(history, "\n"), maxSummaryChars)
}

func (a *App) compressContext(ctx context.Context, client openai.Client, store *runStore, items []inputItem, cfg RuntimeConfig) ([]inputItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before := estimateTokens(items)
	items = compactModelContext(items)
	keep := cfg.Context.KeepRecentItems
	start := recentStart(items, keep)
	budget := max(1, int(float64(cfg.Context.MaxInputTokens)*cfg.Context.TriggerRatio)/2)
	for keep > 1 && estimateTokens(items[start:]) > budget {
		keep = max(1, keep/2)
		start = recentStart(items, keep)
	}
	if start == 0 {
		return items, saveAgentContext(ctx, store, items, nil)
	}
	firstUser, lastUser := -1, -1
	for i, item := range items {
		if itemString(item, "role") == "user" {
			if firstUser < 0 {
				firstUser = i
			}
			lastUser = i
		}
	}
	goal, latest := "", ""
	if firstUser >= 0 {
		goal, latest = userText(items[firstUser]), userText(items[lastUser])
	}
	var previousSummary contextSummary
	if strings.HasPrefix(itemString(items[0], "content"), summaryPrefix) {
		if err := json.Unmarshal([]byte(strings.TrimPrefix(itemString(items[0], "content"), summaryPrefix)), &previousSummary); err != nil {
			return nil, fmt.Errorf("读取已有上下文摘要: %w", err)
		}
	}
	var summary contextSummary
	compressionCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Context.TimeoutSeconds*float64(time.Second)))
	params := responses.ResponseNewParams{Model: cfg.LLM.Main.Model, MaxOutputTokens: openai.Int(int64(cfg.Context.SummaryMaxTokens))}
	if len(cfg.LLM.Main.ReasoningEfforts) == 0 {
		params.Temperature = openai.Float(0.1)
	}
	params.Reasoning.Effort = shared.ReasoningEffort(cfg.LLM.Main.ReasoningEffort)
	response, err := modelResponse(compressionCtx, client, params, compressionPrompt(items[:start], goal, latest), false, nil)
	cancel()
	if err == nil {
		text := strings.TrimSpace(response.OutputText())
		if strings.HasPrefix(text, "```") {
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			}
			text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
		}
		err = json.Unmarshal([]byte(text), &summary)
		if err == nil && summary.Goal == "" && len(summary.Pending) == 0 && len(summary.Completed) == 0 {
			err = errors.New("压缩结果缺少目标或进度信息")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	errorText := ""
	if err != nil {
		errorText = err.Error()
		slog.Warn("上下文压缩失败，保留需求与已有摘要", "run_id", store.id, "error", err)
		summary = previousSummary
		summary.Errors = append(summary.Errors, "本次模型压缩失败: "+shortenText(errorText, 1000))
		if len(summary.Pending) == 0 {
			summary.Pending = []string{"根据保留的用户需求和已有文件继续未完成任务"}
		}
	}
	if summary.Goal == "" {
		summary.Goal = shortenText(goal, 4000)
	}
	summary.LatestUserIntent = shortenText(latest, 4000)
	seen := map[string]bool{}
	for _, path := range summary.Artifacts {
		seen[path] = true
	}
	for _, path := range previousSummary.Artifacts {
		if !seen[path] {
			summary.Artifacts = append(summary.Artifacts, path)
			seen[path] = true
		}
	}
	writes := completedWrites(items)
	for _, item := range items {
		if path := writes[itemString(item, "call_id")]; itemString(item, "type") == "function_call_output" && path != "" && !seen[path] {
			summary.Artifacts = append(summary.Artifacts, path)
			seen[path] = true
		}
	}
	for _, list := range []*[]string{&summary.Decisions, &summary.Completed, &summary.Pending, &summary.Artifacts, &summary.Errors, &summary.Uncertain} {
		if *list == nil {
			*list = []string{}
		}
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	compressed := []inputItem{{"role": "assistant", "content": summaryPrefix + string(data)}}
	if firstUser >= 0 && firstUser < start {
		compressed = append(compressed, items[firstUser])
	}
	if lastUser >= 0 && lastUser < start && lastUser != firstUser {
		compressed = append(compressed, items[lastUser])
	}
	compressed = append(compressed, items[start:]...)
	if err := appendAgentEvent(ctx, store, "context_compressed", map[string]any{"summary": summary, "input_items": compressed, "removed_items": start, "tokens_before": before, "tokens_after": estimateTokens(compressed), "compression_error": errorText}); err != nil {
		return nil, err
	}
	return compressed, saveAgentContext(ctx, store, compressed, summary)
}
