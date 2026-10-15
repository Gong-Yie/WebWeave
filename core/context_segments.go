package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"webweave/workspace"
)

func observationFacts(raw string) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &object)
	facts := map[string]json.RawMessage{}
	for _, key := range []string{"ok", "returncode", "path", "error", "status", "mode", "pid", "log_path", "size", "url", "file_name", "truncated", "max_chars"} {
		if value, ok := object[key]; ok {
			facts[key] = value
		}
	}
	var sources []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if json.Unmarshal([]byte(raw), &sources) == nil && len(sources) > 0 && sources[0].URL != "" {
		facts["sources"], _ = json.Marshal(sources)
	}
	return facts
}

// Only prose fields enter the token compressor, never an enclosing JSON object.
func observationProse(name, raw string) string {
	if name == "file_read" {
		var output struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(raw), &output) == nil {
			ext := strings.ToLower(filepath.Ext(output.Path))
			if ext == ".txt" || ext == ".md" {
				return output.Content
			}
		}
	}
	if name == "web_search" {
		var results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		}
		if json.Unmarshal([]byte(raw), &results) == nil {
			parts := []string{}
			for _, result := range results {
				parts = append(parts, result.Title+"\n"+result.Snippet)
			}
			return strings.Join(parts, "\n\n")
		}
	}
	return ""
}

func (a *App) compactAgentContext(ctx context.Context, store *runStore, items []inputItem, cfg RuntimeConfig) ([]inputItem, error) {
	if !cfg.Context.Enabled {
		return compactModelContext(items), nil
	}
	result := compactModelContext(items)
	names := map[string]string{}
	writes := completedWrites(items)
	for i, item := range items {
		if itemString(item, "type") == "function_call" {
			names[callID(item)] = itemString(item, "name")
			if itemString(item, "name") == "create_file" && writes[callID(item)] != "" && itemString(result[i], "arguments") != itemString(item, "arguments") {
				segment, err := workspace.SaveContextSegment(a.root, store.id, "create_file_arguments", writes[callID(item)], itemString(item, "arguments"))
				if err != nil {
					return items, err
				}
				var args map[string]json.RawMessage
				if err := json.Unmarshal([]byte(itemString(result[i], "arguments")), &args); err != nil {
					return items, err
				}
				args["content"], _ = json.Marshal("[已成功写入 " + writes[callID(item)] + "；原始写入参数用 read_context_segment 回读片段 " + segment.ID + "；修改前读取当前文件]")
				data, err := json.Marshal(args)
				if err != nil {
					return items, err
				}
				result[i]["arguments"] = string(data)
			}
		}
	}
	start := recentStart(items, cfg.Context.KeepRecentItems)
	for i, item := range items {
		if itemString(item, "type") != "function_call_output" {
			continue
		}
		// Recent observations and explicit original reads must remain exact.
		result[i] = item
		raw, name := itemString(item, "output"), names[itemString(item, "call_id")]
		if name == "read_context_segment" || utf8.RuneCountInString(raw) <= maxToolContextChars {
			continue
		}
		var existing struct {
			SegmentID string `json:"segment_id"`
		}
		if json.Unmarshal([]byte(raw), &existing) == nil && existing.SegmentID != "" {
			continue
		}
		facts := observationFacts(raw)
		source := name
		if value, ok := facts["path"]; ok {
			_ = json.Unmarshal(value, &source)
		}
		segment, err := workspace.SaveContextSegment(a.root, store.id, name, source, raw)
		if err != nil {
			return items, err
		}
		if i >= start || strings.HasPrefix(raw, "工具执行失败") || facts["returncode"] != nil && string(facts["returncode"]) != "0" || facts["error"] != nil && string(facts["error"]) != "null" {
			continue
		}
		preview := shortenText(raw, 1000)
		local := cfg.Context.LocalCompression
		if prose := observationProse(name, raw); local.Enabled && prose != "" {
			if segment.Compressed == "" || segment.Model != local.Model || segment.Rate != local.Rate {
				compressionCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Context.TimeoutSeconds*float64(time.Second)))
				compressed, err := a.compressText(compressionCtx, local, prose)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return items, ctx.Err()
					}
					if eventErr := appendAgentEvent(ctx, store, "context_segment_compression_failed", map[string]any{"segment_id": segment.ID, "error": err.Error()}); eventErr != nil {
						return items, eventErr
					}
					return items, fmt.Errorf("%w (local_worker): %w", errContextCompression, err)
				}
				segment.Compressed, segment.Model, segment.Rate = compressed.Text, local.Model, local.Rate
				if err := workspace.UpdateContextSegment(a.root, store.id, segment); err != nil {
					return items, err
				}
				kind := "context_segment_compressed"
				if compressed.OutputTokens == compressed.InputTokens {
					kind = "context_segment_compression_skipped"
				}
				if err := appendAgentEvent(ctx, store, kind, map[string]any{"segment_id": segment.ID, "input_tokens": compressed.InputTokens, "output_tokens": compressed.OutputTokens, "estimated": true}); err != nil {
					return items, err
				}
			}
			if segment.Compressed == prose {
				continue
			}
			preview = segment.Compressed
		}
		payload := map[string]any{"context_compacted": true, "segment_id": segment.ID, "source": source, "facts": facts, "preview": preview, "context_note": "此为旧观察的有损预览；完整原文用 read_context_segment 按 ID 回读。修改代码前以实际文件为准。"}
		data, err := json.Marshal(payload)
		if err != nil {
			return items, err
		}
		copy := make(inputItem, len(item))
		for key, value := range item {
			copy[key] = value
		}
		copy["output"] = string(data)
		result[i] = copy
	}
	return result, nil
}

