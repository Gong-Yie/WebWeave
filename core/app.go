package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"webweave/skills"
	"webweave/tools"
	"webweave/workspace"
)

type runJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type runRuntime struct {
	mu         sync.Mutex
	instanceMu sync.Mutex
	store      *runStore
	job        *runJob
	processes  map[string]*tools.Process
}

// App owns the local workspace, background jobs and per-run persistence locks.
type App struct {
	root         string
	config       *configManager
	skills       *skills.Loader
	tools        *tools.Executor
	python       *pythonCompressor
	compressText func(context.Context, LocalCompressionConfig, string) (localCompressionResult, error)
	runs         sync.Map
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	backgroundMu sync.Mutex
	templateMu   sync.Mutex
}

func New(root string) (*App, error) {
	resolved, err := workspace.CanonicalPath(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(resolved, "webui", "index.html"))
	if err != nil || info.IsDir() {
		return nil, errors.New("工作空间缺少 webui/index.html，请从项目根目录启动")
	}
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{root: resolved, config: newConfigManager(resolved), ctx: ctx, cancel: cancel}
	app.python = newPythonCompressor(ctx, resolved)
	app.compressText = app.python.Compress
	app.skills = skills.NewLoader(filepath.Join(resolved, "data", "skills"))
	app.tools = tools.NewExecutor(resolved, app.skills, func(ctx context.Context, id string, args tools.Args) (any, error) {
		return app.instanceAction(ctx, id, args)
	})
	return app, nil
}

func (a *App) runtime(id string) (*runRuntime, error) {
	if !workspace.ValidName(id) {
		return nil, errors.New("run_id 无效")
	}
	path, err := workspace.ResolveWithin(filepath.Join(a.root, "data", "run"), id, false)
	if err != nil {
		return nil, err
	}
	value, _ := a.runs.LoadOrStore(id, &runRuntime{store: &runStore{id: id, dir: path}})
	return value.(*runRuntime), nil
}

func (a *App) active(id string) bool {
	run, err := a.runtime(id)
	if err != nil {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.job != nil
}

func (a *App) Close() {
	a.cancel()
	a.wg.Wait()
	a.python.Close()
	a.runs.Range(func(key, value any) bool {
		run := value.(*runRuntime)
		run.instanceMu.Lock()
		for instanceID, process := range run.processes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*second)
			if err := process.Stop(ctx); err != nil {
				slog.Error("清理实例进程失败", "run_id", key, "instance_id", instanceID, "error", err)
			}
			cancel()
		}
		run.instanceMu.Unlock()
		return true
	})
}

