package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"webweave/workspace"
)

func (a *App) Chat(ctx context.Context, input io.Reader, output io.Writer, id string) error {
	if id == "" {
		id = workspace.NewID()
	}
	if err := a.prepareDirectories(id); err != nil {
		return err
	}
	run, err := a.runtime(id)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "WebWeave Agent 已启动。输入 q 退出。\n当前运行 ID: %s\n", id)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	lines := make(chan string)
	readerDone := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-readerCtx.Done():
				return
			}
		}
		readerDone <- scanner.Err()
	}()
	for {
		fmt.Fprint(output, "\n你: ")
		var line string
		select {
		case line = <-lines:
		case err := <-readerDone:
			return err
		case <-ctx.Done():
			return run.store.setStatus("stopped", "用户中断")
		}
		content := strings.TrimSpace(line)
		if content == "q" || content == "quit" || content == "exit" {
			return run.store.setStatus("stopped", "用户退出")
		}
		if content == "" {
			continue
		}
		before, err := run.store.readState()
		if err != nil {
			return err
		}
		approvedDecision := ""
		if decision := before.PendingDecision; decision != nil {
			choice := content
			if number, parseErr := strconv.Atoi(content); parseErr == nil && number > 0 && number <= len(decision.Options) {
				choice = decision.Options[number-1]
			}
			if slices.Contains(decision.Options, choice) || len(decision.Options) == 0 && (content == "确认" || content == "同意" || content == "approve") && decision.Kind != "question" {
				if len(decision.Options) == 0 {
					choice = "approve"
				}
				approvedDecision = decision.ID
				content, err = a.answerDecision(id, decision.ID, choice)
				if err != nil {
					return err
				}
			}
		}
		if err := a.startMessage(id, content, "", nil, nil, false, nil, approvedDecision); err != nil {
			return err
		}
		run.mu.Lock()
		job := run.job
		run.mu.Unlock()
		if job != nil {
			select {
			case <-job.done:
			case <-ctx.Done():
				_, _ = a.stopRun(id)
				<-job.done
				return ctx.Err()
			}
		}
		detail, err := a.detail(id)
		if err != nil {
			return err
		}
		for i := len(detail.Events) - 1; i >= before.EventCount; i-- {
			event := detail.Events[i]
			if event.Type == "model_output" {
				fmt.Fprintf(output, "\nAgent: %v\n", event.Payload["output_text"])
				break
			}
		}
		if detail.Status == "failed" {
			fmt.Fprintln(output, "任务失败，请检查运行记录和服务日志。")
		}
		if decision := detail.State.PendingDecision; decision != nil {
			fmt.Fprintf(output, "\n需要你决定：%s\n", decision.Question)
			for index, option := range decision.Options {
				fmt.Fprintf(output, "%d. %s\n", index+1, option)
			}
			if len(decision.Options) > 0 {
				fmt.Fprintln(output, "输入选项编号或完整选项，或直接说明需要调整的内容。")
			} else if decision.Kind != "question" {
				fmt.Fprintln(output, "输入“确认”同意并继续，或直接输入需要调整的内容。")
			}
		}
	}
}
