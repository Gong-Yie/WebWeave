package core

import (
	"encoding/json"
	"testing"

	"webweave/workspace"
)

func TestContextConfigurationCompatibility(t *testing.T) {
	for _, raw := range []string{`{"context":{"summary_input_tokens":0}}`, `{"context":{"summary_model_id":"missing"}}`, `{"context":{"local_compression":{"enabled":true,"device":"invalid"}}}`, `{"context":{"local_compression":{"rate":0}}}`} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid context configuration accepted: %s", raw)
		}
	}
	app := testApp(t)
	cfg := defaultConfig()
	cfg.LLM.Main.APIKey = "saved-key"
	cfg.Context.SummaryInputTokens = 6000
	cfg.Context.LocalCompression.Rate = 0.8
	if err := workspace.WriteJSONAtomic(app.config.path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(redactedConfig(cfg))
	var legacy map[string]json.RawMessage
	_ = json.Unmarshal(data, &legacy)
	legacy["context"] = json.RawMessage(`{"enabled":true,"max_input_tokens":32768,"compression_trigger_ratio":0.8,"keep_recent_items":12,"summary_max_tokens":4096,"compression_timeout_seconds":120}`)
	data, _ = json.Marshal(legacy)
	saved, err := app.config.save(data)
	if err != nil || saved.Context.SummaryInputTokens != 6000 || saved.Context.LocalCompression.Rate != 0.8 {
		t.Fatal("old client erased context configuration", err)
	}
	if !saved.Tools["read_context_segment"].Enabled || !saved.Tools["read_context_segment"].RunScoped {
		t.Fatal("original-read tool not enabled by default")
	}
}