func (a *App) startMessage(id, content, agentID string, attachments []string, instanceContext any, resume bool, requestedEffort *string, approvedDecision ...string) error {
	run, err := a.runtime(id)
	if err != nil {
		return err
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if a.ctx.Err() != nil {
		return errors.New("服务正在停止")
	}
	if run.job != nil {
		return apiError{409, "RUN_ALREADY_ACTIVE", "当前对话仍在执行"}
	}
	if !resume && strings.TrimSpace(content) == "" {
		return errors.New("消息内容不能为空")
	}
	var items []inputItem
	if agentID == "" && resume {
		state, err := run.store.readState()
		if err != nil {
			return err
		}
		agentID = state.AgentID
	}
	agent, err := findAgent(a.config.get().Agents, agentID)
	if err != nil {
		return err
	}
	provider, err := providerForAgent(a.config.get(), agent)
	if err != nil {
		return err
	}
	state, err := run.store.readState()
	if err != nil {
		return err
	}
	var task *taskCheckpoint
	var previousTask *taskCheckpoint
	if state.TaskID != "" {
		saved, err := run.store.loadTask(state.TaskID)
		if err != nil {
			return err
		}
		if saved != nil && saved.AgentID == agent.ID && saved.Status != "completed" {
			task, items = saved, saved.Items
			task.Round, task.Stalled = 0, 0
		}
		if saved != nil && saved.AgentID == agent.ID {
			previousTask = saved
		}
	}
	if task == nil {
		items, err = run.store.loadItems()
		if err != nil {
			return err
		}
		task = &taskCheckpoint{ID: workspace.NewID(), AgentID: agent.ID, Items: items, Tools: map[string]*taskTool{}}
		if previousTask != nil {
			task.Work, task.TokenUsage = previousTask.Work, previousTask.TokenUsage
		}
	}
	if resume && len(items) == 0 {
		return apiError{400, "INVALID_REQUEST", "当前对话没有可继续的上下文"}
	}
	if resume && state.PendingDecision != nil {
		return apiError{409, "DECISION_REQUIRED", "请先回答当前问题或确认方案"}
	}
	effort := ""
	if state.AgentID == agent.ID {
		effort = state.ReasoningEffort
	}
	if requestedEffort != nil {
		effort = *requestedEffort
	}
	if effort != "" && !slices.Contains(provider.ReasoningEfforts, effort) {
		return apiError{400, "INVALID_REASONING_EFFORT", "当前 Agent 的模型不支持所选思考强度"}
	}
	if err := run.store.selectTask(task.ID); err != nil {
		return err
	}
	if err = run.store.selectAgent(agent); err != nil {
		return err
	}
	if err = run.store.selectReasoningEffort(effort); err != nil {
		return err
	}
	if err = run.store.start(); err != nil {
		return err
	}
	if !resume {
		task.ReadRepeats, task.ReadsSinceNote = 0, 0
		task.RequestRevision = contentKey(content)
		keepDecision := ""
		if len(approvedDecision) > 0 {
			keepDecision = approvedDecision[0]
		}
		if err := run.store.revokeDeployment(keepDecision); err != nil {
			return err
		}
		if state.PendingDecision != nil {
			if err := run.store.setDecision(nil); err != nil {
				return err
			}
		}
		content = strings.TrimSpace(content)
		modelText := content
		if len(attachments) > 0 {
			modelText += "\n\n[本次附件]"
			for _, name := range attachments {
				modelText += "\n- " + filepath.ToSlash(filepath.Join("data", "resources", id, name))
			}
		}
		if instanceContext != nil {
			data, err := jsonBytes(instanceContext)
			if err != nil {
				return err
			}
			modelText += "\n\n[实例检查上下文]\n" + string(data)
		}
		item := inputItem{"role": "user", "content": modelText}
		if len(pendingTaskCalls(task.Items)) > 0 {
			task.QueuedInput = append(task.QueuedInput, item)
			if keepDecision == "" {
				task.ReconcileFeedback = true
				// A later correction supersedes an earlier retry approval too.
				for _, record := range task.Tools {
					record.RetryApproved = false
					if record.ChildID == "" {
						continue
					}
					child, err := run.store.loadTask(record.ChildID)
					if err != nil {
						return err
					}
					if child == nil {
						continue
					}
					changed := false
					for _, operation := range child.Tools {
						if operation.RetryApproved {
							operation.RetryApproved, changed = false, true
						}
					}
					if changed {
						if err := run.store.saveTask(child); err != nil {
							return err
						}
					}
				}
			}
		} else {
			items = append(items, item)
		}
		if err = run.store.append("user_message", map[string]any{"item": item, "display_text": content, "attachments": attachments, "instance_context": instanceContext, "agent_id": agent.ID, "agent_name": agent.Name}); err != nil {
			return err
		}
		if err = run.store.saveContext(items, nil); err != nil {
			return err
		}
	}
	task.Items = items
	if err := run.store.saveTask(task); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	ctx = context.WithValue(ctx, agentExecutionKey{}, agentExecution{effort: effort, checkpoint: task, interactive: true})
	job := &runJob{cancel: cancel, done: make(chan struct{})}
	run.job = job
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		var jobErr error
		if len(attachments) > 0 && len(items) > 0 {
			target := items
			if len(task.QueuedInput) > 0 {
				target = task.QueuedInput
			}
			target[len(target)-1], jobErr = a.prepareUserItem(ctx, id, itemString(target[len(target)-1], "content"), attachments)
			if jobErr == nil {
				jobErr = run.store.saveTask(task)
			}
		}
		if jobErr == nil {
			_, jobErr = a.runAgent(ctx, run.store, items, agent, false)
		}
		var pause *taskPause
		if err := func() error {
			if errors.As(jobErr, &pause) {
				return nil
			}
			return a.ensureLaunchFiles(run)
		}(); err != nil {
			jobErr = errors.Join(jobErr, fmt.Errorf("生成启动文件失败: %w", err))
			if ctx.Err() != nil {
				slog.Error("生成启动文件失败", "run_id", id, "error", err)
			}
		}
		run.mu.Lock()
		defer run.mu.Unlock()
		status, reason := "completed", ""
		if ctx.Err() != nil {
			status, reason = "stopped", "用户请求停止"
		} else if jobErr != nil {
			if errors.As(jobErr, &pause) {
				status, reason = pause.Status, pause.Reason
			} else {
				status, reason = "failed", jobErr.Error()
				slog.Error("Agent 任务失败", "run_id", id, "error", jobErr)
			}
		}
		if err := run.store.setStatus(status, reason); err != nil {
			slog.Error("保存任务状态失败", "run_id", id, "error", err)
		}
		run.job = nil
		close(job.done)
	}()
	return nil
}

func (a *App) stopRun(id string) (bool, error) {
	run, err := a.runtime(id)
	if err != nil {
		return false, err
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.job == nil {
		return false, nil
	}
	if err := run.store.setStatus("stopping", "用户从 WebUI 请求停止"); err != nil {
		return false, err
	}
	run.job.cancel()
	return true, nil
}

func (a *App) requireRun(id string) (*runRuntime, error) {
	run, err := a.runtime(id)
	if err != nil {
		return nil, apiError{400, "INVALID_RUN_ID", err.Error()}
	}
	info, err := os.Stat(run.store.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, apiError{404, "RUN_NOT_FOUND", "对话不存在"}
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("运行路径不是目录")
	}
	return run, nil
}
