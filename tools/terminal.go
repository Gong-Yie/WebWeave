package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"webweave/workspace"
)

func (e *Executor) terminalRun(ctx context.Context, id string, args Args) (any, error) {
	cwd, err := workspace.WorkingDirectory(e.root, id, args.Cwd, false)
	if err != nil {
		return nil, err
	}
	cmd, err := ShellCommand(args.Command, cwd)
	if err != nil {
		return nil, err
	}
	if args.Background {
		logPath := filepath.Join(e.runRoot("run", id), "terminal", workspace.NewID()+".log")
		process, err := StartProcess(cmd, logPath)
		if err != nil {
			return nil, err
		}
		return map[string]any{"mode": "background", "command": args.Command, "cwd": cwd, "pid": process.Cmd.Process.Pid, "log_path": logPath}, nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := TerminateProcessTree(cleanupCtx, cmd.Process.Pid)
		cancel()
		if stopErr != nil {
			return nil, fmt.Errorf("终端超时或停止，进程清理失败: %w", stopErr)
		}
		<-done
		return nil, ctx.Err()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return nil, waitErr
	}
	return map[string]any{"mode": "foreground", "command": args.Command, "cwd": cwd, "returncode": cmd.ProcessState.ExitCode(), "stdout": strings.ToValidUTF8(stdout.String(), "�"), "stderr": strings.ToValidUTF8(stderr.String(), "�")}, nil
}
