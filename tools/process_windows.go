package tools

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200}
}

func ProcessIdentity(pid int) string {
	handle, err := syscall.OpenProcess(0x1000, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(handle)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return ""
	}
	return strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 10)
}

func TerminateProcessTree(ctx context.Context, pid int) error {
	if ProcessIdentity(pid) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	configureProcess(cmd)
	err := cmd.Run()
	if err != nil && ProcessIdentity(pid) == "" {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
		return nil
	}
	return err
}
