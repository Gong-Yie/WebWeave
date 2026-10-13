package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"webweave/tools"
	"webweave/workspace"
)

type ProviderConfig struct {
	Model            string            `json:"model"`
	BaseURL          string            `json:"base_url"`
	APIKey           string            `json:"api_key"`
	Headers          map[string]string `json:"headers,omitempty"`
	ReasoningEfforts []string          `json:"reasoning_efforts,omitempty"`
	ReasoningEffort  string            `json:"reasoning_effort,omitempty"`
}

type ModelConfig struct {
	ID                 string `json:"id"`
	CredentialSourceID string `json:"credential_source_id,omitempty"`
	ProviderConfig
}

type VisionConfig struct {
	ProviderConfig
	Enabled bool `json:"enabled"`
}

type ToolConfig = tools.Settings

type ContextConfig struct {
	Enabled          bool    `json:"enabled"`
	MaxInputTokens   int     `json:"max_input_tokens"`
	TriggerRatio     float64 `json:"compression_trigger_ratio"`
	KeepRecentItems  int     `json:"keep_recent_items"`
	SummaryMaxTokens int     `json:"summary_max_tokens"`
	TimeoutSeconds   float64 `json:"compression_timeout_seconds"`
}

type RuntimeConfig struct {
	LLM struct {
		Main          ProviderConfig `json:"main"`
		Vision        VisionConfig   `json:"vision"`
		Models        []ModelConfig  `json:"models,omitempty"`
		MainModelID   string         `json:"main_model_id,omitempty"`
		VisionModelID string         `json:"vision_model_id,omitempty"`
	} `json:"llm"`
	Model struct {
		Stream           bool `json:"stream"`
		ReasoningSummary bool `json:"reasoning_summary"`
	} `json:"model"`
	Tools       map[string]ToolConfig `json:"tools"`
	ToolTimeout struct {
		DefaultSeconds float64 `json:"default_seconds"`
		MaxSeconds     float64 `json:"max_seconds"`
	} `json:"tool_timeout"`
	Context ContextConfig `json:"context"`
	Agents  []AgentConfig `json:"agents"`
}

func defaultConfig() RuntimeConfig {
	var cfg RuntimeConfig
	cfg.LLM.Main = ProviderConfig{Model: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com"}
	cfg.Model.Stream = true
	cfg.ToolTimeout.DefaultSeconds, cfg.ToolTimeout.MaxSeconds = 120, 1800
	cfg.Context = ContextConfig{true, 32768, 0.8, 12, 2000, 120}
	cfg.Agents = defaultAgents()
	cfg.Tools = map[string]ToolConfig{}
	for _, name := range []string{"list_skills", "read_skill", "web_search", "create_file", "file_read", "terminal_run", "web_instance"} {
		cfg.Tools[name] = ToolConfig{true, name != "list_skills" && name != "read_skill"}
	}
	return cfg
}

func parseConfig(data []byte) (RuntimeConfig, error) {
	cfg := defaultConfig()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return cfg, err
	}
	if object == nil {
		return cfg, errors.New("根配置必须是 JSON 对象")
	}
	// JSON null is not a valid boolean, number, text or configuration section.
	if err := rejectConfigNull(object, ""); err != nil {
		return cfg, err
	}
	if _, ok := object["tools"]; ok {
		cfg.Tools = nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	// Upgrade saved rosters without replacing edited roles or custom agents.
	if _, err := findAgent(cfg.Agents, "template"); err != nil {
		cfg.Agents = append(cfg.Agents, defaultAgents()[4])
	}
	if err := validateAgents(cfg.Agents); err != nil {
		return cfg, err
	}
	if err := validateModelConfig(&cfg); err != nil {
		return cfg, err
	}
	for _, p := range []*ProviderConfig{&cfg.LLM.Main, &cfg.LLM.Vision.ProviderConfig} {
		p.Model, p.BaseURL, p.APIKey = strings.TrimSpace(p.Model), strings.TrimSpace(p.BaseURL), strings.TrimSpace(p.APIKey)
	}
	if cfg.LLM.Main.Model == "" || cfg.LLM.Main.BaseURL == "" {
		return cfg, errors.New("主模型名称和兼容接口地址不能为空")
	}
	if cfg.LLM.Vision.Enabled && (cfg.LLM.Vision.Model == "" || cfg.LLM.Vision.BaseURL == "" || cfg.LLM.Vision.APIKey == "") {
		return cfg, errors.New("启用视觉模型时，model、base_url 和 api_key 不能为空")
	}
	for name := range cfg.Tools {
		if !workspace.ValidName(name) {
			return cfg, errors.New("工具名称无效")
		}
	}
	for _, n := range []float64{cfg.ToolTimeout.DefaultSeconds, cfg.ToolTimeout.MaxSeconds, cfg.Context.TimeoutSeconds, cfg.Context.TriggerRatio} {
		if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return cfg, errors.New("超时和压缩触发比例必须是正数")
		}
	}
	if cfg.ToolTimeout.MaxSeconds < cfg.ToolTimeout.DefaultSeconds {
		return cfg, errors.New("最大超时不能小于默认超时")
	}
	if cfg.Context.TriggerRatio >= 1 || cfg.Context.MaxInputTokens < 1 || cfg.Context.KeepRecentItems < 1 || cfg.Context.SummaryMaxTokens < 1 {
		return cfg, errors.New("上下文参数无效：Token 和保留项目数必须为正整数，触发比例必须小于 1")
	}
	return cfg, nil
}

