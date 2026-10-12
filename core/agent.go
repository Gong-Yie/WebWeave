package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"webweave/workspace"
)

const second = time.Second

const systemPrompt = `你是一个“网页项目生成型 Agent”，使用完整的 ReAct 工作循环解决用户任务。
理解用户需求，按需读取当前运行 ID 的资源，检索必要资料，把最终文件写入当前运行 ID 的结果目录。
当前运行 ID：{run_id}。
可用工具：list_skills、read_skill、file_read、web_search、create_file、terminal_run、web_instance。
先分析目标和上下文，再按需调用工具；需要事实、源码或已有产物时先读取文件，需要外部资料时搜索网页。
写文件前确认路径和完整内容；以实际工具返回结果为准。所有生成网站的内容默认使用中文。
每次工具调用必须填写 timeout（秒），默认 {timeout_default} 秒，最大 {timeout_max} 秒。
一次响应可能包含多个工具调用，必须返回所有工具结果后继续下一轮，直到模型不再调用工具。
缺少关键需求时向用户澄清。不得声称未执行的操作已经完成。
安全边界：只能读取当前运行的 data/resources/{run_id}、data/result/{run_id}、data/download/{run_id}。
只能通过 create_file 写入当前运行 result 目录，通过 web_search 下载到当前运行 download 目录。
terminal_run 的 cwd 必须在当前运行的 resources、result 或 download 目录内。
Web 服务必须通过 web_instance 启动，不得使用 terminal_run 后台模式代替实例注册。
web_instance 只能从当前运行 result 目录启动，每个运行同时只能有一个活动实例。
web_instance start 的 command 必须包含 {host} 和 {port}；port=0 自动分配端口，restart 复用启动参数。
配置及本地 SKILL 文件会重新读取；Go 工具源码修改后需要重新编译。`

func jsonBytes(value any) ([]byte, error) { return json.Marshal(value) }

func newClient(provider ProviderConfig) (openai.Client, error) {
	if provider.APIKey == "" {
		return openai.Client{}, errors.New("未配置大模型 API Key，请在模型配置页面填写")
	}
	return openai.NewClient(option.WithAPIKey(provider.APIKey), option.WithBaseURL(strings.TrimRight(provider.BaseURL, "/")+"/"), option.WithMaxRetries(0), option.WithRequestTimeout(10*time.Minute)), nil
}

func (a *App) runAgent(ctx context.Context, store *runStore, items []inputItem, agent AgentConfig, delegated bool) (string, error) {
	ctx = context.WithValue(ctx, agentExecutionKey{}, agentExecution{agent, delegated})
	items = repairItems(items)
	if err := saveAgentContext(ctx, store, items, nil); err != nil {
		return "", err
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cfg := a.config.get()
		client, err := newClient(cfg.LLM.Main)
		if err != nil {
			return "", err
		}
		if cfg.Context.Enabled && estimateTokens(items) >= int(float64(cfg.Context.MaxInputTokens)*cfg.Context.TriggerRatio) {
			items, err = a.compressContext(ctx, client, store, items, cfg)
			if err != nil {
				return "", err
			}
		}
		params := responses.ResponseNewParams{Model: cfg.LLM.Main.Model, Instructions: openai.String(agentInstructions(agent, cfg, store.id)), Temperature: openai.Float(0.7), Tools: agentTools(agent, cfg)}
		response, err := modelResponse(ctx, client, params, items, cfg.Model.Stream, store)
		if err != nil {
			return "", err
		}
		outputs := []inputItem{}
		for _, output := range response.Output {
			var item inputItem
			if err := json.Unmarshal([]byte(output.RawJSON()), &item); err != nil {
				return "", fmt.Errorf("解析模型输出: %w", err)
			}
			outputs = append(outputs, item)
		}
		text := response.OutputText()
		if len(outputs) == 0 && text != "" {
			outputs = append(outputs, inputItem{"role": "assistant", "content": text})
		}
		items = append(items, outputs...)
		if err = appendAgentEvent(ctx, store, "model_output", map[string]any{"items": outputs, "output_text": text}); err != nil {
			return "", err
		}
		if err = saveAgentContext(ctx, store, items, nil); err != nil {
			return "", err
		}
		calls := []inputItem{}
		for _, item := range outputs {
			if itemString(item, "type") == "function_call" {
				calls = append(calls, item)
			}
		}
		if len(calls) == 0 {
			return text, ctx.Err()
		}
		for _, call := range calls {
			started := time.Now()
			cancelled := ctx.Err() != nil
			name, arguments := itemString(call, "name"), itemString(call, "arguments")
			var result string
			if cancelled {
				result = `{"ok":false,"error":{"code":"RUN_STOPPED","message":"运行已由用户停止，工具未执行"}}`
			} else if name == "call_agent" {
				result = a.callAgent(ctx, store, agent, arguments)
			} else {
				result = a.executeTool(ctx, store.id, name, arguments)
			}
			id := itemString(call, "call_id")
			if id == "" {
				id = itemString(call, "id")
			}
			item := inputItem{"type": "function_call_output", "call_id": id, "output": result}
			items = append(items, item)
			if err := appendAgentEvent(ctx, store, "tool_output", map[string]any{"tool_name": name, "arguments": arguments, "duration_seconds": time.Since(started).Seconds(), "cancelled": cancelled, "item": item}); err != nil {
				return "", err
			}
			if err := saveAgentContext(ctx, store, items, nil); err != nil {
				return "", err
			}
		}
	}
}

