package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"webweave/tools"
	"webweave/workspace"
)

type instanceRecord struct {
	InstanceID      string  `json:"instance_id"`
	Revision        int     `json:"revision"`
	Status          string  `json:"status"`
	PID             *int    `json:"pid"`
	ProcessIdentity *string `json:"process_identity"`
	Host            string  `json:"host"`
	Port            int     `json:"port"`
	AppURL          string  `json:"app_url"`
	PreviewURL      string  `json:"preview_url"`
	Cwd             string  `json:"cwd"`
	HealthPath      string  `json:"health_path"`
	LogPath         string  `json:"log_path"`
	StartedAt       string  `json:"started_at"`
	ReadyAt         *string `json:"ready_at"`
	UpdatedAt       string  `json:"updated_at"`
	Command         string  `json:"command"`
	RequestedPort   int     `json:"requested_port"`
	ErrorCode       *string `json:"error_code"`
	ErrorMessage    *string `json:"error_message"`
}

type instanceResult struct {
	*instanceRecord
	ProtocolVersion string     `json:"protocol_version"`
	OK              bool       `json:"ok"`
	Error           *errorBody `json:"error,omitempty"`
}

type instanceError struct {
	code, message string
	record        *instanceRecord
}

func (e instanceError) Error() string { return e.message }

func previewURL(id string) string { return "/preview/" + url.PathEscape(id) + "/" }

func (a *App) instanceAlive(run *runRuntime, record *instanceRecord) bool {
	if record.PID == nil || record.ProcessIdentity == nil {
		return false
	}
	if run.process != nil && run.process.Cmd.Process.Pid == *record.PID {
		return run.process.Alive()
	}
	return *record.ProcessIdentity != "" && tools.ProcessIdentity(*record.PID) == *record.ProcessIdentity
}

func persistInstance(store *runStore, record *instanceRecord) error {
	record.UpdatedAt = workspace.Timestamp()
	return store.saveInstance(record)
}

