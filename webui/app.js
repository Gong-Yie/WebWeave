const state = {
  runs: [],
  activeRunId: null,
  activeRun: null,
  action: "idle",
  pendingFiles: [],
  pendingContexts: [],
  selections: [],
  currentFileText: "",
  renderedRunKey: "",
  frameKey: "",
  loadSequence: 0,
  inspectorWidth:
    Number(localStorage.getItem("webweave-inspector-width")) || 400,
  inspectorHidden:
    window.innerWidth <= 720 ||
    localStorage.getItem("webweave-inspector-hidden") === "true",
  conversationHidden: localStorage.getItem("webweave-conversation-hidden") === "true",
  previewToolbarHidden: localStorage.getItem("webweave-preview-toolbar-hidden") === "true",
  inspectorTab: "activity",
  sidebarCollapsed:
    window.innerWidth <= 960 ||
    localStorage.getItem("webweave-sidebar-collapsed") === "true",
  inspectMode: "preview",
  config: null,
  configReady: false,
  settingsPage: "config",
  skills: [],
  skillErrors: [],
  agents: [],
  selectedAgentId: "main",
  selectedReasoningEffort: "",
  editingAgentId: null,
  modelsDraft: [],
  mainModelId: "",
  visionModelId: "",
  editingModelId: null,
  modelDiscovery: [],
  runFiles: [],
  templates: [],
  reasoningOpen: new Map(),
  selectedSubagentId: null,
  knownSubagentIds: null,
  instanceBusy: false,
  selectedInstanceId: null,
  appearance: {
    theme: localStorage.getItem("webweave-theme") || "system",
    backgroundUrl: "",
  },
};

const $ = (selector) => document.querySelector(selector);
const appShell = $(".app-shell");
const workspaceGrid = $("#workspace-grid");
const sidebarToggle = $("#sidebar-toggle");
const inspectorToggle = $("#inspector-toggle");
const conversationToggle = $("#conversation-toggle");
const previewToolbarToggle = $("#preview-toolbar-toggle");
const inspectorRail = $("#inspector-rail");
const closeInspector = $("#close-inspector");
const resizeHandle = $("#resize-handle");
const promptInput = $("#prompt-input");
const primaryAction = $("#primary-action");
const runStatus = $(".run-status");
const attachmentList = $("#attachment-list");
const fileInput = $("#file-input");
const composer = $(".composer");
const toast = $("#toast");
const instanceFrame = $("#instance-frame");
const selectionContext = $("#selection-context");
const previewSelectionMime = "application/x-webweave-selection";
let previewContextMenu = null;

function refreshIcons() {
  if (window.lucide) window.lucide.createIcons();
}

function element(tag, className = "", text = "") {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text) node.textContent = text;
  return node;
}

function icon(name) {
  const node = document.createElement("i");
  node.dataset.lucide = name;
  return node;
}

async function api(path, options = {}) {
  const request = { ...options, headers: { ...(options.headers || {}) } };
  if (request.body && typeof request.body === "string") {
    request.headers["Content-Type"] = "application/json";
  }
  const response = await fetch(path, request);
  const contentType = response.headers.get("Content-Type") || "";
  const payload = contentType.includes("application/json")
    ? await response.json()
    : null;
  if (!response.ok || payload?.ok === false) {
    throw new Error(payload?.error?.message || `请求失败 (${response.status})`);
  }
  return payload;
}

function showToast(message) {
  toast.textContent = message;
  toast.classList.add("is-visible");
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(
    () => toast.classList.remove("is-visible"),
    2600,
  );
}

function setConnection(connected) {
  $("#connection-dot").classList.toggle("is-offline", !connected);
  $("#connection-label").textContent = connected ? "已连接" : "连接失败";
}

function switchView(viewName) {
  if (viewName === "settings") $("#nav-settings").open = true;
  document.querySelectorAll(".nav-item").forEach((item) => {
    const active =
      item.dataset.view === viewName &&
      (viewName !== "settings" || item.dataset.settingsPage === state.settingsPage);
    item.classList.toggle("is-active", active);
    if (active) item.setAttribute("aria-current", "page");
    else item.removeAttribute("aria-current");
  });
  document.querySelectorAll(".view").forEach((view) => {
    view.classList.toggle("is-active", view.id === `${viewName}-view`);
  });
  $("#console-page-title").textContent =
    viewName === "settings"
      ? $("#settings-title").textContent
      : { home: "仪表盘", workspace: "工作台", templates: "前端模板", skills: "Skills", agents: "Agent 管理" }[viewName];
  if (viewName === "workspace") renderWorkspaceState();
  if (viewName === "templates") runSafely(loadTemplates);
  const settingsHash = state.settingsPage === "config"
    ? "settings"
    : `settings/${state.settingsPage}`;
  window.location.hash = viewName === "settings" ? settingsHash : viewName;
}

function switchSettingsPage(pageName) {
  const isStyle = pageName === "style";
  const pageMeta = {
    config: ["配置管理", "运行配置", "管理工具调用、超时与上下文压缩，保存后自动热加载。"],
    llm: ["配置管理", "模型配置", "配置主模型和可选的视觉模型。"],
    style: ["配置管理", "外观设置", "主题与背景更改会自动保存。"],
  };
  state.settingsPage = pageName;
  const [eyebrow, title, description] = pageMeta[pageName] || pageMeta.config;
  document.querySelectorAll(".settings-page").forEach((page) => {
    page.classList.toggle("is-active", page.id === `settings-${pageName}-page`);
  });
  $("#settings-eyebrow").textContent = eyebrow;
  $("#settings-title").textContent = title;
  $("#settings-description").textContent = description;
  $(".settings-actions").hidden = isStyle;
  switchView("settings");
}