func callID(item inputItem) string {
	id := itemString(item, "call_id")
	if id == "" {
		id = itemString(item, "id")
	}
	return id
}

// Summaries read original observations, not the output of another lossy model.
func (a *App) summaryHistory(store *runStore, items []inputItem) ([]string, error) {
	groups := []string{}
	group := []inputItem{}
	pending := map[string]bool{}
	names := map[string]string{}
	for _, item := range items {
		if itemString(item, "type") == "function_call" {
			names[callID(item)] = itemString(item, "name")
		}
	}
	for _, original := range items {
		if itemString(original, "type") == "reasoning" || strings.HasPrefix(itemString(original, "content"), summaryPrefix) {
			continue
		}
		item := make(inputItem, len(original))
		for key, value := range original {
			item[key] = value
		}
		if itemString(item, "type") == "function_call_output" {
			var preview struct {
				SegmentID string `json:"segment_id"`
			}
			_ = json.Unmarshal([]byte(itemString(item, "output")), &preview)
			if preview.SegmentID != "" || utf8.RuneCountInString(itemString(item, "output")) > maxToolContextChars {
				var segment workspace.ContextSegment
				var err error
				if preview.SegmentID != "" {
					segment, err = workspace.ReadContextSegment(a.root, store.id, preview.SegmentID)
				} else {
					kind := names[itemString(item, "call_id")]
					source := kind
					if path := observationFacts(itemString(item, "output"))["path"]; path != nil {
						_ = json.Unmarshal(path, &source)
					}
					segment, err = workspace.SaveContextSegment(a.root, store.id, kind, source, itemString(item, "output"))
				}
				if err != nil {
					return nil, err
				}
				item["output"] = segment.Original
				item["context_segment_id"] = segment.ID
				if segment.Kind == "file_read" {
					var object map[string]json.RawMessage
					if json.Unmarshal([]byte(segment.Original), &object) == nil && observationProse("file_read", segment.Original) == "" {
						object["content"], _ = json.Marshal("[源码原文用 read_context_segment 回读片段 " + segment.ID + "；此处只记录文件元数据]")
						data, err := json.Marshal(object)
						if err != nil {
							return nil, err
						}
						item["output"] = string(data)
					}
				}
				if segment.Kind == "terminal_run" {
					var object map[string]json.RawMessage
					if json.Unmarshal([]byte(segment.Original), &object) == nil && string(object["returncode"]) == "0" {
						object["stdout"], _ = json.Marshal("[旧终端输出原文片段 " + segment.ID + "；需核对具体检查结果时回读，不据退出码推断页面无缺陷]")
						data, err := json.Marshal(object)
						if err != nil {
							return nil, err
						}
						item["output"] = string(data)
					}
				}
			}
		}
		// Large source payloads are referenced exactly rather than paraphrased.
		if itemString(item, "type") == "function_call" && itemString(item, "name") == "create_file" {
			var args map[string]json.RawMessage
			var content, path string
			if json.Unmarshal([]byte(itemString(item, "arguments")), &args) == nil && json.Unmarshal(args["content"], &content) == nil && len(content) > 1000 {
				_ = json.Unmarshal(args["file_path"], &path)
				segment, err := workspace.SaveContextSegment(a.root, store.id, "create_file_arguments", path, itemString(item, "arguments"))
				if err != nil {
					return nil, err
				}
				args["content"], _ = json.Marshal("[源码原文片段 " + segment.ID + "；不得据文件写入推断验证已通过]")
				data, _ := json.Marshal(args)
				item["arguments"] = string(data)
			}
		}
		group = append(group, item)
		switch itemString(item, "type") {
		case "function_call":
			pending[callID(item)] = true
		case "function_call_output":
			delete(pending, callID(item))
		}
		if len(pending) == 0 {
			data, err := json.Marshal(group)
			if err != nil {
				return nil, err
			}
			groups = append(groups, string(data))
			group = []inputItem{}
		}
	}
	if len(group) > 0 {
		return nil, fmt.Errorf("摘要历史含未配对的工具调用")
	}
	return groups, nil
}
