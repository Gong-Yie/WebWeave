//go:build !windows

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func ProcessIdentity(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		closing := strings.LastIndexByte(string(data), ')')
		if closing >= 0 {
			fields := strings.Fields(string(data[closing+1:]))
			if len(fields) > 19 {
				return fields[19]
			}
		}
	}
	data, err = exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func TerminateProcessTree(ctx context.Context, pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return nil
		}
		select {
		case <-ctx.Done():
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}
