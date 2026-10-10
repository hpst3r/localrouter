"use strict";
// A deliberately small DOM for running app.js under node:test without
// third-party packages. It implements only the APIs app.js uses; anything
// else is undefined so accidental use of HTML sinks fails loudly.

const VOID = new Set(["meta", "link", "input", "br", "hr", "img"]);

class Node {
  constructor(doc) {
    this.ownerDocument = doc;
    this.parentNode = null;
  }
  remove() {
    if (this.parentNode) this.parentNode.removeChild(this);
  }
}

class Text extends Node {
  constructor(doc, data) {
    super(doc);
    this.nodeType = 3;
    this.data = String(data);
  }
  get textContent() {
    return this.data;
  }
  set textContent(v) {
    this.data = String(v);
  }
}

class Element extends Node {
  constructor(doc, tag) {
    super(doc);
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.localName = tag.toLowerCase();
    this.attrs = new Map();
    this.childNodes = [];
    this.listeners = new Map();
    this._value = undefined;
    this.disabled = false;
    this.open = false;
  }
  get children() {
    return this.childNodes.filter((n) => n.nodeType === 1);
  }
  get id() {
    return this.getAttribute("id") || "";
  }
  set id(v) {
    this.setAttribute("id", v);
  }
  get className() {
    return this.getAttribute("class") || "";
  }
  set className(v) {
    this.setAttribute("class", v);
  }
  get hidden() {
    return this.attrs.has("hidden");
  }
  set hidden(v) {
    if (v) this.attrs.set("hidden", "");
    else this.attrs.delete("hidden");
  }
  get type() {
    return this.getAttribute("type") || (this.localName === "button" ? "submit" : "");
  }
  set type(v) {
    this.setAttribute("type", v);
  }
  get href() {
    return this.getAttribute("href");
  }
  get form() {
    let p = this.parentNode;
    while (p && p.localName !== "form") p = p.parentNode;
    return p;
  }
  get options() {
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (c.localName === "option") out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }
  get value() {
    if (this.localName === "select") {
      if (this._value !== undefined) return this._value;
      const opts = this.options;
      const sel = opts.find((o) => o.attrs.has("selected")) || opts[0];
      return sel ? sel.value : "";
    }
    if (this.localName === "option") {
      return this.attrs.has("value") ? this.getAttribute("value") : this.textContent;
    }
    if (this._value !== undefined) return this._value;
    return this.getAttribute("value") || "";
  }
  set value(v) {
    this._value = String(v);
  }
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join("");
  }
  set textContent(v) {
    for (const c of this.childNodes) c.parentNode = null;
    this.childNodes = [];
    if (v !== "" && v !== null && v !== undefined) this.appendChild(new Text(this.ownerDocument, v));
  }
  getAttribute(k) {
    return this.attrs.has(k) ? this.attrs.get(k) : null;
  }
  setAttribute(k, v) {
    if (/^on/i.test(k) || k === "style" || k === "srcdoc") throw new Error("fakedom: forbidden attribute " + k);
    this.attrs.set(k, String(v));
  }
  removeAttribute(k) {
    this.attrs.delete(k);
  }
  hasAttribute(k) {
    return this.attrs.has(k);
  }
  appendChild(n) {
    if (typeof n === "string") n = new Text(this.ownerDocument, n);
    if (!(n instanceof Node)) throw new Error("fakedom: appendChild of non-node");
    if (n.parentNode) n.parentNode.removeChild(n);
    n.parentNode = this;
    this.childNodes.push(n);
    return n;
  }
  append(...nodes) {
    for (const n of nodes) this.appendChild(n);
  }
  replaceChildren(...nodes) {
    this.textContent = "";
    this.append(...nodes);
  }
  removeChild(n) {
    const i = this.childNodes.indexOf(n);
    if (i >= 0) this.childNodes.splice(i, 1);
    n.parentNode = null;
    return n;
  }
  contains(n) {
    for (let p = n; p; p = p.parentNode) if (p === this) return true;
    return false;
  }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(fn);
  }
  removeEventListener(type, fn) {
    const l = this.listeners.get(type) || [];
    const i = l.indexOf(fn);
    if (i >= 0) l.splice(i, 1);
  }
  dispatchEvent(ev) {
    ev.target = ev.target || this;
    for (let n = this; n; n = n.parentNode) {
      ev.currentTarget = n;
      for (const fn of [...(n.listeners.get(ev.type) || [])]) fn.call(n, ev);
      if (ev._stopped || !ev.bubbles) break;
    }
    return !ev.defaultPrevented;
  }
  click() {
    if (this.disabled) return;
    const ev = new Event("click", { bubbles: true });
    if (this.dispatchEvent(ev) && this.localName === "button" && this.type === "submit" && this.form) {
      this.form.requestSubmit();
    }
  }
  requestSubmit() {
    this.dispatchEvent(new Event("submit", { bubbles: true }));
  }
  focus() {
    this.ownerDocument.activeElement = this;
  }
  select() {}
  showModal() {
    this.open = true;
    this.attrs.set("open", "");
  }
  close() {
    this.open = false;
    this.attrs.delete("open");
    this.dispatchEvent(new Event("close"));
  }
}

