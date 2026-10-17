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
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"webweave/workspace"
)

const second = time.Second

var errModelRedirect = errors.New("模型接口返回重定向，请检查 Base URL")

const systemPrompt = `你是一个“网页项目生成型 Agent”，使用完整的 ReAct 工作循环解决用户任务。
理解用户需求，按需读取当前运行 ID 的资源，检索必要资料，把最终文件写入当前运行 ID 的结果目录。
当前运行 ID：{run_id}。
可用工具：list_skills、read_skill、file_read、read_context_segment、web_search、create_file、terminal_run、web_instance。
先分析目标和上下文，再按需调用工具；需要事实、源码或已有产物时先读取文件，需要外部资料时搜索网页。
写文件前确认路径和完整内容；以实际工具返回结果为准。所有生成网站的内容默认使用中文。
每次工具调用必须填写 timeout（秒），默认 {timeout_default} 秒，最大 {timeout_max} 秒。
一次响应可能包含多个工具调用，必须返回所有工具结果后继续下一轮，直到模型不再调用工具。
缺少关键需求时向用户澄清。不得声称未执行的操作已经完成。
执行节奏：先完成核心页面并启动预览，再检查影响使用的缺陷。用户未明确要求多页面时，优先交付完整单页网站，不自行扩大项目范围。
不要用长篇设计文档、重复验证或反复全文读取代替交付。已生成源码以文件为准，修改时读取相关片段；终端检查只输出必要结论和证据。
检查通过后停止该项检查；只修复有证据且影响交付的问题，不为轻微风格问题反复委派审计。
压缩观察仅是预览。需要精确源码、错误或历史事实时，先用 read_context_segment 回读原文；修改源码前读取实际文件。
安全边界：只能读取当前运行的 data/resources/{run_id}、data/result/{run_id}、data/download/{run_id}。
只能通过 create_file 写入当前运行 result 目录，通过 web_search 下载到当前运行 download 目录。
terminal_run 的 cwd 必须在当前运行的 resources、result 或 download 目录内。
Web 服务必须通过 web_instance 启动，不得使用 terminal_run 后台模式代替实例注册。
web_instance 只能从当前运行 result 目录启动，同一运行可同时启动多个独立实例。保存每次返回的 instance_id，查看、停止和重启时必须指定目标 instance_id；status 返回 instances 列表。不同实例有独立端口、日志和预览地址。
每次交付可运行网页前，必须在 result 根目录创建 webweave-start.json，字段为 command（PowerShell 命令，包含 {host} 和 {port}）、cwd（相对于 result 的项目目录）、port（0 自动分配）、health_path（默认 /）。例如 {"command":"python -m http.server {port} --bind {host}","cwd":".","port":0,"health_path":"/"}；使用项目真实可用的启动命令，不要为动态项目套用静态服务器。
系统在每次执行结束时生成 start-webweave.ps1，用户可在实例页手动启动，也可在项目目录用 PowerShell 执行该脚本。启动文件始终放在 result 根目录，不放进只有三个文件的模板子目录。
web_instance start 不传 instance_id 时新开实例，可以只传 action=start 读取启动文件，也可以传包含 {host} 和 {port} 的 command；显式启动参数自动保存到启动文件。port=0 自动分配端口，避免多个实例占用同一固定端口；restart 只重启指定实例并复用其参数。已有实例可用时直接复用，不重复启动。
配置及本地 SKILL 文件会重新读取；Go 工具源码修改后需要重新编译。`

func jsonBytes(value any) ([]byte, error) { return json.Marshal(value) }

func newClient(provider ProviderConfig) (openai.Client, error) {
	if provider.APIKey == "" {
		return openai.Client{}, errors.New("未配置大模型 API Key，请在模型配置页面填写")
	}
	options := []option.RequestOption{option.WithAPIKey(provider.APIKey), option.WithBaseURL(strings.TrimRight(provider.BaseURL, "/") + "/"), option.WithMaxRetries(0), option.WithRequestTimeout(10 * time.Minute), option.WithHTTPClient(&http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errModelRedirect }})}
	for key, value := range provider.Headers {
		options = append(options, option.WithHeader(key, value))
	}
	return openai.NewClient(options...), nil
}

