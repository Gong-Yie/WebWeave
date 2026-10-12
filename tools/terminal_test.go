package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTerminalExecution(t *testing.T) {
	app := testExecutor(t)
	value, err := app.terminalRun(context.Background(), "file-run", Args{Command: "Write-Output '中文输出'"})
	if err != nil || !strings.Contains(value.(map[string]any)["stdout"].(string), "中文输出") {
		t.Fatalf("terminal UTF-8: %+v %v", value, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = app.terminalRun(ctx, "file-run", Args{Command: "Start-Sleep -Seconds 20"})
	cancel()
	if err == nil {
		t.Fatal("terminal did not time out")
	}
}
