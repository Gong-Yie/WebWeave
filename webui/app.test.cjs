const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8")
  .replace(/initialize\(\);\s*$/, "");

// Minimal DOM surface for configuration and workspace behavior checks.
class Element {
  constructor(tag = "div") {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.attributes = new Map();
    this.listeners = new Map();
    this.lastChild = { textContent: "" };
    const classes = new Set();
    this.classList = {
      add(...values) { values.forEach((value) => classes.add(value)); },
      remove(...values) { values.forEach((value) => classes.delete(value)); },
      contains(value) { return classes.has(value); },
      toggle(value, enabled = !classes.has(value)) {
        if (enabled) classes.add(value); else classes.delete(value);
        return enabled;
      },
    };
  }

  append(...children) {
    children.forEach((child) => { if (child instanceof Element) child.parentElement = this; });
    this.children.push(...children);
    this.lastChild = this.children.at(-1);
  }

  replaceChildren(...children) {
    this.children = [];
    this.append(...children);
  }

  querySelectorAll(selector) {
    const matches = [];
    const visit = (node) => {
      if (!(node instanceof Element)) return;
      const match = selector.startsWith(".") ? (node.className || "").split(" ").includes(selector.slice(1))
        : selector.startsWith("input") && node.tagName === "INPUT" && (!selector.includes(":checked") || node.checked);
      if (match) matches.push(node);
      node.children.forEach(visit);
    };
    this.children.forEach(visit);
    return matches;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  remove() { if (this.parentElement) this.parentElement.children = this.parentElement.children.filter((child) => child !== this); }
  setAttribute(name, value) { this.attributes.set(name, value); }
  removeAttribute(name) { this.attributes.delete(name); }
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(listener);
  }
  focus() {}
}

function workspaceSandbox(fetch = async () => { throw new Error("Unexpected request"); }) {
  let modelSerial = 0;
  const nodes = new Map();
  const node = (selector) => {
    if (!nodes.has(selector)) nodes.set(selector, new Element());
    return nodes.get(selector);
  };
  const nav = node("workspace-nav");
  nav.dataset.view = "workspace";
  const sandbox = vm.createContext({
    window: {
      innerWidth: 1200, location: { hash: "" },
      matchMedia() { return { matches: false, addEventListener() {} }; },
      addEventListener() {}, confirm() { return true; },
      clearTimeout() {}, setTimeout() {},
      crypto: { randomUUID() { return (++modelSerial).toString(16).padStart(32, "0"); } },
    },
    localStorage: { getItem() { return null; } },
    document: {
      querySelector: node,
      querySelectorAll(selector) {
        if (selector === ".nav-item, [data-open-view]") return [nav];
        for (const root of ["#model-supported-efforts", "#model-discovery-list", "#model-headers"]) {
          if (selector.startsWith(root + " ")) return node(root).querySelectorAll(selector.slice(root.length + 1));
        }
        return [];
      },
      createElement(tag) { return new Element(tag); },
      createTextNode(text) { return { textContent: text }; },
    },
    console, fetch,
  });
  vm.runInContext(source, sandbox, { filename: "app.js" });
  return { sandbox, node, nav };
}

test("workspace navigation shows empty state without creating a run", async () => {
  const { sandbox, node, nav } = workspaceSandbox();
  vm.runInContext("bindEvents()", sandbox);
  await nav.listeners.get("click")[0]();
  assert.equal(sandbox.window.location.hash, "workspace");
  assert.equal(node("#workspace-empty").hidden, false);
  assert.equal(node("#workspace-grid").hidden, true);
  assert.equal(node(".workspace-header").hidden, true);
  assert.equal(vm.runInContext("state.activeRunId", sandbox), null);
});

