package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type localCompressionResult struct {
	OK           bool   `json:"ok"`
	Text         string `json:"text"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Error        string `json:"error"`
}

type pythonCompressor struct {
	ctx     context.Context
	root    string
	lock    chan struct{}
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	decoder *json.Decoder
	log     *os.File
	config  LocalCompressionConfig
}

func newPythonCompressor(ctx context.Context, root string) *pythonCompressor {
	return &pythonCompressor{ctx: ctx, root: root, lock: make(chan struct{}, 1)}
}

func (p *pythonCompressor) stop() {
	if p.cmd == nil {
		return
	}
	_ = p.stdin.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	_ = p.log.Close()
	p.cmd, p.stdin, p.decoder, p.log = nil, nil, nil, nil
}

func (p *pythonCompressor) Close() {
	p.lock <- struct{}{}
	defer func() { <-p.lock }()
	p.stop()
}

func (p *pythonCompressor) start(cfg LocalCompressionConfig) error {
	if p.cmd != nil && p.config.Python == cfg.Python && p.config.Model == cfg.Model && p.config.Device == cfg.Device {
		return nil
	}
	p.stop()
	python := cfg.Python
	if !filepath.IsAbs(python) && strings.ContainsAny(python, `/\`) {
		python = filepath.Join(p.root, filepath.FromSlash(python))
	}
	logPath := filepath.Join(p.root, "data", "compression", "worker.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(p.ctx, python, "-u", filepath.Join(p.root, "compression", "worker.py"), "--model", cfg.Model, "--device", cfg.Device)
	configurePythonProcess(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "TOKENIZERS_PARALLELISM=false", "HF_HUB_DISABLE_PROGRESS_BARS=1")
	cmd.Stderr = log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = log.Close()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = log.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		_ = log.Close()
		return err
	}
	p.cmd, p.stdin, p.decoder, p.log, p.config = cmd, stdin, json.NewDecoder(stdout), log, cfg
	return nil
}

func (p *pythonCompressor) Compress(ctx context.Context, cfg LocalCompressionConfig, text string) (localCompressionResult, error) {
	if err := ctx.Err(); err != nil {
		return localCompressionResult{}, err
	}
	select {
	case p.lock <- struct{}{}:
	case <-ctx.Done():
		return localCompressionResult{}, ctx.Err()
	}
	defer func() { <-p.lock }()
	if err := p.start(cfg); err != nil {
		return localCompressionResult{}, fmt.Errorf("启动 Python 压缩进程: %w", err)
	}
	request := struct {
		Text string  `json:"text"`
		Rate float64 `json:"rate"`
	}{text, cfg.Rate}
	type reply struct {
		result localCompressionResult
		err    error
	}
	done := make(chan reply, 1)
	stdin, decoder := p.stdin, p.decoder
	go func() {
		var result localCompressionResult
		err := json.NewEncoder(stdin).Encode(request)
		if err != nil {
			done <- reply{result, fmt.Errorf("发送压缩片段: %w", err)}
			return
		}
		err = decoder.Decode(&result)
		done <- reply{result, err}
	}()
	select {
	case <-ctx.Done():
		p.stop()
		<-done
		return localCompressionResult{}, ctx.Err()
	case result := <-done:
		if result.err != nil {
			p.stop()
			return result.result, fmt.Errorf("读取 Python 压缩结果，检查 data/compression/worker.log: %w", result.err)
		}
		if !result.result.OK {
			return result.result, errors.New("Python 压缩失败: " + result.result.Error)
		}
		if result.result.Text == "" || result.result.InputTokens < 1 || result.result.OutputTokens < 1 || result.result.OutputTokens > result.result.InputTokens {
			return result.result, errors.New("Python 压缩结果无效或增大了内容")
		}
		return result.result, nil
	}
}
