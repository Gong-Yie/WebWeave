(() => {
  const selectionMime = "application/x-webweave-selection";
  const outline = document.createElement("div");
  outline.id = "ww-inspect-outline";
  outline.dataset.wwInspectorUi = "";
  document.documentElement.append(outline);
  const selections = new Map();
  const modifications = new Map();
  let contextTarget = null;
  let mode = "preview";
  let ordinal = 0;

  const selector = (element) => {
    if (element.id) return `#${CSS.escape(element.id)}`;
    const explicit = element.getAttribute("data-ww-selector") || element.getAttribute("data-selector");
    if (explicit) return explicit;
    const parts = [];
    for (let node = element; node && node.nodeType === 1 && parts.length < 5; node = node.parentElement) {
      let part = node.tagName.toLowerCase();
      if (node.classList.length) part += `.${[...node.classList].slice(0, 2).map(value => CSS.escape(value)).join(".")}`;
      if (node.parentElement) {
        const peers = [...node.parentElement.children].filter(item => item.tagName === node.tagName);
        if (peers.length > 1) part += `:nth-of-type(${peers.indexOf(node) + 1})`;
      }
      parts.unshift(part);
    }
    return parts.join(" > ");
  };
  const targetFor = element => element.closest("[data-ww-component],[data-component]") || element;
  const detail = (target) => {
    const rect = target.getBoundingClientRect();
    return {
      selection_id: crypto.randomUUID(),
      ordinal: ++ordinal,
      component: target.getAttribute("data-ww-component") || target.getAttribute("data-component") || target.tagName.toLowerCase(),
      selector: selector(target),
      source: target.getAttribute("data-ww-source") || null,
      tag: target.tagName.toLowerCase(),
      text: (target.innerText || target.getAttribute("aria-label") || "").trim().slice(0, 180),
      rect: { x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height) },
    };
  };
  const publish = (entry) => {
    const rect = entry.target.getBoundingClientRect();
    entry.data.rect = { x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height) };
    if (entry.mode === "point") {
      const x = Math.round(rect.left + entry.offset.x);
      const y = Math.round(rect.top + entry.offset.y);
      entry.data.point = { x: Math.round(x + scrollX), y: Math.round(y + scrollY), clientX: x, clientY: y };
    }
    parent.postMessage({ type: "webweave:selection", mode: entry.mode, selection: entry.data }, location.origin);
  };
  const positionMarker = (entry) => {
    entry.marker.hidden = !entry.target.isConnected;
    if (entry.marker.hidden) return;
    const rect = entry.target.getBoundingClientRect();
    if (entry.mode === "component") {
      Object.assign(entry.marker.style, { left: `${rect.left}px`, top: `${rect.top}px`, width: `${rect.width}px`, height: `${rect.height}px` });
      entry.marker.classList.toggle("ww-inspect-label-inside", rect.top < 28);
    } else {
      Object.assign(entry.marker.style, { left: `${rect.left + entry.offset.x}px`, top: `${rect.top + entry.offset.y}px` });
    }
  };
  const owned = target => target instanceof Element && target.closest("[data-ww-inspector-ui]");

  document.addEventListener("pointermove", (event) => {
    if (mode !== "component" || owned(event.target)) return;
    const rect = targetFor(event.target).getBoundingClientRect();
    Object.assign(outline.style, { display: "block", left: `${rect.left}px`, top: `${rect.top}px`, width: `${rect.width}px`, height: `${rect.height}px` });
  }, true);

  const mark = (target, selectionMode, x, y) => {
    const rect = target.getBoundingClientRect();
    const offset = { x: x - rect.left, y: y - rect.top };
    const existing = [...selections.values()].find(entry => entry.mode === selectionMode && entry.target === target &&
      (selectionMode === "component" || (Math.round(entry.offset.x) === Math.round(offset.x) && Math.round(entry.offset.y) === Math.round(offset.y))));
    if (existing) {
      publish(existing);
      return;
    }
    const data = detail(target);
    if (selectionMode === "point") data.point = {};
    const marker = document.createElement("div");
    marker.className = `ww-inspect-selection ww-inspect-${selectionMode}`;
    marker.dataset.wwInspectorUi = "";
    marker.dataset.selectionId = data.selection_id;
    const handle = document.createElement("span");
    const kind = selectionMode === "component" ? "组件" : "定点";
    handle.className = "ww-inspect-drag";
    handle.draggable = true;
    handle.textContent = selectionMode === "component" ? `${data.ordinal} · ${data.component}` : String(data.ordinal);
    handle.title = `拖到对话输入区：${kind} ${data.ordinal}`;
    handle.addEventListener("dragstart", (dragEvent) => {
      dragEvent.dataTransfer.effectAllowed = "copy";
      dragEvent.dataTransfer.setData(selectionMime, data.selection_id);
      dragEvent.dataTransfer.setData("text/plain", `${kind} ${data.ordinal}`);
      parent.postMessage({ type: "webweave:selection-drag", selection_id: data.selection_id }, location.origin);
    });
    marker.append(handle);
    const entry = { mode: selectionMode, target, data, marker, offset };
    selections.set(data.selection_id, entry);
    document.documentElement.append(marker);
    positionMarker(entry);
    publish(entry);
  };

  document.addEventListener("click", (event) => {
    if (mode === "preview" || owned(event.target)) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    mark(targetFor(event.target), mode, event.clientX, event.clientY);
  }, true);

  // Remember only properties we change; restoring must not replace the site's full style attribute.
  const setTemporaryStyle = (mod, name, value) => {
    if (!mod.original.has(name)) {
      mod.original.set(name, { value: mod.target.style.getPropertyValue(name), priority: mod.target.style.getPropertyPriority(name) });
    }
    mod.target.style.setProperty(name, value, "important");
  };
  const restoreStyle = (mod, names) => {
    names.forEach((name) => {
      const original = mod.original.get(name);
      if (!original) return;
      if (original.value) mod.target.style.setProperty(name, original.value, original.priority);
      else mod.target.style.removeProperty(name);
      mod.original.delete(name);
    });
  };
  const positionMoveHandle = (mod) => {
    mod.handle.hidden = !mod.movable || mod.hidden || !mod.target.isConnected;
    if (mod.handle.hidden) return;
    const rect = mod.target.getBoundingClientRect();
    Object.assign(mod.handle.style, { left: `${Math.max(0, rect.right - 56)}px`, top: `${Math.max(0, rect.top)}px` });
  };
  const modificationFor = (target) => {
    if (modifications.has(target)) return modifications.get(target);
    const handle = document.createElement("button");
    handle.type = "button";
    handle.className = "ww-inspect-move";
    handle.dataset.wwInspectorUi = "";
    handle.textContent = "⠿ 移动";
    handle.title = "拖动以临时移动组件；右键可还原";
    const mod = { target, handle, original: new Map(), x: 0, y: 0, movable: false, hidden: false, drag: null };
    handle.addEventListener("pointerdown", (event) => {
      if (event.button !== 0) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      if (!mod.original.has("transform")) {
        const computed = getComputedStyle(target);
        mod.baseTransform = computed.transform === "none" ? "" : computed.transform;
        if (computed.display === "inline") setTemporaryStyle(mod, "display", "inline-block");
      }
      mod.drag = { id: event.pointerId, x: event.clientX, y: event.clientY, startX: mod.x, startY: mod.y };
      handle.setPointerCapture(event.pointerId);
    });
    handle.addEventListener("pointermove", (event) => {
      if (!mod.drag || mod.drag.id !== event.pointerId) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      mod.x = mod.drag.startX + event.clientX - mod.drag.x;
      mod.y = mod.drag.startY + event.clientY - mod.drag.y;
      setTemporaryStyle(mod, "transition", "none");
      setTemporaryStyle(mod, "transform", `translate(${mod.x}px, ${mod.y}px) ${mod.baseTransform}`);
      selections.forEach(positionMarker);
      modifications.forEach(positionMoveHandle);
    });
    const finishDrag = (event) => {
      if (!mod.drag || mod.drag.id !== event.pointerId) return;
      mod.drag = null;
      if (handle.hasPointerCapture(event.pointerId)) handle.releasePointerCapture(event.pointerId);
      selections.forEach(publish);
    };
    ["pointerup", "pointercancel", "lostpointercapture"].forEach(type => handle.addEventListener(type, finishDrag));
    handle.addEventListener("click", event => { event.preventDefault(); event.stopImmediatePropagation(); });
    modifications.set(target, mod);
    document.documentElement.append(handle);
    positionMoveHandle(mod);
    return mod;
  };
  const resetPosition = (mod) => {
    mod.drag = null;
    mod.x = 0;
    mod.y = 0;
    restoreStyle(mod, ["transform", "transition", "display"]);
    positionMoveHandle(mod);
  };
  const removeSelection = (id) => {
    const entry = selections.get(id);
    if (!entry) return;
    entry.marker.remove();
    selections.delete(id);
    parent.postMessage({ type: "webweave:selection-removed", selection_id: id }, location.origin);
  };

  document.addEventListener("contextmenu", (event) => {
    if (!(event.target instanceof Element)) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    const marker = event.target.closest("[data-selection-id]");
    const move = [...modifications.values()].find(mod => mod.handle === event.target);
    const marked = marker && owned(marker) ? selections.get(marker.dataset.selectionId) : null;
    contextTarget = marked?.target || move?.target || targetFor(event.target);
    const entry = marked || [...selections.values()].find(item => item.target === contextTarget && item.mode === "component") ||
      [...selections.values()].find(item => item.target.contains(contextTarget));
    if (entry) contextTarget = entry.target;
    selections.forEach(publish);
    parent.postMessage({
      type: "webweave:context-menu", x: event.clientX, y: event.clientY,
      selection_id: entry?.data.selection_id || null,
      can_mark: !entry && !owned(contextTarget) && ![document.body, document.documentElement].includes(contextTarget),
      components: [...selections.values()].filter(item => item.mode === "component" && item.target.isConnected).map((item) => {
        const mod = modifications.get(item.target);
        return { selection_id: item.data.selection_id, movable: Boolean(mod?.movable), hidden: Boolean(mod?.hidden), moved: Boolean(mod?.x || mod?.y) };
      }),
      modified: [...modifications.values()].some(mod => mod.original.size || mod.movable),
    }, location.origin);
  }, true);
  document.addEventListener("pointerdown", () => parent.postMessage({ type: "webweave:close-menu" }, location.origin), true);
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") parent.postMessage({ type: "webweave:close-menu" }, location.origin);
  }, true);

  const applyAction = (message) => {
    if (message.action === "mark" && contextTarget?.isConnected && !owned(contextTarget) &&
        ![document.body, document.documentElement].includes(contextTarget)) {
      const rect = contextTarget.getBoundingClientRect();
      mark(contextTarget, "component", rect.left, rect.top);
    } else if (message.action === "restore-all") {
      modifications.forEach((mod) => {
        resetPosition(mod);
        restoreStyle(mod, [...mod.original.keys()]);
        mod.handle.remove();
      });
      modifications.clear();
    } else if (Array.isArray(message.selection_ids)) {
      message.selection_ids.forEach((id) => {
        const entry = selections.get(id);
        if (!entry || !entry.target.isConnected) return;
        if (message.action === "remove") removeSelection(id);
        if (entry.mode !== "component" || !["move", "reset", "hide"].includes(message.action)) return;
        const mod = modificationFor(entry.target);
        if (message.action === "move") mod.movable = message.enabled === true;
        if (message.action === "reset") resetPosition(mod);
        if (message.action === "hide") {
          mod.hidden = message.enabled === true;
          if (mod.hidden) setTemporaryStyle(mod, "visibility", "hidden");
          else restoreStyle(mod, ["visibility"]);
        }
        positionMoveHandle(mod);
      });
    }
    selections.forEach(positionMarker);
    selections.forEach(publish);
  };

  let scheduled = false;
  const updateMarkers = () => {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(() => {
      scheduled = false;
      outline.style.display = "none";
      selections.forEach(positionMarker);
      modifications.forEach(positionMoveHandle);
    });
  };
  const viewportChanged = () => {
    updateMarkers();
    parent.postMessage({ type: "webweave:close-menu" }, location.origin);
  };
  addEventListener("scroll", viewportChanged, true);
  addEventListener("resize", viewportChanged);

  addEventListener("message", (event) => {
    if (event.source !== parent || event.origin !== location.origin) return;
    if (event.data?.type === "webweave:set-mode") {
      mode = ["preview", "component", "point"].includes(event.data.mode) ? event.data.mode : "preview";
      document.documentElement.dataset.wwMode = mode;
      outline.style.display = "none";
    } else if (event.data?.type === "webweave:clear-selection") {
      [...selections.keys()].forEach(removeSelection);
      outline.style.display = "none";
    } else if (event.data?.type === "webweave:remove-selection") {
      removeSelection(event.data.selection_id);
    } else if (event.data?.type === "webweave:context-action") {
      applyAction(event.data);
    }
  });
  parent.postMessage({ type: "webweave:preview-ready" }, location.origin);
})();