test("template creation opens a conversation with a persisted template agent", async () => {
  const requests = [];
  const run = { run_id: "new-template", title: "新对话", status: "waiting", job_active: false, event_count: 0, state: { agent_id: "template" }, events: [], instance: null };
  const { sandbox, node } = workspaceSandbox(async (url, options) => {
    requests.push({ url, options });
    let payload;
    if (url === "/api/runs" && options.method === "POST") {
      assert.equal(JSON.parse(options.body).agent_id, "template");
      payload = { run };
    } else if (url === "/api/runs") payload = { runs: [run] };
    else if (url === "/api/runs/new-template") payload = { run };
    else if (url === "/api/runs/new-template/files") payload = { files: [] };
    else throw new Error(`Unexpected URL ${url}`);
    return { ok: true, headers: { get() { return "application/json"; } }, json: async () => ({ ok: true, ...payload }) };
  });
  vm.runInContext('state.agents = [{ id: "main", name: "主Agent" }, { id: "template", name: "前端模板制作Agent" }]; bindEvents();', sandbox);
  await node("#template-create").listeners.get("click")[0]();
  assert.equal(requests.filter(({ options }) => options.method === "POST").length, 1);
  assert.equal(vm.runInContext("state.activeRunId", sandbox), "new-template");
  assert.equal(vm.runInContext("state.selectedAgentId", sandbox), "template");
  assert.equal(node("#workspace-empty").hidden, true);
  assert.equal(node("#workspace-grid").hidden, false);
  assert.equal(node("#template-save-form").hidden, true);
});

test("token ring distinguishes estimates and clamps its progress", () => {
  const { sandbox, node } = workspaceSandbox();
  for (const usage of [
    { input_tokens: 400, threshold: 1000, estimated: true, compression_enabled: true },
    { input_tokens: 1200, threshold: 1000, estimated: false, compression_enabled: false },
  ]) {
    sandbox.usage = usage;
    vm.runInContext("renderContextMeter({ context_usage: usage })", sandbox);
    assert.equal(node("#context-meter").attributes.get("aria-valuenow"), usage.estimated ? "40" : "100");
    assert.match(node("#context-meter-label").textContent, usage.estimated ? /估算 400/ : /API 1,200/);
    if (!usage.compression_enabled) assert.match(node("#context-meter").title, /已关闭/);
  }
});

test("subagent tasks retain separate identities and recover interrupted status", () => {
  const { sandbox } = workspaceSandbox();
  sandbox.events = [
    { type: "subagent_started", payload: { subagent_id: "first", agent_name: "代码Agent", task: "第一次编程" } },
    { type: "subagent_finished", payload: { subagent_id: "first", status: "completed" } },
    { type: "subagent_started", payload: { subagent_id: "second", agent_name: "代码Agent", task: "再次编程" } },
    { type: "tool_started", payload: { subagent_id: "second", tool_name: "create_file", arguments: '{"file_path":"index.html"}' } },
  ];
  const tasks = vm.runInContext("collectSubagents({ events, job_active: true })", sandbox);
  assert.equal(tasks.length, 2);
  assert.equal(tasks[0].status, "completed");
  assert.equal(tasks[1].status, "running");
  assert.match(tasks[1].activity, /create_file.*index.html/);
  const recovered = vm.runInContext("collectSubagents({ events, job_active: false })", sandbox);
  assert.equal(recovered[1].status, "stopped");
});

test("reasoning renders once for streaming and nonstreaming responses", () => {
  for (const streaming of [false, true]) {
    const { sandbox, node } = workspaceSandbox();
    sandbox.run = {
      run_id: "reasoning", title: "任务", job_active: false,
      events: [
        ...(streaming ? [{ type: "model_reasoning_delta", payload: { agent_name: "主Agent", reasoning_key: "rs:summary:0", delta: "先检查" } }] : []),
        { type: "model_output", payload: { agent_name: "主Agent", output_text: "完成", items: [{ type: "reasoning", summary: [{ text: "先检查，再制作" }] }] } },
      ],
    };
    vm.runInContext("renderConversation(run)", sandbox);
    const blocks = node("#conversation-scroll").children.filter((child) => child.className === "reasoning-block");
    assert.equal(blocks.length, 1);
    assert.equal(blocks[0].children[1].textContent, "先检查，再制作");
    assert.equal(node("#conversation-scroll").children.filter((child) => child.className === "message message-agent").length, 1);
  }
});

