(() => {
  const selectionMime = "application/x-webweave-selection";
  const outline = document.createElement("div");
  outline.id = "ww-inspect-outline";
  outline.dataset.wwInspectorUi = "";
  document.documentElement.append(outline);
  const selections = new Map();
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
  const publish = entry => parent.postMessage({ type: "webweave:selection", mode: entry.mode, selection: entry.data }, location.origin);
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

  document.addEventListener("click", (event) => {
    if (mode === "preview" || owned(event.target)) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    const target = targetFor(event.target);
    const rect = target.getBoundingClientRect();
    const offset = { x: event.clientX - rect.left, y: event.clientY - rect.top };
    const existing = [...selections.values()].find(entry => entry.mode === mode && entry.target === target &&
      (mode === "component" || (Math.round(entry.offset.x) === Math.round(offset.x) && Math.round(entry.offset.y) === Math.round(offset.y))));
    if (existing) {
      publish(existing);
      return;
    }
    const data = detail(target);
    if (mode === "point") data.point = { x: Math.round(event.pageX), y: Math.round(event.pageY), clientX: Math.round(event.clientX), clientY: Math.round(event.clientY) };
    const marker = document.createElement("div");
    marker.className = `ww-inspect-selection ww-inspect-${mode}`;
    marker.dataset.wwInspectorUi = "";
    marker.dataset.selectionId = data.selection_id;
    const handle = document.createElement("span");
    const kind = mode === "component" ? "组件" : "定点";
    handle.className = "ww-inspect-drag";
    handle.draggable = true;
    handle.textContent = mode === "component" ? `${data.ordinal} · ${data.component}` : String(data.ordinal);
    handle.title = `拖到对话输入区：${kind} ${data.ordinal}`;
    handle.addEventListener("dragstart", (dragEvent) => {
      dragEvent.dataTransfer.effectAllowed = "copy";
      dragEvent.dataTransfer.setData(selectionMime, data.selection_id);
      dragEvent.dataTransfer.setData("text/plain", `${kind} ${data.ordinal}`);
      parent.postMessage({ type: "webweave:selection-drag", selection_id: data.selection_id }, location.origin);
    });
    marker.append(handle);
    const entry = { mode, target, data, marker, offset };
    selections.set(data.selection_id, entry);
    document.documentElement.append(marker);
    positionMarker(entry);
    publish(entry);
  }, true);

  let scheduled = false;
  const updateMarkers = () => {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(() => {
      scheduled = false;
      outline.style.display = "none";
      selections.forEach(positionMarker);
    });
  };
  addEventListener("scroll", updateMarkers, true);
  addEventListener("resize", updateMarkers);

  addEventListener("message", (event) => {
    if (event.source !== parent || event.origin !== location.origin) return;
    if (event.data?.type === "webweave:set-mode") {
      mode = ["preview", "component", "point"].includes(event.data.mode) ? event.data.mode : "preview";
      document.documentElement.dataset.wwMode = mode;
      outline.style.display = "none";
    } else if (event.data?.type === "webweave:clear-selection") {
      selections.forEach(entry => entry.marker.remove());
      selections.clear();
      outline.style.display = "none";
    } else if (event.data?.type === "webweave:remove-selection") {
      const entry = selections.get(event.data.selection_id);
      if (entry) {
        entry.marker.remove();
        selections.delete(event.data.selection_id);
      }
    }
  });
  parent.postMessage({ type: "webweave:preview-ready" }, location.origin);
})();
