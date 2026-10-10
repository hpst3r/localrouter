"use strict";
// harness loads the real index.html and app.js into a fresh vm context with
// a fake DOM, a scripted fetch and no storage, console or location globals.

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { parse, allText } = require("./fakedom.js");

const STATIC = path.join(__dirname, "..", "static");
const INDEX = fs.readFileSync(path.join(STATIC, "index.html"), "utf8");
const APP = fs.readFileSync(path.join(STATIC, "app.js"), "utf8");

function jsonResponse(status, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: (k) => (k.toLowerCase() === "content-type" && body !== undefined ? "application/json" : null) },
    json: async () => {
      if (body === undefined) throw new SyntaxError("no body");
      return JSON.parse(JSON.stringify(body));
    },
  };
}

// setup starts the app. routes maps "METHOD /path?query" to a response spec
// {status, body, wait} or a function(init) returning one. wait is a promise
// the response waits for. A spec of {network: true} rejects like fetch does.
function setup(routes, opts = {}) {
  const doc = parse(INDEX);
  const calls = [];
  const unknown = [];
  const clipboard = [];
  const fetch = async (url, init = {}) => {
    const method = (init.method || "GET").toUpperCase();
    const key = method + " " + url;
    calls.push({ key, url, method, init, body: init.body === undefined ? undefined : JSON.parse(init.body) });
    let spec = routes[key];
    if (typeof spec === "function") spec = spec(init, calls[calls.length - 1]);
    if (!spec) {
      unknown.push(key);
      return jsonResponse(404, { error: { message: "no route " + key } });
    }
    if (spec.wait) await spec.wait;
    if (spec.network) throw new TypeError("Failed to fetch");
    return jsonResponse(spec.status, spec.body);
  };
  const windowListeners = new Map();
  const ctx = {
    document: doc,
    fetch,
    URLSearchParams,
    TextEncoder,
    encodeURIComponent,
    Date,
    JSON,
    Promise,
    setTimeout,
    clearTimeout,
    Number,
    String,
    Object,
    Array,
    Math,
    navigator: {
      clipboard: opts.noClipboard
        ? undefined
        : {
            writeText: async (t) => {
              clipboard.push(t);
            },
          },
    },
    addEventListener: (type, fn) => {
      if (!windowListeners.has(type)) windowListeners.set(type, []);
      windowListeners.get(type).push(fn);
    },
    __LR_UI_TEST__: {},
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(APP, ctx, { filename: "app.js" });
  const hook = ctx.__LR_UI_TEST__;
  const $ = (id) => {
    const el = doc.getElementById(id);
    if (!el) throw new Error("no element #" + id);
    return el;
  };
  // idle waits until the app has no outstanding work, including work queued
  // by work that just finished.
  const idle = async () => {
    for (let i = 0; i < 50; i++) {
      await new Promise((r) => setImmediate(r));
      if (typeof hook.idle !== "function") {
        if (i >= 5) return;
        continue;
      }
      await hook.idle();
      await new Promise((r) => setImmediate(r));
      if (hook.pending() === 0) return;
    }
    throw new Error("app never became idle");
  };
  const fire = (window, type) => (windowListeners.get(type) || []).forEach((fn) => fn({ type }));
  return { doc, $, calls, unknown, clipboard, idle, text: () => allText(doc), fireWindow: (t) => fire(ctx, t) };
}

// visibleView returns the id of the single visible view section.
function visibleView(doc) {
  const main = doc.getElementById("main");
  const shown = main.children.filter((c) => c.localName === "section" && !c.hidden).map((c) => c.id);
  if (shown.length !== 1) throw new Error("visible views: " + JSON.stringify(shown));
  return shown[0];
}

module.exports = { setup, visibleView };
