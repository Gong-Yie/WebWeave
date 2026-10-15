package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"webweave/tools"
	"webweave/workspace"
)

const launchConfigName = "webweave-start.json"
const launchScriptName = "start-webweave.ps1"

type launchConfig struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	Port       int    `json:"port"`
	HealthPath string `json:"health_path"`
}

const launchScript = `param([ValidateRange(0, 65535)][int]$Port = 0)
$ErrorActionPreference = 'Stop'
$config = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'webweave-start.json') -Raw -Encoding UTF8 | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace($config.command)) {
    throw 'No launch command is available. Ask the Agent to finish the project launch configuration.'
}
if (-not $config.command.Contains('{host}') -or -not $config.command.Contains('{port}')) {
    throw 'The launch command must contain {host} and {port}.'
}
if (-not $PSBoundParameters.ContainsKey('Port')) { $Port = [int]$config.port }
if ($Port -lt 0 -or $Port -gt 65535) { throw 'Invalid launch port.' }
if ($Port -eq 0) {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        $Port = $listener.LocalEndpoint.Port
    } finally { $listener.Stop() }
}
$command = $config.command.Replace('{host}', '127.0.0.1').Replace('{port}', [string]$Port)
Write-Host "Web instance: http://127.0.0.1:$Port"
Push-Location -LiteralPath (Join-Path $PSScriptRoot $config.cwd)
try {
    $global:LASTEXITCODE = 0
    & ([scriptblock]::Create($command))
    if ($LASTEXITCODE -ne 0) { throw "Launch process exited with code $LASTEXITCODE" }
} finally { Pop-Location }
`

func (a *App) writeLaunchFiles(id string, config launchConfig) error {
	root := a.runRoot("result", id)
	configPath, err := workspace.ResolveWithin(root, launchConfigName, false)
	if err != nil {
		return err
	}
	scriptPath, err := workspace.ResolveWithin(root, launchScriptName, false)
	if err != nil {
		return err
	}
	if err := workspace.WriteJSONAtomic(configPath, config); err != nil {
		return err
	}
	return workspace.WriteAtomic(scriptPath, []byte(launchScript), 0644)
}

// The caller holds instanceMu so completion and manual starts cannot rewrite each other's files.
func (a *App) prepareLaunchFiles(run *runRuntime, previous *instanceRecord) (launchConfig, error) {
	config := launchConfig{Cwd: ".", HealthPath: "/"}
	path, err := workspace.ResolveWithin(a.runRoot("result", run.store.id), launchConfigName, false)
	if err != nil {
		return config, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return config, fmt.Errorf("读取 %s: %w", launchConfigName, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return config, err
	}
	if strings.TrimSpace(config.Command) == "" && previous != nil {
		cwd, err := workspace.WorkingDirectory(a.root, run.store.id, previous.Cwd, true)
		if err != nil {
			return config, err
		}
		relative, err := filepath.Rel(a.runRoot("result", run.store.id), cwd)
		if err != nil {
			return config, err
		}
		config = launchConfig{previous.Command, filepath.ToSlash(relative), previous.RequestedPort, previous.HealthPath}
	}
	if strings.TrimSpace(config.Command) != "" {
		if _, err := a.launchArgs(run.store.id, config); err != nil {
			return config, err
		}
	}
	return config, a.writeLaunchFiles(run.store.id, config)
}

func (a *App) ensureLaunchFiles(run *runRuntime) error {
	run.instanceMu.Lock()
	defer run.instanceMu.Unlock()
	state, err := run.store.readState()
	if err != nil {
		return err
	}
	_, err = a.prepareLaunchFiles(run, state.Instance)
	return err
}

func (a *App) launchArgs(id string, config launchConfig) (tools.Args, error) {
	if strings.TrimSpace(config.Command) == "" {
		return tools.Args{}, instanceError{"LAUNCH_NOT_READY", "当前项目尚无启动命令，请让 Agent 完成 webweave-start.json", nil}
	}
	if !strings.Contains(config.Command, "{host}") || !strings.Contains(config.Command, "{port}") || len([]rune(config.Command)) > 20000 {
		return tools.Args{}, instanceError{"INVALID_ARGUMENT", "启动命令须包含 {host} 和 {port}，且不超过 20000 字符", nil}
	}
	if config.Port < 0 || config.Port > 65535 || !strings.HasPrefix(config.HealthPath, "/") || strings.HasPrefix(config.HealthPath, "//") {
		return tools.Args{}, instanceError{"INVALID_ARGUMENT", "启动文件的端口或健康检查路径无效", nil}
	}
	if filepath.IsAbs(config.Cwd) {
		return tools.Args{}, instanceError{"INVALID_ARGUMENT", "启动文件的 cwd 必须相对于当前 result 目录", nil}
	}
	if _, err := workspace.WorkingDirectory(a.root, id, config.Cwd, true); err != nil {
		return tools.Args{}, instanceError{"INVALID_ARGUMENT", err.Error(), nil}
	}
	return tools.Args{Action: "start", Command: config.Command, Cwd: config.Cwd, Port: &config.Port, HealthPath: config.HealthPath}, nil
}
