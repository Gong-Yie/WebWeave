const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "preview_bridge.js"), "utf8");

function bridgeSandbox() {
  const messages = [];
  const documentListeners = new Map();
  const windowListeners = new Map();
  const listen = (listeners, name, callback) => {
    if (!listeners.has(name)) listeners.set(name, []);
    listeners.get(name).push(callback);
  };
  class Element {
    constructor(tag = "div") {
      this.tagName = tag.toUpperCase();
      this.id = "";
      this.dataset = {};
      this.children = [];
      this.listeners = new Map();
      this.attributes = new Map();
      this.classList = { length: 0, toggle() {} };
      const properties = new Map();
      this.style = {
        setProperty(name, value, priority = "") { properties.set(name, { value, priority }); },
        getPropertyValue(name) { return properties.get(name)?.value || ""; },
        getPropertyPriority(name) { return properties.get(name)?.priority || ""; },
        removeProperty(name) { properties.delete(name); },
      };
    }
    append(child) { child.parentElement = this; this.children.push(child); }
    remove() { this.parentElement.children = this.parentElement.children.filter(child => child !== this); this.parentElement = null; }
    get isConnected() { return this === document.documentElement || Boolean(this.parentElement?.isConnected); }
    getAttribute(name) { return this.attributes.get(name) ?? null; }
    contains(target) { return target === this || this.children.some(child => child.contains(target)); }
    closest(selector) {
      if (selector === "[data-ww-inspector-ui]" && "wwInspectorUi" in this.dataset) return this;
      if (selector === "[data-selection-id]" && this.dataset.selectionId) return this;
      return this.parentElement?.closest(selector) || null;
    }
    getBoundingClientRect() {
      const translation = this.style.getPropertyValue("transform").match(/^translate\(([-\d.]+)px, ([-\d.]+)px\)/);
      const x = 20 + Number(translation?.[1] || 0);
      const y = 30 + Number(translation?.[2] || 0);
      return { x, y, left: x, top: y, right: x + 100, bottom: y + 40, width: 100, height: 40 };
    }
    addEventListener(name, callback) { listen(this.listeners, name, callback); }
    setPointerCapture(id) { this.capture = id; }
    hasPointerCapture(id) { return this.capture === id; }
    releasePointerCapture() { this.capture = null; }
  }
  const document = {
    documentElement: new Element("html"), body: new Element("body"),
    createElement(tag) { return new Element(tag); },
    addEventListener(name, callback) { listen(documentListeners, name, callback); },
  };
  document.documentElement.append(document.body);
  const parent = { postMessage(message) { messages.push(structuredClone(message)); } };
  const origin = "http://127.0.0.1:8765";
  let serial = 0;
  const sandbox = vm.createContext({
    document, parent, location: { origin }, Element, scrollX: 0, scrollY: 0,
    crypto: { randomUUID() { return `selection-${++serial}`; } },
    CSS: { escape(value) { return value; } },
    getComputedStyle(target) { return { transform: target.style.getPropertyValue("transform") || "none", display: "block" }; },
    requestAnimationFrame(callback) { callback(); },
    addEventListener(name, callback) { listen(windowListeners, name, callback); },
  });
  vm.runInContext(source, sandbox, { filename: "preview_bridge.js" });
  const emit = (listeners, type, fields = {}) => {
    const event = { button: 0, clientX: 25, clientY: 35, pointerId: 1, preventDefault() { this.prevented = true; }, stopImmediatePropagation() {}, ...fields };
    (listeners.get(type) || []).forEach(callback => callback(event));
    return event;
  };
  const receive = (data, fields = {}) => emit(windowListeners, "message", { source: parent, origin, data, ...fields });
  const command = (action, selection_ids = [], enabled) => receive({ type: "webweave:context-action", action, selection_ids, enabled });
  const target = (id) => {
    const element = new Element();
    element.id = id;
    document.body.append(element);
    return element;
  };
  const mark = (element) => {
    emit(documentListeners, "contextmenu", { target: element });
    command("mark");
    return messages.findLast(message => message.type === "webweave:selection").selection.selection_id;
  };
  const handles = () => document.documentElement.children.filter(child => child.className === "ww-inspect-move");
  return { document, parent, origin, messages, documentListeners, emit, receive, command, target, mark, handles };
}

