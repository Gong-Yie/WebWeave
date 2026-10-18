package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"webweave/workspace"
)

type summaryProgress struct {
	Key       string         `json:"key"`
	Summary   contextSummary `json:"summary"`
	Remaining []string       `json:"remaining"`
	Batches   int            `json:"batches"`
}

// Enforce limits locally as compatible endpoints do not all support maxItems.
// The archive chains exact prior snapshots; no path or error ID is cut in half.
func (a *App) boundSummary(store *runStore, summary contextSummary, maximum int) (contextSummary, error) {
	count := max(1, min(6, maximum/1024))
	chars := max(1, min(96, maximum/32))
	goalChars := max(1, min(192, maximum/8))
	data, err := json.Marshal(summary)
	if err != nil {
		return summary, err
	}
	bounded := utf8.RuneCountInString(summary.Goal) <= goalChars && utf8.RuneCountInString(summary.LatestUserIntent) <= goalChars
	for _, values := range [][]string{summary.Decisions, summary.Completed, summary.Pending, summary.Artifacts, summary.Errors, summary.Uncertain} {
		bounded = bounded && len(values) <= count
		for _, value := range values {
			bounded = bounded && (utf8.RuneCountInString(value) <= chars || strings.HasPrefix(value, "[原文片段:"))
		}
	}
	if bounded {
		return summary, nil
	}
	segment, err := workspace.SaveContextSegment(a.root, store.id, "context_summary", "历史摘要完整记录", string(data))
	if err != nil {
		return summary, err
	}
	reference := "[原文片段:" + segment.ID + "]"
	if utf8.RuneCountInString(summary.Goal) > goalChars {
		summary.Goal = reference
	}
	if utf8.RuneCountInString(summary.LatestUserIntent) > goalChars {
		summary.LatestUserIntent = reference
	}
	for _, values := range []*[]string{&summary.Decisions, &summary.Completed, &summary.Pending, &summary.Artifacts, &summary.Errors, &summary.Uncertain} {
		copy := append([]string(nil), (*values)...)
		if len(copy) > count {
			copy = append(copy[:count-1], reference)
		}
		for i, value := range copy {
			if utf8.RuneCountInString(value) > chars {
				copy[i] = reference
			}
		}
		*values = copy
	}
	summary.Archive = segment.ID
	return summary, nil
}

type summaryRequest struct {
	Previous contextSummary `json:"previous_summary"`
	Goal     string         `json:"original_goal"`
	Latest   string         `json:"latest_user_intent"`
	History  []string       `json:"history"`
}

func summaryParams(provider ProviderConfig, maximum int) responses.ResponseNewParams {
	properties := map[string]any{}
	required := []string{"goal", "latest_user_intent", "decisions", "completed", "pending", "artifacts", "errors", "uncertain"}
	// Leave output room for JSON syntax and tokenization differences; each batch
	// replaces a bounded working memory instead of appending a growing transcript.
	goalChars := max(1, min(192, maximum/8))
	listItems := max(1, min(6, maximum/1024))
	itemChars := max(1, min(96, maximum/32))
	for _, name := range required {
		if name == "goal" || name == "latest_user_intent" {
			properties[name] = map[string]any{"type": "string", "pattern": fmt.Sprintf(`^[\s\S]{0,%d}$`, goalChars)}
		} else {
			properties[name] = map[string]any{
				"type": "array", "description": fmt.Sprintf("最多 %d 条，每批归并去重，不逐条追加执行日志", listItems),
				"items": map[string]any{"type": "string", "pattern": fmt.Sprintf(`^[\s\S]{0,%d}$`, itemChars)},
			}
		}
	}
	params := responses.ResponseNewParams{Model: provider.Model, Instructions: openai.String(compressionInstructions), MaxOutputTokens: openai.Int(int64(maximum))}
	params.Text.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{Name: "context_summary", Strict: openai.Bool(true), Schema: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
	if strings.HasPrefix(provider.Model, "deepseek-") {
		params.Reasoning.Effort = shared.ReasoningEffortNone
		params.Temperature = openai.Float(0.1)
	} else if len(provider.ReasoningEfforts) == 0 {
		params.Temperature = openai.Float(0.1)
	} else {
		for _, effort := range provider.ReasoningEfforts {
			if effort == "none" || effort == "minimal" || effort == "low" {
				params.Reasoning.Effort = shared.ReasoningEffort(effort)
				break
			}
		}
	}
	return params
}

// Whole turns stay together when they fit. Oversized turns are continued in
// later batches without discarding their middle or altering the actual history.
func nextSummaryBatch(params responses.ResponseNewParams, summary contextSummary, goal, latest string, history []string, budget int) (string, []string, error) {
	request := summaryRequest{summary, goal, latest, []string{}}
	encode := func() (string, bool, error) {
		data, err := json.Marshal(request)
		text := string(data)
		return text, estimateRequestTokens(params, []inputItem{{"role": "user", "content": text}}) <= budget, err
	}
	for len(history) > 0 {
		request.History = append(request.History, history[0])
		text, fits, err := encode()
		if err != nil {
			return "", history, err
		}
		if fits {
			history = history[1:]
			if len(history) == 0 {
				return text, history, nil
			}
			continue
		}
		request.History = request.History[:len(request.History)-1]
		if len(request.History) > 0 {
			text, _, err = encode()
			return text, history, err
		}
		runes := []rune(history[0])
		low, high := 0, len(runes)
		for low < high {
			middle := (low + high + 1) / 2
			request.History = []string{"[连续历史片段]\n" + string(runes[:middle])}
			_, fits, err = encode()
			if err != nil {
				return "", history, err
			}
			if fits {
				low = middle
			} else {
				high = middle - 1
			}
		}
		if low == 0 {
			return "", history, errors.New("摘要、用户需求和输出 Schema 已超过摘要输入预算，不能丢弃需求进行压缩")
		}
		request.History = []string{"[连续历史片段]\n" + string(runes[:low])}
		text, _, err = encode()
		rest := append([]string{string(runes[low:])}, history[1:]...)
		return text, rest, err
	}
	return "", history, errors.New("没有需要总结的历史记录")
}
