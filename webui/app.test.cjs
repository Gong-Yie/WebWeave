const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8")
  .replace(/initialize\(\);\s*$/, "");

// Only the DOM operations used during configuration loading are needed here.
class Element {
  constructor() {
    this.children = [];
    this.dataset = {};
    this.lastChild = { textContent: "" };
    this.classList = { add() {}, remove() {} };
  }

  append(...children) {
    this.children.push(...children);
    this.lastChild = this.children.at(-1);
  }

  replaceChildren(...children) {
    this.children = [];
    this.append(...children);
  }

  querySelectorAll() { return []; }
  setAttribute() {}
  addEventListener() {}
}

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
    const controls = ["#settings-save", "#llm-main-model", "#timeout-default", "#context-enabled"];
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
    assert.equal(node("#llm-main-model").value, "configured-model");
    assert.equal(node("#llm-main-api-key").value, "********");
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
