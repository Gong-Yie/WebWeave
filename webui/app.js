const state = {
  runs: [],
  activeRunId: null,
  activeRun: null,
  action: "idle",
  pendingFiles: [],
  pendingContexts: [],
  selection: null,
  currentFileText: "",
  renderedRunKey: "",
  frameKey: "",
  loadSequence: 0,
  inspectorWidth:
    Number(localStorage.getItem("webweave-inspector-width")) || 400,
  inspectorHidden:
    window.innerWidth <= 720 ||
    localStorage.getItem("webweave-inspector-hidden") === "true",
  sidebarCollapsed:
    window.innerWidth <= 960 ||
    localStorage.getItem("webweave-sidebar-collapsed") === "true",
  inspectMode: "preview",
  config: null,
  configReady: false,
  skills: [],
  skillErrors: [],
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
  document.querySelectorAll(".nav-item").forEach((item) => {
    item.classList.toggle("is-active", item.dataset.view === viewName);
  });
  document.querySelectorAll(".view").forEach((view) => {
    view.classList.toggle("is-active", view.id === `${viewName}-view`);
  });
}

function switchSettingsPage(pageName) {
  const isStyle = pageName === "style";
  const pageMeta = {
    config: ["config.json", "配置", "保存后由 Agent 热加载。"],
    llm: ["llm", "大模型", "配置主模型和可选的视觉模型。"],
    style: ["appearance", "样式", "主题与背景更改会自动保存。"],
  };
  const [eyebrow, title, description] = pageMeta[pageName] || pageMeta.config;
  document.querySelectorAll(".settings-tab").forEach((tab) => {
    tab.classList.toggle("is-active", tab.dataset.settingsPage === pageName);
  });
  document.querySelectorAll(".settings-page").forEach((page) => {
    page.classList.toggle("is-active", page.id === `settings-${pageName}-page`);
  });
  $("#settings-eyebrow").textContent = eyebrow;
  $("#settings-title").textContent = title;
  $("#settings-description").textContent = description;
  $(".settings-actions").hidden = isStyle;
  window.location.hash = pageName === "config" ? "settings" : `settings/${pageName}`;
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
  localStorage.setItem(
    "webweave-sidebar-collapsed",
    String(state.sidebarCollapsed),
  );
  refreshIcons();
}

function applyInspectorState() {
  document.documentElement.style.setProperty(
    "--inspector-width",
    `${state.inspectorWidth}px`,
  );
  workspaceGrid.classList.toggle("inspector-hidden", state.inspectorHidden);
  const label = state.inspectorHidden ? "展开执行检查区" : "隐藏执行检查区";
  inspectorToggle.replaceChildren(
    icon(state.inspectorHidden ? "panel-right-open" : "panel-right-close"),
  );
  inspectorToggle.title = label;
  inspectorToggle.setAttribute("aria-label", label);
  localStorage.setItem(
    "webweave-inspector-hidden",
    String(state.inspectorHidden),
  );
  refreshIcons();
}

function setInspectorHidden(hidden) {
  state.inspectorHidden = hidden;
  applyInspectorState();
}

function switchInspectorTab(tabName) {
  document.querySelectorAll(".inspector-tab").forEach((tab) => {
    tab.classList.toggle("is-active", tab.dataset.tab === tabName);
  });
  document.querySelectorAll(".inspector-panel").forEach((panel) => {
    panel.classList.toggle("is-active", panel.id === `${tabName}-panel`);
  });
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

  if (!state.runs.length) {
    history.append(element("div", "empty-list", "暂无对话"));
    recent.append(element("div", "empty-panel", "暂无最近工作"));
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
      `${run.run_id} · ${statusLabel(run.status)}`,
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
          `${run.run_id} · ${statusLabel(run.status)}`,
        ),
      );
      recentItem.append(
        mark,
        copy,
        element("time", "", formatTime(run.updated_at)),
        icon("chevron-right"),
      );
      recentItem.addEventListener("click", () => activateRun(run.run_id));
      recent.append(recentItem);
    }
  });
  refreshIcons();
}

async function loadRuns() {
  const payload = await api("/api/runs");
  state.runs = payload.runs;
  renderRuns();
}

function renderSkills() {
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
    state.selection = null;
    selectionContext.hidden = true;
    renderPendingContext();
    switchView("home");
  }
  await loadRuns();
  showToast("对话已删除");
}

async function createRun() {
  const payload = await api("/api/runs", { method: "POST" });
  await loadRuns();
  await activateRun(payload.run.run_id);
  promptInput.focus();
}

async function activateRun(runId) {
  state.activeRunId = runId;
  state.renderedRunKey = "";
  state.frameKey = "";
  state.selection = null;
  selectionContext.hidden = true;
  renderRuns();
  switchView("workspace");
  await loadActiveRun(true);
}

