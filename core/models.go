package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"webweave/workspace"
)

var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func validHeaderValue(value string) bool {
	for i := range value {
		if (value[i] < ' ' && value[i] != '\t') || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func validateProvider(provider *ProviderConfig, requireModel bool) error {
	provider.Model = strings.TrimSpace(provider.Model)
	provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
	provider.APIKey = strings.TrimSpace(provider.APIKey)
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Base URL 必须是无账号、查询参数及片段的 HTTP 或 HTTPS 地址")
	}
	if requireModel && provider.Model == "" {
		return errors.New("模型名称不能为空")
	}
	for key, value := range provider.Headers {
		if !headerName.MatchString(key) || !validHeaderValue(value) {
			return errors.New("自定义 Header 名称或值无效")
		}
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") {
			return errors.New("不能自定义 Host、Content-Length 或 Transfer-Encoding")
		}
	}
	seen := map[string]bool{}
	for _, effort := range provider.ReasoningEfforts {
		if !slices.Contains(reasoningEfforts, effort) || seen[effort] {
			return errors.New("思考强度档位无效或重复")
		}
		seen[effort] = true
	}
	if provider.ReasoningEffort != "" && !seen[provider.ReasoningEffort] {
		return errors.New("默认思考强度必须是该模型支持的档位")
	}
	return nil
}

func findModel(models []ModelConfig, id string) (ModelConfig, error) {
	for _, model := range models {
		if model.ID == id {
			return model, nil
		}
	}
	return ModelConfig{}, fmt.Errorf("模型配置不存在: %s", id)
}

func validateModelConfig(cfg *RuntimeConfig) error {
	seen := map[string]bool{}
	for i := range cfg.LLM.Models {
		model := &cfg.LLM.Models[i]
		if !workspace.ValidName(model.ID) || seen[model.ID] {
			return errors.New("模型配置 ID 无效或重复")
		}
		seen[model.ID] = true
		if err := validateProvider(&model.ProviderConfig, true); err != nil {
			return err
		}
	}
	if len(cfg.LLM.Models) > 0 && cfg.LLM.MainModelID == "" {
		return errors.New("请选择主模型")
	}
	if cfg.LLM.MainModelID != "" {
		model, err := findModel(cfg.LLM.Models, cfg.LLM.MainModelID)
		if err != nil {
			return err
		}
		cfg.LLM.Main = model.ProviderConfig
	}
	if cfg.LLM.VisionModelID != "" {
		model, err := findModel(cfg.LLM.Models, cfg.LLM.VisionModelID)
		if err != nil {
			return err
		}
		cfg.LLM.Vision.ProviderConfig = model.ProviderConfig
	}
	if err := validateProvider(&cfg.LLM.Main, true); err != nil {
		return err
	}
	if cfg.LLM.Vision.Enabled {
		if err := validateProvider(&cfg.LLM.Vision.ProviderConfig, true); err != nil {
			return err
		}
	}
	for _, agent := range cfg.Agents {
		if agent.ModelID != "" {
			if _, err := findModel(cfg.LLM.Models, agent.ModelID); err != nil {
				return fmt.Errorf("%s: %w", agent.Name, err)
			}
		}
	}
	return nil
}

func providerForAgent(cfg RuntimeConfig, agent AgentConfig) (ProviderConfig, error) {
	if agent.ModelID == "" || agent.ID == "main" {
		return cfg.LLM.Main, nil
	}
	model, err := findModel(cfg.LLM.Models, agent.ModelID)
	return model.ProviderConfig, err
}

func redactProvider(provider ProviderConfig) ProviderConfig {
	if provider.APIKey != "" {
		provider.APIKey = "********"
	}
	if len(provider.Headers) > 0 {
		headers := make(map[string]string, len(provider.Headers))
		for key := range provider.Headers {
			headers[key] = "********"
		}
		provider.Headers = headers
	}
	return provider
}

func restoreProviderSecrets(provider *ProviderConfig, previous ProviderConfig) error {
	if provider.APIKey == "********" && previous.APIKey == "" {
		return errors.New("未找到该模型已保存的 API Key，请重新填写")
	}
	if provider.APIKey == "" || provider.APIKey == "********" {
		provider.APIKey = previous.APIKey
	}
	for key, value := range provider.Headers {
		if value != "********" {
			continue
		}
		found := false
		for oldKey, oldValue := range previous.Headers {
			if strings.EqualFold(oldKey, key) {
				provider.Headers[key], found = oldValue, true
				break
			}
		}
		if !found {
			return errors.New("未找到已保存的 Header 值，请重新填写")
		}
	}
	return nil
}

