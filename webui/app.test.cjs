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
    this.style = { setProperty(name, value) { this[name] = value; } };
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
  contains(node) { return node === this || this.children.some((child) => child instanceof Element && child.contains(node)); }
  remove() { if (this.parentElement) this.parentElement.children = this.parentElement.children.filter((child) => child !== this); }
  setAttribute(name, value) { this.attributes.set(name, value); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  removeAttribute(name) { this.attributes.delete(name); }
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(listener);
  }
  removeEventListener(name, listener) {
    this.listeners.set(name, (this.listeners.get(name) || []).filter((item) => item !== listener));
  }
  getBoundingClientRect() { return this.bounds || { left: 0, top: 0, right: 1000, width: 1000, height: 400 }; }
  setPointerCapture(id) { this.pointerId = id; }
  hasPointerCapture(id) { return this.pointerId === id; }
  releasePointerCapture() { this.pointerId = null; }
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
  const stored = new Map();
  const windowListeners = new Map();
  const sandbox = vm.createContext({
    window: {
      innerWidth: 1200, innerHeight: 800, location: { hash: "" },
      matchMedia() { return { matches: false, addEventListener() {} }; },
      addEventListener(type, callback) {
        if (!windowListeners.has(type)) windowListeners.set(type, []);
        windowListeners.get(type).push(callback);
      }, confirm() { return true; },
      clearTimeout() {}, setTimeout() {},
      crypto: { randomUUID() { return (++modelSerial).toString(16).padStart(32, "0"); } },
    },
    localStorage: { getItem(key) { return stored.get(key) ?? null; }, setItem(key, value) { stored.set(key, value); } },
    document: {
      documentElement: node("html"), body: node("body"),
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
    console, fetch, URL,
  });
  vm.runInContext(source, sandbox, { filename: "app.js" });
  return { sandbox, node, nav, stored, windowListeners };
}

test("workspace panels hide independently and retain recovery controls", () => {
  const { sandbox, node, stored } = workspaceSandbox();
  vm.runInContext('bindEvents(); applyInspectorState(); switchInspectorTab("instance");', sandbox);
  node("#conversation-toggle").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("conversation-hidden"), true);
  assert.equal(node("#conversation-toggle").attributes.get("aria-expanded"), "false");
  assert.equal(stored.get("webweave-conversation-hidden"), "true");
  node("#preview-toolbar-toggle").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("preview-toolbar-hidden"), true);
  vm.runInContext('switchInspectorTab("files")', sandbox);
  assert.equal(node("#workspace-grid").classList.contains("preview-toolbar-hidden"), false);
  assert.equal(node("#preview-toolbar-toggle").disabled, true);
  vm.runInContext('switchInspectorTab("instance")', sandbox);
  assert.equal(node("#workspace-grid").classList.contains("preview-toolbar-hidden"), true);
  node("#preview-toolbar-toggle").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("preview-toolbar-hidden"), false);
  node("#inspector-toggle").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("inspector-hidden"), true);
  assert.equal(node("#workspace-grid").classList.contains("conversation-hidden"), false);
  node("#conversation-toggle").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("inspector-hidden"), false);
  assert.equal(node("#workspace-grid").classList.contains("conversation-hidden"), true);
  node("#conversation-rail").listeners.get("click")[0]();
  assert.equal(node("#workspace-grid").classList.contains("conversation-hidden"), false);
  assert.equal(node("#conversation-toggle").attributes.get("aria-expanded"), "true");
});