function resolvedTheme() {
  if (state.appearance.theme !== "system") return state.appearance.theme;
  return window.matchMedia("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

function applyAppearance() {
  document.documentElement.dataset.theme = resolvedTheme();
  document.documentElement.classList.toggle(
    "has-background-image",
    Boolean(state.appearance.backgroundUrl),
  );
  document.documentElement.style.setProperty(
    "--app-background-image",
    state.appearance.backgroundUrl
      ? `url(${JSON.stringify(state.appearance.backgroundUrl)})`
      : "none",
  );
  document.querySelectorAll('input[name="appearance-theme"]').forEach((input) => {
    input.checked = input.value === state.appearance.theme;
  });
  const previewImage = $("#background-preview-image");
  const hasBackground = Boolean(state.appearance.backgroundUrl);
  previewImage.hidden = !hasBackground;
  previewImage.src = hasBackground ? state.appearance.backgroundUrl : "";
  $("#background-empty").hidden = hasBackground;
  $("#background-remove").disabled = !hasBackground;
}

function setTheme(theme) {
  state.appearance.theme = theme;
  localStorage.setItem("webweave-theme", theme);
  applyAppearance();
}

async function loadAppearance() {
  const payload = await api("/api/appearance");
  state.appearance.backgroundUrl = payload.appearance.background_url || "";
  applyAppearance();
}

async function setBackgroundImage(file) {
  const supportedTypes = ["image/png", "image/jpeg", "image/webp"];
  if (!supportedTypes.includes(file.type)) {
    showToast("请选择 PNG、JPG 或 WebP 图片");
    return;
  }
  if (file.size > 2 * 1024 * 1024) {
    showToast("背景图片不能超过 2 MB");
    return;
  }
  const payload = await api(
    `/api/appearance/background?name=${encodeURIComponent(file.name)}`,
    {
      method: "PUT",
      body: file,
    },
  );
  state.appearance.backgroundUrl = payload.appearance.background_url || "";
  applyAppearance();
  showToast("背景图已保存到 data/background");
}

async function removeBackgroundImage() {
  const payload = await api("/api/appearance/background", {
    method: "DELETE",
  });
  state.appearance.backgroundUrl = payload.appearance.background_url || "";
  applyAppearance();
  showToast("背景图已移除");
}

function applySidebarState() {
  appShell.classList.toggle("sidebar-collapsed", state.sidebarCollapsed);
  const label = state.sidebarCollapsed ? "展开侧栏" : "收起侧栏";
  sidebarToggle.replaceChildren(
    icon(state.sidebarCollapsed ? "panel-left-open" : "panel-left-close"),
  );
  sidebarToggle.title = label;
  sidebarToggle.setAttribute("aria-label", label);
  sidebarToggle.setAttribute("aria-expanded", String(!state.sidebarCollapsed));
  localStorage.setItem(
    "webweave-sidebar-collapsed",
    String(state.sidebarCollapsed),
  );
  refreshIcons();
}

function applyInspectorState() {
  if (state.inspectorHidden) state.conversationHidden = false;
  document.documentElement.style.setProperty(
    "--inspector-width",
    `${state.inspectorWidth}px`,
  );
  workspaceGrid.classList.toggle("inspector-hidden", state.inspectorHidden);
  workspaceGrid.classList.toggle("conversation-hidden", state.conversationHidden);
  workspaceGrid.classList.toggle("preview-toolbar-hidden", state.previewToolbarHidden && state.inspectorTab === "instance");
  const label = state.inspectorHidden ? "展开执行检查区" : "隐藏执行检查区";
  inspectorToggle.replaceChildren(
    icon(state.inspectorHidden ? "panel-right-open" : "panel-right-close"),
  );
  inspectorToggle.title = label;
  inspectorToggle.setAttribute("aria-label", label);
  inspectorToggle.setAttribute("aria-expanded", String(!state.inspectorHidden));
  const conversationLabel = state.conversationHidden ? "展开对话" : "隐藏对话";
  conversationToggle.replaceChildren(icon(state.conversationHidden ? "panel-left-open" : "panel-left-close"));
  conversationToggle.title = conversationLabel;
  conversationToggle.setAttribute("aria-label", conversationLabel);
  conversationToggle.setAttribute("aria-expanded", String(!state.conversationHidden));
  const toolbarLabel = state.previewToolbarHidden ? "展开预览工具栏" : "隐藏预览工具栏";
  previewToolbarToggle.replaceChildren(icon(state.previewToolbarHidden ? "panel-top-open" : "panel-top-close"));
  previewToolbarToggle.title = toolbarLabel;
  previewToolbarToggle.setAttribute("aria-label", toolbarLabel);
  previewToolbarToggle.setAttribute("aria-expanded", String(!state.previewToolbarHidden));
  previewToolbarToggle.disabled = state.inspectorHidden || state.inspectorTab !== "instance";
  updateResizeValue();
  localStorage.setItem(
    "webweave-inspector-hidden",
    String(state.inspectorHidden),
  );
  localStorage.setItem("webweave-conversation-hidden", String(state.conversationHidden));
  localStorage.setItem("webweave-preview-toolbar-hidden", String(state.previewToolbarHidden));
  refreshIcons();
}

function setInspectorHidden(hidden) {
  state.inspectorHidden = hidden;
  applyInspectorState();
}

function setConversationHidden(hidden) {
  state.conversationHidden = hidden;
  if (hidden) state.inspectorHidden = false;
  applyInspectorState();
}

function switchInspectorTab(tabName) {
  state.inspectorTab = tabName;
  document.querySelectorAll(".inspector-tab").forEach((tab) => {
    tab.classList.toggle("is-active", tab.dataset.tab === tabName);
  });
  document.querySelectorAll(".inspector-panel").forEach((panel) => {
    panel.classList.toggle("is-active", panel.id === `${tabName}-panel`);
  });
  applyInspectorState();
}

function formatTime(value) {
  if (!value) return "尚未运行";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("zh-CN", {
    month: "numeric",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

function statusLabel(status) {
  return {
    waiting: "等待输入",
    running: "正在执行",
    stopping: "正在停止",
    stopped: "已停止",
    completed: "已完成",
    failed: "执行失败",
  }[status] || status || "等待输入";
}

function renderRuns() {
  const history = $("#history-list");
  const recent = $("#recent-run-list");
  history.replaceChildren();
  recent.replaceChildren();
  $("#recent-count").textContent = `${state.runs.length} 个对话`;
  renderDashboardRuns();

  if (!state.runs.length) {
    history.append(element("div", "empty-list", "暂无对话"));
    recent.append(element("div", "empty-panel", "还没有任务，点击“新建对话”开始构建网页。"));
    return;
  }

  state.runs.forEach((run, index) => {
    const item = element("div", "history-item");
    item.classList.toggle("is-active", run.run_id === state.activeRunId);
    const openButton = element("button", "history-open");
    openButton.type = "button";
    const title = element("strong", "history-prompt", run.title);
    const meta = element(
      "span",
      "",
      `${statusLabel(run.status)} · ${formatTime(run.updated_at)}`,
    );
    openButton.append(title, meta);
    openButton.addEventListener("click", () => activateRun(run.run_id));
    const deleteButton = element("button", "history-delete icon-button subtle danger");
    deleteButton.type = "button";
    deleteButton.title = "删除对话";
    deleteButton.setAttribute("aria-label", `删除对话：${run.title}`);
    deleteButton.append(icon("trash-2"));
    deleteButton.addEventListener("click", (event) => {
      event.stopPropagation();
      runSafely(() => deleteRun(run.run_id));
    });
    item.append(openButton, deleteButton);
    if (run.run_id === state.activeRunId && state.activeRun) {
      const tasks = collectSubagents(state.activeRun);
      if (tasks.length) {
        const tree = element("div", "subagent-tree");
        tasks.forEach((task) => {
          const child = element("button", "subagent-row");
          child.type = "button";
          child.dataset.status = task.status;
          child.title = `${task.task}\n${task.reason || task.activity}`;
          child.append(element("span", "subagent-dot"));
          const copy = element("span", "subagent-copy");
          copy.append(element("strong", "", task.name), element("small", "", task.activity));
          child.append(copy, element("span", "subagent-status", statusLabel(task.status)));
          child.addEventListener("click", () => {
            switchView("workspace");
            openSubagent(task.id);
          });
          tree.append(child);
        });
        item.append(tree);
      }
    }
    history.append(item);

    if (index < 6) {
      const recentItem = element("button");
      recentItem.type = "button";
      const mark = element("span", `recent-run-icon tone-${index % 3}`);
      mark.append(icon(run.has_instance ? "monitor-up" : "layout-template"));
      const copy = element("span");
      copy.append(
        element("strong", "", run.title),
        element(
          "small",
          "",
          run.run_id,
        ),
      );
      recentItem.append(
        mark,
        copy,
        element("span", `recent-status status-${run.status}`, statusLabel(run.status)),
        element("time", "", formatTime(run.updated_at)),
        icon("chevron-right"),
      );
      recentItem.addEventListener("click", () => activateRun(run.run_id));
      recent.append(recentItem);
    }
  });
  refreshIcons();
}

function renderDashboardRuns() {
  const groups = [
    { label: "正在执行", tone: "running", statuses: ["running", "stopping"] },
    { label: "已完成", tone: "completed", statuses: ["completed"] },
    { label: "执行失败", tone: "failed", statuses: ["failed"] },
    { label: "等待 / 已停止", tone: "waiting", statuses: ["waiting", "stopped"] },
  ];
  const counts = groups.map((group) =>
    state.runs.filter((run) => group.statuses.includes(run.status)).length,
  );
  $("#dashboard-total").textContent = state.runs.length;
  ["running", "completed", "failed"].forEach((name, index) => {
    $(`#dashboard-${name}`).textContent = counts[index];
  });
  const bar = $("#dashboard-status-bar");
  const list = $("#dashboard-status-list");
  bar.replaceChildren();
  list.replaceChildren();
  groups.forEach((group, index) => {
    const count = counts[index];
    if (count) {
      const segment = element("span", `status-segment status-${group.tone}`);
      segment.style.width = `${(count / state.runs.length) * 100}%`;
      segment.title = `${group.label}：${count}`;
      bar.append(segment);
    }
    const row = element("div");
    const label = element("span");
    label.append(
      element("i", `status-dot status-${group.tone}`),
      document.createTextNode(group.label),
    );
    row.append(label, element("strong", "", String(count)));
    list.append(row);
  });
}

async function loadRuns() {
  const payload = await api("/api/runs");
  state.runs = payload.runs;
  renderRuns();
}

function renderSkills() {
  $("#dashboard-skills").textContent = state.skills.length;
  const list = $("#skills-list");
  const errors = $("#skills-errors");
  list.replaceChildren();
  errors.replaceChildren();
  if (!state.skills.length) {
    list.append(element("div", "empty-panel", "暂无可用 Skills"));
  } else {
    state.skills.forEach((skill) => {
      const item = element("article", "skill-item");
      item.append(
        element("div", "skill-item-heading", skill.name),
        element("p", "skill-description", skill.description),
        element("code", "skill-path", `data/skills/${skill.path}`),
      );
      list.append(item);
    });
  }
  if (state.skillErrors.length) {
    const heading = element("strong", "skills-errors-heading", "未加载的 Skill");
    errors.append(heading);
    state.skillErrors.forEach((message) => {
      errors.append(element("div", "skills-error", message));
    });
  }
  refreshIcons();
}

async function loadSkills() {
  const payload = await api("/api/skills");
  state.skills = Array.isArray(payload.skills) ? payload.skills : [];
  state.skillErrors = Array.isArray(payload.errors) ? payload.errors : [];
  renderSkills();
}

async function deleteRun(runId) {
  if (!window.confirm("确定删除这个对话吗？对话记录和相关文件都会被删除。")) return;
  await api(`/api/runs/${encodeURIComponent(runId)}`, { method: "DELETE" });
  const wasActive = state.activeRunId === runId;
  if (wasActive) {
    state.loadSequence += 1;
    state.activeRunId = null;
    state.activeRun = null;
    state.renderedRunKey = "";
    state.frameKey = "";
    state.pendingFiles = [];
    state.pendingContexts = [];
    state.selections = [];
    selectionContext.hidden = true;
    renderPendingContext();
    switchView("workspace");
  }
  await loadRuns();
  showToast("对话已删除");
}

async function createRun(agentId = "main") {
  const payload = await api("/api/runs", { method: "POST", body: JSON.stringify({ agent_id: agentId }) });
  await loadRuns();
  await activateRun(payload.run.run_id);
  promptInput.focus();
}

async function activateRun(runId) {
  state.activeRunId = runId;
  state.selectedInstanceId = null;
  state.selectedSubagentId = null;
  state.knownSubagentIds = null;
  state.runFiles = [];
  state.renderedRunKey = "";
  state.frameKey = "";
  state.selections = [];
  state.pendingFiles = [];
  state.pendingContexts = [];
  promptInput.value = "";
  renderPendingContext();
  selectionContext.hidden = true;
  renderRuns();
  switchView("workspace");
  await loadActiveRun(true);
  state.selectedAgentId = state.activeRun?.state?.agent_id || "main";
  state.selectedReasoningEffort = state.activeRun?.state?.reasoning_effort || "";
  renderAgentSelector();
}

async function loadActiveRun(force = false) {
  if (!state.activeRunId) return;
  const sequence = ++state.loadSequence;
  const payload = await api(`/api/runs/${encodeURIComponent(state.activeRunId)}`);
  if (sequence !== state.loadSequence || payload.run.run_id !== state.activeRunId) {
    return;
  }
  state.activeRun = payload.run;
  renderWorkspaceState();
  renderContextMeter(payload.run);
  renderTemplateSave(payload.run);
  const key = [
    payload.run.event_count,
    payload.run.status,
    payload.run.instance?.revision || 0,
    payload.run.instance?.status || "none",
  ].join(":");
  renderRunHeader(payload.run);
  updateActionFromRun(payload.run);
  renderInstances(payload.run);
  if (force || key !== state.renderedRunKey) {
    state.renderedRunKey = key;
    renderConversation(payload.run);
    renderSubagents(payload.run);
    renderActivity(payload.run);
    await loadFiles();
    await loadRuns();
  }
}

function renderRunHeader(run) {
  $("#active-run-title").textContent = run.title;
  $("#active-run-id").textContent = run.run_id;
  $("#fact-run-id").textContent = run.run_id;
  $("#files-run-id").textContent = run.run_id;
}

function renderWorkspaceState() {
  const empty = !state.activeRunId;
  $("#workspace-empty").hidden = !empty;
  $(".workspace-header").hidden = empty;
  workspaceGrid.hidden = empty;
}

function renderContextMeter(run) {
  const usage = run.context_usage;
  const meter = $("#context-meter");
  if (!usage) {
    $("#context-meter-label").textContent = "等待模型请求";
    meter.setAttribute("aria-valuenow", "0");
    $("#token-ring-value").style.strokeDasharray = "0 100";
    return;
  }
  const percentage = Math.min(100, Math.max(0, usage.input_tokens / usage.threshold * 100));
  const source = usage.estimated ? "估算" : "API";
  const label = `${source} ${usage.input_tokens.toLocaleString()} / ${usage.threshold.toLocaleString()} Token`;
  $("#context-meter-label").textContent = label;
  meter.title = `${usage.agent_name || "当前对话"}${usage.delegated ? " · 子 Agent" : ""} · 最近一次 API 输入${usage.estimated ? "（含指令和工具定义的估算值）" : "（接口返回值）"} · 压缩阈值${usage.compression_enabled ? "" : "（自动压缩已关闭）"}`;
  meter.dataset.level = percentage >= 90 ? "high" : "normal";
  meter.setAttribute("aria-valuenow", String(Math.round(percentage)));
  meter.setAttribute("aria-valuetext", label);
  $("#token-ring-value").style.strokeDasharray = `${percentage} 100`;
}

function collectSubagents(run) {
  const tasks = new Map();
  for (const event of run.events) {
    const payload = event.payload || {};
    const id = payload.subagent_id;
    if (!id) continue;
    if (event.type === "subagent_started") {
      tasks.set(id, { id, name: payload.agent_name, task: payload.task, status: "running", activity: "等待模型响应" });
    }
    const task = tasks.get(id);
    if (!task) continue;
    if (event.type === "tool_started") task.activity = `调用 ${payload.tool_name}${toolArgumentHint(payload)}`;
    if (event.type === "tool_output") task.activity = `已执行 ${payload.tool_name}`;
    if (event.type === "context_usage" && payload.estimated) task.activity = "等待模型响应";
    if (event.type === "model_reasoning_delta") task.activity = "正在思考";
    if (event.type === "model_output_delta") task.activity = "正在生成";
    if (event.type === "subagent_finished") {
      task.status = payload.status;
      task.reason = payload.reason;
      task.activity = payload.reason || (payload.status === "completed" ? "结果已返回主 Agent" : statusLabel(payload.status));
    }
  }
  for (const task of tasks.values()) {
    if (task.status === "running" && !run.job_active) {
      task.status = "stopped";
      task.activity = "执行已中断";
    }
  }
  return [...tasks.values()];
}

function openSubagent(taskId) {
  if (!state.activeRun) return;
  state.selectedSubagentId = taskId;
  setInspectorHidden(false);
  switchInspectorTab("subagents");
  renderSubagents(state.activeRun);
}

function renderSubagents(run) {
  const tasks = collectSubagents(run);
  const newest = tasks.findLast((task) => task.status === "running" && !state.knownSubagentIds?.has(task.id));
  const shouldOpen = newest && (state.knownSubagentIds !== null || run.job_active);
  state.knownSubagentIds = new Set(tasks.map((task) => task.id));
  if (shouldOpen) state.selectedSubagentId = newest.id;
  if (!tasks.some((task) => task.id === state.selectedSubagentId)) {
    state.selectedSubagentId = tasks.at(-1)?.id || null;
  }
  if (shouldOpen) {
    setInspectorHidden(false);
    switchInspectorTab("subagents");
  }
  $("#subagent-count").textContent = String(tasks.length);
  $("#subagent-empty").hidden = tasks.length > 0;
  $("#subagent-session").hidden = tasks.length === 0;
  const sessions = $("#subagent-sessions");
  const previousSessionScroll = sessions.scrollLeft;
  const focusedSession = sessions.contains(document.activeElement) ? document.activeElement.id : null;
  sessions.replaceChildren();
  tasks.forEach((task, index) => {
    const button = element("button", "subagent-session-tab");
    button.type = "button";
    button.id = `subagent-tab-${task.id}`;
    button.setAttribute("role", "tab");
    button.setAttribute("aria-selected", String(task.id === state.selectedSubagentId));
    button.setAttribute("aria-controls", "subagent-conversation");
    button.dataset.status = task.status;
    button.title = task.task;
    button.append(element("span", "subagent-dot"), document.createTextNode(`${task.name} · ${index + 1}`));
    button.addEventListener("click", () => openSubagent(task.id));
    sessions.append(button);
  });
  sessions.scrollLeft = previousSessionScroll;
  if (focusedSession) document.getElementById(focusedSession)?.focus({ preventScroll: true });
  if (shouldOpen) document.getElementById(`subagent-tab-${state.selectedSubagentId}`)?.scrollIntoView({ block: "nearest", inline: "nearest" });
  const selected = tasks.find((task) => task.id === state.selectedSubagentId);
  if (selected) {
    $("#subagent-name").textContent = selected.name;
    const badge = $("#subagent-session-status");
    badge.textContent = statusLabel(selected.status);
    badge.dataset.status = selected.status;
    $("#subagent-conversation").setAttribute("aria-labelledby", `subagent-tab-${selected.id}`);
    renderConversation(run, selected);
  } else {
    $("#subagent-conversation").replaceChildren();
  }
  refreshIcons();
}

function createSubagentCard(task) {
  const card = element("button", "subagent-task-card");
  card.type = "button";
  card.dataset.status = task.status;
  card.append(icon("messages-square"));
  const copy = element("span", "subagent-task-copy");
  copy.append(element("strong", "", task.name), element("small", "", task.task));
  const status = element("span", "subagent-task-status");
  status.append(element("span", "subagent-dot"), document.createTextNode(statusLabel(task.status)));
  card.append(copy, status, icon("chevron-right"));
  card.setAttribute("aria-label", `打开${task.name}的独立会话：${task.task}`);
  card.addEventListener("click", () => openSubagent(task.id));
  return card;
}

async function loadTemplates() {
  const payload = await api("/api/templates");
  state.templates = payload.templates;
  const list = $("#templates-list");
  list.replaceChildren();
  if (!state.templates.length) {
    const empty = element("div", "template-empty");
    empty.append(icon("layout-template"), element("h2", "", "还没有保存的模板"), element("p", "", "创建模板会开启专用对话，由前端模板制作 Agent 完成。检查结果并确认保存后，模板会出现在这里。"));
    list.append(empty);
  }
  [...state.templates].reverse().forEach((template) => {
    const card = element("article", "template-card");
    const mark = element("div", "template-card-mark");
    mark.append(icon("panels-top-left"), element("span", "", `${template.file_count} 个文件`));
    card.append(mark, element("h2", "", template.name), element("p", "", formatTime(template.created_at)), element("code", "template-path", template.path));
    const open = element("button", "text-button", "打开制作对话");
    open.type = "button";
    open.disabled = !state.runs.some((run) => run.run_id === template.run_id);
    open.title = open.disabled ? "原对话已删除，模板文件仍保留在模板库" : "查看制作过程与原始预览";
    open.addEventListener("click", () => runSafely(() => activateRun(template.run_id)));
    card.append(open);
    list.append(card);
  });
  refreshIcons();
}

function renderTemplateSave(run) {
  const ready = run.state?.agent_id === "template" && run.status === "completed" && !run.job_active;
  $("#template-save-form").hidden = !ready;
  const input = $("#template-save-name");
  if (input.dataset.runId !== run.run_id) {
    input.dataset.runId = run.run_id;
    input.value = run.title.slice(0, 120);
    delete input.dataset.sourceFolder;
  }
  if (ready) {
    const resultFiles = state.runFiles.filter((file) => file.scope === "result");
    const folders = [...new Set(resultFiles.map((file) => file.path.split("/")[0]))];
    const candidates = folders.filter((folder) => {
      const files = resultFiles.filter((file) => file.path.startsWith(folder + "/"));
      return files.length === 3 && ["index.html", "style.css", "script.js"].every((name) => files.some((file) => file.path === `${folder}/${name}`));
    });
    if (candidates.length === 1) {
      input.dataset.sourceFolder = candidates[0];
      if (input.value === run.title.slice(0, 120)) input.value = candidates[0];
    }
  }
}

async function saveActiveTemplate() {
  const run = state.activeRun;
  const name = $("#template-save-name").value.trim();
  if (!name || !run || run.job_active) return;
  if (!window.confirm(`确认将“${name}”保存到 data/templates/${name}？请先检查预览和三个模板文件；同名模板不会被覆盖。`)) return;
  const button = $("#template-save");
  button.disabled = true;
  try {
    const request = { name, approved: true };
    if ($("#template-save-name").dataset.sourceFolder) request.source_folder = $("#template-save-name").dataset.sourceFolder;
    await api(`/api/runs/${encodeURIComponent(run.run_id)}/template`, { method: "POST", body: JSON.stringify(request) });
    await loadTemplates();
    showToast("模板已保存到模板库");
  } finally {
    button.disabled = false;
  }
}

function conversationIntro(run) {
  const intro = element("div", "conversation-intro");
  const introIcon = element("span", "intro-icon");
  introIcon.append(icon("wand-sparkles"));
  const title = element("h1", "", run.title);
  const idLine = element("p");
  idLine.append("运行 ID：", element("code", "", run.run_id));
  intro.append(introIcon, title, idLine);
  return intro;
}

function renderConversation(run, task = null) {
  const container = task ? $("#subagent-conversation") : $("#conversation-scroll");
  const conversationId = `${run.run_id}:${task?.id || "main"}`;
  const switched = container.dataset.conversationId !== conversationId;
  const shouldStick =
    switched || container.scrollHeight - container.scrollTop - container.clientHeight < 100;
  const previousScroll = container.scrollTop;
  container.dataset.conversationId = conversationId;
  const intro = task ? createUserMessage({ display_text: task.task }) : conversationIntro(run);
  if (task) intro.querySelector(".message-author").textContent = "主 Agent · 委派任务";
  container.replaceChildren(intro);
  // A delegation entry belongs in the parent; its transcript belongs only in its own session.
  const events = run.events.filter((event) => task
    ? event.payload?.subagent_id === task.id
    : event.type === "subagent_started" || (!event.payload?.delegated && !event.payload?.subagent_id));
  const tasks = task ? [] : collectSubagents(run);
  let toolEvents = [];

  const flushTools = () => {
    if (!toolEvents.length) return;
    container.append(createActivityCard(toolEvents));
    toolEvents = [];
  };
  let streamedText = "";
  let streamedAgent = "WebWeave";
  let streamedDelegated = false;
  let streamedSubagent = "";
  let reasoningBlock = null;
  const reasoningParts = new Map();

  const showReasoning = (text, payload, key) => {
    if (!reasoningBlock) {
      const block = element("details", "reasoning-block");
      block.dataset.subagentId = payload.subagent_id || "";
      const openKey = `${conversationId}:${key}`;
      block.open = state.reasoningOpen.get(openKey) ?? run.job_active;
      const summary = element("summary");
      summary.append(icon("chevron-right"), element("span", "", `${payload.agent_name || "WebWeave"} · 思考过程`));
      const body = element("div", "reasoning-body");
      block.append(summary, body);
      block.addEventListener("toggle", () => {
        state.reasoningOpen.set(openKey, block.open);
      });
      container.append(block);
      reasoningBlock = body;
    }
    reasoningBlock.textContent = text;
  };

  const flushStreamedText = () => {
    if (!streamedText.trim()) {
      streamedText = "";
      return;
    }
    container.append(createAgentMessage(streamedText, streamedAgent, streamedDelegated, streamedSubagent));
    streamedText = "";
  };

  events.forEach((event, index) => {
    if (!task && event.type === "subagent_started") {
      flushTools();
      flushStreamedText();
      reasoningBlock = null;
      reasoningParts.clear();
      const delegated = tasks.find((entry) => entry.id === event.payload?.subagent_id);
      if (delegated) container.append(createSubagentCard(delegated));
      return;
    }
    if (event.type === "model_reasoning_delta") {
      flushTools();
      flushStreamedText();
      const payload = event.payload || {};
      const key = payload.reasoning_key || "reasoning";
      reasoningParts.set(key, (reasoningParts.get(key) || "") + (payload.delta || ""));
      showReasoning([...reasoningParts.values()].join("\n\n"), payload, index);
      return;
    }
    if (event.type === "tool_output") {
      flushStreamedText();
      toolEvents.push(event);
      return;
    }
    if (event.type === "user_message") {
      flushTools();
      flushStreamedText();
      container.append(createUserMessage(event.payload || {}));
      return;
    }
    if (event.type === "model_output_delta") {
      flushTools();
      const agentName = event.payload?.agent_name || "WebWeave";
      if (agentName !== streamedAgent) flushStreamedText();
      streamedAgent = agentName;
      streamedDelegated = Boolean(event.payload?.delegated);
      streamedSubagent = event.payload?.subagent_id || "";
      const delta = event.payload?.delta;
      if (typeof delta === "string") streamedText += delta;
      return;
    }
    if (event.type === "model_output") {
      const reasoning = (event.payload?.items || []).filter((item) => item.type === "reasoning").map((item) => {
        const parts = item.summary?.length ? item.summary : item.content;
        return (parts || []).map((part) => part.text || "").filter(Boolean).join("\n\n");
      }).filter(Boolean).join("\n\n");
      if (reasoning) showReasoning(reasoning, event.payload || {}, index);
      reasoningBlock = null;
      reasoningParts.clear();
      const text = event.payload?.output_text;
      flushTools();
      if (typeof text === "string" && text.trim()) {
        streamedText = text;
      }
      streamedAgent = event.payload?.agent_name || "WebWeave";
      streamedDelegated = Boolean(event.payload?.delegated);
      streamedSubagent = event.payload?.subagent_id || "";
      flushStreamedText();
    }
  });
  flushTools();
  flushStreamedText();

  if (task && task.status !== "running") {
    const result = element("div", "subagent-result", task.reason || task.activity);
    result.dataset.status = task.status;
    container.append(result);
  }
  if (task ? task.status === "running" : run.job_active) {
    const working = element("div", "agent-working");
    const latest = [...events].reverse().find((event) => ["context_usage", "model_reasoning_delta", "model_output_delta", "tool_started", "tool_output", "llm_retry"].includes(event.type));
    const runningTask = tasks.find((entry) => entry.status === "running");
    const stage = runningTask ? `${runningTask.name}正在执行，可在右侧查看`
      : latest?.type === "tool_started" ? `正在调用 ${latest.payload.tool_name}`
      : latest?.type === "model_reasoning_delta" ? "正在思考"
      : latest?.type === "model_output_delta" ? "正在生成"
      : latest?.type === "llm_retry" ? "连接中断，正在重试"
      : "等待模型响应";
    working.append(element("span"), element("span"), element("span"), element("small", "", stage));
    container.append(working);
  }
  refreshIcons();
  container.scrollTop = shouldStick ? container.scrollHeight : previousScroll;
}

function createUserMessage(payload) {
  const article = element("article", "message message-user");
  const item = payload.item || {};
  const displayed = payload.display_text || item.content || "";
  article.append(element("div", "message-author", "你"));
  const body = element("div", "message-body", String(displayed));
  const attachments = Array.isArray(payload.attachments)
    ? payload.attachments
    : [];
  if (attachments.length) {
    const files = element("div", "message-files");
    attachments.forEach((name) => {
      const chip = element("span");
      chip.append(icon("file"), document.createTextNode(name));
      files.append(chip);
    });
    body.append(files);
  }
  if (Array.isArray(payload.instance_context) && payload.instance_context.length) {
    const contexts = element("div", "context-card-list message-context-cards");
    payload.instance_context.forEach((context) => {
      if (context && ["component", "point"].includes(context.type)) contexts.append(createContextCard(context));
    });
    body.append(contexts);
  }
  article.append(body);
  return article;
}

function createAgentMessage(text, agentName = "WebWeave", delegated = false, subagentId = "") {
  const article = element("article", "message message-agent");
  article.dataset.subagentId = subagentId;
  article.append(element("div", "agent-avatar", "W"));
  const content = element("div", "message-content");
  content.append(element("div", "message-author", agentName + (delegated ? " · 子 Agent" : "")));
  const body = element("div", "message-body prose");
  text.split(/\n{2,}/).forEach((paragraph) => {
    body.append(element("p", "", paragraph));
  });
  content.append(body);
  article.append(content);
  return article;
}

function createActivityCard(events) {
  const card = element("article", "activity-card");
  card.dataset.subagentId = events[0]?.payload?.subagent_id || "";
  const summary = element("button", "activity-summary");
  summary.type = "button";
  summary.setAttribute("aria-expanded", "false");
  const activityIcon = element("span", "activity-icon");
  activityIcon.append(icon("terminal-square"));
  const copy = element("span");
  const cancelled = events.filter((event) => event.payload?.cancelled).length;
  copy.append(
    element("strong", "", `${events.length} 次工具调用`),
    element("small", "", cancelled ? `${cancelled} 次因停止取消` : "查看调用记录"),
  );
  const chevron = icon("chevron-down");
  chevron.classList.add("activity-chevron");
  summary.append(activityIcon, copy, chevron);
  const details = element("div", "activity-details");
  events.forEach((event) => details.append(createToolDetail(event)));
  summary.addEventListener("click", () => {
    const open = card.classList.toggle("is-open");
    summary.setAttribute("aria-expanded", String(open));
  });
  card.append(summary, details);
  return card;
}

function createToolDetail(event) {
  const row = element("div");
  const cancelled = Boolean(event.payload?.cancelled);
  row.append(icon(cancelled ? "minus" : "check"));
  row.append(
    element(
      "span",
      "",
      `${event.payload?.agent_name ? event.payload.agent_name + " · " : ""}${event.payload?.tool_name || "tool"}${toolArgumentHint(event.payload)}`,
    ),
  );
  row.append(
    element(
      "time",
      "",
      `${Number(event.payload?.duration_seconds || 0).toFixed(1)}s`,
    ),
  );
  return row;
}

function toolArgumentHint(payload) {
  try {
    const args = JSON.parse(payload?.arguments || "{}");
    const target = state.agents.find((agent) => agent.id === args.agent_id);
    const hint = target?.name || args.agent_id || args.path || args.file_path || args.action || args.cwd;
    return hint ? ` · ${String(hint).slice(0, 80)}` : "";
  } catch {
    return "";
  }
}

function renderActivity(run) {
  const tools = run.events.filter((event) => event.type === "tool_output");
  $("#fact-tool-count").textContent = String(tools.length);
  $("#fact-event-count").textContent = String(run.event_count);
  const badge = $("#activity-status");
  badge.textContent = statusLabel(run.status);
  badge.dataset.status = run.status;
  const list = $("#tool-list");
  list.replaceChildren();
  if (!tools.length) {
    list.append(element("div", "empty-panel compact-empty", "暂无工具调用"));
    return;
  }
  [...tools].reverse().forEach((event) => {
    const row = element("button", "tool-row");
    row.type = "button";
    const toolState = element(
      "span",
      `tool-state ${event.payload?.cancelled ? "cancelled" : "success"}`,
    );
    toolState.append(icon(event.payload?.cancelled ? "minus" : "check"));
    const copy = element("span");
    copy.append(
      element("strong", "", event.payload?.tool_name || "tool"),
      element("small", "", [event.payload?.agent_name, toolArgumentHint(event.payload).replace(/^ · /, "")].filter(Boolean).join(" · ") || "完成"),
    );
    row.append(
      toolState,
      copy,
      element(
        "time",
        "",
        `${Number(event.payload?.duration_seconds || 0).toFixed(1)}s`,
      ),
    );
    list.append(row);
  });
  refreshIcons();
}

function updateActionFromRun(run) {
  if (run.job_active || ["running", "stopping"].includes(run.status)) {
    updateAction("running", run.status);
  } else if (run.status === "stopped" && !promptInput.value.trim()) {
    updateAction("paused", run.status);
  } else {
    updateAction("idle", run.status);
  }
}

function updateAction(action, status = null) {
  state.action = action;
  $("#agent-select").disabled = action === "running" || !state.agents.length;
  $("#reasoning-effort").disabled = action === "running";
  primaryAction.classList.toggle("is-running", action === "running");
  primaryAction.classList.toggle("is-paused", action === "paused");
  if (action === "running") {
    primaryAction.replaceChildren(element("span", "", "停止"), icon("square"));
  } else if (action === "paused" && !promptInput.value.trim()) {
    primaryAction.replaceChildren(element("span", "", "继续"), icon("play"));
  } else {
    primaryAction.replaceChildren(element("span", "", "发送"), icon("arrow-up"));
  }
  const resolved = status || state.activeRun?.status || "waiting";
  runStatus.replaceChildren(element("span"), document.createTextNode(statusLabel(resolved)));
  runStatus.classList.toggle(
    "is-running",
    ["running", "stopping"].includes(resolved),
  );
  refreshIcons();
}

async function handlePrimaryAction() {
  if (!state.activeRunId) return;
  if (state.action === "running") {
    await api(`/api/runs/${encodeURIComponent(state.activeRunId)}/stop`, {
      method: "POST",
    });
    showToast("已请求停止，当前操作结束后生效");
    await loadActiveRun(true);
    return;
  }
  if (state.action === "paused" && !promptInput.value.trim()) {
    await api(`/api/runs/${encodeURIComponent(state.activeRunId)}/continue`, {
      method: "POST",
      body: JSON.stringify({ agent_id: state.selectedAgentId, reasoning_effort: state.selectedReasoningEffort }),
    });
    showToast("已继续运行");
    await loadActiveRun(true);
    return;
  }
  const content = promptInput.value.trim();
  if (!content) {
    promptInput.focus();
    showToast("请输入任务内容");
    return;
  }

  primaryAction.disabled = true;
  try {
    const attachmentNames = [];
    for (const file of state.pendingFiles) {
      const payload = await api(
        `/api/runs/${encodeURIComponent(state.activeRunId)}/attachments?name=${encodeURIComponent(file.name)}`,
        { method: "POST", body: file },
      );
      attachmentNames.push(payload.file.name);
    }
    await api(`/api/runs/${encodeURIComponent(state.activeRunId)}/messages`, {
      method: "POST",
      body: JSON.stringify({
        content,
        agent_id: state.selectedAgentId,
        reasoning_effort: state.selectedReasoningEffort,
        attachments: attachmentNames,
        instance_context: state.pendingContexts,
      }),
    });
    promptInput.value = "";
    state.pendingFiles = [];
    state.pendingContexts = [];
    renderPendingContext();
    await loadActiveRun(true);
  } finally {
    primaryAction.disabled = false;
  }
}

function addPendingFiles(files) {
  Array.from(files).forEach((file) => state.pendingFiles.push(file));
  renderPendingContext();
}

function renderPendingContext() {
  attachmentList.replaceChildren();
  const contexts = $("#context-card-list");
  contexts.replaceChildren();
  state.pendingFiles.forEach((file, index) => {
    attachmentList.append(
      pendingChip("file", file.name, () => {
        state.pendingFiles.splice(index, 1);
        renderPendingContext();
      }),
    );
  });
  state.pendingContexts.forEach((context, index) => {
    contexts.append(
      createContextCard(context, { draggable: true, onRemove: () => {
        state.pendingContexts.splice(index, 1);
        renderPendingContext();
      } }),
    );
  });
  refreshIcons();
}

function createContextCard(context, { onRemove = null, draggable = false } = {}) {
  const kind = context.type === "component" ? "组件" : "定点";
  const label = `${kind}${context.ordinal ? ` ${context.ordinal}` : ""} · ${context.component || context.tag || "页面"}`;
  const card = element("div", "context-card");
  card.dataset.type = context.type;
  card.dataset.selectionId = context.selection_id || "";
  const copy = element("div", "context-card-copy");
  copy.append(element("strong", "", label));
  const description = context.type === "point"
    ? `位置 (${context.point?.x}, ${context.point?.y})`
    : context.text || "网页中的组件";
  copy.append(element("span", "", description));
  card.append(icon(context.type === "component" ? "scan-search" : "crosshair"), copy);
  if (onRemove) {
    const remove = element("button", "icon-button subtle");
    remove.type = "button";
    remove.title = `移除${kind}`;
    remove.setAttribute("aria-label", `移除${label}`);
    remove.append(icon("x"));
    remove.addEventListener("click", onRemove);
    card.append(remove);
  }
  if (draggable && context.selection_id) {
    card.draggable = true;
    card.title = "拖入对话输入区";
    card.addEventListener("dragstart", (event) => {
      event.dataTransfer.effectAllowed = "copy";
      event.dataTransfer.setData(previewSelectionMime, context.selection_id);
      event.dataTransfer.setData("text/plain", label);
      if (state.conversationHidden) setConversationHidden(false);
    });
  }
  return card;
}

function addPendingContexts(contexts) {
  contexts.forEach((context) => {
    if (context.run_id === state.activeRunId && !state.pendingContexts.some(item => item.selection_id === context.selection_id)) {
      state.pendingContexts.push(context);
    }
  });
  renderPendingContext();
  promptInput.focus();
}

function pendingChip(iconName, label, onRemove) {
  const chip = element("span", "attachment-chip");
  const remove = element("button");
  remove.type = "button";
  remove.title = "移除";
  remove.append(icon("x"));
  remove.addEventListener("click", onRemove);
  chip.append(icon(iconName), element("span", "", label), remove);
  return chip;
}

async function loadFiles() {
  if (!state.activeRunId) return;
  const payload = await api(
    `/api/runs/${encodeURIComponent(state.activeRunId)}/files`,
  );
  state.runFiles = payload.files;
  if (state.activeRun) renderTemplateSave(state.activeRun);
  renderFileTree(payload.files);
}

function renderFileTree(files) {
  const tree = $("#file-tree");
  tree.replaceChildren();
  const scopes = ["resources", "result", "download"];
  scopes.forEach((scope) => {
    const scoped = files.filter((file) => file.scope === scope);
    const folder = element("div", "tree-folder is-open");
    const toggle = element("button");
    toggle.type = "button";
    toggle.append(icon("chevron-down"), icon("folder-open"), element("span", "", scope));
    const children = element("div", "tree-children");
    if (!scoped.length) {
      children.append(element("span", "tree-empty", "空"));
    }
    scoped.forEach((file) => {
      const button = element("button", "tree-file");
      button.type = "button";
      button.append(icon(fileIcon(file)), element("span", "", file.path));
      button.addEventListener("click", () => openFile(file));
      children.append(button);
    });
    toggle.addEventListener("click", () => {
      const open = folder.classList.toggle("is-open");
      toggle.replaceChildren(
        icon(open ? "chevron-down" : "chevron-right"),
        icon(open ? "folder-open" : "folder"),
        element("span", "", scope),
      );
      refreshIcons();
    });
    folder.append(toggle, children);
    tree.append(folder);
  });
  refreshIcons();
}

function fileIcon(file) {
  if (file.mime.startsWith("image/")) return "image";
  if (/\.(html|css|js|jsx|ts|tsx|py)$/i.test(file.path)) return "file-code-2";
  return "file-text";
}

async function openFile(file) {
  const query = new URLSearchParams({ scope: file.scope, path: file.path });
  const payload = await api(
    `/api/runs/${encodeURIComponent(state.activeRunId)}/file?${query}`,
  );
  const data = payload.file;
  $("#preview-name").textContent = file.path;
  $("#preview-meta").textContent = `${data.mime} · ${formatBytes(data.size)}`;
  $("#file-browser").hidden = true;
  $("#file-preview").hidden = false;
  const isImage = data.mime.startsWith("image/");
  $("#preview-code").hidden = !data.is_text;
  $("#preview-image").hidden = !isImage;
  state.currentFileText = data.content || "";
  $("#preview-code").textContent = data.is_text
    ? data.content
    : isImage
      ? ""
      : "该文件不支持文本预览。";
  if (isImage) {
    query.set("raw", "1");
    $("#preview-image-content").src =
      `/api/runs/${encodeURIComponent(state.activeRunId)}/file?${query}`;
    $("#preview-image-meta").textContent = `${file.path} · ${formatBytes(data.size)}`;
  }
}

function formatBytes(size) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

function currentInstance() {
  const run = state.activeRun;
  const instances = run?.instances || (run?.instance ? [run.instance] : []);
  return instances.find((instance) => instance.instance_id === state.selectedInstanceId) || null;
}

function renderInstances(run) {
  const instances = run.instances || (run.instance ? [run.instance] : []);
  if (!instances.some((instance) => instance.instance_id === state.selectedInstanceId)) {
    state.selectedInstanceId = run.instance?.instance_id || instances.at(-1)?.instance_id || null;
  }
  const sessions = $("#instance-sessions");
  const previousScroll = sessions.scrollLeft;
  const focused = sessions.contains(document.activeElement) ? document.activeElement.id : null;
  sessions.replaceChildren();
  sessions.hidden = instances.length === 0;
  instances.forEach((instance, index) => {
    const button = element("button", "instance-session-tab");
    button.type = "button";
    button.id = `instance-tab-${instance.instance_id}`;
    button.setAttribute("role", "tab");
    button.setAttribute("aria-selected", String(instance.instance_id === state.selectedInstanceId));
    button.setAttribute("aria-controls", "instance-canvas");
    button.dataset.status = instance.status;
    button.title = `${instance.cwd}\n${instance.status}`;
    button.append(element("span", "subagent-dot"), document.createTextNode(`实例 ${index + 1} · ${instance.port || "未启动"}`));
    button.addEventListener("click", () => {
      state.selectedInstanceId = instance.instance_id;
      clearSelection();
      renderInstances(state.activeRun);
    });
    sessions.append(button);
  });
  sessions.scrollLeft = previousScroll;
  if (focused) document.getElementById(focused)?.focus({ preventScroll: true });
  renderInstance(currentInstance());
  refreshIcons();
}

function renderInstance(instance) {
  const active = instance && ["starting", "running", "unhealthy"].includes(instance.status);
  $("#instance-empty").hidden = Boolean(active);
  $("#instance-frame").hidden = !active;
  $("#instance-address").textContent = instance?.app_url
    ? instance.app_url.replace(/^https?:\/\//, "")
    : "未启动";
  $("#instance-live-dot").classList.toggle("is-offline", !active);
  $("#instance-start").disabled = Boolean(state.instanceBusy || state.activeRun.job_active);
  $("#instance-start-label").textContent = state.activeRun.instances?.length ? "新开实例" : "手动启动";
  $("#instance-start").title = state.activeRun.job_active ? "请等待当前任务结束" : "读取启动文件，新开一个独立实例";
  ["#instance-refresh", "#instance-restart", "#instance-open", "#instance-stop"].forEach((selector) => {
    $(selector).disabled = !active || state.instanceBusy;
  });
  $("#instance-restart").disabled = !instance || state.instanceBusy;
  $("#instance-delete").disabled = !instance || state.instanceBusy;
  $("#instance-empty").textContent = instance ? "当前实例未运行，可点击上方重启按钮启动" : "当前对话没有运行中的实例";
  if (!active) {
    closePreviewMenu();
    instanceFrame.removeAttribute("src");
    state.frameKey = "";
    clearSelection();
    return;
  }
  const frameKey = `${instance.instance_id}:${instance.revision}:${instance.preview_url}`;
  if (frameKey !== state.frameKey) {
    closePreviewMenu();
    state.frameKey = frameKey;
    instanceFrame.src = instance.preview_url;
  }
}

async function instanceAction(action) {
  if (!state.activeRunId || state.instanceBusy) return;
  const runId = state.activeRunId;
  const instanceId = action === "start" ? undefined : state.selectedInstanceId;
  if (action === "delete") {
    if (!instanceId || !currentInstance()) return;
    if (!window.confirm("确定删除当前实例？运行中的实例会先停止，生成文件和启动配置会保留。")) return;
  }
  state.instanceBusy = true;
  renderInstance(currentInstance());
  try {
    const payload = await api(
      `/api/runs/${encodeURIComponent(runId)}/instance`,
      { method: "POST", body: JSON.stringify({ action, instance_id: instanceId, timeout: 30 }) },
    );
    if (runId !== state.activeRunId) return;
    if (action === "start") showToast("实例已启动");
    if (action === "restart") showToast("实例已重启");
    if (action === "stop") showToast("实例已停止");
    if (action === "delete") {
      showToast("实例已删除");
      clearSelection();
    }
    if (action === "start") state.selectedInstanceId = payload.instance_id;
    await loadActiveRun(true);
  } finally {
    state.instanceBusy = false;
    if (state.activeRun) renderInstances(state.activeRun);
  }
}

function setInspectMode(mode) {
  state.inspectMode = mode;
  document.querySelectorAll(".inspect-mode").forEach((button) => {
    button.classList.toggle("is-active", button.dataset.mode === mode);
  });
  instanceFrame.contentWindow?.postMessage(
    { type: "webweave:set-mode", mode },
    window.location.origin,
  );
}

function receivePreviewMessage(event) {
  if (event.origin !== window.location.origin || event.source !== instanceFrame.contentWindow) {
    return;
  }
  if (event.data?.type === "webweave:preview-ready") {
    closePreviewMenu();
    state.selections = [];
    renderSelections();
    setInspectMode(state.inspectMode);
    return;
  }
  if (event.data?.type === "webweave:close-menu") {
    closePreviewMenu();
    return;
  }
  if (event.data?.type === "webweave:context-menu") {
    if (instanceFrame.hidden || !Number.isFinite(event.data.x) || !Number.isFinite(event.data.y)) return;
    const rect = instanceFrame.getBoundingClientRect();
    const scaleX = rect.width / (instanceFrame.offsetWidth || rect.width);
    const scaleY = rect.height / (instanceFrame.offsetHeight || rect.height);
    showPreviewMenu(rect.left + event.data.x * scaleX, rect.top + event.data.y * scaleY, event.data);
    return;
  }
  if (event.data?.type === "webweave:selection-removed") {
    state.selections = state.selections.filter(item => item.selection_id !== event.data.selection_id);
    renderSelections();
    closePreviewMenu();
    return;
  }
  if (event.data?.type === "webweave:selection-drag") {
    if (state.selections.some(item => item.selection_id === event.data.selection_id) && state.conversationHidden) setConversationHidden(false);
    return;
  }
  if (event.data?.type !== "webweave:selection" || !["component", "point"].includes(event.data.mode) || typeof event.data.selection?.selection_id !== "string") return;
  const selection = {
    ...event.data.selection,
    type: event.data.mode,
    run_id: state.activeRunId,
    instance_id: currentInstance()?.instance_id || null,
    revision: currentInstance()?.revision || null,
    app_url: currentInstance()?.app_url || null,
  };
  const index = state.selections.findIndex(item => item.selection_id === selection.selection_id);
  if (index < 0) state.selections.push(selection);
  else state.selections[index] = selection;
  state.pendingContexts = state.pendingContexts.map(item => item.selection_id === selection.selection_id ? selection : item);
  renderPendingContext();
  renderSelections();
}

function closePreviewMenu() {
  previewContextMenu?.remove();
  previewContextMenu = null;
}

function refreshInstancePreview() {
  closePreviewMenu();
  if (instanceFrame.getAttribute("src")) instanceFrame.contentWindow.location.reload();
}

function showPreviewMenu(x, y, context = {}) {
  closePreviewMenu();
  const menu = element("div", "preview-context-menu");
  previewContextMenu = menu;
  menu.setAttribute("role", "menu");
  menu.setAttribute("aria-label", "实例预览菜单");
  menu.tabIndex = -1;
  const instance = currentInstance();
  const active = Boolean(instance && ["starting", "running", "unhealthy"].includes(instance.status) && !instanceFrame.hidden);
  const frameKey = state.frameKey;
  const selected = state.selections.find(item => item.selection_id === context.selection_id);
  const components = (Array.isArray(context.components) ? context.components : []).filter(item =>
    state.selections.some(selection => selection.type === "component" && selection.selection_id === item.selection_id));
  menu.append(element("div", "preview-menu-heading", selected ? `${selected.type === "component" ? "组件" : "定点"} ${selected.ordinal || ""} · ${selected.component || selected.tag || "页面"}` : "实例预览"));

  const position = () => {
    const bounds = menu.getBoundingClientRect();
    menu.style.left = `${Math.max(8, Math.min(x, window.innerWidth - bounds.width - 8))}px`;
    menu.style.top = `${Math.max(8, Math.min(y, window.innerHeight - bounds.height - 8))}px`;
  };
  const action = (name, entries = [], enabled) => {
    instanceFrame.contentWindow?.postMessage({
      type: "webweave:context-action", action: name, selection_ids: entries.map(item => item.selection_id), enabled,
    }, window.location.origin);
  };
  const button = (container, label, iconName, callback, disabled = false) => {
    const control = element("button", "preview-menu-item");
    control.type = "button";
    control.setAttribute("role", "menuitem");
    control.disabled = disabled;
    control.append(icon(iconName), element("span", "", label));
    control.addEventListener("click", () => {
      if (control.disabled) return;
      closePreviewMenu();
      if (frameKey === state.frameKey) return runSafely(callback);
    });
    container.append(control);
    return control;
  };
  const submenu = (label, iconName) => {
    const group = element("div", "preview-menu-group");
    group.hidden = true;
    group.setAttribute("role", "group");
    group.setAttribute("aria-label", label);
    const control = element("button", "preview-menu-item");
    control.type = "button";
    control.setAttribute("role", "menuitem");
    control.setAttribute("aria-expanded", "false");
    control.append(icon(iconName), element("span", "", label), icon("chevron-down"));
    control.addEventListener("click", () => {
      group.hidden = !group.hidden;
      control.setAttribute("aria-expanded", String(!group.hidden));
      position();
    });
    menu.append(control, group);
    return group;
  };
  const componentActions = (container, entries) => {
    const allMovable = entries.every(item => item.movable);
    const allHidden = entries.every(item => item.hidden);
    button(container, allMovable ? "关闭自由移动（临时）" : "开启自由移动（临时）", "move", () => action("move", entries, !allMovable), !active);
    button(container, "还原组件位置", "undo-2", () => action("reset", entries), !active || !entries.some(item => item.moved));
    button(container, allHidden ? "恢复组件显示" : "临时隐藏组件", allHidden ? "eye" : "eye-off", () => action("hide", entries, !allHidden), !active);
    button(container, "加入对话", "corner-left-up", () => {
      if (state.conversationHidden) setConversationHidden(false);
      addPendingContexts(state.selections.filter(item => entries.some(entry => entry.selection_id === item.selection_id)));
    }, !active);
  };

  if (selected) {
    const component = components.find(item => item.selection_id === selected.selection_id);
    if (component) componentActions(menu, [component]);
    else button(menu, "加入对话", "corner-left-up", () => {
      if (state.conversationHidden) setConversationHidden(false);
      addPendingContexts([selected]);
    }, !active);
    button(menu, "复制定位信息", "copy", async () => {
      await navigator.clipboard.writeText(JSON.stringify({ component: selected.component, selector: selected.selector, rect: selected.rect, ...(selected.point ? { point: selected.point } : {}) }, null, 2));
      showToast("定位信息已复制");
    }, !active);
    button(menu, "取消此标记", "x", () => action("remove", [selected]), !active);
  } else if (context.can_mark) {
    button(menu, "标记此组件", "scan-search", () => action("mark"), !active);
  }
  if (components.length > 1) componentActions(submenu(`全部已标记组件（${components.length}）`, "scan-search"), components);
  menu.append(element("div", "preview-menu-separator"));
  button(menu, "刷新预览", "refresh-cw", refreshInstancePreview, !active || state.instanceBusy);
  button(menu, "在新窗口打开", "external-link", () => window.open(instance.app_url, "_blank", "noopener,noreferrer"), !active || !instance?.app_url);
  button(menu, "复制预览地址", "link", async () => {
    await navigator.clipboard.writeText(new URL(instance.preview_url, window.location.href).href);
    showToast("预览地址已复制");
  }, !active || !instance?.preview_url);
  const modes = submenu("切换模式", "mouse-pointer-2");
  [["preview", "预览", "mouse-pointer-2"], ["component", "组件选择", "scan-search"], ["point", "定点评价", "crosshair"]].forEach(([mode, label, iconName]) => {
    button(modes, `${state.inspectMode === mode ? "✓ " : ""}${label}`, iconName, () => setInspectMode(mode), !active);
  });
  menu.append(element("div", "preview-menu-separator"));
  button(menu, "还原全部临时修改", "rotate-ccw", () => action("restore-all"), !active || context.modified === false);
  button(menu, "清除全部标记", "eraser", clearSelection, !active || !state.selections.length);
  menu.append(element("div", "preview-menu-note", "移动和隐藏仅在当前预览生效，刷新后失效"));
  menu.addEventListener("contextmenu", event => event.preventDefault());
  menu.addEventListener("keydown", (event) => {
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const controls = [...menu.querySelectorAll("button:not(:disabled)")].filter(item => item.getClientRects().length);
    if (!controls.length) return;
    const index = controls.indexOf(document.activeElement);
    const next = event.key === "Home" ? 0 : event.key === "End" ? controls.length - 1 :
      index < 0 ? (event.key === "ArrowUp" ? controls.length - 1 : 0) :
      (index + (event.key === "ArrowUp" ? -1 : 1) + controls.length) % controls.length;
    controls[next].focus();
  });
  document.body.append(menu);
  refreshIcons();
  position();
  menu.focus({ preventScroll: true });
}

function bindPreviewMenu() {
  $("#instance-canvas").addEventListener("contextmenu", (event) => {
    event.preventDefault();
    showPreviewMenu(event.clientX, event.clientY);
  });
  window.addEventListener("pointerdown", (event) => {
    if (previewContextMenu && !previewContextMenu.contains(event.target)) closePreviewMenu();
  }, true);
  window.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || !previewContextMenu) return;
    event.preventDefault();
    closePreviewMenu();
    instanceFrame.focus({ preventScroll: true });
  });
  window.addEventListener("scroll", (event) => {
    if (previewContextMenu && !previewContextMenu.contains(event.target)) closePreviewMenu();
  }, true);
  ["resize", "blur", "hashchange"].forEach(type => window.addEventListener(type, closePreviewMenu));
}

function renderSelections() {
  selectionContext.hidden = state.selections.length === 0;
  $("#selection-kind").textContent = `已选 ${state.selections.length} 处`;
  const list = $("#selection-list");
  list.replaceChildren();
  state.selections.forEach((selection) => {
    list.append(createContextCard(selection, { draggable: true, onRemove: () => {
      state.selections = state.selections.filter(item => item.selection_id !== selection.selection_id);
      instanceFrame.contentWindow?.postMessage({ type: "webweave:remove-selection", selection_id: selection.selection_id }, window.location.origin);
      renderSelections();
    } }));
  });
  refreshIcons();
}

function clearSelection() {
  closePreviewMenu();
  state.selections = [];
  renderSelections();
  instanceFrame.contentWindow?.postMessage(
    { type: "webweave:clear-selection" },
    window.location.origin,
  );
  setInspectMode("preview");
}

function injectSelectionIntoPrompt() {
  if (!state.selections.length) return;
  if (state.conversationHidden) setConversationHidden(false);
  addPendingContexts(state.selections);
}

const effortLabels = { none: "关闭", minimal: "最少", low: "低", medium: "中", high: "高", xhigh: "更高", max: "最高" };

function configuredModels(config = state.config) {
  if (config?.llm?.models?.length) return config.llm.models;
  const models = [];
  if (config?.llm?.main?.model) models.push({ ...config.llm.main, id: "legacy-main" });
  if (config?.llm?.vision?.model) models.push({ ...config.llm.vision, id: "legacy-vision" });
  return models;
}

function fillModelSelect(select, models, selected, emptyLabel = null) {
  select.replaceChildren();
  if (emptyLabel !== null) {
    const option = element("option", "", emptyLabel);
    option.value = "";
    select.append(option);
  }
  models.forEach((model) => {
    const option = element("option", "", `${model.model} · ${model.base_url}`);
    option.value = model.id;
    select.append(option);
  });
  select.value = selected;
}

function renderModelSettings() {
  fillModelSelect($("#llm-main-select"), state.modelsDraft, state.mainModelId);
  fillModelSelect($("#llm-vision-select"), state.modelsDraft, state.visionModelId, "不使用独立视觉模型");
  fillModelSelect($("#context-model"), state.modelsDraft, state.summaryModelId || "", "跟随当前 Agent 模型");
  const list = $("#model-list");
  list.replaceChildren();
  state.modelsDraft.forEach((model) => {
    const row = element("div", "model-row");
    const name = element("div", "model-row-name");
    name.append(element("strong", "", model.model));
    const roles = [model.id === state.mainModelId ? "主模型" : "", model.id === state.visionModelId ? "视觉模型" : "", model.id === state.summaryModelId ? "摘要模型" : ""].filter(Boolean);
    if (roles.length) name.append(element("small", "model-role-label", roles.join(" · ")));
    const url = element("code", "model-row-url", model.base_url);
    url.title = model.base_url;
    const effort = element("span", "model-row-effort", model.reasoning_efforts?.length ? (effortLabels[model.reasoning_effort] || "接口默认") : "—");
    const actions = element("div", "model-row-actions");
    const edit = element("button", "text-button", "编辑");
    edit.type = "button";
    edit.addEventListener("click", () => openModelEditor(model));
    const remove = element("button", "text-button danger", "删除");
    remove.type = "button";
    remove.disabled = model.id === state.mainModelId;
    remove.title = remove.disabled ? "请先选择其他主模型" : "从列表移除，保存配置后生效";
    remove.addEventListener("click", () => {
      if (state.agents.some((agent) => agent.model_id === model.id)) { showToast("请先将使用此模型的 Agent 改为其他模型或跟随主模型"); return; }
      if (state.summaryModelId === model.id) { showToast("请先更换摘要模型或选择跟随当前 Agent"); return; }
      state.modelsDraft = state.modelsDraft.filter((entry) => entry.id !== model.id);
      if (state.visionModelId === model.id) state.visionModelId = "";
      if (state.editingModelId === model.id) closeModelEditor();
      renderModelSettings();
      markConfigDirty();
    });
    actions.append(edit, remove);
    row.append(name, url, effort, actions);
    list.append(row);
  });
  $("#model-add").disabled = !state.configReady;
  refreshIcons();
}

function renderReasoningSelector() {
  const agent = state.agents.find((entry) => entry.id === state.selectedAgentId);
  const models = configuredModels();
  const modelId = agent?.id !== "main" && agent?.model_id ? agent.model_id : state.config?.llm?.main_model_id;
  const provider = models.find((entry) => entry.id === modelId) || state.config?.llm?.main || {};
  const efforts = provider.reasoning_efforts || [];
  const select = $("#reasoning-effort");
  select.replaceChildren();
  const defaultOption = element("option", "", `默认 · ${effortLabels[provider.reasoning_effort] || "接口默认"}`);
  defaultOption.value = "";
  select.append(defaultOption);
  efforts.forEach((effort) => {
    const option = element("option", "", effortLabels[effort] || effort);
    option.value = effort;
    select.append(option);
  });
  if (!efforts.includes(state.selectedReasoningEffort)) state.selectedReasoningEffort = "";
  select.value = state.selectedReasoningEffort;
  select.disabled = state.action === "running";
  $("#reasoning-effort-wrap").hidden = !efforts.length;
}

function addModelHeader(key = "", value = "") {
  const row = element("div", "model-header-row");
  const name = element("input", "model-header-name");
  name.value = key;
  name.placeholder = "Header 名称，例如 X-Project";
  name.setAttribute("aria-label", "Header 名称");
  const content = element("input", "model-header-value");
  content.type = "password";
  content.value = value;
  content.placeholder = "Header 值";
  content.setAttribute("aria-label", "Header 值");
  if (value === "********") setSecretInput(content, value);
  const remove = element("button", "icon-button subtle danger");
  remove.type = "button";
  remove.setAttribute("aria-label", "移除 Header");
  remove.append(icon("x"));
  remove.addEventListener("click", () => row.remove());
  row.append(name, content, remove);
  $("#model-headers").append(row);
  refreshIcons();
}

function openModelEditor(model = null) {
  state.editingModelId = model?.id || null;
  state.modelDiscovery = [];
  $("#model-editor-title").textContent = model ? "编辑模型" : "添加模型";
  $("#model-editor-save").textContent = model ? "更新列表" : "加入列表";
  $("#model-base-url").value = model?.base_url || "";
  $("#model-name").value = model?.model || "";
  if (model?.api_key && model.api_key !== "********") {
    $("#model-api-key").value = model.api_key;
    $("#model-api-key").dataset.masked = "false";
    $("#model-api-key").onfocus = null;
  } else setSecretInput($("#model-api-key"), model?.api_key);
  $("#model-headers").replaceChildren();
  Object.entries(model?.headers || {}).forEach(([key, value]) => addModelHeader(key, value));
  $("#model-advanced").open = false;
  $("#model-discovery").hidden = true;
  $("#model-supported-efforts").replaceChildren();
  Object.entries(effortLabels).forEach(([effort, label]) => {
    const item = element("label");
    const input = element("input");
    input.type = "checkbox";
    input.value = effort;
    input.checked = Boolean(model?.reasoning_efforts?.includes(effort));
    input.addEventListener("change", renderModelEfforts);
    item.append(input, document.createTextNode(`${label} · ${effort}`));
    $("#model-supported-efforts").append(item);
  });
  $("#model-supports-reasoning").checked = Boolean(model?.reasoning_efforts?.length);
  renderModelEfforts(model?.reasoning_effort || "");
  $("#model-editor").hidden = false;
  $("#model-base-url").focus();
}

function closeModelEditor() {
  $("#model-editor").hidden = true;
  state.editingModelId = null;
}

function checkedModelEfforts() {
  return $("#model-supports-reasoning").checked ? [...document.querySelectorAll("#model-supported-efforts input:checked")].map((input) => input.value) : [];
}

function renderModelEfforts(preferred = null) {
  const select = $("#model-default-effort");
  const selected = typeof preferred === "string" ? preferred : select.value;
  const efforts = checkedModelEfforts();
  select.replaceChildren();
  const option = element("option", "", "接口默认");
  option.value = "";
  select.append(option);
  efforts.forEach((effort) => {
    const entry = element("option", "", effortLabels[effort]);
    entry.value = effort;
    select.append(entry);
  });
  select.value = efforts.includes(selected) ? selected : "";
  $("#model-reasoning-settings").hidden = !$("#model-supports-reasoning").checked;
}

function readModelEditor() {
  const headers = {};
  const names = new Set();
  document.querySelectorAll("#model-headers .model-header-row").forEach((row) => {
    const key = row.querySelector(".model-header-name").value.trim();
    const input = row.querySelector(".model-header-value");
    if (!key) throw new Error("请填写 Header 名称，或移除空行");
    if (names.has(key.toLowerCase())) throw new Error("Header 名称不能重复");
    names.add(key.toLowerCase());
    headers[key] = input.dataset.masked === "true" ? "********" : input.value;
  });
  const model = {
    model: $("#model-name").value.trim(),
    base_url: $("#model-base-url").value.trim().replace(/\/+$/, ""),
    api_key: $("#model-api-key").dataset.masked === "true" ? $("#model-api-key").value : $("#model-api-key").value.trim(),
  };
  if (!model.base_url) throw new Error("请填写 Base URL");
  if (Object.keys(headers).length) model.headers = headers;
  const efforts = checkedModelEfforts();
  if ($("#model-supports-reasoning").checked && !efforts.length) throw new Error("请勾选该模型实际支持的思考强度档位");
  if (efforts.length) {
    model.reasoning_efforts = efforts;
    if ($("#model-default-effort").value) model.reasoning_effort = $("#model-default-effort").value;
  }
  return model;
}

function saveModelEditor() {
  const model = readModelEditor();
  if (!model.model) throw new Error("请填写 Model name，或识别并导入模型");
  model.id = state.editingModelId || window.crypto.randomUUID().replaceAll("-", "");
  const index = state.modelsDraft.findIndex((entry) => entry.id === model.id);
  if (index < 0) state.modelsDraft.push(model);
  else state.modelsDraft[index] = model;
  if (!state.mainModelId) state.mainModelId = model.id;
  closeModelEditor();
  renderModelSettings();
  markConfigDirty();
  showToast("模型列表已更新，保存配置后生效");
}

async function detectModels() {
  const provider = readModelEditor();
  const button = $("#model-detect");
  button.disabled = true;
  try {
    const payload = await api("/api/models/discover", { method: "POST", body: JSON.stringify({ ...provider, id: state.editingModelId || "" }) });
    state.modelDiscovery = payload.models;
    $("#model-discovery-count").textContent = `${payload.models.length} 个可用模型`;
    const list = $("#model-discovery-list");
    list.replaceChildren();
    payload.models.forEach((model) => {
      const row = element("label");
      const input = element("input");
      input.type = "checkbox";
      input.value = model;
      input.checked = true;
      row.append(input, document.createTextNode(model));
      list.append(row);
    });
    $("#model-discovery-all").checked = true;
    $("#model-import").disabled = !payload.models.length;
    $("#model-discovery").hidden = false;
  } finally {
    button.disabled = false;
  }
}

function importDetectedModels() {
  if (!state.modelDiscovery.length) throw new Error("请先识别当前接口的模型列表");
  const provider = readModelEditor();
  const selected = [...document.querySelectorAll("#model-discovery-list input:checked")].map((input) => input.value);
  let count = 0;
  selected.forEach((name) => {
    if (state.modelsDraft.some((entry) => entry.model === name && entry.base_url.replace(/\/+$/, "") === provider.base_url)) return;
    const entry = { ...provider, model: name, id: window.crypto.randomUUID().replaceAll("-", "") };
    if (state.editingModelId && (!provider.api_key || provider.api_key === "********" || Object.values(provider.headers || {}).includes("********"))) entry.credential_source_id = state.editingModelId;
    state.modelsDraft.push(entry);
    if (!state.mainModelId) state.mainModelId = entry.id;
    count++;
  });
  renderModelSettings();
  if (count) markConfigDirty();
  showToast(count ? `已导入 ${count} 个模型，保存配置后生效` : "所选模型已在列表中");
}

async function loadConfig() {
  setConfigAvailability(false);
  const payload = await api("/api/config");
  state.config = payload.config;
  renderConfig();
  setConfigAvailability(true);
  $("#template-create").disabled = !state.agents.some((agent) => agent.id === "template");
  if (state.activeRun) renderContextMeter(state.activeRun);
}

function renderAgentSelector() {
  const select = $("#agent-select");
  select.replaceChildren();
  if (!state.agents.length) {
    const option = element("option", "", "主Agent");
    option.value = "main";
    select.append(option);
  }
  state.agents.forEach((agent) => {
    const option = element("option", "", agent.name);
    option.value = agent.id;
    select.append(option);
  });
  if (!state.agents.some((agent) => agent.id === state.selectedAgentId)) {
    state.selectedAgentId = "main";
  }
  select.value = state.selectedAgentId;
  select.disabled = state.action === "running" || !state.agents.length;
  renderReasoningSelector();
}

function renderAgents() {
  const list = $("#agents-list");
  list.replaceChildren();
  if (!state.agents.length) {
    list.append(element("div", "empty-panel", "当前服务尚未支持 Agent 管理，请更新并重启服务。"));
  }
  state.agents.forEach((agent) => {
    const builtIn = ["main", "design", "code", "audit", "template"].includes(agent.id);
    const card = element("article", `agent-card${agent.id === "main" ? " agent-card-main" : ""}`);
    const heading = element("header");
    const avatar = element("span", "agent-card-icon");
    avatar.append(icon({ main: "network", design: "palette", code: "code-2", audit: "shield-check", template: "layout-template" }[agent.id] || "bot"));
    const copy = element("div");
    copy.append(element("h2", "", agent.name), element("span", "agent-card-role", agent.id === "main" ? "默认执行 · 可调用全部 Agent" : builtIn ? "专业 Agent" : "自定义 Agent"));
    heading.append(avatar, copy);
    card.append(heading, element("p", "skill-description", agent.description || "未填写职责描述"));
    const model = configuredModels().find((entry) => entry.id === agent.model_id);
    card.append(element("p", "agent-model-caption", agent.id === "main" ? `主模型 · ${state.config?.llm?.main?.model || "未配置"}` : model ? `独立模型 · ${model.model}` : "跟随主模型"));
    const footer = element("footer");
    const edit = element("button", "text-button", "编辑");
    edit.type = "button";
    edit.setAttribute("aria-label", `编辑 ${agent.name}`);
    edit.addEventListener("click", () => openAgentEditor(agent));
    footer.append(edit);
    if (!builtIn) {
      const remove = element("button", "text-button danger", "删除");
      remove.type = "button";
      remove.setAttribute("aria-label", `删除 ${agent.name}`);
      remove.addEventListener("click", () => runSafely(() => deleteAgent(agent)));
      footer.append(remove);
    }
    card.append(footer);
    list.append(card);
  });
  $("#agent-add").disabled = !state.agents.length;
  renderAgentSelector();
  refreshIcons();
}

function openAgentEditor(agent = null) {
  state.editingAgentId = agent?.id || null;
  $("#agent-editor-title").textContent = agent ? "编辑 Agent" : "添加 Agent";
  $("#agent-name").value = agent?.name || "";
  $("#agent-description").value = agent?.description || "";
  $("#agent-prompt").value = agent?.prompt || "";
  fillModelSelect($("#agent-model"), configuredModels(), agent?.model_id || "", "跟随主模型");
  $("#agent-model").disabled = agent?.id === "main";
  $("#agent-form").hidden = false;
  $(".agents-content").classList.add("is-editing");
  $("#agent-name").focus();
}

function closeAgentEditor() {
  $("#agent-form").hidden = true;
  $(".agents-content").classList.remove("is-editing");
  state.editingAgentId = null;
}

async function saveAgent() {
  const button = $("#agent-save");
  const id = state.editingAgentId;
  const agent = {
    name: $("#agent-name").value.trim(),
    description: $("#agent-description").value.trim(),
    prompt: $("#agent-prompt").value.trim(),
    model_id: id === "main" ? "" : $("#agent-model").value,
  };
  if (!agent.name || !agent.prompt) {
    showToast("请填写 Agent 名称和角色指令");
    return;
  }
  button.disabled = true;
  try {
    const payload = await api(id ? `/api/agents/${encodeURIComponent(id)}` : "/api/agents", {
      method: id ? "PUT" : "POST",
      body: JSON.stringify(agent),
    });
    state.agents = payload.agents;
    closeAgentEditor();
    renderAgents();
    showToast(id ? "Agent 已保存" : "Agent 已添加");
  } finally {
    button.disabled = false;
  }
}

async function deleteAgent(agent) {
  if (!window.confirm(`删除「${agent.name}」？已有对话记录和生成文件会保留。`)) return;
  const payload = await api(`/api/agents/${encodeURIComponent(agent.id)}`, { method: "DELETE" });
  state.agents = payload.agents;
  if (state.editingAgentId === agent.id) closeAgentEditor();
  renderAgents();
  showToast("Agent 已删除");
}

function setConfigAvailability(available) {
  state.configReady = available;
  document
    .querySelectorAll(
      "#settings-config-page input, #settings-llm-page input, #settings-llm-page select, #settings-llm-page button, #settings-save",
    )
    .forEach((input) => {
      input.disabled = !available;
    });
  $("#model-add").disabled = !available;
}

function renderConfig() {
  const config = state.config;
  // The running binary may predate the Agent API while serving newer UI files.
  state.agents = config.agents ?? [];
  renderAgents();
  $("#dashboard-model").textContent = config.llm?.main?.model || "未配置";
  const enabledTools = Object.values(config.tools).filter((tool) => tool.enabled).length;
  $("#dashboard-tools").textContent = `${enabledTools} / ${Object.keys(config.tools).length}`;
  $("#dashboard-context").textContent = config.context.enabled ? "已启用" : "未启用";
  const tools = $("#settings-tools");
  tools.querySelectorAll(".settings-row:not(.settings-table-head)").forEach((row) => row.remove());
  Object.entries(config.tools).forEach(([name, settings]) => {
    const row = element("div", "settings-row");
    const label = element("span");
    label.append(icon(toolIcon(name)), document.createTextNode(name));
    row.append(
      label,
      switchControl("tool-enabled", settings.enabled),
      switchControl("tool-run-scoped", settings.run_scoped),
    );
    row.dataset.tool = name;
    tools.append(row);
  });
  $("#model-stream").checked = config.model.stream;
  $("#model-reasoning-summary").checked = Boolean(config.model.reasoning_summary);
  $("#template-create").disabled = !state.agents.some((agent) => agent.id === "template");
  $("#timeout-default").value = config.tool_timeout.default_seconds;
  $("#timeout-max").value = config.tool_timeout.max_seconds;
  $("#context-enabled").checked = config.context.enabled;
  $("#context-max-input").value = config.context.max_input_tokens;
  $("#context-trigger").value = config.context.compression_trigger_ratio;
  $("#context-keep").value = config.context.keep_recent_items;
  $("#context-summary").value = config.context.summary_max_tokens;
  $("#context-timeout").value = config.context.compression_timeout_seconds;
  state.summaryModelId = config.context.summary_model_id || "";
  $("#context-summary-input").value = config.context.summary_input_tokens;
  const localCompression = config.context.local_compression || {};
  $("#context-local-enabled").checked = Boolean(localCompression.enabled);
  $("#context-python").value = localCompression.python || "compression/.venv/Scripts/python.exe";
  $("#context-local-model").value = localCompression.model || "microsoft/llmlingua-2-xlm-roberta-large-meetingbank";
  $("#context-local-device").value = localCompression.device || "cuda";
  $("#context-local-rate").value = localCompression.rate ?? 0.7;
  const llm = config.llm || {};
  state.modelsDraft = configuredModels(config).map((model) => ({ ...model, headers: model.headers ? { ...model.headers } : undefined }));
  state.mainModelId = llm.main_model_id || state.modelsDraft[0]?.id || "";
  state.visionModelId = llm.vision?.enabled ? (llm.vision_model_id || "legacy-vision") : "";
  renderModelSettings();
  $("#settings-state").classList.remove("is-dirty");
  $("#settings-state").lastChild.textContent = "配置已同步";
  document
    .querySelectorAll("#settings-config-page input")
    .forEach((input) => {
      input.onchange = markConfigDirty;
      input.oninput = markConfigDirty;
    });
  refreshIcons();
}

function setSecretInput(input, value) {
  const configured = Boolean(value);
  input.value = configured ? "********" : "";
  input.dataset.masked = String(configured);
  input.placeholder = configured
    ? "已配置，修改请直接输入新的 API Key"
    : "请输入 API Key";
  input.onfocus = () => {
    if (input.dataset.masked === "true") {
      input.value = "";
      input.dataset.masked = "false";
    }
  };
}

function toolIcon(name) {
  return {
    list_skills: "list-tree",
    read_skill: "book-open",
    web_search: "search",
    create_file: "file-plus-2",
    file_read: "file-search",
    read_context_segment: "archive",
    terminal_run: "terminal",
    web_instance: "monitor-up",
  }[name] || "wrench";
}

function switchControl(className, checked) {
  const label = element("label", "switch");
  const input = element("input", className);
  input.type = "checkbox";
  input.checked = checked;
  label.append(input, element("span"));
  return label;
}

function markConfigDirty() {
  const status = $("#settings-state");
  status.classList.add("is-dirty");
  status.lastChild.textContent = "有未保存修改";
}

async function saveConfig() {
  const main = state.modelsDraft.find((model) => model.id === state.mainModelId);
  const vision = state.modelsDraft.find((model) => model.id === state.visionModelId);
  if (!main) throw new Error("请添加并选择主模型");
  const tools = {};
  document.querySelectorAll("#settings-tools [data-tool]").forEach((row) => {
    tools[row.dataset.tool] = {
      enabled: row.querySelector(".tool-enabled").checked,
      run_scoped: row.querySelector(".tool-run-scoped").checked,
    };
  });
  const payload = {
    llm: {
      main: { ...main },
      vision: vision ? { ...vision, enabled: true } : { enabled: false, model: "", base_url: "", api_key: "" },
      models: state.modelsDraft,
      main_model_id: state.mainModelId,
      vision_model_id: state.visionModelId,
    },
    tools,
    model: {
      stream: $("#model-stream").checked,
      reasoning_summary: $("#model-reasoning-summary").checked,
    },
    tool_timeout: {
      default_seconds: Number($("#timeout-default").value),
      max_seconds: Number($("#timeout-max").value),
    },
    context: {
      ...state.config.context,
      enabled: $("#context-enabled").checked,
      max_input_tokens: Number($("#context-max-input").value),
      compression_trigger_ratio: Number($("#context-trigger").value),
      keep_recent_items: Number($("#context-keep").value),
      summary_max_tokens: Number($("#context-summary").value),
      compression_timeout_seconds: Number($("#context-timeout").value),
      summary_model_id: state.summaryModelId || "",
      summary_input_tokens: Number($("#context-summary-input").value),
      local_compression: {
        enabled: $("#context-local-enabled").checked,
        python: $("#context-python").value.trim(),
        model: $("#context-local-model").value.trim(),
        device: $("#context-local-device").value,
        rate: Number($("#context-local-rate").value),
      },
    },
  };
  const response = await api("/api/config", {
    method: "PUT",
    body: JSON.stringify(payload),
  });
  state.config = response.config;
  renderConfig();
  showToast("配置已保存并等待热加载");
}

function bindEvents() {
  let wasMobile = window.innerWidth <= 720;
  document.querySelectorAll(".nav-item, [data-open-view]").forEach((button) => {
    button.addEventListener("click", () =>
      runSafely(async () => {
        const view = button.dataset.view || button.dataset.openView;
        if (view === "settings") switchSettingsPage(button.dataset.settingsPage);
        else switchView(view);
      }),
    );
  });
  sidebarToggle.addEventListener("click", () => {
    state.sidebarCollapsed = !state.sidebarCollapsed;
    applySidebarState();
  });
  ["#new-run", "#home-new-run", "#home-start-button", "#workspace-new-run"].forEach((selector) => {
    $(selector).addEventListener("click", () => runSafely(createRun));
  });
  $("#template-create").addEventListener("click", () => runSafely(() => createRun("template")));
  $("#templates-refresh").addEventListener("click", () => runSafely(loadTemplates));
  $("#template-save-form").addEventListener("submit", (event) => {
    event.preventDefault();
    runSafely(saveActiveTemplate);
  });
  document.querySelectorAll(".inspector-tab").forEach((tab) => {
    tab.addEventListener("click", () => switchInspectorTab(tab.dataset.tab));
  });
  inspectorToggle.addEventListener("click", () => setInspectorHidden(!state.inspectorHidden));
  inspectorRail.addEventListener("click", () => setInspectorHidden(false));
  closeInspector.addEventListener("click", () => setInspectorHidden(true));
  conversationToggle.addEventListener("click", () => setConversationHidden(!state.conversationHidden));
  $("#conversation-rail").addEventListener("click", () => setConversationHidden(false));
  previewToolbarToggle.addEventListener("click", () => {
    state.previewToolbarHidden = !state.previewToolbarHidden;
    applyInspectorState();
  });
  $("#activity-refresh").addEventListener("click", () => runSafely(() => loadActiveRun(true)));
  $("#files-refresh").addEventListener("click", () => runSafely(loadFiles));
  $("#back-to-tree").addEventListener("click", () => {
    $("#file-browser").hidden = false;
    $("#file-preview").hidden = true;
  });
  $("#copy-file").addEventListener("click", async () => {
    if (!state.currentFileText) return;
    await navigator.clipboard.writeText(state.currentFileText);
    showToast("文件内容已复制");
  });
  $("#attach-button").addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => {
    addPendingFiles(fileInput.files);
    fileInput.value = "";
  });
  composer.addEventListener("dragover", (event) => {
    event.preventDefault();
    composer.classList.add("is-dragging");
  });
  composer.addEventListener("dragleave", () => composer.classList.remove("is-dragging"));
  composer.addEventListener("drop", (event) => {
    event.preventDefault();
    composer.classList.remove("is-dragging");
    const selectionId = event.dataTransfer.getData(previewSelectionMime);
    if (selectionId) {
      const selection = [...state.selections, ...state.pendingContexts].find(item => item.selection_id === selectionId);
      if (selection) addPendingContexts([selection]);
      return;
    }
    addPendingFiles(event.dataTransfer.files);
  });
  promptInput.addEventListener("input", () => {
    if (state.activeRun) updateActionFromRun(state.activeRun);
  });
  promptInput.addEventListener("keydown", (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
      event.preventDefault();
      primaryAction.click();
    }
  });
  primaryAction.addEventListener("click", () => runSafely(handlePrimaryAction));
  $("#agent-select").addEventListener("change", (event) => {
    state.selectedAgentId = event.target.value;
    state.selectedReasoningEffort = "";
    renderReasoningSelector();
  });
  $("#reasoning-effort").addEventListener("change", (event) => { state.selectedReasoningEffort = event.target.value; });
  $("#model-add").addEventListener("click", () => openModelEditor());
  $("#model-editor-close").addEventListener("click", closeModelEditor);
  $("#model-editor").addEventListener("submit", (event) => { event.preventDefault(); runSafely(saveModelEditor); });
  $("#model-detect").addEventListener("click", () => runSafely(detectModels));
  $("#model-import").addEventListener("click", () => runSafely(importDetectedModels));
  $("#model-header-add").addEventListener("click", () => addModelHeader());
  $("#model-supports-reasoning").addEventListener("change", renderModelEfforts);
  $("#model-discovery-all").addEventListener("change", (event) => {
    document.querySelectorAll("#model-discovery-list input").forEach((input) => { input.checked = event.target.checked; });
  });
  ["#model-base-url", "#model-api-key", "#model-headers"].forEach((selector) => {
    $(selector).addEventListener("input", () => { state.modelDiscovery = []; $("#model-discovery").hidden = true; });
  });
  $("#llm-main-select").addEventListener("change", (event) => { state.mainModelId = event.target.value; renderModelSettings(); markConfigDirty(); });
  $("#llm-vision-select").addEventListener("change", (event) => { state.visionModelId = event.target.value; renderModelSettings(); markConfigDirty(); });
  $("#context-model").addEventListener("change", (event) => { state.summaryModelId = event.target.value; renderModelSettings(); markConfigDirty(); });
  $("#agent-add").addEventListener("click", () => openAgentEditor());
  $("#agent-editor-close").addEventListener("click", closeAgentEditor);
  $("#agent-form").addEventListener("submit", (event) => {
    event.preventDefault();
    runSafely(saveAgent);
  });
  document.querySelectorAll(".viewport-button").forEach((button) => {
    button.addEventListener("click", () => {
      document.querySelectorAll(".viewport-button").forEach((item) => item.classList.remove("is-active"));
      button.classList.add("is-active");
      instanceFrame.style.width = button.dataset.width;
    });
  });
  document.querySelectorAll(".inspect-mode").forEach((button) => {
    button.addEventListener("click", () => setInspectMode(button.dataset.mode));
  });
  $("#inject-selection").addEventListener("click", injectSelectionIntoPrompt);
  $("#clear-selection").addEventListener("click", clearSelection);
  $("#instance-refresh").addEventListener("click", refreshInstancePreview);
  $("#instance-restart").addEventListener("click", () => runSafely(() => instanceAction("restart")));
  $("#instance-start").addEventListener("click", () => runSafely(() => instanceAction("start")));
  $("#instance-stop").addEventListener("click", () => runSafely(() => instanceAction("stop")));
  $("#instance-delete").addEventListener("click", () => runSafely(() => instanceAction("delete")));
  $("#instance-open").addEventListener("click", () => {
    const url = currentInstance()?.app_url;
    if (url) window.open(url, "_blank", "noopener,noreferrer");
  });
  $("#settings-save").addEventListener("click", () => runSafely(saveConfig));
  document.querySelectorAll('input[name="appearance-theme"]').forEach((input) => {
    input.addEventListener("change", () => setTheme(input.value));
  });
  const colorScheme = window.matchMedia("(prefers-color-scheme: dark)");
  colorScheme.addEventListener("change", () => {
    if (state.appearance.theme === "system") applyAppearance();
  });
  $("#background-choose").addEventListener("click", () => $("#background-input").click());
  $("#background-input").addEventListener("change", (event) => {
    const [file] = event.target.files;
    if (file) runSafely(() => setBackgroundImage(file));
    event.target.value = "";
  });
  $("#background-remove").addEventListener("click", () => runSafely(removeBackgroundImage));
  window.addEventListener("message", receivePreviewMessage);
  bindPreviewMenu();
  window.addEventListener("hashchange", () => {
    const target = window.location.hash.slice(1);
    if (["settings", "settings/llm", "settings/style"].includes(target)) {
      switchSettingsPage(target === "settings" ? "config" : target.split("/")[1]);
    } else if (["home", "workspace", "templates", "skills", "agents"].includes(target)) {
      switchView(target);
    }
  });
  window.addEventListener("resize", () => {
    const isMobile = window.innerWidth <= 720;
    if (isMobile && !wasMobile) setInspectorHidden(true);
    wasMobile = isMobile;
    updateResizeValue();
  });
  bindResize();
}

function updateResizeValue() {
  const bounds = workspaceGrid.getBoundingClientRect();
  const width = state.conversationHidden ? bounds.width - 50 : Math.min(state.inspectorWidth, bounds.width - 248);
  const percent = bounds.width > 0 ? Math.round(Math.max(0, width) / bounds.width * 100) : 0;
  resizeHandle.setAttribute("aria-valuenow", String(percent));
  resizeHandle.setAttribute("aria-valuetext", state.conversationHidden ? "对话已隐藏" : `检查区宽度 ${percent}%`);
}

function resizeWorkspace(clientX) {
  const bounds = workspaceGrid.getBoundingClientRect();
  if (clientX - bounds.left <= 80) {
    if (!state.conversationHidden) setConversationHidden(true);
    return;
  }
  state.inspectorWidth = Math.max(280, Math.min(bounds.right - clientX - 4, bounds.width - 248));
  if (state.conversationHidden) setConversationHidden(false);
  document.documentElement.style.setProperty("--inspector-width", `${state.inspectorWidth}px`);
  updateResizeValue();
}

function bindResize() {
  resizeHandle.addEventListener("keydown", (event) => {
    if (window.innerWidth <= 720 || state.inspectorHidden) return;
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    if (event.key === "Home") setConversationHidden(true);
    else if (event.key === "End") setInspectorHidden(true);
    else if (state.conversationHidden) {
      if (event.key === "ArrowRight") setConversationHidden(false);
    } else {
      const bounds = resizeHandle.getBoundingClientRect();
      resizeWorkspace(bounds.left + bounds.width / 2 + (event.key === "ArrowLeft" ? -32 : 32));
    }
    localStorage.setItem("webweave-inspector-width", String(Math.round(state.inspectorWidth)));
  });
  resizeHandle.addEventListener("pointerdown", (event) => {
    if (window.innerWidth <= 720 || state.inspectorHidden || event.button !== 0 || document.body.classList.contains("is-resizing")) return;
    event.preventDefault();
    document.body.classList.add("is-resizing");
    resizeHandle.setPointerCapture(event.pointerId);
    const onMove = (moveEvent) => {
      if (moveEvent.pointerId === event.pointerId) resizeWorkspace(moveEvent.clientX);
    };
    const onEnd = (endEvent) => {
      if (endEvent.pointerId !== event.pointerId) return;
      if (endEvent.type === "pointerup") resizeWorkspace(endEvent.clientX);
      document.body.classList.remove("is-resizing");
      localStorage.setItem(
        "webweave-inspector-width",
        String(Math.round(state.inspectorWidth)),
      );
      resizeHandle.removeEventListener("pointermove", onMove);
      resizeHandle.removeEventListener("pointerup", onEnd);
      resizeHandle.removeEventListener("pointercancel", onEnd);
      resizeHandle.removeEventListener("lostpointercapture", onEnd);
      if (resizeHandle.hasPointerCapture(event.pointerId)) resizeHandle.releasePointerCapture(event.pointerId);
      updateResizeValue();
    };
    resizeHandle.addEventListener("pointermove", onMove);
    resizeHandle.addEventListener("pointerup", onEnd);
    resizeHandle.addEventListener("pointercancel", onEnd);
    resizeHandle.addEventListener("lostpointercapture", onEnd);
  });
}

async function runSafely(operation) {
  try {
    await operation();
    setConnection(true);
  } catch (error) {
    console.error(error);
    showToast(error.message || "操作失败");
  }
}

async function poll() {
  try {
    const onDashboard = $("#home-view").classList.contains("is-active");
    if (state.activeRunId && !onDashboard) await loadActiveRun();
    else await loadRuns();
    setConnection(true);
  } catch (error) {
    console.error(error);
    setConnection(false);
  }
}

async function initialize() {
  const target = window.location.hash.slice(1);
  applyAppearance();
  bindEvents();
  setConfigAvailability(false);
  applySidebarState();
  applyInspectorState();
  refreshIcons();
  try {
    await Promise.all([
      loadRuns(),
      loadSkills(),
      loadConfig(),
      loadAppearance(),
      api("/api/health"),
    ]);
    setConnection(true);
  } catch (error) {
    console.error(error);
    setConnection(false);
    showToast(error.message || "无法连接本地服务");
  }
  if (["skills", "agents", "templates"].includes(target)) {
    switchView(target);
  }
  if (["settings", "settings/llm", "settings/style"].includes(target)) {
    switchSettingsPage(target === "settings" ? "config" : target.split("/")[1]);
  }
  if (target === "workspace") {
    if (state.runs[0]) await activateRun(state.runs[0].run_id);
    else switchView("workspace");
  }
  if (["activity", "files", "instance"].includes(target) && state.runs[0]) {
    await activateRun(state.runs[0].run_id);
    setInspectorHidden(false);
    switchInspectorTab(target);
  }
  window.setInterval(poll, 1500);
}

initialize();
