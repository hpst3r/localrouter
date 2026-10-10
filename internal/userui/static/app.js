"use strict";
// LocalRouter user UI. Static, same-origin only. Server strings are rendered
// with textContent; the CSRF token and any new API key live only in memory
// (and, for the key, one DOM node) and are dropped on sign-out or 401.
(function (root) {
  const doc = root.document;

  const state = {
    user: null,
    csrf: "",
    gen: 0, // bumped on sign-out; responses from older generations are dropped
  };
  const pending = new Set();

  class ApiError extends Error {
    constructor(status, type) {
      super("request failed");
      this.status = status;
      this.type = type || "";
    }
  }
  const STALE = -1;

  function $(id) {
    return doc.getElementById(id);
  }

  // run executes an async UI action, tracking it and reporting failures as
  // fixed messages.
  function run(fn) {
    const p = Promise.resolve()
      .then(fn)
      .catch((err) => showError(err));
    pending.add(p);
    p.finally(() => pending.delete(p));
    return p;
  }

  async function api(method, path, body) {
    const gen = state.gen;
    const headers = { Accept: "application/json" };
    const init = { method, headers, credentials: "same-origin", mode: "same-origin", cache: "no-store", redirect: "error" };
    if (method !== "GET") {
      // Unsafe requests carry the in-memory CSRF token; the browser adds
      // Origin and Sec-Fetch-Site itself.
      if (!state.csrf) throw new ApiError(401);
      headers["X-LocalRouter-CSRF"] = state.csrf;
      if (body !== undefined) {
        headers["Content-Type"] = "application/json";
        init.body = JSON.stringify(body);
      }
    }
    let res;
    try {
      res = await fetch(path, init);
    } catch (e) {
      throw new ApiError(gen === state.gen ? 0 : STALE);
    }
    if (gen !== state.gen) throw new ApiError(STALE);
    if (res.status === 401) {
      signOutLocal();
      throw new ApiError(401);
    }
    if (!res.ok) throw new ApiError(res.status, await errorType(res));
    if (res.status === 204) return null;
    try {
      return await res.json();
    } catch (e) {
      throw new ApiError(502);
    }
  }

  // Messages are fixed strings chosen by status; response bodies are never
  // shown because they may carry internal detail.
  const MESSAGES = {
    0: "Network error. Check your connection and try again.",
    400: "The request was rejected. Check the values and try again.",
    403: "You do not have permission to do that.",
    404: "Not found. It may already have been removed.",
    409: "That action is not allowed in the current state.",
    413: "The request was too large.",
    429: "Too many requests. Try again shortly.",
    502: "Unexpected response from the server.",
    503: "Service temporarily unavailable. Try again shortly.",
  };

  // TYPE_MESSAGES maps machine-readable error types to fixed messages.
  const TYPE_MESSAGES = {
    key_limit: "You have reached the maximum number of API keys. Revoke one first.",
    reauth_required: "Your last sign-in is too old for this action. Sign out and sign in again.",
    self_action: "You cannot disable or delete your own account.",
    csrf: "Your session check failed. Reload the page and try again.",
  };

  // errorType reads only error.type from a JSON error body; the message is
  // never used.
  async function errorType(res) {
    try {
      const b = await res.json();
      const t = b && b.error && b.error.type;
      return typeof t === "string" && Object.prototype.hasOwnProperty.call(TYPE_MESSAGES, t) ? t : "";
    } catch (e) {
      return "";
    }
  }

  function errorMessage(err) {
    if (!(err instanceof ApiError)) return "Something went wrong.";
    return TYPE_MESSAGES[err.type] || MESSAGES[err.status] || "The server could not complete the request.";
  }

  function showError(err, overrides) {
    if (err instanceof ApiError && (err.status === STALE || err.status === 401)) return;
    const msg = (err instanceof ApiError && !err.type && overrides && overrides[err.status]) || errorMessage(err);
    const box = $("error");
    box.textContent = msg;
    box.hidden = false;
  }

  function showMessage(msg) {
    const box = $("error");
    box.textContent = msg;
    box.hidden = false;
  }

  function clearError() {
    const box = $("error");
    box.textContent = "";
    box.hidden = true;
  }

  function showView(name) {
    for (const s of $("main").children) {
      if (s.localName === "section") s.hidden = s.id !== "view-" + name;
    }
    for (const b of navButtons()) {
      if (b.getAttribute("data-view") === name) b.setAttribute("aria-current", "page");
      else b.removeAttribute("aria-current");
    }
  }

  function navButtons() {
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (c.localName === "button") out.push(c);
        else walk(c);
      }
    };
    walk($("nav"));
    return out;
  }

  // views maps a navigation name to its loader. Admin views are only
  // reachable when /me reported the admin role; the server enforces it too.
  const views = {
    profile: { admin: false, load: null },
    keys: { admin: false, load: loadKeys },
    usage: { admin: false, load: loadUsage },
    users: { admin: true, load: () => loadUsers(false) },
    audit: { admin: true, load: () => loadAudit(false) },
    global: { admin: true, load: loadGlobal },
    system: { admin: true, load: loadSystem },
  };

  function openView(name) {
    const v = views[name];
    if (!v || !state.user || (v.admin && state.user.role !== "admin")) return;
    clearError();
    clearNewKey();
    clearNotice();
    showView(name);
    if (v.load) run(v.load);
  }

  // loadInto shows a loading state in target while fn runs; fn renders into
  // target itself.
  async function loadInto(target, fn) {
    target.setAttribute("aria-busy", "true");
    target.replaceChildren(el("p", { class: "muted" }, "Loading…"));
    try {
      await fn();
    } catch (err) {
      if (!(err instanceof ApiError && err.status === STALE)) {
        target.replaceChildren(el("p", { class: "muted" }, "Could not load."));
      }
      throw err;
    } finally {
      target.removeAttribute("aria-busy");
    }
  }

  function table(columns, rows, empty) {
    if (rows.length === 0) return el("p", { class: "muted" }, empty);
    const head = el("tr", null, ...columns.map((c) => el("th", { scope: "col" }, c)));
    const body = rows.map((cells) => el("tr", null, ...cells.map((c) => el("td", null, c))));
    return el("table", null, el("thead", null, head), el("tbody", null, ...body));
  }

  // fmtTime renders an RFC 3339 time as "YYYY-MM-DD HH:MM UTC"; missing,
  // invalid and Go zero times render as a dash.
  function fmtTime(v) {
    const d = parseTime(v);
    return d ? d.toISOString().slice(0, 16).replace("T", " ") + " UTC" : "—";
  }

  function parseTime(v) {
    if (typeof v !== "string" || v === "") return null;
    const d = new Date(v);
    if (Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1) return null;
    return d;
  }

  // renderJSON renders an arbitrary JSON document as text-only tables and
  // definition lists, bounded in depth and size.
  const MAX_ROWS = 500;
  const MAX_COLS = 24;
  function isObj(v) {
    return v !== null && typeof v === "object" && !Array.isArray(v);
  }

  function humanize(k) {
    const s = String(k).replace(/_usd$/, " (USD)").replace(/_/g, " ");
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  function scalar(v, key) {
    if (v === null || v === undefined || v === "") return "—";
    if (typeof v === "boolean") return v ? "yes" : "no";
    if (typeof v === "number") {
      if (!Number.isFinite(v)) return "—";
      if (/(^|_)usd$/.test(key || "")) return "$" + v.toFixed(4);
      if (Number.isInteger(v)) return String(v).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
      return String(Number(v.toFixed(6)));
    }
    if (typeof v === "string") {
      if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(v)) return fmtTime(v);
      return v;
    }
    return "—";
  }

  function renderJSON(v, depth, key) {
    if (depth > 4) return doc.createTextNode("…");
    if (Array.isArray(v)) {
      if (v.length === 0) return el("span", { class: "muted" }, "None");
      if (v.every(isObj)) {
        const cols = [];
        for (const row of v.slice(0, MAX_ROWS)) {
          for (const k of Object.keys(row)) if (!cols.includes(k) && cols.length < MAX_COLS) cols.push(k);
        }
        const rows = v.slice(0, MAX_ROWS).map((row) => cols.map((c) => renderJSON(row[c], depth + 1, c)));
        return table(cols.map(humanize), rows, "None");
      }
      return doc.createTextNode(v.slice(0, MAX_ROWS).map((x) => (isObj(x) || Array.isArray(x) ? "…" : scalar(x, key))).join(", "));
    }
    if (isObj(v)) {
      const items = [];
      for (const [k, val] of Object.entries(v)) {
        if (k === "schema_version") continue;
        items.push(el("dt", null, humanize(k)), el("dd", null, renderJSON(val, depth + 1, k)));
      }
      return items.length ? el("dl", { class: "kv" }, ...items) : el("span", { class: "muted" }, "None");
    }
    return doc.createTextNode(scalar(v, key));
  }

  // loadPanels fetches each [targetId, path] into its container in parallel.
  // A failing panel shows "Could not load." and one fixed error; the others
  // still render.
  async function loadPanels(panels) {
    let first = null;
    await Promise.all(
      panels.map(([id, path, render]) =>
        loadInto($(id), async () => {
          const body = await api("GET", path);
          $(id).replaceChildren(render ? render(body) : renderJSON(body, 0, ""));
        }).catch((err) => {
          if (!first) first = err;
        })
      )
    );
    if (first) throw first;
  }

  function query(params) {
    return "?" + new URLSearchParams(params).toString();
  }

  // usageRows renders a usage summary document's rows as a table.
  function usageRows(body) {
    return renderJSON(body && Array.isArray(body.rows) ? body.rows : [], 0, "");
  }

  async function loadUsage() {
    const range = $("usage-range").value;
    const group = $("usage-group").value;
    await loadPanels([
      ["usage-budget", "/ui/v1/me/budget"],
      ["usage-summary", "/ui/v1/me/usage" + query({ since: range, group }), usageRows],
      ["usage-analytics", "/ui/v1/me/analytics" + query({ range })],
      ["usage-dimensions", "/ui/v1/me/dimensions" + query({ range })],
    ]);
  }

  // Admin: users. usersList accumulates pages; usersCursor is the server's
  // opaque next_cursor.
  let usersList = [];
  let usersCursor = "";

  function userList(body) {
    return body && Array.isArray(body.users) ? body.users.filter((u) => u && typeof u.id === "string" && u.id !== "") : [];
  }

  async function loadUsers(more) {
    const target = $("users");
    const path = "/ui/v1/admin/users" + (more && usersCursor ? query({ after: usersCursor }) : "");
    if (!more) $("user-keys").hidden = true;
    const fetchPage = async () => {
      const body = await api("GET", path);
      usersList = more ? usersList.concat(userList(body)) : userList(body);
      usersCursor = body && typeof body.next_cursor === "string" ? body.next_cursor : "";
      renderUsers();
    };
    if (more) await fetchPage();
    else await loadInto(target, fetchPage);
  }

  function renderUsers() {
    const rows = usersList.map((u) => {
      const name = str(u.display_name);
      const label = name || str(u.email) || u.id;
      const status = str(u.status);
      const actions = el("span", { class: "row-actions" });
      const button = (text, cls, aria, fn) => {
        const b = el("button", { type: "button", class: cls, "aria-label": aria + " " + label }, text);
        b.addEventListener("click", () => run(() => fn(u, label)));
        actions.appendChild(b);
      };
      button("Keys", null, "Show keys of", showUserKeys);
      if (u.id === state.user.id) {
        actions.appendChild(el("span", { class: "muted" }, "You"));
      } else if (status === "active") {
        button("Disable", "danger", "Disable", disableUser);
        button("Delete", "danger", "Delete", deleteUser);
      } else if (status === "disabled") {
        button("Enable", null, "Enable", enableUser);
        button("Delete", "danger", "Delete", deleteUser);
      }
      return [name || "—", str(u.email) || "—", u.id, str(u.role), status || "—", fmtTime(u.last_login_at), actions];
    });
    $("users").replaceChildren(table(["Name", "Email", "ID", "Role", "Status", "Last sign-in", "Actions"], rows, "No users."));
    $("users-more").hidden = usersCursor === "";
  }

  async function showUserKeys(u, label) {
    const base = "/ui/v1/admin/users/" + encodeURIComponent(u.id) + "/keys";
    const list = $("user-keys-list");
    $("user-keys-title").textContent = "Keys of " + label;
    $("user-keys").hidden = false;
    const revoke = async (k) => {
      const name = str(k.name) || str(k.id);
      if (!(await confirmAction("Revoke key \u201c" + name + "\u201d of \u201c" + label + "\u201d? Applications using it stop working immediately.", "Revoke key"))) return;
      clearError();
      await api("DELETE", base + "/" + encodeURIComponent(k.id));
      showNotice("Key revoked.");
      await load();
    };
    const load = () =>
      loadInto(list, async () => {
        const body = await api("GET", base);
        list.replaceChildren(table(["Name", "ID", "Created", "Expires", "Status", "Actions"], keyRows(keyList(body), revoke), "No API keys."));
      });
    await load();
  }

  const SELF_DENIED = { 409: "You cannot disable or delete your own account." };

  async function userAction(u, action, notice) {
    clearError();
    try {
      await api("POST", "/ui/v1/admin/users/" + encodeURIComponent(u.id) + "/" + action);
    } catch (err) {
      showError(err, SELF_DENIED);
      return;
    }
    showNotice(notice);
    await loadUsers(false);
  }

  async function disableUser(u, label) {
    const msg = "Disable \u201c" + label + "\u201d? Their API keys and sessions are revoked immediately and are not restored if the account is enabled again.";
    if (await confirmAction(msg, "Disable user")) await userAction(u, "disable", "User disabled.");
  }

  async function enableUser(u) {
    await userAction(u, "enable", "User enabled.");
  }

  async function deleteUser(u, label) {
    const msg = "Delete \u201c" + label + "\u201d? Their profile, keys and sessions are removed and they cannot sign in again. This cannot be undone.";
    if (await confirmAction(msg, "Delete user")) await userAction(u, "delete", "User deleted.");
  }

  // Admin: audit log, paged by the numeric sequence in next_cursor.
  let auditList = [];
  let auditCursor = null;

  async function loadAudit(more) {
    const target = $("audit");
    const path = "/ui/v1/admin/audit" + (more && auditCursor !== null ? query({ after: String(auditCursor) }) : "");
    const fetchPage = async () => {
      const body = await api("GET", path);
      const events = body && Array.isArray(body.events) ? body.events.filter(isObj) : [];
      auditList = more ? auditList.concat(events) : events;
      auditCursor = body && Number.isSafeInteger(body.next_cursor) && body.next_cursor >= 0 ? body.next_cursor : null;
      renderAudit();
    };
    if (more) await fetchPage();
    else await loadInto(target, fetchPage);
  }

  function renderAudit() {
    const rows = auditList.map((e) => [
      scalar(e.seq), fmtTime(e.at), scalar(str(e.action)), scalar(str(e.actor_kind)), scalar(str(e.actor_user_id)),
      scalar(str(e.target_user_id)), scalar(str(e.target_key_id)), scalar(str(e.outcome)), scalar(str(e.reason)),
    ]);
    $("audit").replaceChildren(
      table(["Seq", "Time", "Action", "Actor", "Actor user", "Target user", "Target key", "Outcome", "Reason"], rows, "No audit events.")
    );
    $("audit-more").hidden = auditCursor === null;
  }

  async function loadGlobal() {
    const range = $("global-range").value;
    const group = $("global-group").value;
    await loadPanels([
      ["global-summary", "/ui/v1/admin/usage" + query({ since: range, group }), usageRows],
      ["global-analytics", "/ui/v1/admin/analytics" + query({ range })],
      ["global-dimensions", "/ui/v1/admin/dimensions" + query({ range })],
    ]);
  }

  async function lookupBudget() {
    clearError();
    const key = $("budget-key").value.trim();
    if (key === "") {
      showMessage("Enter a name or ID to look up.");
      $("budget-key").focus();
      return;
    }
    await loadPanels([["global-budgets", "/ui/v1/admin/budgets" + query({ scope: $("budget-scope").value, key })]]);
  }

  async function loadSystem() {
    await loadPanels([
      ["system-status", "/ui/v1/admin/status"],
      ["system-diagnostics", "/ui/v1/admin/diagnostics"],
    ]);
  }

  function keyStatus(k) {
    if (parseTime(k.revoked_at)) return "Revoked";
    const exp = parseTime(k.expires_at);
    if (exp && exp.getTime() <= Date.now()) return "Expired";
    return "Active";
  }

  function keyRows(keys, onRevoke) {
    return keys.map((k) => {
      const status = keyStatus(k);
      const name = str(k.name);
      const actions = el("span", { class: "row-actions" });
      if (status === "Active") {
        const b = el("button", { type: "button", class: "danger", "aria-label": "Revoke key " + (name || str(k.id)) }, "Revoke");
        b.addEventListener("click", () => run(() => onRevoke(k)));
        actions.appendChild(b);
      }
      return [name || "—", str(k.id), fmtTime(k.created_at), fmtTime(k.expires_at), status, actions];
    });
  }

  function keyList(body) {
    return body && Array.isArray(body.keys) ? body.keys.filter((k) => k && typeof k.id === "string") : [];
  }

  async function loadKeys() {
    const target = $("keys");
    await loadInto(target, async () => {
      const body = await api("GET", "/ui/v1/me/keys");
      const rows = keyRows(keyList(body), revokeOwnKey);
      target.replaceChildren(table(["Name", "ID", "Created", "Expires", "Status", "Actions"], rows, "No API keys yet."));
    });
  }

  // confirmAction shows the modal confirmation dialog with the safe choice
  // focused and resolves true only when the destructive button is pressed.
  let confirmResolve = null;
  let confirmResult = false;
  function confirmAction(message, okLabel) {
    const dlg = $("confirm");
    if (confirmResolve) return Promise.resolve(false);
    $("confirm-text").textContent = message;
    $("confirm-ok").textContent = okLabel;
    confirmResult = false;
    return new Promise((resolve) => {
      confirmResolve = resolve;
      dlg.showModal();
      $("confirm-cancel").focus();
    });
  }

  function closeConfirm(result) {
    confirmResult = result;
    const dlg = $("confirm");
    if (dlg.open) dlg.close();
  }

  async function revokeOwnKey(k) {
    const label = str(k.name) || str(k.id);
    if (!(await confirmAction("Revoke key \u201c" + label + "\u201d? Applications using it stop working immediately.", "Revoke key"))) return;
    clearError();
    await api("DELETE", "/ui/v1/me/keys/" + encodeURIComponent(k.id));
    showNotice("Key revoked.");
    await loadKeys();
  }

  // newKeyToken is the only copy of a freshly created key held by this page.
  let newKeyToken = null;

  function showNewKey(token) {
    newKeyToken = token;
    $("new-key-token").textContent = token;
    $("new-key").hidden = false;
    $("new-key-copy").focus();
  }

  function clearNewKey() {
    newKeyToken = null;
    $("new-key-token").textContent = "";
    $("new-key").hidden = true;
  }

  async function logout() {
    clearError();
    const button = $("logout");
    button.disabled = true;
    try {
      await api("POST", "/auth/logout");
    } catch (err) {
      showError(err, { 403: "Sign out failed. Reload the page and try again." });
      return;
    } finally {
      button.disabled = false;
    }
    signOutLocal();
    showNotice("Signed out.");
  }

  async function copyNewKey() {
    const token = newKeyToken;
    if (!token) return;
    const cb = root.navigator && root.navigator.clipboard;
    try {
      if (!cb || typeof cb.writeText !== "function") throw new Error("no clipboard");
      await cb.writeText(token);
      showNotice("Copied to clipboard.");
    } catch (e) {
      showNotice("Copy failed. Select the key and copy it manually.");
    }
  }

  let noticeTimer = null;
  function clearNotice() {
    if (noticeTimer) clearTimeout(noticeTimer);
    noticeTimer = null;
    $("notice").textContent = "";
  }

  function showNotice(msg) {
    $("notice").textContent = msg;
    if (noticeTimer) clearTimeout(noticeTimer);
    noticeTimer = setTimeout(() => {
      $("notice").textContent = "";
      noticeTimer = null;
    }, 5000);
  }

  async function createKey() {
    clearError();
    clearNewKey();
    const nameInput = $("key-name");
    const name = nameInput.value.trim();
    if (name === "") {
      showMessage("Enter a name for the key.");
      nameInput.focus();
      return;
    }
    if (new TextEncoder().encode(name).length > 64) {
      showMessage("Key names can be at most 64 bytes.");
      nameInput.focus();
      return;
    }
    const req = { name };
    const ttl = Number($("key-ttl").value);
    if (Number.isInteger(ttl) && ttl > 0) req.ttl_seconds = ttl;
    const button = $("key-create");
    button.disabled = true;
    try {
      const body = await api("POST", "/ui/v1/me/keys", req);
      if (!body || typeof body.token !== "string" || body.token === "") throw new ApiError(502);
      nameInput.value = "";
      showNewKey(body.token);
    } finally {
      button.disabled = false;
    }
    await loadKeys();
  }

  // signOutLocal forgets everything tied to the session and shows the login
  // view. It never navigates; the login link is a fixed same-origin path.
  function signOutLocal() {
    state.gen++;
    state.user = null;
    state.csrf = "";
    clearNewKey();
    closeConfirm(false);
    usersList = [];
    usersCursor = "";
    auditList = [];
    auditCursor = null;
    for (const id of ["session-name", "session-role", "profile", "notice", "user-keys-title", ...DATA_IDS]) $(id).replaceChildren();
    for (const id of ["key-name", "budget-key"]) $(id).value = "";
    $("user-keys").hidden = true;
    $("users-more").hidden = true;
    $("audit-more").hidden = true;
    $("nav").hidden = true;
    $("session").hidden = true;
    showView("login");
  }

  // DATA_IDS are the containers that hold server data and are emptied on
  // sign-out.
  const DATA_IDS = [
    "keys", "usage-budget", "usage-summary", "usage-analytics", "usage-dimensions",
    "users", "user-keys-list", "audit", "global-summary", "global-analytics",
    "global-dimensions", "global-budgets", "system-status", "system-diagnostics",
  ];

  // el builds an element. Children are nodes or strings; strings always
  // become text nodes.
  function el(tag, attrs, ...children) {
    const e = doc.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of children) {
      if (c === null || c === undefined) continue;
      e.appendChild(typeof c === "object" ? c : doc.createTextNode(String(c)));
    }
    return e;
  }

  function str(v) {
    return typeof v === "string" ? v : "";
  }

  function kvList(target, pairs) {
    const nodes = [];
    for (const [k, v] of pairs) nodes.push(el("dt", null, k), el("dd", null, v === "" ? "—" : v));
    target.replaceChildren(...nodes);
  }

  async function loadMe() {
    const body = await api("GET", "/ui/v1/me");
    const u = body && body.user;
    if (!u || typeof u.id !== "string" || u.id === "" || typeof body.csrf_token !== "string" || body.csrf_token === "") {
      throw new ApiError(502);
    }
    state.user = {
      id: u.id,
      role: u.role === "admin" ? "admin" : "user",
      status: str(u.status),
      display_name: str(u.display_name),
      email: str(u.email),
    };
    state.csrf = body.csrf_token;
    renderSession();
    showView("profile");
  }

  function renderSession() {
    const u = state.user;
    $("session-name").textContent = u.display_name || u.email || u.id;
    $("session-role").textContent = u.role;
    $("session").hidden = false;
    $("nav").hidden = false;
    $("admin-nav").hidden = u.role !== "admin";
    kvList($("profile"), [
      ["User ID", u.id],
      ["Role", u.role],
      ["Status", u.status],
      ["Display name", u.display_name],
      ["Email", u.email],
    ]);
  }

  $("key-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    run(createKey);
  });

  $("confirm-ok").addEventListener("click", () => closeConfirm(true));
  $("confirm-cancel").addEventListener("click", () => closeConfirm(false));
  // Escape closes the dialog natively without pressing either button.
  $("confirm").addEventListener("close", () => {
    const resolve = confirmResolve;
    confirmResolve = null;
    $("confirm-text").textContent = "";
    if (resolve) resolve(confirmResult);
    confirmResult = false;
  });

  $("usage-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearError();
    run(loadUsage);
  });

  $("global-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearError();
    run(loadGlobal);
  });
  $("budget-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    run(lookupBudget);
  });
  $("system-refresh").addEventListener("click", () => {
    clearError();
    run(loadSystem);
  });
  $("users-more").addEventListener("click", () => run(() => loadUsers(true)));
  $("audit-more").addEventListener("click", () => run(() => loadAudit(true)));
  $("logout").addEventListener("click", () => run(logout));
  $("new-key-copy").addEventListener("click", () => run(copyNewKey));
  $("new-key-done").addEventListener("click", () => {
    clearNewKey();
    $("key-name").focus();
  });
  // Drop the key before the page can enter the back/forward cache.
  root.addEventListener("pagehide", clearNewKey);

  $("nav").addEventListener("click", (ev) => {
    const name = ev.target && ev.target.getAttribute && ev.target.getAttribute("data-view");
    if (name) openView(name);
  });

  async function boot() {
    const loading = $("view-loading");
    loading.replaceChildren(el("p", { class: "muted" }, "Loading…"));
    showView("loading");
    try {
      await loadMe();
    } catch (err) {
      if (err instanceof ApiError && (err.status === STALE || err.status === 401)) throw err;
      const retry = el("button", { type: "button" }, "Retry");
      retry.addEventListener("click", () => {
        clearError();
        run(boot);
      });
      loading.replaceChildren(el("p", { class: "muted" }, "Could not load your session."), el("p", null, retry));
      throw err;
    }
  }

  if (root.__LR_UI_TEST__) {
    root.__LR_UI_TEST__.idle = () => Promise.allSettled([...pending]);
    root.__LR_UI_TEST__.pending = () => pending.size;
  }
  run(boot);
})(window);
