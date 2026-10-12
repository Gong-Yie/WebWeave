package core

import (
	"context"

	"webweave/tools"
)

func (a *App) executeTool(ctx context.Context, id, name, raw string) string {
	cfg := a.config.get()
	return a.tools.Execute(ctx, id, name, raw, tools.Config{Tools: cfg.Tools, DefaultSeconds: cfg.ToolTimeout.DefaultSeconds, MaxSeconds: cfg.ToolTimeout.MaxSeconds})
}
