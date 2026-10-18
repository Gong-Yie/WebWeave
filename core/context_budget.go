package core

import (
	"math"
	"net/url"
	"strings"
)

// Defaults apply only to documented models on the official endpoint.
func modelContextWindow(provider ProviderConfig) int {
	window := provider.ContextWindow
	endpoint, _ := url.Parse(provider.BaseURL)
	if window == 0 && endpoint != nil && endpoint.Hostname() == "api.deepseek.com" {
		switch provider.Model {
		case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-pro":
			window = 1000000
		}
	}
	return window
}

func modelContextBudget(provider ProviderConfig, fallback int) (input, output int) {
	window := modelContextWindow(provider)
	output = provider.MaxOutputTokens
	if window == 0 {
		input = fallback
	} else {
		if output == 0 {
			output = min(65536, max(1, window/8))
		}
		input = max(1, window-output)
	}
	if provider.InputBudget > 0 {
		if window == 0 {
			input = provider.InputBudget
		} else {
			input = min(input, provider.InputBudget)
		}
	}
	return input, output
}

type taskTokenUsage struct {
	Provider  string `json:"provider"`
	Estimated int    `json:"estimated"`
	Actual    int    `json:"actual"`
}

func providerIdentity(provider ProviderConfig) string {
	return strings.TrimRight(provider.BaseURL, "/") + "/" + provider.Model
}

func calibratedTokens(raw int, provider ProviderConfig, usage taskTokenUsage) int {
	if usage.Provider != providerIdentity(provider) || usage.Estimated <= 0 || usage.Actual <= 0 {
		return raw
	}
	// Preserve a margin and damp changes in the mixture of code and prose.
	ratio := max(0.5, min(2, float64(usage.Actual)/float64(usage.Estimated)*1.15))
	known := min(raw, usage.Estimated)
	return max(1, int(math.Ceil(float64(known)*ratio))+max(0, raw-known))
}