test("template saving requires confirmation and binds the approved run", async () => {
  const requests = [];
  const { sandbox, node } = workspaceSandbox(async (url, options) => {
    requests.push({ url, options });
    return { ok: true, headers: { get() { return "application/json"; } }, json: async () => ({ ok: true, templates: [] }) };
  });
  node("#template-save-name").value = "模板";
  vm.runInContext('state.activeRun = { run_id: "approved-run", job_active: false };', sandbox);
  sandbox.window.confirm = () => false;
  await vm.runInContext("saveActiveTemplate()", sandbox);
  assert.equal(requests.length, 0);
  sandbox.window.confirm = () => true;
  await vm.runInContext("saveActiveTemplate()", sandbox);
  assert.equal(requests[0].url, "/api/runs/approved-run/template");
  assert.deepEqual(JSON.parse(requests[0].options.body), { name: "模板", approved: true });
  assert.equal(node("#template-save").disabled, false);
});

for (const supportsAgents of [false, true]) {
  test(`configuration loads with ${supportsAgents ? "current" : "legacy"} backend`, async () => {
    const nodes = new Map();
    const node = (selector) => {
      if (!nodes.has(selector)) nodes.set(selector, new Element());
      return nodes.get(selector);
    };
    const config = {
      llm: {
        main: { model: "configured-model", base_url: "https://example.invalid", api_key: "********" },
        vision: { enabled: false, model: "", base_url: "", api_key: "" },
      },
      model: { stream: true },
      tools: { file_read: { enabled: true, run_scoped: true } },
      tool_timeout: { default_seconds: 120, max_seconds: 1800 },
      context: {
        enabled: true, max_input_tokens: 32768, compression_trigger_ratio: 0.8,
        keep_recent_items: 12, summary_max_tokens: 2000, compression_timeout_seconds: 120,
      },
    };
    if (supportsAgents) {
      config.agents = [
        { id: "main", name: "主Agent", description: "协调任务", prompt: "协调所有 Agent" },
        { id: "custom", name: "自定义Agent", description: "自定义任务", prompt: "处理任务" },
      ];
    }
    const controls = ["#settings-save", "#llm-main-select", "#timeout-default", "#context-enabled"];
    const sandbox = vm.createContext({
      window: { innerWidth: 1200 },
      localStorage: { getItem() { return null; } },
      document: {
        querySelector: node,
        querySelectorAll() { return controls.map(node); },
        createElement() { return new Element(); },
        createTextNode(text) { return { textContent: text }; },
      },
      fetch: async (url) => {
        assert.equal(url, "/api/config");
        return {
          ok: true,
          headers: { get() { return "application/json"; } },
          json: async () => ({ ok: true, config }),
        };
      },
    });
    vm.runInContext(source, sandbox, { filename: "app.js" });
    await vm.runInContext("loadConfig()", sandbox);

    assert.equal(vm.runInContext("state.configReady", sandbox), true);
    assert.equal(node("#settings-save").disabled, false);
    assert.equal(node("#llm-main-select").value, "legacy-main");
    assert.equal(vm.runInContext("state.modelsDraft[0].model", sandbox), "configured-model");
    assert.equal(vm.runInContext("state.modelsDraft[0].api_key", sandbox), "********");
    assert.equal(node("#timeout-default").value, 120);
    assert.equal(node("#context-enabled").checked, true);
    assert.equal(node("#agent-add").disabled, !supportsAgents);
    assert.equal(node("#agent-select").disabled, !supportsAgents);
    assert.equal(node("#agent-select").value, "main");
    assert.equal(node("#agent-select").children[0].textContent, "主Agent");
    if (supportsAgents) {
      assert.equal(node("#agent-select").children.length, 2);
      assert.equal(node("#agents-list").children.length, 2);
    } else {
      assert.match(node("#agents-list").children[0].textContent, /更新并重启服务/);
    }
  });
}