func rejectConfigNull(object map[string]json.RawMessage, prefix string) error {
	for key, value := range object {
		name := prefix + key
		if string(value) == "null" && name != "llm.vision" {
			return fmt.Errorf("%s 不能为 null", name)
		}
		if len(value) > 0 && value[0] == '{' {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(value, &nested); err != nil {
				return err
			}
			if err := rejectConfigNull(nested, name+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

type configManager struct {
	mu     sync.Mutex
	path   string
	mtime  int64
	loaded bool
	config RuntimeConfig
}

func newConfigManager(root string) *configManager {
	return &configManager{path: filepath.Join(root, "config.json"), config: defaultConfig()}
}

func (m *configManager) get() RuntimeConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadLocked()
	return m.config
}

func (m *configManager) reloadLocked() {
	info, err := os.Stat(m.path)
	mtime := int64(-1)
	if err == nil {
		mtime = info.ModTime().UnixNano()
	}
	if m.loaded && mtime == m.mtime {
		return
	}
	m.loaded, m.mtime = true, mtime
	if errors.Is(err, os.ErrNotExist) {
		m.config = defaultConfig()
		return
	}
	if err != nil {
		slog.Warn("读取配置失败，保留上一份配置", "error", err)
		return
	}
	data, err := os.ReadFile(m.path)
	if err == nil {
		var cfg RuntimeConfig
		cfg, err = parseConfig(data)
		if err == nil {
			m.config = cfg
			return
		}
	}
	slog.Warn("配置热加载失败，保留上一份配置", "error", err)
}

func redactedConfig(cfg RuntimeConfig) RuntimeConfig {
	cfg.LLM.Main = redactProvider(cfg.LLM.Main)
	cfg.LLM.Vision.ProviderConfig = redactProvider(cfg.LLM.Vision.ProviderConfig)
	cfg.LLM.Models = append([]ModelConfig(nil), cfg.LLM.Models...)
	for i := range cfg.LLM.Models {
		cfg.LLM.Models[i].ProviderConfig = redactProvider(cfg.LLM.Models[i].ProviderConfig)
	}
	return cfg
}

func (m *configManager) save(data []byte) (RuntimeConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadLocked()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return RuntimeConfig{}, err
	}
	if object == nil {
		return RuntimeConfig{}, errors.New("根配置必须是 JSON 对象")
	}
	// Older configuration clients must not reset saved custom agents.
	if _, ok := object["agents"]; !ok {
		object["agents"], _ = json.Marshal(m.config.Agents)
		data, _ = json.Marshal(object)
	}
	var llm map[string]json.RawMessage
	if raw, ok := object["llm"]; ok {
		if err := json.Unmarshal(raw, &llm); err != nil {
			return RuntimeConfig{}, err
		}
		for _, name := range []string{"main", "vision"} {
			var provider map[string]json.RawMessage
			if raw, ok := llm[name]; !ok || string(raw) == "null" {
				continue
			} else if err := json.Unmarshal(raw, &provider); err != nil {
				return RuntimeConfig{}, err
			}
			var key string
			if raw, ok := provider["api_key"]; ok {
				if err := json.Unmarshal(raw, &key); err != nil {
					return RuntimeConfig{}, err
				}
			}
			if key == "" || key == "********" {
				previous := m.config.LLM.Main.APIKey
				if name == "vision" {
					previous = m.config.LLM.Vision.APIKey
				}
				provider["api_key"], _ = json.Marshal(previous)
				llm[name], _ = json.Marshal(provider)
			}
		}
		object["llm"], _ = json.Marshal(llm)
		data, _ = json.Marshal(object)
	}
	data, err := preserveModelSecrets(data, m.config)
	if err != nil {
		return RuntimeConfig{}, err
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if err := workspace.WriteJSONAtomic(m.path, cfg); err != nil {
		return RuntimeConfig{}, err
	}
	m.config, m.loaded = cfg, false
	return redactedConfig(cfg), nil
}