func (a *App) runAgent(ctx context.Context, store *runStore, items []inputItem, agent AgentConfig, delegated bool) (string, error) {
	execution, _ := ctx.Value(agentExecutionKey{}).(agentExecution)
	execution.agent, execution.delegated = agent, delegated
	execution.readOnly = execution.readOnly || agent.ID == "audit"
	ctx = context.WithValue(ctx, agentExecutionKey{}, execution)
	templateSkill := ""
	if agent.ID == "template" {
		document, err := a.skills.Read("template-maker")
		if err != nil {
			return "", fmt.Errorf("加载默认模板制作 Skill: %w", err)
		}
		templateSkill = "\n\n默认 Skill：template-maker\n" + document.Instructions
	}
	items = repairItems(items)
	if err := saveAgentContext(ctx, store, items, nil); err != nil {
		return "", err
	}
	limit := agentRoundLimit(agent.ID)
	var compressionErr error
	localCompressionFailed := false
	for round := 0; ; round++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cfg := a.config.get()
		var err error
		compactCfg := cfg
		if localCompressionFailed {
			compactCfg.Context.LocalCompression.Enabled = false
		}
		items, err = a.compactAgentContext(ctx, store, items, compactCfg)
		if err != nil {
			if !errors.Is(err, errContextCompression) {
				return "", err
			}
			localCompressionFailed = true
		}
		provider, err := providerForAgent(cfg, agent)
		if err != nil {
			return "", err
		}
		effort := execution.effort
		if effort == "" {
			effort = provider.ReasoningEffort
		}
		if effort != "" && !slices.Contains(provider.ReasoningEfforts, effort) {
			return "", errors.New("当前模型不支持所选思考强度，请重新选择")
		}
		client, err := newClient(provider)
		if err != nil {
			return "", err
		}
		params := responses.ResponseNewParams{Model: provider.Model, Instructions: openai.String(agentInstructions(agent, cfg, store.id) + templateSkill), Tools: agentTools(agent, cfg)}
		if execution.readOnly {
			params.Tools = readOnlyAgentTools(params.Tools)
			params.Instructions = openai.String(params.Instructions.Value + "\n本次任务只读：不得修改项目文件、下载文件、执行终端命令或改变实例状态。需要修改或运行检查时返回具体要求交主 Agent 安排。")
		}
		if delegated {
			params.Text.Format.OfJSONSchema = agentResultFormat()
			params.Instructions = openai.String(params.Instructions.Value + "\n\n" + delegatedResultInstructions)
		}
		if len(provider.ReasoningEfforts) == 0 {
			params.Temperature = openai.Float(0.7)
		}
		if effort != "" {
			params.Reasoning.Effort = shared.ReasoningEffort(effort)
		}
		if cfg.Model.ReasoningSummary {
			params.Reasoning.Summary = shared.ReasoningSummaryAuto
		}
		if round == limit {
			if execution.outcome != nil {
				execution.outcome.budgetReached = true
			}
			params.Tools = nil
			params.ToolChoice.OfToolChoiceMode = openai.Opt(responses.ToolChoiceOptionsNone)
			params.Instructions = openai.String(params.Instructions.Value + "\n本次执行已达到工具轮次预算。本轮必须收尾：说明实际完成的文件、验证结果、预览入口和未完成事项，不再调用工具，不得把未完成任务说成已完成。")
			if err := appendAgentEvent(ctx, store, "agent_budget_reached", map[string]any{"tool_rounds": limit}); err != nil {
				return "", err
			}
		}
		if cfg.Context.Enabled && compressionErr == nil && estimateRequestTokens(params, items) >= int(float64(cfg.Context.MaxInputTokens)*cfg.Context.TriggerRatio) {
			cfg.LLM.Main = provider
			items, err = a.compressContext(ctx, client, store, items, cfg)
			if err != nil {
				if !errors.Is(err, errContextCompression) {
					return "", err
				}
				// Keep the history and avoid repeating a failed compression each round.
				compressionErr = err
			}
		}
		inputTokens := estimateRequestTokens(params, items)
		if cfg.Context.Enabled && inputTokens > cfg.Context.MaxInputTokens {
			if compressionErr != nil {
				return "", fmt.Errorf("上下文估算输入 %d Token 超过输入预算 %d，原上下文已保留；调整模型或压缩配置后继续: %w", inputTokens, cfg.Context.MaxInputTokens, compressionErr)
			}
			return "", fmt.Errorf("压缩后上下文估算输入 %d Token 仍超过输入预算 %d，无法继续请求模型", inputTokens, cfg.Context.MaxInputTokens)
		}
		if err := appendAgentEvent(ctx, store, "context_usage", map[string]any{"input_tokens": inputTokens, "estimated": true}); err != nil {
			return "", err
		}
		response, err := modelResponse(ctx, client, params, items, cfg.Model.Stream, store)
		if err != nil {
			return "", err
		}
		if response.Usage.InputTokens > 0 {
			if err := appendAgentEvent(ctx, store, "context_usage", map[string]any{"input_tokens": response.Usage.InputTokens, "estimated": false}); err != nil {
				return "", err
			}
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
		if round == limit && (len(calls) > 0 || strings.TrimSpace(text) == "") {
			return "", fmt.Errorf("%s 已达到 %d 轮工具预算，模型未按要求收尾；已有文件已保留", agent.Name, limit)
		}
		if len(calls) == 0 {
			return text, ctx.Err()
		}
		for _, call := range calls {
			started := time.Now()
			cancelled := ctx.Err() != nil
			name, arguments := itemString(call, "name"), itemString(call, "arguments")
			if !cancelled {
				if err := appendAgentEvent(ctx, store, "tool_started", map[string]any{"tool_name": name, "arguments": arguments}); err != nil {
					return "", err
				}
			}
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
			return response, err
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
	if errors.Is(err, errModelRedirect) {
		return false
	}
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
	var reasoning strings.Builder
	reasoningKey := ""
	flushReasoning := func() error {
		if reasoning.Len() == 0 || store == nil {
			return nil
		}
		err := appendAgentEvent(ctx, store, "model_reasoning_delta", map[string]any{"delta": reasoning.String(), "reasoning_key": reasoningKey})
		reasoning.Reset()
		return err
	}
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
			_ = flushReasoning()
			_ = flush()
			return nil, err
		}
		switch event.Type {
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			key := fmt.Sprintf("%s:%s:%d:%d", event.ItemID, event.Type, event.SummaryIndex, event.ContentIndex)
			if key != reasoningKey {
				if err := flushReasoning(); err != nil {
					return nil, err
				}
				reasoningKey = key
			}
			reasoning.WriteString(event.Delta)
			if reasoning.Len() >= 256 || time.Since(lastFlush) >= 200*time.Millisecond {
				if err := flushReasoning(); err != nil {
					return nil, err
				}
				lastFlush = time.Now()
			}
		case "response.output_text.delta":
			if err := flushReasoning(); err != nil {
				return nil, err
			}
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
			if err := flushReasoning(); err != nil {
				return nil, err
			}
			if err := flush(); err != nil {
				return nil, err
			}
			if event.Type != "error" {
				response := event.Response
				response.Status = responses.ResponseStatus(strings.TrimPrefix(event.Type, "response."))
				return validateResponse(&response)
			}
			return nil, fmt.Errorf("模型响应未完成: %s %s", event.Type, event.Message)
		}
	}
	if err := flushReasoning(); err != nil {
		return nil, err
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
	if response == nil {
		return nil, errors.New("模型返回空响应")
	}
	if response.Status == "failed" || response.Status == "incomplete" {
		return response, fmt.Errorf("模型响应未完成: status=%s reason=%s error_code=%s output_tokens=%d reasoning_tokens=%d", response.Status, response.IncompleteDetails.Reason, response.Error.Code, response.Usage.OutputTokens, response.Usage.OutputTokensDetails.ReasoningTokens)
	}
	return response, nil
}

func estimateTokens(items []inputItem) int {
	data, _ := json.Marshal(items)
	return max(1, (utf8.RuneCount(data)+1)/2)
}

// Includes the instructions and tool schemas sent alongside conversation items.
func estimateRequestTokens(params responses.ResponseNewParams, items []inputItem) int {
	data, _ := json.Marshal(params.Tools)
	format, _ := json.Marshal(params.Text)
	return estimateTokens(items) + (utf8.RuneCountInString(params.Instructions.Value)+utf8.RuneCount(data)+utf8.RuneCount(format)+1)/2
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
			params := responses.ResponseNewParams{Model: vision.Model}
			if len(vision.ReasoningEfforts) == 0 {
				params.Temperature = openai.Float(0.1)
			}
			params.Reasoning.Effort = shared.ReasoningEffort(vision.ReasoningEffort)
			response, err = modelResponse(ctx, client, params, []inputItem{{"role": "user", "content": visionContent}}, false, nil)
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