async function loadActiveRun(force = false) {
  if (!state.activeRunId) return;
  const sequence = ++state.loadSequence;
  const payload = await api(`/api/runs/${encodeURIComponent(state.activeRunId)}`);
  if (sequence !== state.loadSequence || payload.run.run_id !== state.activeRunId) {
    return;
  }
  state.activeRun = payload.run;
  const key = [
    payload.run.event_count,
    payload.run.status,
    payload.run.instance?.revision || 0,
    payload.run.instance?.status || "none",
  ].join(":");
  renderRunHeader(payload.run);
  updateActionFromRun(payload.run);
  renderInstance(payload.run.instance);
  if (force || key !== state.renderedRunKey) {
    state.renderedRunKey = key;
    renderConversation(payload.run);
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

function renderConversation(run) {
  const container = $("#conversation-scroll");
  const shouldStick =
    container.scrollHeight - container.scrollTop - container.clientHeight < 100;
  container.replaceChildren(conversationIntro(run));
  let toolEvents = [];

  const flushTools = () => {
    if (!toolEvents.length) return;
    container.append(createActivityCard(toolEvents));
    toolEvents = [];
  };
  let streamedText = "";

  const flushStreamedText = () => {
    if (!streamedText.trim()) {
      streamedText = "";
      return;
    }
    container.append(createAgentMessage(streamedText));
    streamedText = "";
  };

  run.events.forEach((event) => {
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
      const delta = event.payload?.delta;
      if (typeof delta === "string") streamedText += delta;
      return;
    }
    if (event.type === "model_output") {
      const text = event.payload?.output_text;
      flushTools();
      if (typeof text === "string" && text.trim()) {
        streamedText = text;
      }
      flushStreamedText();
    }
  });
  flushTools();
  flushStreamedText();

  if (run.job_active) {
    const working = element("div", "agent-working");
    working.append(element("span"), element("span"), element("span"));
    container.append(working);
  }
  refreshIcons();
  if (shouldStick || run.job_active) container.scrollTop = container.scrollHeight;
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
  article.append(body);
  return article;
}

function createAgentMessage(text) {
  const article = element("article", "message message-agent");
  article.append(element("div", "agent-avatar", "W"));
  const content = element("div", "message-content");
  content.append(element("div", "message-author", "WebWeave"));
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
      `${event.payload?.tool_name || "tool"}${toolArgumentHint(event.payload)}`,
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
    const hint = args.path || args.file_path || args.action || args.cwd;
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
      element("small", "", toolArgumentHint(event.payload).replace(/^ · /, "") || "完成"),
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
  if (!state.activeRunId) {
    await createRun();
  }
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
  state.pendingFiles.forEach((file, index) => {
    attachmentList.append(
      pendingChip("file", file.name, () => {
        state.pendingFiles.splice(index, 1);
        renderPendingContext();
      }),
    );
  });
  state.pendingContexts.forEach((context, index) => {
    const label = context.type === "component"
      ? `组件 ${context.component}`
      : `定点 (${context.point?.x}, ${context.point?.y})`;
    attachmentList.append(
      pendingChip(context.type === "component" ? "scan-search" : "crosshair", label, () => {
        state.pendingContexts.splice(index, 1);
        renderPendingContext();
      }),
    );
  });
  refreshIcons();
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

function renderInstance(instance) {
  const active = instance && ["starting", "running", "unhealthy"].includes(instance.status);
  state.activeRun.instance = instance;
  $("#instance-empty").hidden = Boolean(active);
  $("#instance-frame").hidden = !active;
  $("#instance-address").textContent = instance?.app_url
    ? instance.app_url.replace(/^https?:\/\//, "")
    : "未启动";
  $("#instance-live-dot").classList.toggle("is-offline", !active);
  ["#instance-refresh", "#instance-restart", "#instance-open", "#instance-stop"].forEach((selector) => {
    $(selector).disabled = !active;
  });
  if (!active) {
    instanceFrame.removeAttribute("src");
    state.frameKey = "";
    clearSelection();
    return;
  }
  const frameKey = `${instance.revision}:${instance.preview_url}`;
  if (frameKey !== state.frameKey) {
    state.frameKey = frameKey;
    instanceFrame.src = instance.preview_url;
  }
}

async function instanceAction(action) {
  if (!state.activeRunId) return;
  const payload = await api(
    `/api/runs/${encodeURIComponent(state.activeRunId)}/instance`,
    { method: "POST", body: JSON.stringify({ action, timeout: 30 }) },
  );
  if (action === "restart") showToast("实例已重启");
  if (action === "stop") showToast("实例已停止");
  renderInstance(payload);
  await loadActiveRun(true);
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
    setInspectMode(state.inspectMode);
    return;
  }
  if (event.data?.type !== "webweave:selection") return;
  const selection = {
    type: event.data.mode,
    run_id: state.activeRunId,
    app_url: state.activeRun?.instance?.app_url || null,
    ...event.data.selection,
  };
  renderSelection(selection);
}

function renderSelection(selection) {
  state.selection = selection;
  selectionContext.hidden = false;
  $("#selection-kind").textContent = selection.type === "component" ? "组件" : "定点";
  $("#selection-title").textContent = selection.component || selection.tag;
  $("#selection-detail").textContent = selection.source || selection.selector;
}

function clearSelection() {
  state.selection = null;
  selectionContext.hidden = true;
  instanceFrame.contentWindow?.postMessage(
    { type: "webweave:clear-selection" },
    window.location.origin,
  );
  setInspectMode("preview");
}

function injectSelectionIntoPrompt() {
  if (!state.selection) return;
  const selection = state.selection;
  let context;
  if (selection.type === "component") {
    context = [
      "[组件评价]",
      `组件: ${selection.component}`,
      `选择器: ${selection.selector}`,
      selection.source ? `源码: ${selection.source}` : null,
      "修改要求: ",
    ].filter(Boolean).join("\n");
  } else {
    context = [
      "[定点评价]",
      `位置: x=${selection.point?.x}, y=${selection.point?.y}`,
      `目标: ${selection.component} (${selection.selector})`,
      "评价: ",
    ].join("\n");
  }
  const current = promptInput.value.trim();
  promptInput.value = current ? `${current}\n\n${context}` : context;
  state.pendingContexts.push(selection);
  renderPendingContext();
  clearSelection();
  promptInput.focus();
  promptInput.setSelectionRange(promptInput.value.length, promptInput.value.length);
}

async function loadConfig() {
  setConfigAvailability(false);
  const payload = await api("/api/config");
  state.config = payload.config;
  renderConfig();
  setConfigAvailability(true);
}

function setConfigAvailability(available) {
  state.configReady = available;
  document
    .querySelectorAll(
      "#settings-config-page input, #settings-llm-page input, #settings-save",
    )
    .forEach((input) => {
      input.disabled = !available;
    });
  if (available) setVisionFieldsState();
}

function renderConfig() {
  const config = state.config;
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
  $("#timeout-default").value = config.tool_timeout.default_seconds;
  $("#timeout-max").value = config.tool_timeout.max_seconds;
  $("#context-enabled").checked = config.context.enabled;
  $("#context-max-input").value = config.context.max_input_tokens;
  $("#context-trigger").value = config.context.compression_trigger_ratio;
  $("#context-keep").value = config.context.keep_recent_items;
  $("#context-summary").value = config.context.summary_max_tokens;
  $("#context-timeout").value = config.context.compression_timeout_seconds;
  const llm = config.llm || {};
  const main = llm.main || {};
  const vision = llm.vision || {};
  $("#llm-main-model").value = main.model || "";
  $("#llm-main-base-url").value = main.base_url || "";
  setSecretInput($("#llm-main-api-key"), main.api_key);
  $("#llm-vision-enabled").checked = Boolean(vision.enabled);
  $("#llm-vision-model").value = vision.model || "";
  $("#llm-vision-base-url").value = vision.base_url || "";
  setSecretInput($("#llm-vision-api-key"), vision.api_key);
  setVisionFieldsState();
  $("#settings-state").classList.remove("is-dirty");
  $("#settings-state").lastChild.textContent = "配置已同步";
  document
    .querySelectorAll("#settings-config-page input, #settings-llm-page input")
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

function secretInputValue(selector) {
  const input = $(selector);
  return input.dataset.masked === "true" ? "" : input.value.trim();
}

function setVisionFieldsState() {
  const enabled = $("#llm-vision-enabled").checked;
  ["#llm-vision-model", "#llm-vision-base-url", "#llm-vision-api-key"].forEach(
    (selector) => {
      $(selector).disabled = !state.configReady || !enabled;
    },
  );
}

function toolIcon(name) {
  return {
    list_skills: "list-tree",
    read_skill: "book-open",
    web_search: "search",
    create_file: "file-plus-2",
    file_read: "file-search",
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
  const tools = {};
  document.querySelectorAll("#settings-tools [data-tool]").forEach((row) => {
    tools[row.dataset.tool] = {
      enabled: row.querySelector(".tool-enabled").checked,
      run_scoped: row.querySelector(".tool-run-scoped").checked,
    };
  });
  const payload = {
    llm: {
      main: {
        model: $("#llm-main-model").value.trim(),
        base_url: $("#llm-main-base-url").value.trim(),
        api_key: secretInputValue("#llm-main-api-key"),
      },
      vision: {
        enabled: $("#llm-vision-enabled").checked,
        model: $("#llm-vision-model").value.trim(),
        base_url: $("#llm-vision-base-url").value.trim(),
        api_key: secretInputValue("#llm-vision-api-key"),
      },
    },
    tools,
    model: {
      stream: $("#model-stream").checked,
    },
    tool_timeout: {
      default_seconds: Number($("#timeout-default").value),
      max_seconds: Number($("#timeout-max").value),
    },
    context: {
      enabled: $("#context-enabled").checked,
      max_input_tokens: Number($("#context-max-input").value),
      compression_trigger_ratio: Number($("#context-trigger").value),
      keep_recent_items: Number($("#context-keep").value),
      summary_max_tokens: Number($("#context-summary").value),
      compression_timeout_seconds: Number($("#context-timeout").value),
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
  document.querySelectorAll(".nav-item").forEach((button) => {
    button.addEventListener("click", async () => {
      if (button.dataset.view === "workspace" && !state.activeRunId) {
        await createRun();
        return;
      }
      if (button.dataset.view === "settings") switchSettingsPage("config");
      switchView(button.dataset.view);
    });
  });
  sidebarToggle.addEventListener("click", () => {
    state.sidebarCollapsed = !state.sidebarCollapsed;
    applySidebarState();
  });
  ["#new-run", "#home-new-run", "#home-start-button"].forEach((selector) => {
    $(selector).addEventListener("click", () => runSafely(createRun));
  });
  document.querySelectorAll(".inspector-tab").forEach((tab) => {
    tab.addEventListener("click", () => switchInspectorTab(tab.dataset.tab));
  });
  inspectorToggle.addEventListener("click", () => setInspectorHidden(!state.inspectorHidden));
  inspectorRail.addEventListener("click", () => setInspectorHidden(false));
  closeInspector.addEventListener("click", () => setInspectorHidden(true));
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
  $("#instance-refresh").addEventListener("click", () => {
    if (instanceFrame.src) instanceFrame.contentWindow.location.reload();
  });
  $("#instance-restart").addEventListener("click", () => runSafely(() => instanceAction("restart")));
  $("#instance-stop").addEventListener("click", () => runSafely(() => instanceAction("stop")));
  $("#instance-open").addEventListener("click", () => {
    const url = state.activeRun?.instance?.app_url;
    if (url) window.open(url, "_blank", "noopener,noreferrer");
  });
  $("#settings-save").addEventListener("click", () => runSafely(saveConfig));
  $("#llm-vision-enabled").addEventListener("change", () => {
    setVisionFieldsState();
    markConfigDirty();
  });
  document.querySelectorAll(".settings-tab").forEach((tab) => {
    tab.addEventListener("click", () => switchSettingsPage(tab.dataset.settingsPage));
  });
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
  window.addEventListener("resize", () => {
    const isMobile = window.innerWidth <= 720;
    if (isMobile && !wasMobile) setInspectorHidden(true);
    wasMobile = isMobile;
  });
  bindResize();
}

function bindResize() {
  resizeHandle.addEventListener("pointerdown", (event) => {
    if (window.innerWidth <= 720) return;
    event.preventDefault();
    document.body.classList.add("is-resizing");
    resizeHandle.setPointerCapture(event.pointerId);
    const onMove = (moveEvent) => {
      const bounds = workspaceGrid.getBoundingClientRect();
      const maximum = Math.min(720, bounds.width - 440);
      state.inspectorWidth = Math.max(
        280,
        Math.min(bounds.right - moveEvent.clientX, maximum),
      );
      document.documentElement.style.setProperty(
        "--inspector-width",
        `${state.inspectorWidth}px`,
      );
    };
    const onEnd = () => {
      document.body.classList.remove("is-resizing");
      localStorage.setItem(
        "webweave-inspector-width",
        String(Math.round(state.inspectorWidth)),
      );
      resizeHandle.removeEventListener("pointermove", onMove);
      resizeHandle.removeEventListener("pointerup", onEnd);
    };
    resizeHandle.addEventListener("pointermove", onMove);
    resizeHandle.addEventListener("pointerup", onEnd);
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
    if (state.activeRunId) await loadActiveRun();
    else await loadRuns();
    setConnection(true);
  } catch (error) {
    console.error(error);
    setConnection(false);
  }
}

async function initialize() {
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
  const target = window.location.hash.slice(1);
  if (target === "skills") {
    switchView("skills");
  }
  if (["settings", "settings/llm", "settings/style"].includes(target)) {
    switchView("settings");
    switchSettingsPage(target === "settings" ? "config" : target.split("/")[1]);
  }
  if (target === "workspace" && state.runs[0]) await activateRun(state.runs[0].run_id);
  if (["activity", "files", "instance"].includes(target) && state.runs[0]) {
    await activateRun(state.runs[0].run_id);
    setInspectorHidden(false);
    switchInspectorTab(target);
  }
  window.setInterval(poll, 1500);
}

initialize();