func healthy(ctx context.Context, record *instanceRecord) bool {
	checkCtx, cancel := context.WithTimeout(ctx, second)
	defer cancel()
	request, err := http.NewRequestWithContext(checkCtx, "GET", record.AppURL+record.HealthPath, nil)
	if err != nil {
		return false
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode < 400
}

func (a *App) instanceAction(ctx context.Context, id string, args tools.Args) (instanceResult, error) {
	run, err := a.runtime(id)
	if err != nil {
		return instanceResult{}, err
	}
	run.instanceMu.Lock()
	defer run.instanceMu.Unlock()
	state, err := run.store.readState()
	if err != nil {
		return instanceResult{}, err
	}
	record := state.Instance
	if args.Action == "" {
		args.Action = "status"
	}
	if args.Action != "start" && (args.Command != "" || args.Cwd != "" || args.Port != nil || args.HealthPath != "") {
		return instanceResult{}, instanceError{"INVALID_ARGUMENT", "该操作不接受启动参数；restart 会复用原启动参数", record}
	}
	if args.Action != "start" && record == nil {
		return instanceResult{}, instanceError{"INSTANCE_NOT_FOUND", "当前运行没有 Web 实例记录", nil}
	}
	if record != nil && (record.Status == "starting" || record.Status == "running" || record.Status == "unhealthy" || record.Status == "stopping") {
		if !a.instanceAlive(run, record) {
			record.Status = "exited"
			code, message := "PROCESS_EXITED", "Web 实例进程已退出"
			record.ErrorCode, record.ErrorMessage = &code, &message
			if err := persistInstance(run.store, record); err != nil {
				return instanceResult{}, err
			}
		}
	}
	switch args.Action {
	case "start":
		record, err = a.startInstance(ctx, run, args, record)
	case "status":
		if a.instanceAlive(run, record) {
			status := "unhealthy"
			if healthy(ctx, record) {
				status = "running"
			}
			if record.Status != status {
				record.Status = status
				if status == "running" {
					record.ErrorCode, record.ErrorMessage = nil, nil
				} else {
					code, message := "HEALTH_CHECK_FAILED", "Web 实例健康检查失败"
					record.ErrorCode, record.ErrorMessage = &code, &message
				}
				err = persistInstance(run.store, record)
			}
		}
	case "stop":
		err = a.stopInstance(ctx, run, record)
	case "restart":
		err = a.stopInstance(ctx, run, record)
		if err == nil {
			port := record.RequestedPort
			record, err = a.startInstance(ctx, run, tools.Args{Command: record.Command, Cwd: record.Cwd, Port: &port, HealthPath: record.HealthPath}, record)
		}
	default:
		err = instanceError{"INVALID_ARGUMENT", "action 必须是 start、status、stop 或 restart", record}
	}
	if err != nil {
		return instanceResult{}, err
	}
	copy := *record
	copy.PreviewURL = previewURL(id)
	result := instanceResult{instanceRecord: &copy, ProtocolVersion: "1.0", OK: true}
	if record.ErrorCode != nil {
		result.Error = &errorBody{*record.ErrorCode, derefString(record.ErrorMessage)}
	}
	return result, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (a *App) startInstance(ctx context.Context, run *runRuntime, args tools.Args, previous *instanceRecord) (*instanceRecord, error) {
	if previous != nil && a.instanceAlive(run, previous) {
		return nil, instanceError{"INSTANCE_ALREADY_RUNNING", "当前运行已有活动 Web 实例", previous}
	}
	if !strings.Contains(args.Command, "{host}") || !strings.Contains(args.Command, "{port}") {
		return nil, instanceError{"INVALID_ARGUMENT", "command 必须同时包含 {host} 和 {port}", previous}
	}
	port := 0
	if args.Port != nil {
		port = *args.Port
	}
	if port < 0 || port > 65535 {
		return nil, instanceError{"INVALID_ARGUMENT", "port 必须在 0 到 65535 之间", previous}
	}
	healthPath := args.HealthPath
	if healthPath == "" {
		healthPath = "/"
	}
	if !strings.HasPrefix(healthPath, "/") || strings.HasPrefix(healthPath, "//") {
		return nil, instanceError{"INVALID_ARGUMENT", "health_path 必须是站内路径", previous}
	}
	cwd, err := workspace.WorkingDirectory(a.root, run.store.id, args.Cwd, true)
	if err != nil {
		return nil, instanceError{"INVALID_ARGUMENT", err.Error(), previous}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, instanceError{"PORT_IN_USE", "端口不可用", previous}
	}
	selected := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	record := &instanceRecord{InstanceID: workspace.NewID(), Revision: 1, Status: "starting", Host: "127.0.0.1", Port: selected, AppURL: fmt.Sprintf("http://127.0.0.1:%d", selected), Cwd: cwd, HealthPath: healthPath, StartedAt: workspace.Timestamp(), Command: args.Command, RequestedPort: port}
	if previous != nil {
		record.InstanceID, record.Revision = previous.InstanceID, previous.Revision+1
	}
	record.PreviewURL = previewURL(run.store.id)
	record.LogPath = filepath.Join(run.store.dir, "instance", strconv.Itoa(record.Revision)+".log")
	if err := persistInstance(run.store, record); err != nil {
		return nil, err
	}
	command := strings.NewReplacer("{host}", record.Host, "{port}", strconv.Itoa(record.Port)).Replace(record.Command)
	cmd, err := tools.ShellCommand(command, cwd)
	if err == nil {
		cmd.Env = append(os.Environ(), "WEBWEAVE_INSTANCE_ID="+record.InstanceID)
		run.process, err = tools.StartProcess(cmd, record.LogPath)
	}
	if err != nil {
		return nil, a.failedInstanceStart(run, record, "PROCESS_EXITED", err)
	}
	pid, identity := run.process.Cmd.Process.Pid, run.process.Identity
	record.PID, record.ProcessIdentity = &pid, &identity
	if err := persistInstance(run.store, record); err != nil {
		return nil, a.failedInstanceStart(run, record, "INTERNAL_ERROR", err)
	}
	if identity == "" {
		return nil, a.failedInstanceStart(run, record, "PROCESS_EXITED", errors.New("无法确认进程身份"))
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !run.process.Alive() {
			return nil, a.failedInstanceStart(run, record, "PROCESS_EXITED", errors.New("Web 实例在就绪前退出"))
		}
		if healthy(ctx, record) {
			now := workspace.Timestamp()
			record.ReadyAt, record.Status = &now, "running"
			if err := persistInstance(run.store, record); err != nil {
				return nil, a.failedInstanceStart(run, record, "INTERNAL_ERROR", err)
			}
			return record, nil
		}
		select {
		case <-ctx.Done():
			return nil, a.failedInstanceStart(run, record, "STARTUP_TIMEOUT", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (a *App) failedInstanceStart(run *runRuntime, record *instanceRecord, code string, cause error) error {
	if run.process != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*second)
		cleanupErr := run.process.Stop(ctx)
		cancel()
		if cleanupErr != nil {
			code = "STOP_FAILED"
			cause = fmt.Errorf("%w; 进程清理失败: %v", cause, cleanupErr)
		}
	}
	record.Status = "failed"
	message := cause.Error()
	record.ErrorCode, record.ErrorMessage = &code, &message
	if err := persistInstance(run.store, record); err != nil {
		return fmt.Errorf("实例启动失败且状态保存失败: %w", err)
	}
	return instanceError{code, message, record}
}

func (a *App) stopInstance(ctx context.Context, run *runRuntime, record *instanceRecord) error {
	if record.Status == "stopped" {
		return nil
	}
	record.Status, record.ErrorCode, record.ErrorMessage = "stopping", nil, nil
	if err := persistInstance(run.store, record); err != nil {
		return err
	}
	if a.instanceAlive(run, record) {
		var err error
		if run.process != nil && run.process.Cmd.Process.Pid == *record.PID {
			err = run.process.Stop(ctx)
		} else {
			err = tools.TerminateProcessTree(ctx, *record.PID)
		}
		if err != nil {
			code, message := "STOP_FAILED", err.Error()
			record.ErrorCode, record.ErrorMessage = &code, &message
			_ = persistInstance(run.store, record)
			return instanceError{code, message, record}
		}
	}
	record.Status, record.PID, record.ProcessIdentity = "stopped", nil, nil
	run.process = nil
	return persistInstance(run.store, record)
}
