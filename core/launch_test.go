package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webweave/tools"
	"webweave/workspace"
)

func TestLaunchFiles(t *testing.T) {
	for _, source := range []string{"agent-config", "legacy-instance", "no-project"} {
		t.Run(source, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "launch-files")
			root := app.runRoot("result", run.store.id)
			cwd := filepath.Join(root, "site files")
			if err := os.MkdirAll(cwd, 0755); err != nil {
				t.Fatal(err)
			}
			want := launchConfig{Command: "echo {host} {port}", Cwd: "site files", Port: 1234, HealthPath: "/index.html"}
			switch source {
			case "agent-config":
				if err := workspace.WriteJSONAtomic(filepath.Join(root, launchConfigName), want); err != nil {
					t.Fatal(err)
				}
			case "legacy-instance":
				if err := run.store.saveInstance(&instanceRecord{Command: want.Command, Cwd: cwd, RequestedPort: want.Port, HealthPath: want.HealthPath}); err != nil {
					t.Fatal(err)
				}
			case "no-project":
				want = launchConfig{Cwd: ".", HealthPath: "/"}
			}
			if err := app.ensureLaunchFiles(run); err != nil {
				t.Fatal(err)
			}
			var got launchConfig
			if err := readJSONFile(filepath.Join(root, launchConfigName), &got); err != nil || got != want {
				t.Fatalf("launch parameters: %+v, want %+v; %v", got, want, err)
			}
			if info, err := os.Stat(filepath.Join(root, launchScriptName)); err != nil || info.Size() == 0 {
				t.Fatalf("launch script missing: %v", err)
			}
		})
	}
}

func TestLaunchRejectsInvalidConfig(t *testing.T) {
	for name, config := range map[string]launchConfig{
		"missing-command": {Cwd: ".", HealthPath: "/"},
		"missing-host":    {Command: "echo {port}", Cwd: ".", HealthPath: "/"},
		"outside-result":  {Command: "echo {host} {port}", Cwd: "..", HealthPath: "/"},
		"invalid-port":    {Command: "echo {host} {port}", Cwd: ".", Port: 65536, HealthPath: "/"},
		"external-health": {Command: "echo {host} {port}", Cwd: ".", HealthPath: "//example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "invalid-launch")
			if err := workspace.WriteJSONAtomic(filepath.Join(app.runRoot("result", run.store.id), launchConfigName), config); err != nil {
				t.Fatal(err)
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/invalid-launch/instance", map[string]any{"action": "start"}), 409)
			if len(run.processes) != 0 {
				t.Fatal("invalid startup configuration launched a process")
			}
		})
	}
}

func TestLaunchFilesOnRunEnd(t *testing.T) {
	for _, status := range []string{"completed", "failed", "stopped"} {
		t.Run(status, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "launch-completion")
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				if status == "stopped" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				close(started)
				switch status {
				case "completed":
					serveModelResponse(w, false, []inputItem{messageOutput("完成")})
				case "failed":
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"message":"synthetic failure","type":"invalid_request_error"}}`)
				case "stopped":
					<-r.Context().Done()
				}
			}))
			t.Cleanup(server.Close)
			useProvider(t, app, server.URL, status == "stopped")
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/launch-completion/messages", map[string]any{"content": "测试启动文件"}), 202)
			done := jobDone(t, run)
			if status == "stopped" {
				awaitJob(t, started)
				if _, err := app.stopRun(run.store.id); err != nil {
					t.Fatal(err)
				}
			}
			awaitJob(t, done)
			state, err := run.store.readState()
			if err != nil || state.Status != status {
				t.Fatalf("status = %s, want %s; %v", state.Status, status, err)
			}
			for _, name := range []string{launchConfigName, launchScriptName} {
				if _, err := os.Stat(filepath.Join(app.runRoot("result", run.store.id), name)); err != nil {
					t.Fatalf("%s was not generated on %s: %v", name, status, err)
				}
			}
		})
	}
}

func TestLaunchScript(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "launch-script")
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "& '" + strings.ReplaceAll(program, "'", "''") + "' '-test.run=TestInstanceHelper' -- {host} {port}"
	if err := app.writeLaunchFiles(run.store.id, launchConfig{Command: command, Cwd: ".", HealthPath: "/"}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cmd, err := tools.ShellCommand(fmt.Sprintf("& './%s' -Port %d", launchScriptName, port), app.runRoot("result", run.store.id))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WEBWEAVE_INSTANCE_ID=launch-script-test")
	logPath := filepath.Join(t.TempDir(), "instance.log")
	process, err := tools.StartProcess(cmd, logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*second)
		defer cancel()
		if err := process.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*second)
	defer cancel()
	record := &instanceRecord{AppURL: fmt.Sprintf("http://127.0.0.1:%d", port), HealthPath: "/"}
	for !healthy(ctx, record) {
		if ctx.Err() != nil || !process.Alive() {
			data, _ := os.ReadFile(logPath)
			t.Fatalf("standalone launch failed: %v\n%s", ctx.Err(), data)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestLaunchManualAPI(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "manual-launch")
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "& '" + strings.ReplaceAll(program, "'", "''") + "' '-test.run=TestInstanceHelper' -- {host} {port}"
	// A legacy record can be started without asking the model or supplying a command over HTTP.
	if err := run.store.saveInstance(&instanceRecord{InstanceID: "legacy-instance", Revision: 1, Command: command, Cwd: app.runRoot("result", run.store.id), HealthPath: "/", Status: "exited"}); err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/manual-launch/instance"
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "start", "command": command}), 400)
	response := apiRequest(t, app, "POST", path, map[string]any{"action": "start", "timeout": 10})
	assertStatus(t, response, 200)
	var instance instanceRecord
	if err := json.Unmarshal(response.Body.Bytes(), &instance); err != nil || instance.Status != "running" {
		t.Fatalf("manual launch failed: %s; %v", response.Body.String(), err)
	}
	assertStatus(t, apiRequest(t, app, "GET", previewURL(run.store.id), nil), 200)
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "start", "instance_id": instance.InstanceID}), 409)
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "stop"}), 200)
	assertStatus(t, apiRequest(t, app, "POST", path, map[string]any{"action": "start", "timeout": 10}), 200)
}