test("bridge right-click marks targets and numbered labels while excluding point selections from movement", () => {
  const page = bridgeSandbox();
  const target = page.target("hero");
  const id = page.mark(target);
  const marker = page.document.documentElement.children.find(element => element.dataset.selectionId === id);
  const event = page.emit(page.documentListeners, "contextmenu", { target: marker.children[0] });
  assert.equal(event.prevented, true);
  assert.equal(page.messages.at(-1).selection_id, id);
  assert.equal(page.messages.at(-1).components.length, 1);
  page.receive({ type: "webweave:set-mode", mode: "point" });
  page.emit(page.documentListeners, "click", { target, clientX: 40, clientY: 45 });
  const point = page.messages.at(-1).selection.selection_id;
  page.command("move", [point], true);
  assert.equal(page.handles().length, 0);
  assert.equal(page.messages.at(-1).selection.point.x, 40);
  page.emit(page.documentListeners, "contextmenu", { target: page.document.body });
  assert.equal(page.messages.at(-1).can_mark, false);
});

test("bridge drag preserves existing transforms and restores only changed properties", () => {
  const page = bridgeSandbox();
  const target = page.target("hero");
  target.style.setProperty("transform", "scale(1.2)", "important");
  target.style.setProperty("transition", "opacity 1s");
  const id = page.mark(target);
  page.command("move", [id], true);
  const handle = page.handles()[0];
  page.emit(handle.listeners, "pointerdown");
  page.emit(handle.listeners, "pointermove", { clientX: 65, clientY: 55 });
  page.emit(handle.listeners, "pointerup");
  assert.equal(target.style.getPropertyValue("transform"), "translate(40px, 20px) scale(1.2)");
  assert.equal(page.messages.at(-1).selection.rect.x, 60);
  target.style.setProperty("color", "red");
  page.command("move", [id], false);
  assert.equal(handle.hidden, true);
  assert.match(target.style.getPropertyValue("transform"), /^translate/);
  page.command("reset", [id]);
  assert.equal(target.style.getPropertyValue("transform"), "scale(1.2)");
  assert.equal(target.style.getPropertyPriority("transform"), "important");
  assert.equal(target.style.getPropertyValue("transition"), "opacity 1s");
  assert.equal(target.style.getPropertyValue("color"), "red");
});

test("bridge batch hiding and clearing markers preserve temporary changes until restore-all", () => {
  const page = bridgeSandbox();
  const targets = [page.target("first"), page.target("second")];
  const ids = targets.map(page.mark);
  page.command("hide", ids, true);
  targets.forEach(target => assert.equal(target.style.getPropertyValue("visibility"), "hidden"));
  page.receive({ type: "webweave:clear-selection" });
  assert.equal(page.messages.filter(message => message.type === "webweave:selection-removed").length, 2);
  targets.forEach(target => assert.equal(target.style.getPropertyValue("visibility"), "hidden"));
  page.command("restore-all");
  targets.forEach(target => assert.equal(target.style.getPropertyValue("visibility"), ""));
  assert.equal(page.handles().length, 0);
});

test("bridge rejects foreign commands, unknown IDs and detached components", () => {
  const page = bridgeSandbox();
  const target = page.target("hero");
  const id = page.mark(target);
  for (const fields of [{ origin: "https://foreign.invalid" }, { source: {} }]) {
    page.receive({ type: "webweave:context-action", action: "hide", selection_ids: [id], enabled: true }, fields);
    assert.equal(target.style.getPropertyValue("visibility"), "");
  }
  page.command("hide", ["unknown"], true);
  target.remove();
  page.command("move", [id], true);
  assert.equal(page.handles().length, 0);
  page.emit(page.documentListeners, "contextmenu", { target: page.document.body });
  assert.equal(page.messages.at(-1).components.length, 0);
});
