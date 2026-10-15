package core

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type summaryRequest struct {
	Previous contextSummary `json:"previous_summary"`
	Goal     string         `json:"original_goal"`
	Latest   string         `json:"latest_user_intent"`
	History  []string       `json:"history"`
}

func summaryParams(provider ProviderConfig, maximum int) responses.ResponseNewParams {
	properties := map[string]any{}
	required := []string{"goal", "latest_user_intent", "decisions", "completed", "pending", "artifacts", "errors", "uncertain"}
	for _, name := range required {
		if name == "goal" || name == "latest_user_intent" {
			properties[name] = map[string]any{"type": "string"}
		} else {
			properties[name] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
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