// Resolve masked values by stable model ID, never by position in the model list.
func preserveModelSecrets(data []byte, previous RuntimeConfig) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	var llm map[string]json.RawMessage
	if raw, ok := object["llm"]; !ok {
		return data, nil
	} else if err := json.Unmarshal(raw, &llm); err != nil {
		return nil, err
	}
	if llm == nil {
		return data, nil
	}
	var models []ModelConfig
	if raw, ok := llm["models"]; ok {
		if err := json.Unmarshal(raw, &models); err != nil {
			return nil, err
		}
		for i := range models {
			old, err := findModel(previous.LLM.Models, models[i].ID)
			if err != nil {
				if models[i].ID == "legacy-main" {
					old.ProviderConfig = previous.LLM.Main
				}
				if models[i].ID == "legacy-vision" {
					old.ProviderConfig = previous.LLM.Vision.ProviderConfig
				}
			}
			if models[i].CredentialSourceID != "" {
				source, err := findModel(previous.LLM.Models, models[i].CredentialSourceID)
				if err != nil {
					if models[i].CredentialSourceID == "legacy-main" {
						source.ProviderConfig = previous.LLM.Main
					}
					if models[i].CredentialSourceID == "legacy-vision" {
						source.ProviderConfig = previous.LLM.Vision.ProviderConfig
					}
				}
				if strings.TrimRight(source.BaseURL, "/") != strings.TrimRight(models[i].BaseURL, "/") {
					return nil, errors.New("导入模型的凭据来源与 Base URL 不一致")
				}
				old = source
			}
			if err := restoreProviderSecrets(&models[i].ProviderConfig, old.ProviderConfig); err != nil {
				return nil, err
			}
			if models[i].APIKey == "********" {
				return nil, errors.New("新模型必须填写 API Key 或使用已保存的同接口凭据")
			}
			models[i].CredentialSourceID = ""
		}
	} else if len(previous.LLM.Models) > 0 {
		models = append([]ModelConfig(nil), previous.LLM.Models...)
		// Preserve the model list when an older settings client edits the main role.
		for role, selected := range map[string]string{"main": previous.LLM.MainModelID, "vision": previous.LLM.VisionModelID} {
			if raw, ok := llm[role]; ok && string(raw) != "null" {
				for i := range models {
					if models[i].ID != selected {
						continue
					}
					updated := models[i].ProviderConfig
					updated.Headers = nil
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						return nil, err
					}
					if _, ok := fields["headers"]; !ok {
						updated.Headers = models[i].Headers
					}
					if err := json.Unmarshal(raw, &updated); err != nil {
						return nil, err
					}
					if err := restoreProviderSecrets(&updated, models[i].ProviderConfig); err != nil {
						return nil, err
					}
					models[i].ProviderConfig = updated
				}
			}
		}
	}
	if models != nil {
		llm["models"], _ = json.Marshal(models)
	}
	for key, id := range map[string]string{"main_model_id": previous.LLM.MainModelID, "vision_model_id": previous.LLM.VisionModelID} {
		if _, ok := llm[key]; !ok && id != "" {
			llm[key], _ = json.Marshal(id)
		}
	}
	for key, old := range map[string]ProviderConfig{"main": previous.LLM.Main, "vision": previous.LLM.Vision.ProviderConfig} {
		var selected string
		if raw, ok := llm[key+"_model_id"]; ok {
			if err := json.Unmarshal(raw, &selected); err != nil {
				return nil, err
			}
		}
		if selected != "" {
			if model, err := findModel(models, selected); err == nil {
				old = model.ProviderConfig
			}
		}
		if raw, ok := llm[key]; ok && string(raw) != "null" {
			var provider ProviderConfig
			if err := json.Unmarshal(raw, &provider); err != nil {
				return nil, err
			}
			if err := restoreProviderSecrets(&provider, old); err != nil {
				return nil, err
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, err
			}
			fields["api_key"], _ = json.Marshal(provider.APIKey)
			if provider.Headers != nil {
				fields["headers"], _ = json.Marshal(provider.Headers)
			}
			llm[key], _ = json.Marshal(fields)
		}
	}
	object["llm"], _ = json.Marshal(llm)
	return json.Marshal(object)
}

func (a *App) discoverModels(r *http.Request) (any, int, error) {
	var request struct{ ModelConfig }
	if err := readJSONBody(r, &request); err != nil {
		return nil, 0, err
	}
	provider := request.ProviderConfig
	if err := validateProvider(&provider, false); err != nil {
		return nil, 0, apiError{400, "INVALID_PROVIDER", err.Error()}
	}
	needsSaved := provider.APIKey == "" || provider.APIKey == "********"
	for _, value := range provider.Headers {
		needsSaved = needsSaved || value == "********"
	}
	if needsSaved {
		previous, err := findModel(a.config.get().LLM.Models, request.ID)
		if err != nil {
			cfg := a.config.get()
			if request.ID == "legacy-main" {
				previous.ProviderConfig = cfg.LLM.Main
			}
			if request.ID == "legacy-vision" {
				previous.ProviderConfig = cfg.LLM.Vision.ProviderConfig
			}
		}
		if strings.TrimRight(previous.BaseURL, "/") != provider.BaseURL {
			return nil, 0, apiError{400, "API_KEY_REQUIRED", "地址已更改或未保存，请重新填写 API Key"}
		}
		if err := restoreProviderSecrets(&provider, previous.ProviderConfig); err != nil {
			return nil, 0, apiError{400, "SAVED_CREDENTIALS_MISSING", err.Error()}
		}
	}
	client, err := newClient(provider)
	if err != nil {
		return nil, 0, apiError{400, "API_KEY_REQUIRED", "请填写 API Key"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var response *http.Response
	page, err := client.Models.List(ctx, option.WithResponseInto(&response))
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			return nil, 0, apiError{502, "MODEL_DISCOVERY_FAILED", fmt.Sprintf("模型列表请求失败（HTTP %d），请检查 Base URL、API Key 和 Headers", apiErr.StatusCode)}
		}
		return nil, 0, apiError{502, "MODEL_DISCOVERY_FAILED", "无法读取模型列表，请检查接口地址和网络连接"}
	}
	// The SDK treats 3xx as successful; a redirected model list is not a result.
	if response == nil || response.StatusCode != http.StatusOK {
		return nil, 0, apiError{502, "MODEL_DISCOVERY_FAILED", "模型列表接口返回了重定向或非标准响应，请检查 Base URL"}
	}
	ids, seen := []string{}, map[string]bool{}
	for _, model := range page.Data {
		if model.ID != "" && !seen[model.ID] {
			ids = append(ids, model.ID)
			seen[model.ID] = true
		}
	}
	slices.Sort(ids)
	return map[string]any{"ok": true, "models": ids}, 200, nil
}
