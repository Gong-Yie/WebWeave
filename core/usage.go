package core

type contextUsage struct {
	InputTokens int    `json:"input_tokens"`
	Threshold   int    `json:"threshold"`
	Estimated   bool   `json:"estimated"`
	Enabled     bool   `json:"compression_enabled"`
	AgentName   string `json:"agent_name"`
	Delegated   bool   `json:"delegated"`
}

func (a *App) runContextUsage(events []runEvent) contextUsage {
	cfg := a.config.get().Context
	usage := contextUsage{Threshold: max(1, int(float64(cfg.MaxInputTokens)*cfg.TriggerRatio)), Enabled: cfg.Enabled, Estimated: true}
	for _, event := range events {
		if event.Type != "context_usage" {
			continue
		}
		if tokens, ok := event.Payload["input_tokens"].(float64); ok {
			usage.InputTokens = int(tokens)
		}
		usage.Estimated, _ = event.Payload["estimated"].(bool)
		usage.AgentName, _ = event.Payload["agent_name"].(string)
		usage.Delegated, _ = event.Payload["delegated"].(bool)
	}
	return usage
}