class Event {
  constructor(type, opts = {}) {
    this.type = type;
    this.bubbles = !!opts.bubbles;
    this.defaultPrevented = false;
    this.target = null;
    this.key = opts.key;
  }
  preventDefault() {
    this.defaultPrevented = true;
  }
  stopPropagation() {
    this._stopped = true;
  }
}

class Document {
  constructor() {
    this.documentElement = new Element(this, "html");
    this.body = null;
    this.activeElement = null;
    this.readyState = "complete";
    this.listeners = new Map();
  }
  createElement(tag) {
    return new Element(this, tag);
  }
  createTextNode(t) {
    return new Text(this, t);
  }
  getElementById(id) {
    let found = null;
    const walk = (n) => {
      for (const c of n.children) {
        if (found) return;
        if (c.id === id) {
          found = c;
          return;
        }
        walk(c);
      }
    };
    walk(this.documentElement);
    return found;
  }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(fn);
  }
}

function decode(s) {
  return s.replace(/&(amp|lt|gt|quot|#39);/g, (m, e) => ({ amp: "&", lt: "<", gt: ">", quot: '"', "#39": "'" })[e]);
}

// parse builds a Document from the trusted, well-formed index.html.
function parse(html) {
  const doc = new Document();
  const stack = [doc.documentElement];
  const re = /<!--[\s\S]*?-->|<!doctype[^>]*>|<\/([a-zA-Z0-9]+)\s*>|<([a-zA-Z0-9]+)((?:\s+[^\s=>/]+(?:\s*=\s*"[^"]*")?)*)\s*\/?>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html))) {
    const top = stack[stack.length - 1];
    if (m[1]) {
      const tag = m[1].toLowerCase();
      if (tag === "html") continue;
      while (stack.length > 1 && stack.pop().localName !== tag);
    } else if (m[2]) {
      const tag = m[2].toLowerCase();
      if (tag === "html") continue;
      const el = doc.createElement(tag);
      const are = /([^\s=]+)(?:\s*=\s*"([^"]*)")?/g;
      let a;
      while ((a = are.exec(m[3] || ""))) el.attrs.set(a[1].toLowerCase(), decode(a[2] || ""));
      top.appendChild(el);
      if (tag === "body") doc.body = el;
      if (!VOID.has(tag)) stack.push(el);
    } else if (m[4] && m[4].trim() !== "") {
      top.appendChild(doc.createTextNode(decode(m[4])));
    }
  }
  return doc;
}

// allText returns every text and attribute value in the document, for
// asserting that secrets or PII are gone.
function allText(doc) {
  const out = [];
  const walk = (n) => {
    if (n.nodeType === 3) {
      out.push(n.data);
      return;
    }
    for (const v of n.attrs.values()) out.push(v);
    if (n._value !== undefined) out.push(n._value);
    for (const c of n.childNodes) walk(c);
  };
  walk(doc.documentElement);
  return out.join("\n");
}

module.exports = { parse, allText, Event, Element, Text };