func modelResponse(ctx context.Context, client openai.Client, params responses.ResponseNewParams, items any, streaming bool, store *runStore) (*responses.Response, error) {
	for attempt := 1; attempt <= 5; attempt++ {
		response, err := requestResponse(ctx, client, params, items, streaming, store)
		if err == nil {
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableModelError(err) || attempt == 5 {
			return nil, err
		}
		delay := time.Duration(min(1<<(attempt-1), 8)) * time.Second
		if store != nil {
			if err := appendAgentEvent(ctx, store, "llm_retry", map[string]any{"attempt": attempt, "next_attempt": attempt + 1, "delay_seconds": delay.Seconds(), "error": err.Error()}); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, errors.New("模型重试循环未正常结束")
}

func retryableModelError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 408 || apiErr.StatusCode == 409 || apiErr.StatusCode == 425 || apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func requestResponse(ctx context.Context, client openai.Client, params responses.ResponseNewParams, input any, streaming bool, store *runStore) (*responses.Response, error) {
	// Preserve historical and multimodal items without losing SDK union variants.
	options := []option.RequestOption{option.WithJSONSet("input", input)}
	if !streaming {
		response, err := client.Responses.New(ctx, params, options...)
		if err != nil {
			return nil, err
		}
		return validateResponse(response)
	}
	stream := client.Responses.NewStreaming(ctx, params, options...)
	defer stream.Close()
	var completed *responses.Response
	var pending strings.Builder
	lastFlush := time.Now()
	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		if store != nil {
			if err := appendAgentEvent(ctx, store, "model_output_delta", map[string]any{"delta": pending.String()}); err != nil {
				return err
			}
		}
		pending.Reset()
		lastFlush = time.Now()
		return nil
	}
	for stream.Next() {
		event := stream.Current()
		if err := ctx.Err(); err != nil {
			_ = flush()
			return nil, err
		}
		switch event.Type {
		case "response.output_text.delta":
			pending.WriteString(event.Delta)
			if pending.Len() >= 256 || time.Since(lastFlush) >= 200*time.Millisecond {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		case "response.completed":
			response := event.Response
			completed = &response
		case "error", "response.failed", "response.incomplete":
			if err := flush(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("模型响应未完成: %s %s", event.Type, event.Message)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, errors.New("流式响应结束，但未收到 response.completed 事件")
	}
	return validateResponse(completed)
}

func validateResponse(response *responses.Response) (*responses.Response, error) {
	if response == nil || response.Status == "failed" || response.Status == "incomplete" {
		return nil, errors.New("模型未返回完整响应")
	}
	return response, nil
}

func estimateTokens(items []inputItem) int {
	data, _ := json.Marshal(items)
	return max(1, (utf8.RuneCount(data)+1)/2)
}

func recentStart(items []inputItem, keep int) int {
	start := max(0, len(items)-keep)
	for start > 0 && (itemString(items[start], "type") == "function_call" || itemString(items[start], "type") == "function_call_output") {
		start--
	}
	return start
}

func (a *App) compressContext(ctx context.Context, client openai.Client, store *runStore, items []inputItem, cfg RuntimeConfig) ([]inputItem, error) {
	start := recentStart(items, cfg.Context.KeepRecentItems)
	var summary map[string]any
	var err error
	if start > 0 {
		history := make([]string, 0, start)
		for _, item := range items[:start] {
			data, marshalErr := json.Marshal(item)
			if marshalErr != nil {
				return nil, marshalErr
			}
			runes := []rune(string(data))
			if len(runes) > 6000 {
				runes = append(append(runes[:3000:3000], []rune("...[中间内容已截断]...")...), runes[len(runes)-3000:]...)
			}
			history = append(history, string(runes))
		}
		prompt := "你是上下文压缩器，不是任务执行者。保留任务目标、已确认决定、完成和未完成工作、文件路径、错误及最近用户意图。只返回 JSON 对象，字段为 goal、decisions、completed、pending、artifacts、errors、uncertain、latest_user_intent。不编造信息。\n" + strings.Join(history, "\n")
		compressionCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Context.TimeoutSeconds*float64(time.Second)))
		response, requestErr := modelResponse(compressionCtx, client, responses.ResponseNewParams{Model: cfg.LLM.Main.Model, MaxOutputTokens: openai.Int(int64(cfg.Context.SummaryMaxTokens)), Temperature: openai.Float(0.1)}, prompt, false, nil)
		cancel()
		err = requestErr
		if err == nil {
			text := strings.TrimSpace(response.OutputText())
			if strings.HasPrefix(text, "```") {
				if i := strings.IndexByte(text, '\n'); i >= 0 {
					text = text[i+1:]
				}
				text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
			}
			err = json.Unmarshal([]byte(text), &summary)
			if err == nil && summary == nil {
				err = errors.New("压缩结果必须是 JSON 对象")
			}
		}
	} else {
		err = errors.New("没有可压缩的旧上下文")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		slog.Warn("上下文压缩失败，保守截断", "run_id", store.id, "error", err)
		start = recentStart(items, max(1, cfg.Context.KeepRecentItems/2))
		summary = map[string]any{"goal": "历史压缩失败，以最近上下文和运行文件为准", "errors": []string{"模型上下文压缩失败，已执行保守截断"}, "pending": []string{"检查运行记录中的未完成任务"}}
	}
	for _, field := range []string{"goal", "decisions", "completed", "pending", "artifacts", "errors", "uncertain", "latest_user_intent"} {
		if _, ok := summary[field]; !ok {
			summary[field] = []string{}
			if field == "goal" || field == "latest_user_intent" {
				summary[field] = ""
			}
		}
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	compressed := append([]inputItem{{"role": "assistant", "content": "[历史上下文摘要]\n" + string(data)}}, items[start:]...)
	if err := appendAgentEvent(ctx, store, "context_compressed", map[string]any{"summary": summary, "input_items": compressed, "removed_items": start, "tokens_before": estimateTokens(items), "tokens_after": estimateTokens(compressed)}); err != nil {
		return nil, err
	}
	return compressed, saveAgentContext(ctx, store, compressed, summary)
}

func (a *App) prepareUserItem(ctx context.Context, id, text string, attachments []string) (inputItem, error) {
	content := []inputItem{{"type": "input_text", "text": text}}
	visionContent := []inputItem{{"type": "input_text", "text": "逐张读取图片，按文件名分组抄录文字并描述界面布局、物体和状态。看不清的内容明确标注，不臆测。只返回纯文本。"}}
	for _, name := range attachments {
		path, err := workspace.ResolveWithin(a.runRoot("resources", id), name, false)
		if err != nil {
			return nil, err
		}
		mime := workspace.MIMEType(path)
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" && mime != "image/gif" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		image := inputItem{"type": "input_image", "image_url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), "detail": "auto"}
		content = append(content, image)
		visionContent = append(visionContent, inputItem{"type": "input_text", "text": "图片文件名：" + name}, image)
	}
	if len(content) == 1 {
		return inputItem{"role": "user", "content": text}, nil
	}
	vision := a.config.get().LLM.Vision
	if vision.Enabled {
		client, err := newClient(vision.ProviderConfig)
		if err == nil {
			var response *responses.Response
			response, err = modelResponse(ctx, client, responses.ResponseNewParams{Model: vision.Model, Temperature: openai.Float(0.1)}, []inputItem{{"role": "user", "content": visionContent}}, false, nil)
			if err == nil && strings.TrimSpace(response.OutputText()) != "" {
				return inputItem{"role": "user", "content": text + "\n\n[图片视觉描述]\n" + response.OutputText()}, nil
			}
			if err == nil {
				err = errors.New("视觉模型没有返回文字描述")
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn("视觉模型失败，回退主模型多模态输入", "run_id", id, "error", err)
	}
	return inputItem{"role": "user", "content": content}, nil
}
