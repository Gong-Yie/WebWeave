package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"webweave/workspace"
)

// Input items retain arbitrary Responses payloads for compatibility with saved runs.
type inputItem map[string]any

type runEvent struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
}

type runState struct {
	RunID            string          `json:"run_id"`
	AgentID          string          `json:"agent_id"`
	Status           string          `json:"status"`
	UpdatedAt        string          `json:"updated_at"`
	LastEvent        string          `json:"last_event"`
	EventCount       int             `json:"event_count"`
	ContextUpdatedAt string          `json:"context_updated_at,omitempty"`
	ContextVersion   int             `json:"context_version,omitempty"`
	Instance         *instanceRecord `json:"instance,omitempty"`
}

type savedContext struct {
	Version    int         `json:"version"`
	UpdatedAt  string      `json:"updated_at"`
	Summary    any         `json:"summary"`
	InputItems []inputItem `json:"input_items"`
}

type runStore struct {
	mu  sync.Mutex
	id  string
	dir string
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("读取 %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *runStore) stateLocked() (runState, error) {
	state := runState{RunID: s.id, AgentID: "main", Status: "waiting"}
	err := readJSONFile(filepath.Join(s.dir, "state.json"), &state)
	return state, err
}

func (s *runStore) readState() (runState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *runStore) appendLocked(kind string, payload map[string]any, status string) error {
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	event := runEvent{workspace.Timestamp(), kind, payload}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	state.UpdatedAt, state.LastEvent = event.Timestamp, kind
	state.EventCount++
	if status != "" {
		state.Status = status
	}
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

func (s *runStore) append(kind string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(kind, payload, "")
}

func (s *runStore) setStatus(status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked("run_status", map[string]any{"status": status, "reason": reason}, status)
}

func (s *runStore) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	kind := "run_started"
	if state.EventCount > 0 {
		kind = "run_resumed"
	}
	return s.appendLocked(kind, map[string]any{"previous_status": state.Status}, "running")
}

func (s *runStore) selectAgent(agent AgentConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked("agent_selected", map[string]any{"agent_id": agent.ID, "agent_name": agent.Name}, ""); err != nil {
		return err
	}
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	state.AgentID = agent.ID
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

func (s *runStore) saveInstance(record *instanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked("instance_status", map[string]any{"instance": record}, ""); err != nil {
		return err
	}
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	state.Instance = record
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

func (s *runStore) saveContext(items []inputItem, summary any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := savedContext{1, workspace.Timestamp(), summary, items}
	if err := workspace.WriteJSONAtomic(filepath.Join(s.dir, "context.json"), ctx); err != nil {
		return err
	}
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	state.ContextUpdatedAt = ctx.UpdatedAt
	state.ContextVersion++
	return workspace.WriteJSONAtomic(filepath.Join(s.dir, "state.json"), state)
}

func (s *runStore) eventsLocked() ([]runEvent, error) {
	events := []runEvent{}
	file, err := os.Open(filepath.Join(s.dir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 65536), 64*1024*1024)
	for scanner.Scan() {
		var event runEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func (s *runStore) snapshot() (runState, []runEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		return state, nil, err
	}
	events, err := s.eventsLocked()
	return state, events, err
}

func (s *runStore) loadItems() ([]inputItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ctx savedContext
	if err := readJSONFile(filepath.Join(s.dir, "context.json"), &ctx); err != nil {
		return nil, err
	}
	if ctx.InputItems != nil {
		return repairItems(ctx.InputItems), nil
	}
	events, err := s.eventsLocked()
	if err != nil {
		return nil, err
	}
	items := []inputItem{}
	for _, event := range events {
		if delegated, _ := event.Payload["delegated"].(bool); delegated {
			continue
		}
		switch event.Type {
		case "user_message", "tool_output":
			if item, ok := event.Payload["item"].(map[string]any); ok {
				items = append(items, item)
			}
		case "model_output", "context_compressed":
			key := "items"
			if event.Type == "context_compressed" {
				key = "input_items"
				items = []inputItem{}
			}
			output, _ := event.Payload[key].([]any)
			for _, value := range output {
				if item, ok := value.(map[string]any); ok {
					items = append(items, item)
				}
			}
			if len(output) == 0 && event.Type == "model_output" {
				if text, ok := event.Payload["output_text"].(string); ok && text != "" {
					items = append(items, inputItem{"role": "assistant", "content": text})
				}
			}
		}
	}
	return repairItems(items), nil
}

func itemString(item inputItem, key string) string { value, _ := item[key].(string); return value }

func repairItems(items []inputItem) []inputItem {
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		id := itemString(item, "call_id")
		if id == "" {
			id = itemString(item, "id")
		}
		if itemString(item, "type") == "function_call" {
			calls[id] = id != ""
		}
		if itemString(item, "type") == "function_call_output" {
			outputs[id] = id != ""
		}
	}
	result := []inputItem{}
	for _, item := range items {
		kind := itemString(item, "type")
		id := itemString(item, "call_id")
		if id == "" && kind == "function_call" {
			id = itemString(item, "id")
		}
		if (kind == "function_call" || kind == "function_call_output") && (!calls[id] || !outputs[id]) {
			continue
		}
		result = append(result, item)
	}
	return result
}
