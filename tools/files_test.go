package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webweave/workspace"
)

func testExecutor(t *testing.T) *Executor {
	t.Helper()
	root := t.TempDir()
	for _, id := range []string{"file-run", "other-run", "download-run"} {
		if err := workspace.PrepareDirectories(root, id); err != nil {
			t.Fatal(err)
		}
	}
	return NewExecutor(root, nil, nil)
}

func TestPathIsolation(t *testing.T) {
	app := testExecutor(t)
	id := "file-run"
	text := "中文文件"
	result := app.Execute(context.Background(), id, "create_file", `{"file_path":"nested/index.txt","content":"中文文件","timeout":2}`, Config{Tools: map[string]Settings{"create_file": {Enabled: true, RunScoped: true}}, DefaultSeconds: 120, MaxSeconds: 1800})
	if strings.Contains(result, "失败") {
		t.Fatal(result)
	}
	read, err := app.readFile(id, Args{FilePath: "result/nested/index.txt"})
	if err != nil || read.(map[string]any)["content"] != text {
		t.Fatalf("scoped read: %+v %v", read, err)
	}
	for _, raw := range []string{"../escape", filepath.Join(app.runRoot("result", "other-run"), "secret.txt")} {
		if _, err := app.createFile(context.Background(), id, Args{FilePath: raw, Content: &text}); err == nil {
			t.Fatal("cross-run write accepted")
		}
	}
	if _, err := app.readFile(id, Args{FilePath: filepath.Join(app.root, "config.json")}); err == nil {
		t.Fatal("config file exposed to file tool")
	}
	outside := t.TempDir()
	link := filepath.Join(app.runRoot("result", id), "escape-link")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := app.createFile(context.Background(), id, Args{FilePath: "escape-link/out.txt", Content: &text}); err == nil {
			t.Fatal("symlink escape accepted")
		}
	} else {
		t.Logf("symlink test unavailable on this host: %v", err)
	}
	for _, raw := range []string{`[]`, `{"timeout":0}`, `{"max_chars":"many"}`} {
		output := app.Execute(context.Background(), id, "file_read", raw, Config{Tools: map[string]Settings{"file_read": {Enabled: true, RunScoped: true}}, DefaultSeconds: 120, MaxSeconds: 1800})
		if !strings.Contains(output, "失败") && !strings.Contains(output, "必须") {
			t.Fatal("invalid tool arguments accepted")
		}
	}
}