test("workspace divider expands preview and drags the conversation closed and open", () => {
  const { sandbox, node, stored } = workspaceSandbox();
  node("#workspace-grid").bounds = { left: 60, right: 1260, width: 1200 };
  vm.runInContext("bindResize()", sandbox);
  const handle = node("#resize-handle");
  const emit = (type, clientX, pointerId = 1) => {
    const event = { type, clientX, pointerId, button: 0, preventDefault() {} };
    [...(handle.listeners.get(type) || [])].forEach((listener) => listener(event));
  };
  emit("pointerdown", 860);
  emit("pointermove", 400, 2);
  assert.equal(vm.runInContext("state.inspectorWidth", sandbox), 400);
  emit("pointermove", 400);
  assert.equal(vm.runInContext("state.inspectorWidth", sandbox), 856);
  emit("pointermove", 85);
  assert.equal(vm.runInContext("state.conversationHidden", sandbox), true);
  emit("pointermove", 500);
  assert.equal(vm.runInContext("state.conversationHidden", sandbox), false);
  assert.equal(vm.runInContext("state.inspectorWidth", sandbox), 756);
  emit("pointerup", 500);
  assert.equal(stored.get("webweave-inspector-width"), "756");
  assert.equal(node("body").classList.contains("is-resizing"), false);
  assert.equal(handle.hasPointerCapture(1), false);
  for (const type of ["pointermove", "pointerup", "pointercancel", "lostpointercapture"]) {
    assert.equal(handle.listeners.get(type).length, 0);
  }
});

test("workspace divider cancels cleanly and supports keyboard and narrow screens", () => {
  for (const endType of ["pointercancel", "lostpointercapture"]) {
    const { sandbox, node } = workspaceSandbox();
    vm.runInContext("bindResize()", sandbox);
    const handle = node("#resize-handle");
    handle.bounds = { left: 596, width: 8 };
    handle.listeners.get("pointerdown")[0]({ pointerId: 1, button: 0, preventDefault() {} });
    handle.listeners.get(endType)[0]({ type: endType, pointerId: 1 });
    assert.equal(node("body").classList.contains("is-resizing"), false);
    assert.equal(handle.listeners.get("pointermove").length, 0);
    const key = (key) => handle.listeners.get("keydown")[0]({ key, preventDefault() {} });
    key("ArrowLeft");
    assert.equal(vm.runInContext("state.inspectorWidth", sandbox), 428);
    key("Home");
    assert.equal(vm.runInContext("state.conversationHidden", sandbox), true);
    key("ArrowRight");
    assert.equal(vm.runInContext("state.conversationHidden", sandbox), false);
    key("End");
    assert.equal(vm.runInContext("state.inspectorHidden", sandbox), true);
    assert.equal(vm.runInContext("state.conversationHidden", sandbox), false);
    vm.runInContext("setInspectorHidden(false); window.innerWidth = 700;", sandbox);
    handle.listeners.get("pointerdown")[0]({ pointerId: 2, button: 0, preventDefault() {} });
    assert.equal(node("body").classList.contains("is-resizing"), false);
    key("Home");
    assert.equal(vm.runInContext("state.conversationHidden", sandbox), false);
  }
});

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

