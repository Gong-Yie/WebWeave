//go:build !windows

package core

import "os/exec"

func configurePythonProcess(_ *exec.Cmd) {}