test("model discovery imports selected models without duplicating the list", async () => {
  const requests = [];
  const { sandbox, node } = workspaceSandbox(async (url, options) => {
    assert.equal(url, "/api/models/discover");
    requests.push(JSON.parse(options.body));
    return { ok: true, headers: { get() { return "application/json"; } }, json: async () => ({ ok: true, models: ["model-a", "model-b"] }) };
  });
  vm.runInContext("openModelEditor(); addModelHeader('X-Project', 'project-key');", sandbox);
  assert.equal(node("#model-advanced").open, false);
  node("#model-base-url").value = "https://models.invalid/v1/";
  node("#model-api-key").value = "api-secret";
  await vm.runInContext("detectModels()", sandbox);
  assert.deepEqual(requests[0].headers, { "X-Project": "project-key" });
  assert.equal(requests[0].base_url, "https://models.invalid/v1");
  assert.equal(node("#model-discovery").hidden, false);
  const choices = node("#model-discovery-list").querySelectorAll("input");
  choices[1].checked = false;
  vm.runInContext("importDetectedModels()", sandbox);
  assert.equal(vm.runInContext("state.modelsDraft.length", sandbox), 1);
  assert.equal(vm.runInContext("state.modelsDraft[0].model", sandbox), "model-a");
  assert.equal(vm.runInContext("state.modelsDraft[0].api_key", sandbox), "api-secret");
  vm.runInContext("importDetectedModels()", sandbox);
  assert.equal(vm.runInContext("state.modelsDraft.length", sandbox), 1);
});

test("model advanced options start empty and retain unsaved credentials while editing", () => {
  const { sandbox, node } = workspaceSandbox();
  vm.runInContext("openModelEditor()", sandbox);
  assert.equal(node("#model-advanced").open, false);
  assert.equal(node("#model-headers").children.length, 0);
  assert.equal(node("#model-supports-reasoning").checked, false);
  node("#model-base-url").value = "https://models.invalid/v1";
  node("#model-name").value = "reasoning-model";
  node("#model-api-key").value = "draft-secret";
  node("#model-supports-reasoning").checked = true;
  const inputs = node("#model-supported-efforts").querySelectorAll("input");
  inputs.filter((input) => ["low", "high"].includes(input.value)).forEach((input) => { input.checked = true; });
  vm.runInContext("renderModelEfforts('high'); saveModelEditor(); openModelEditor(state.modelsDraft[0]);", sandbox);
  assert.equal(node("#model-api-key").value, "draft-secret");
  assert.equal(node("#model-default-effort").value, "high");
  const model = vm.runInContext("readModelEditor()", sandbox);
  assert.equal(model.api_key, "draft-secret");
  assert.deepEqual(Array.from(model.reasoning_efforts), ["low", "high"]);
  assert.equal(model.reasoning_effort, "high");
});

test("agent model and conversation effort selectors follow the effective provider", () => {
  const { sandbox, node } = workspaceSandbox();
  vm.runInContext(`
    state.config = { llm: { main_model_id: "primary", main: { model: "main-model" }, models: [
      { id: "primary", model: "main-model", base_url: "https://main.invalid", reasoning_efforts: ["low", "high"] },
      { id: "child", model: "child-model", base_url: "https://child.invalid", reasoning_efforts: ["medium"] }
    ] } };
    state.agents = [{ id: "main", name: "主Agent" }, { id: "code", name: "代码Agent", model_id: "child" }];
    state.selectedAgentId = "code";
    renderReasoningSelector();
    openAgentEditor(state.agents[1]);
  `, sandbox);
  assert.equal(node("#agent-model").value, "child");
  assert.equal(node("#agent-model").disabled, false);
  assert.equal(node("#reasoning-effort-wrap").hidden, false);
  assert.deepEqual(node("#reasoning-effort").children.map((option) => option.value), ["", "medium"]);
  vm.runInContext('state.agents[1].model_id = ""; state.selectedReasoningEffort = "medium"; renderReasoningSelector(); openAgentEditor(state.agents[0]);', sandbox);
  assert.equal(node("#agent-model").disabled, true);
  assert.equal(vm.runInContext("state.selectedReasoningEffort", sandbox), "");
  assert.deepEqual(node("#reasoning-effort").children.map((option) => option.value), ["", "low", "high"]);
});

test("template save identifies the named three-file source folder", () => {
  const { sandbox, node } = workspaceSandbox();
  vm.runInContext(`
    state.runFiles = ["index.html", "style.css", "script.js"].map(name => ({ scope: "result", path: "展示模板/" + name }));
    renderTemplateSave({ run_id: "template", title: "制作展示模板", state: { agent_id: "template" }, status: "completed", job_active: false });
  `, sandbox);
  assert.equal(node("#template-save-name").value, "展示模板");
  assert.equal(node("#template-save-name").dataset.sourceFolder, "展示模板");
});