test("preview selection retains multiple targets and adds atomic cards without editing text", () => {
  const { sandbox, node } = workspaceSandbox();
  const messages = [];
  const origin = "http://127.0.0.1:8765";
  sandbox.window.location.origin = origin;
  const frame = { postMessage(message) { messages.push(message); } };
  node("#instance-frame").contentWindow = frame;
  vm.runInContext('state.activeRunId = "selection-run"; state.activeRun = { instances: [{ instance_id: "preview", revision: 2 }] }; state.selectedInstanceId = "preview"; promptInput.value = "保留这段修改要求";', sandbox);
  const selections = [
    { selection_id: "one", ordinal: 1, component: "导航", selector: "#navigation", text: "导航文字" },
    { selection_id: "two", ordinal: 2, component: "标题", selector: ".hero > h1", point: { x: 15, y: 30 } },
  ];
  const receive = (selection, mode, source = frame) => {
    sandbox.previewEvent = { origin, source, data: { type: "webweave:selection", mode, selection } };
    vm.runInContext("receivePreviewMessage(previewEvent)", sandbox);
  };
  receive(selections[0], "component", {});
  assert.equal(vm.runInContext("state.selections.length", sandbox), 0);
  receive(selections[0], "component");
  receive(selections[1], "point");
  receive(selections[0], "component");
  assert.equal(node("#selection-list").children.length, 2);
  vm.runInContext("injectSelectionIntoPrompt(); injectSelectionIntoPrompt()", sandbox);
  assert.equal(node("#prompt-input").value, "保留这段修改要求");
  assert.equal(node("#context-card-list").children.length, 2);
  assert.equal(vm.runInContext("state.selections.length", sandbox), 2);
  assert.equal(vm.runInContext("state.pendingContexts[0].instance_id", sandbox), "preview");
  assert.equal(messages.some(message => message.type === "webweave:clear-selection"), false);
  const card = node("#context-card-list").children[0];
  card.children.at(-1).listeners.get("click")[0]();
  assert.equal(vm.runInContext("state.pendingContexts.length", sandbox), 1);
  const selected = node("#selection-list").children[0];
  selected.children.at(-1).listeners.get("click")[0]();
  assert.equal(vm.runInContext("state.selections.length", sandbox), 1);
  assert.equal(messages.at(-1).type, "webweave:remove-selection");
  vm.runInContext("clearSelection()", sandbox);
  assert.equal(node("#selection-context").hidden, true);
  assert.equal(node("#selection-list").children.length, 0);
  assert.equal(node("#prompt-input").value, "保留这段修改要求");
});

test("preview selection drag accepts component and point IDs and preserves file drops", () => {
  const { sandbox, node } = workspaceSandbox();
  sandbox.contexts = [
    { selection_id: "component", type: "component", run_id: "selection-run", component: "标题", selector: "#hero" },
    { selection_id: "point", type: "point", run_id: "selection-run", component: "按钮", selector: "#button", point: { x: 80, y: 50 } },
  ];
  vm.runInContext('state.activeRunId = "selection-run"; state.selections = contexts; promptInput.value = "正文"; bindEvents(); renderSelections();', sandbox);
  const data = new Map();
  const transfer = { files: [], setData(type, value) { data.set(type, value); }, getData(type) { return data.get(type) || ""; } };
  const drop = () => node(".composer").listeners.get("drop")[0]({ preventDefault() {}, dataTransfer: transfer });
  for (const card of node("#selection-list").children) {
    card.listeners.get("dragstart")[0]({ dataTransfer: transfer });
    assert.equal(data.get("application/x-webweave-selection"), card.dataset.selectionId);
    drop();
    drop();
  }
  assert.equal(node("#context-card-list").children.length, 2);
  assert.equal(node("#prompt-input").value, "正文");
  data.set("application/x-webweave-selection", "unknown-or-cross-run");
  drop();
  assert.equal(vm.runInContext("state.pendingContexts.length", sandbox), 2);
  data.clear();
  transfer.files = [{ name: "reference.png" }];
  drop();
  assert.equal(node("#attachment-list").children.length, 1);
  assert.equal(vm.runInContext("state.pendingFiles[0].name", sandbox), "reference.png");
});

test("preview selection sends structured context and renders history as cards", async () => {
  const requests = [];
  const { sandbox, node } = workspaceSandbox(async (url, options) => {
    requests.push({ url, body: JSON.parse(options.body) });
    return { ok: true, headers: { get() { return "application/json"; } }, json: async () => ({ ok: true }) };
  });
  sandbox.contexts = [
    { selection_id: "one", type: "component", component: "导航", selector: "#navigation", run_id: "selection-run" },
    { selection_id: "two", type: "point", component: "标题", selector: ".hero > h1", point: { x: 15, y: 30 }, run_id: "selection-run" },
  ];
  vm.runInContext('state.activeRunId = "selection-run"; state.activeRun = {}; state.pendingContexts = [...contexts]; state.selections = [...contexts]; promptInput.value = "改成深色"; loadActiveRun = async () => {};', sandbox);
  await vm.runInContext("handlePrimaryAction()", sandbox);
  assert.equal(requests[0].body.content, "改成深色");
  assert.deepEqual(requests[0].body.instance_context, sandbox.contexts);
  assert.equal(vm.runInContext("state.pendingContexts.length", sandbox), 0);
  assert.equal(vm.runInContext("state.selections.length", sandbox), 2);
  const article = vm.runInContext('createUserMessage({ display_text: "改成深色", instance_context: contexts })', sandbox);
  const cards = article.querySelectorAll(".context-card");
  assert.equal(cards.length, 2);
  assert.equal(cards[0].querySelector(".context-card-copy").children[0].textContent.includes("#navigation"), false);
  assert.equal(cards[1].querySelector(".context-card-copy").children[1].textContent, "位置 (15, 30)");
  assert.equal(node("#context-card-list").children.length, 0);
});

