package core

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"webweave/workspace"
)

// Only invoked after the normal compaction path cannot fit a request. Exact
// material remains addressable, including recent observations and large errors.
func (a *App) fitRequestContext(store *runStore, items []inputItem, target int, estimate func([]inputItem) int) ([]inputItem, error) {
	result := append([]inputItem(nil), items...)
	names := map[string]string{}
	for _, item := range items {
		if itemString(item, "type") == "function_call" {
			names[callID(item)] = itemString(item, "name")
		}
	}
	indices := make([]int, 0, len(items))
	sizes := make([]int, len(items))
	for i, item := range items {
		if strings.HasPrefix(itemString(item, "content"), summaryPrefix) {
			continue
		}
		data, err := json.Marshal(item)
		if err != nil {
			return items, err
		}
		sizes[i] = utf8.RuneCount(data)
		indices = append(indices, i)
	}
	sort.SliceStable(indices, func(i, j int) bool { return sizes[indices[i]] > sizes[indices[j]] })
	for _, i := range indices {
		if estimate(result) <= target {
			break
		}
		item := result[i]
		if sizes[i] <= 1024 {
			continue
		}
		data, err := json.Marshal(item)
		if err != nil {
			return items, err
		}
		kind := itemString(item, "type")
		original := string(data)
		if kind == "function_call_output" {
			original = itemString(item, "output")
		}
		segmentKind := "request_" + kind
		if kind == "function_call_output" && names[callID(item)] != "" {
			segmentKind = names[callID(item)]
		}
		segment, err := workspace.SaveContextSegment(a.root, store.id, segmentKind, callID(item), original)
		if err != nil {
			return items, err
		}
		reference := "[材料超出当前请求预算；完整原文用 read_context_segment 回读 " + segment.ID + "，按 offset/max_chars 读取所需范围。用户需求须核对原文后执行。]"
		switch kind {
		case "function_call_output":
			facts := observationFacts(original)
			for key, value := range facts {
				if len(value) > 512 {
					delete(facts, key)
				}
			}
			preview, err := json.Marshal(map[string]any{"context_compacted": true, "segment_id": segment.ID, "facts": facts, "preview": shortenText(original, 256), "context_note": reference})
			if err != nil {
				return items, err
			}
			copy := make(inputItem, len(item))
			for key, value := range item {
				copy[key] = value
			}
			copy["output"] = string(preview)
			result[i] = copy
		case "function_call":
			// All pending calls execute before context fitting, so replacing a
			// historical argument cannot change or replay the saved operation.
			copy := make(inputItem, len(item))
			for key, value := range item {
				copy[key] = value
			}
			args, err := json.Marshal(map[string]string{"context_note": reference})
			if err != nil {
				return items, err
			}
			copy["arguments"] = string(args)
			result[i] = copy
		default:
			role := itemString(item, "role")
			if role == "" {
				role = "assistant"
			}
			result[i] = inputItem{"role": role, "content": reference}
		}
	}
	return result, nil
}
