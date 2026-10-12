package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Process struct {
	Cmd      *exec.Cmd
	done     chan struct{}
	err      error
	Identity string
}

func ShellCommand(command, cwd string) (*exec.Cmd, error) {
	if strings.TrimSpace(command) == "" || len([]rune(command)) > 20000 {
		return nil, errors.New("command 不能为空且不得超过 20000 字符")
	}
	var shell string
	for _, name := range []string{"powershell.exe", "pwsh.exe", "pwsh"} {
		if path, err := exec.LookPath(name); err == nil {
			shell = path
			break
		}
	}
	if shell == "" {
		return nil, errors.New("未找到 PowerShell")
	}
	utf8Command := "$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new(); " + command
	cmd := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", utf8Command)
	cmd.Dir = cwd
	configureProcess(cmd)
	return cmd, nil
}

func StartProcess(cmd *exec.Cmd, logPath string) (*Process, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = file, file
	if err := cmd.Start(); err != nil {
		file.Close()
		return nil, err
	}
	process := &Process{Cmd: cmd, done: make(chan struct{}), Identity: ProcessIdentity(cmd.Process.Pid)}
	go func() { process.err = cmd.Wait(); file.Close(); close(process.done) }()
	return process, nil
}

func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *Process) Stop(ctx context.Context) error {
	if !p.Alive() {
		return nil
	}
	if err := TerminateProcessTree(ctx, p.Cmd.Process.Pid); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