test("preview context menu targets components, filters batch actions and clamps to the viewport", async () => {
  const { sandbox, node } = workspaceSandbox();
  const messages = [];
  const copied = [];
  const frame = { postMessage(message) { messages.push(message); } };
  node("#instance-frame").contentWindow = frame;
  sandbox.navigator = { clipboard: { async writeText(value) { copied.push(value); } } };
  sandbox.window.location.origin = "http://127.0.0.1:8765";
  sandbox.window.location.href = "http://127.0.0.1:8765/#workspace";
  vm.runInContext(`
    state.activeRunId = "menu-run";
    state.activeRun = { instances: [{ instance_id: "first", status: "running", app_url: "http://127.0.0.1:9000", preview_url: "/preview/menu-run/" }] };
    state.selectedInstanceId = "first";
    state.selections = [
      { selection_id: "one", ordinal: 1, type: "component", component: "标题", selector: "#hero", rect: { x: 20, y: 30 }, run_id: "menu-run" },
      { selection_id: "two", ordinal: 2, type: "component", run_id: "menu-run" },
      { selection_id: "point", ordinal: 3, type: "point", run_id: "menu-run" }
    ];
  `, sandbox);
  const open = (selection = "one") => {
    sandbox.menuEvent = { source: frame, origin: sandbox.window.location.origin, data: {
      type: "webweave:context-menu", x: 1199, y: 799, selection_id: selection, modified: true,
      components: ["one", "two", "point", "unknown"].map(selection_id => ({ selection_id })),
    } };
    vm.runInContext("receivePreviewMessage(menuEvent)", sandbox);
    return vm.runInContext("previewContextMenu", sandbox);
  };
  const item = (menu, label) => menu.querySelectorAll(".preview-menu-item").find(control => control.children[1].textContent === label);
  let menu = open();
  assert.equal(menu.style.left, "192px");
  assert.equal(menu.style.top, "392px");
  assert.match(menu.children[0].textContent, /组件 1.*标题/);
  const batch = item(menu, "全部已标记组件（2）");
  batch.listeners.get("click")[0]();
  const group = menu.children[menu.children.indexOf(batch) + 1];
  assert.equal(group.hidden, false);
  await item(group, "开启自由移动（临时）").listeners.get("click")[0]();
  assert.equal(messages.at(-1).action, "move");
  assert.deepEqual(Array.from(messages.at(-1).selection_ids), ["one", "two"]);
  assert.equal(messages.at(-1).enabled, true);
  menu = open();
  await item(menu, "加入对话").listeners.get("click")[0]();
  assert.equal(vm.runInContext("state.pendingContexts.length", sandbox), 1);
  menu = open();
  await item(menu, "复制定位信息").listeners.get("click")[0]();
  assert.equal(JSON.parse(copied[0]).selector, "#hero");
  menu = open("point");
  assert.equal(menu.children.some(child => child.children?.[1]?.textContent === "开启自由移动（临时）"), false);
  await item(menu, "复制预览地址").listeners.get("click")[0]();
  assert.equal(copied.at(-1), "http://127.0.0.1:8765/preview/menu-run/");
});

