package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webweave/workspace"
)

func installTemplateSkill(t *testing.T, app *App) {
	t.Helper()
	skill, err := os.ReadFile(filepath.Join("..", "data", "skills", "template-maker", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteAtomic(filepath.Join(app.root, "data", "skills", "template-maker", "SKILL.md"), skill, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateAgentUpgradeAndCreation(t *testing.T) {
	legacy := defaultAgents()[:4]
	legacy[1].Prompt = "用户修改的设计指令"
	legacy = append(legacy, AgentConfig{"custom", "自定义", "用户角色", "用户指令", ""})
	data, _ := json.Marshal(map[string]any{"agents": legacy})
	cfg, err := parseConfig(data)
	if err != nil || len(cfg.Agents) != 6 || cfg.Agents[1].Prompt != legacy[1].Prompt || cfg.Agents[4].ID != "custom" || cfg.Agents[5].ID != "template" {
		t.Fatalf("legacy roster not preserved: %+v %v", cfg.Agents, err)
	}
	app := testApp(t)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs", map[string]any{"agent_id": "unknown"}), 400)
	response := apiRequest(t, app, "POST", "/api/runs", map[string]any{"agent_id": "template"})
	assertStatus(t, response, 201)
	var payload struct {
		Run runDetail `json:"run"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Run.State.AgentID != "template" || payload.Run.Status != "waiting" || payload.Run.JobActive {
		t.Fatal("template run did not persist selected role")
	}
	assertStatus(t, apiRequest(t, app, "DELETE", "/api/agents/template", nil), 400)
	loader := app.skills
	data, err = os.ReadFile(filepath.Join("..", "data", "skills", "template-maker", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteAtomic(filepath.Join(app.root, "data", "skills", "template-maker", "SKILL.md"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if document, err := loader.Read("template-maker"); err != nil || document.Instructions == "" {
		t.Fatalf("template skill not discoverable: %v", err)
	}
}

func TestTemplateApprovalAndSnapshot(t *testing.T) {
	app := testApp(t)
	run := testRun(t, app, "template-source")
	agent, _ := findAgent(defaultAgents(), "template")
	if err := run.store.selectAgent(agent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(app.runRoot("result", run.store.id), "模板")
	for path, content := range map[string]string{"index.html": validTemplateHTML, "style.css": "body { color: blue; }", "script.js": "document.body.dataset.ready = 'true'"} {
		if err := workspace.WriteAtomic(filepath.Join(root, filepath.FromSlash(path)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	assertStatus(t, apiRequest(t, app, "GET", "/api/templates", nil), 200)
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/template-source/template", templateSaveRequest{Name: "模板"}), 400)
	request := templateSaveRequest{Name: "模板", Approved: true}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/template-source/template", request), 409)
	if err := run.store.setStatus("completed", ""); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/template-source/template", templateSaveRequest{Name: " ", Approved: true}), 400)
	response := apiRequest(t, app, "POST", "/api/runs/template-source/template", request)
	assertStatus(t, response, 201)
	var payload struct {
		Template templateRecord `json:"template"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	record := payload.Template
	if record.FileCount != 3 || record.Name != "模板" || record.RunID != run.store.id || !workspace.ValidName(record.ID) {
		t.Fatalf("unexpected snapshot: %+v", record)
	}
	if record.Path != "data/templates/模板" {
		t.Fatalf("template folder not named after template: %s", record.Path)
	}
	assertStatus(t, apiRequest(t, app, "POST", "/api/runs/template-source/template", request), 409)
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("修改后的原始文件"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(app.root, filepath.FromSlash(record.Path), "index.html"))
	if err != nil || string(data) != validTemplateHTML {
		t.Fatal("snapshot was not independent")
	}
	entries, err := os.ReadDir(filepath.Join(app.root, filepath.FromSlash(record.Path)))
	if err != nil || len(entries) != 3 {
		t.Fatal("template folder does not contain exactly three files")
	}
	if err := app.deleteRun(run.store.id); err != nil {
		t.Fatal(err)
	}
	listing := apiRequest(t, app, "GET", "/api/templates", nil)
	assertStatus(t, listing, 200)
	if !strings.Contains(listing.Body.String(), record.ID) {
		t.Fatal("deleting source run removed template")
	}
}

func TestTemplateRejectedFiles(t *testing.T) {
	for _, scenario := range []string{"empty", "oversized", "outside-link", "wrong-agent", "extra-file", "bad-title", "bad-document-title", "bad-text", "bad-component", "duplicate-id", "missing-reference", "invalid-name"} {
		t.Run(scenario, func(t *testing.T) {
			app := testApp(t)
			run := testRun(t, app, "rejected-template")
			agent, _ := findAgent(defaultAgents(), "template")
			if scenario == "wrong-agent" {
				agent = defaultAgents()[0]
			}
			if err := run.store.selectAgent(agent); err != nil {
				t.Fatal(err)
			}
			if err := run.store.setStatus("completed", ""); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(app.runRoot("result", run.store.id), "拒绝入库")
			if scenario != "empty" {
				markup := validTemplateHTML
				switch scenario {
				case "bad-title":
					markup = strings.Replace(markup, "标题1</h1>", "组件1</h1>", 1)
				case "bad-document-title":
					markup = strings.Replace(markup, "标题0</title>", "组件0</title>", 1)
				case "bad-text":
					markup = strings.Replace(markup, "文本2</p>", "真实内容</p>", 1)
				case "bad-component":
					markup = strings.Replace(markup, `aria-label="组件3"`, `aria-label="提交"`, 1)
				case "duplicate-id":
					markup = strings.Replace(markup, `id="组件3"`, `id="文本2"`, 1)
				case "missing-reference":
					markup = strings.Replace(markup, "script.js", "app.js", 1)
				}
				for name, content := range map[string]string{"index.html": markup, "style.css": "body{}", "script.js": "void 0"} {
					if err := workspace.WriteAtomic(filepath.Join(root, name), []byte(content), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if scenario == "extra-file" {
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("extra"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "style.css")
			if scenario == "oversized" {
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate(100*1024*1024 + 1)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("create oversize fixture: %v %v", err, closeErr)
				}
			}
			if scenario == "outside-link" {
				outside := filepath.Join(t.TempDir(), "secret.txt")
				if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Skipf("platform does not permit symlinks: %v", err)
				}
			}
			status := 400
			if scenario == "wrong-agent" {
				status = 409
			}
			name := "拒绝入库"
			if scenario == "invalid-name" {
				name = "../outside"
			}
			assertStatus(t, apiRequest(t, app, "POST", "/api/runs/rejected-template/template", templateSaveRequest{Name: name, Approved: true}), status)
			listing, err := app.listTemplates()
			if err != nil || len(listing) != 0 {
				t.Fatal("failed save left a visible template")
			}
			entries, err := os.ReadDir(filepath.Join(app.root, "data", "templates"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("failed save left staging files")
			}
		})
	}
}

const validTemplateHTML = `<!doctype html><html lang="zh-CN"><head><title>标题0</title><link rel="stylesheet" href="style.css"></head><body><h1 id="标题1">标题1</h1><p id="文本2">文本2</p><button id="组件3" aria-label="组件3">组件3</button><script src="script.js"></script></body></html>`

func TestTemplateLegacyListing(t *testing.T) {
	app := testApp(t)
	id := workspace.NewID()
	record := templateRecord{ID: id, Name: "旧模板", RunID: "old-run", CreatedAt: workspace.Timestamp(), FileCount: 1, Path: "data/templates/" + id + "/files"}
	if err := workspace.WriteJSONAtomic(filepath.Join(app.root, "data", "templates", id, "template.json"), record); err != nil {
		t.Fatal(err)
	}
	listed, err := app.listTemplates()
	if err != nil || len(listed) != 1 || listed[0].Name != "旧模板" {
		t.Fatalf("legacy template not preserved: %+v %v", listed, err)
	}
}
