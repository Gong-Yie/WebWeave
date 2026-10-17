package core

import (
	"context"
	"encoding/json"

	"webweave/tools"
)

func (a *App) executeTool(ctx context.Context, id, name, raw string) string {
	if execution, ok := ctx.Value(agentExecutionKey{}).(agentExecution); ok && (execution.readOnly || execution.agent.ID == "audit") && !readOnlyToolAllowed(name, raw) {
		return `{"ok":false,"error":{"code":"AGENT_READ_ONLY","message":"本次任务只读，该操作已拒绝；请向主 Agent 返回所需修改或验证要求"}}`
	}
	cfg := a.config.get()
	return a.tools.Execute(ctx, id, name, raw, tools.Config{Tools: cfg.Tools, DefaultSeconds: cfg.ToolTimeout.DefaultSeconds, MaxSeconds: cfg.ToolTimeout.MaxSeconds})
}

func readOnlyToolAllowed(name, raw string) bool {
	switch name {
	case "list_skills", "read_skill", "file_read", "read_context_segment":
		return true
	case "web_search", "web_instance":
		var args tools.Args
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return false
		}
		return name == "web_search" && args.DownloadURL == "" || name == "web_instance" && args.Action == "status"
	default:
		return false
	}
}