test("preview menu dismisses on outside clicks and reload, rejects foreign messages and disables offline actions", () => {
  const { sandbox, node, windowListeners } = workspaceSandbox();
  sandbox.window.location.origin = "http://127.0.0.1:8765";
  const frame = { postMessage() {} };
  node("#instance-frame").contentWindow = frame;
  vm.runInContext("bindPreviewMenu(); showPreviewMenu(20, 30)", sandbox);
  let menu = vm.runInContext("previewContextMenu", sandbox);
  assert.equal(menu.querySelectorAll(".preview-menu-item").filter(item => item.children[1].textContent !== "切换模式").every(item => item.disabled), true);
  windowListeners.get("pointerdown")[0]({ target: menu });
  assert.equal(vm.runInContext("previewContextMenu !== null", sandbox), true);
  windowListeners.get("pointerdown")[0]({ target: node("body") });
  assert.equal(vm.runInContext("previewContextMenu", sandbox), null);
  for (const [origin, source] of [["https://foreign.invalid", frame], [sandbox.window.location.origin, {}]]) {
    sandbox.menuEvent = { origin, source, data: { type: "webweave:context-menu", x: 20, y: 30 } };
    vm.runInContext("receivePreviewMessage(menuEvent)", sandbox);
    assert.equal(vm.runInContext("previewContextMenu", sandbox), null);
  }
  vm.runInContext("showPreviewMenu(20, 30)", sandbox);
  let prevented = false;
  windowListeners.get("keydown")[0]({ key: "Escape", preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(vm.runInContext("previewContextMenu", sandbox), null);
  sandbox.menuEvent = { origin: sandbox.window.location.origin, source: frame, data: { type: "webweave:preview-ready" } };
  vm.runInContext("showPreviewMenu(20, 30); receivePreviewMessage(menuEvent)", sandbox);
  assert.equal(vm.runInContext("previewContextMenu", sandbox), null);
});

test("preview updates selected and pending coordinates after moving and synchronizes removed markers", () => {
  const { sandbox, node } = workspaceSandbox();
  const frame = { postMessage() {} };
  node("#instance-frame").contentWindow = frame;
  sandbox.window.location.origin = "http://127.0.0.1:8765";
  vm.runInContext('state.activeRunId = "moved-run"; state.pendingContexts = [{ selection_id: "one" }];', sandbox);
  sandbox.menuEvent = { source: frame, origin: sandbox.window.location.origin, data: {
    type: "webweave:selection", mode: "component", selection: { selection_id: "one", rect: { x: 80, y: 50 } },
  } };
  vm.runInContext("receivePreviewMessage(menuEvent)", sandbox);
  assert.equal(vm.runInContext("state.pendingContexts[0].rect.x", sandbox), 80);
  assert.equal(vm.runInContext("state.selections[0].rect.y", sandbox), 50);
  sandbox.menuEvent.data = { type: "webweave:selection-removed", selection_id: "one" };
  vm.runInContext("receivePreviewMessage(menuEvent)", sandbox);
  assert.equal(vm.runInContext("state.selections.length", sandbox), 0);
  assert.equal(vm.runInContext("state.pendingContexts.length", sandbox), 1);
});

test("preview menu keyboard starts at the right edge and wraps through visible enabled actions", () => {
  const { sandbox } = workspaceSandbox();
  vm.runInContext("showPreviewMenu(20, 30)", sandbox);
  const menu = vm.runInContext("previewContextMenu", sandbox);
  const controls = [0, 1, 2].map(index => ({
    index, getClientRects() { return [1]; }, focus() { sandbox.document.activeElement = this; },
  }));
  const hidden = { getClientRects() { return []; }, focus() { throw new Error("Hidden action received focus"); } };
  menu.querySelectorAll = () => [...controls, hidden];
  const key = key => menu.listeners.get("keydown")[0]({ key, preventDefault() {} });
  key("ArrowUp");
  assert.equal(sandbox.document.activeElement.index, 2);
  key("ArrowDown");
  assert.equal(sandbox.document.activeElement.index, 0);
  key("End");
  assert.equal(sandbox.document.activeElement.index, 2);
  key("Home");
  assert.equal(sandbox.document.activeElement.index, 0);
  sandbox.document.activeElement = menu;
  key("ArrowDown");
  assert.equal(sandbox.document.activeElement.index, 0);
});

test("preview canvas blank area can restore temporary changes without a component target", async () => {
  const { sandbox, node } = workspaceSandbox();
  const messages = [];
  node("#instance-frame").contentWindow = { postMessage(message) { messages.push(message); } };
  vm.runInContext(`
    state.activeRun = { instances: [{ instance_id: "first", status: "running" }] };
    state.selectedInstanceId = "first";
    showPreviewMenu(20, 30);
  `, sandbox);
  const menu = vm.runInContext("previewContextMenu", sandbox);
  const restore = menu.querySelectorAll(".preview-menu-item").find(item => item.children[1].textContent === "还原全部临时修改");
  assert.equal(restore.disabled, false);
  await restore.listeners.get("click")[0]();
  assert.equal(messages.at(-1).action, "restore-all");
});

test("instance deletion confirms the selected target and switches to the remaining instance", async () => {
  const requests = [];
  const { sandbox, node } = workspaceSandbox(async (url, options) => {
    requests.push({ url, options });
    return { ok: true, headers: { get() { return "application/json"; } }, json: async () => ({ ok: true }) };
  });
  vm.runInContext(`
    state.activeRunId = "delete-run";
    state.activeRun = { instances: [
      { instance_id: "first", status: "stopped" },
      { instance_id: "second", status: "stopped" },
    ], instance: { instance_id: "second" } };
    state.selectedInstanceId = "first";
    loadActiveRun = async () => { state.activeRun.instances.shift(); };
    renderInstances(state.activeRun);
    bindEvents();
  `, sandbox);
  assert.equal(node("#instance-delete").disabled, false);
  sandbox.window.confirm = () => false;
  await node("#instance-delete").listeners.get("click")[0]();
  assert.equal(requests.length, 0);
  sandbox.window.confirm = () => true;
  await node("#instance-delete").listeners.get("click")[0]();
  assert.equal(requests[0].url, "/api/runs/delete-run/instance");
  assert.deepEqual(JSON.parse(requests[0].options.body), { action: "delete", instance_id: "first", timeout: 30 });
  assert.equal(vm.runInContext("state.selectedInstanceId", sandbox), "second");
  assert.equal(vm.runInContext("state.instanceBusy", sandbox), false);
  assert.equal(node("#instance-delete").disabled, false);
  vm.runInContext(`
    state.activeRun = { instances: [], instance: null };
    renderInstances(state.activeRun);
  `, sandbox);
  assert.equal(vm.runInContext("state.selectedInstanceId", sandbox), null);
  assert.equal(node("#instance-delete").disabled, true);
  assert.equal(node("#instance-empty").hidden, false);
  await vm.runInContext('instanceAction("delete")', sandbox);
  assert.equal(requests.length, 1);
});

test("instance deletion disables repeated actions and recovers after failure", async () => {
  let requests = 0;
  let fail;
  const { sandbox, node } = workspaceSandbox(() => {
    requests++;
    return new Promise((resolve, reject) => { fail = reject; });
  });
  vm.runInContext(`
    state.activeRunId = "delete-run";
    state.activeRun = { instances: [{ instance_id: "first", status: "running" }] };
    state.selectedInstanceId = "first";
  `, sandbox);
  const pending = vm.runInContext('instanceAction("delete")', sandbox);
  assert.equal(node("#instance-delete").disabled, true);
  await vm.runInContext('instanceAction("delete")', sandbox);
  assert.equal(requests, 1);
  fail(new Error("停止失败"));
  await assert.rejects(pending, /停止失败/);
  assert.equal(vm.runInContext("state.instanceBusy", sandbox), false);
  assert.equal(vm.runInContext("state.selectedInstanceId", sandbox), "first");
  assert.equal(node("#instance-delete").disabled, false);
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
