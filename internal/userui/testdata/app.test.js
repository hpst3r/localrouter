"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { setup, visibleView } = require("./harness.js");

const CSRF = "csrf-fixture-value";
const unauth = { status: 401, body: { error: { message: "unauthenticated", type: "unauthenticated" } } };

function me(role = "user", extra = {}) {
  return {
    status: 200,
    body: {
      user: { id: "u_self", role, status: "active", display_name: "Ada Example", email: "ada@example.test", ...extra },
      csrf_token: CSRF,
    },
  };
}

test("signed-out session shows only the login view", async () => {
  const t = setup({ "GET /ui/v1/me": unauth });
  await t.idle();
  assert.equal(visibleView(t.doc), "view-login");
  assert.equal(t.$("login-link").href, "/auth/login");
  assert.equal(t.$("nav").hidden, true);
  assert.equal(t.$("session").hidden, true);
  const call = t.calls[0];
  assert.equal(call.key, "GET /ui/v1/me");
  assert.equal(call.init.credentials, "same-origin");
  assert.equal(call.init.cache, "no-store");
  assert.equal(call.init.headers["X-LocalRouter-CSRF"], undefined);
  assert.deepEqual(t.unknown, []);
});

const HOSTILE = '<img src=x onerror="alert(1)">';

test("signed-in user sees profile as plain text and no admin navigation", async () => {
  const t = setup({ "GET /ui/v1/me": me("user", { display_name: HOSTILE }) });
  await t.idle();
  assert.equal(visibleView(t.doc), "view-profile");
  assert.equal(t.$("nav").hidden, false);
  assert.equal(t.$("session").hidden, false);
  assert.equal(t.$("admin-nav").hidden, true);
  assert.equal(t.$("session-name").textContent, HOSTILE);
  assert.equal(t.$("session-role").textContent, "user");
  const profile = t.$("profile").textContent;
  for (const v of ["u_self", "user", "active", HOSTILE, "ada@example.test"]) assert.ok(profile.includes(v), v);
  // The hostile name is one text node, never parsed into elements.
  const imgs = [];
  const walk = (n) => n.childNodes && n.childNodes.forEach((c) => (c.localName === "img" ? imgs.push(c) : walk(c)));
  walk(t.doc.documentElement);
  assert.equal(imgs.length, 0);
});

test("admin sees admin navigation", async () => {
  const t = setup({ "GET /ui/v1/me": me("admin") });
  await t.idle();
  assert.equal(t.$("admin-nav").hidden, false);
  assert.equal(t.$("session-role").textContent, "admin");
});

test("server and network failures render fixed messages only", async () => {
  const leak = "sqlite: /var/lib/localrouter/identity.db is locked";
  for (const [spec, want] of [
    [{ status: 503, body: { error: { message: leak, type: "unavailable" } } }, "Service temporarily unavailable. Try again shortly."],
    [{ status: 500, body: { error: { message: leak } } }, "The server could not complete the request."],
    [{ network: true }, "Network error. Check your connection and try again."],
    [{ status: 200, body: { user: { id: "u1", role: "user" }, csrf_token: "" } }, "Unexpected response from the server."],
  ]) {
    const t = setup({ "GET /ui/v1/me": spec });
    await t.idle();
    assert.equal(t.$("error").hidden, false);
    assert.equal(t.$("error").textContent, want);
    assert.ok(!t.text().includes(leak));
    assert.equal(t.$("nav").hidden, true);
  }
});

const FUTURE = "2999-01-01T00:00:00Z";
const ZERO = "0001-01-01T00:00:00Z";
const keysDoc = {
  keys: [
    { id: "k_live", name: HOSTILE, created_at: "2026-10-01T12:00:00Z", expires_at: FUTURE, revoked_at: null },
    { id: "k_rev", name: "old", created_at: "2026-09-01T12:00:00Z", expires_at: FUTURE, revoked_at: "2026-09-02T08:30:00Z" },
    { id: "k_exp", name: "gone", created_at: "2026-01-01T00:00:00Z", expires_at: "2026-02-01T00:00:00Z", revoked_at: ZERO },
  ],
};

function descendants(node, tag) {
  const out = [];
  const walk = (n) => n.children.forEach((c) => (c.localName === tag && out.push(c), walk(c)));
  walk(node);
  return out;
}

function rowsOf(container) {
  const table = container.children.find((c) => c.localName === "table");
  if (!table) return [];
  const tbody = table.children.find((c) => c.localName === "tbody");
  return tbody.children.map((tr) => tr.children);
}

test("keys view lists own keys with status and revoke only for active keys", async () => {
  const t = setup({ "GET /ui/v1/me": me(), "GET /ui/v1/me/keys": { status: 200, body: keysDoc } });
  await t.idle();
  t.$("nav-keys").click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-keys");
  const rows = rowsOf(t.$("keys"));
  assert.equal(rows.length, 3);
  assert.equal(rows[0][0].textContent, HOSTILE);
  assert.equal(rows[0][1].textContent, "k_live");
  assert.equal(rows[0][2].textContent, "2026-10-01 12:00 UTC");
  assert.deepEqual(rows.map((r) => r[4].textContent), ["Active", "Revoked", "Expired"]);
  const buttons = rows.map((r) => descendants(r[5], "button"));
  assert.equal(buttons[0].length, 1);
  assert.equal(buttons[0][0].textContent, "Revoke");
  assert.equal(buttons[0][0].getAttribute("aria-label"), "Revoke key " + HOSTILE);
  assert.equal(buttons[1].length, 0);
  assert.equal(buttons[2].length, 0);
  assert.equal(t.$("nav-keys").getAttribute("aria-current"), "page");
  assert.deepEqual(t.unknown, []);
});

test("keys view shows an empty state", async () => {
  const t = setup({ "GET /ui/v1/me": me(), "GET /ui/v1/me/keys": { status: 200, body: { keys: [] } } });
  await t.idle();
  t.$("nav-keys").click();
  await t.idle();
  assert.equal(t.$("keys").textContent, "No API keys yet.");
});

test("keys view shows a loading state while fetching", async () => {
  let release;
  const wait = new Promise((r) => (release = r));
  const t = setup({ "GET /ui/v1/me": me(), "GET /ui/v1/me/keys": { status: 200, body: { keys: [] }, wait } });
  await t.idle();
  t.$("nav-keys").click();
  await new Promise((r) => setImmediate(r));
  assert.equal(t.$("keys").textContent, "Loading…");
  assert.equal(t.$("keys").getAttribute("aria-busy"), "true");
  release();
  await t.idle();
  assert.equal(t.$("keys").getAttribute("aria-busy"), null);
});

const TOKEN = "lrk_fixtureTOKENvalue0123456789abcdefghijklmn";
const created = (name) => ({
  status: 201,
  body: { key: { id: "k_new", name, created_at: "2026-10-10T00:00:00Z", expires_at: FUTURE, revoked_at: null }, token: TOKEN },
});

async function signedInKeys(routes) {
  const t = setup({ "GET /ui/v1/me": me(), "GET /ui/v1/me/keys": { status: 200, body: { keys: [] } }, ...routes });
  await t.idle();
  t.$("nav-keys").click();
  await t.idle();
  return t;
}

test("creating a key posts JSON with the CSRF header and shows the token once", async () => {
  const t = await signedInKeys({ "POST /ui/v1/me/keys": created("laptop") });
  t.$("key-name").value = "  laptop  ";
  t.$("key-create").click();
  await t.idle();
  const post = t.calls.find((c) => c.key === "POST /ui/v1/me/keys");
  assert.ok(post, "no POST");
  assert.deepEqual(post.body, { name: "laptop" });
  assert.equal(post.init.headers["X-LocalRouter-CSRF"], CSRF);
  assert.equal(post.init.headers["Content-Type"], "application/json");
  assert.equal(post.init.credentials, "same-origin");
  assert.equal(post.init.mode, "same-origin");
  assert.equal(post.init.headers.Origin, undefined, "Origin is set by the browser, never by script");
  assert.equal(t.$("new-key").hidden, false);
  assert.equal(t.$("new-key-token").textContent, TOKEN);
  assert.equal(t.$("key-name").value, "");
  assert.equal(t.doc.activeElement, t.$("new-key-copy"));
  assert.equal(t.calls.filter((c) => c.key === "GET /ui/v1/me/keys").length, 2, "list refreshed");
  assert.equal(t.text().split(TOKEN).length - 1, 1, "token rendered exactly once");
});

test("creating a key sends the chosen lifetime in seconds", async () => {
  const t = await signedInKeys({ "POST /ui/v1/me/keys": created("ci") });
  t.$("key-name").value = "ci";
  t.$("key-ttl").value = "604800";
  t.$("key-create").click();
  await t.idle();
  assert.deepEqual(t.calls.find((c) => c.method === "POST").body, { name: "ci", ttl_seconds: 604800 });
});

test("creating a key requires a name and sends nothing otherwise", async () => {
  const t = await signedInKeys({});
  t.$("key-name").value = "   ";
  t.$("key-create").click();
  await t.idle();
  assert.equal(t.calls.filter((c) => c.method === "POST").length, 0);
  assert.equal(t.$("error").textContent, "Enter a name for the key.");
  assert.equal(t.doc.activeElement, t.$("key-name"));
});

test("key creation needs fresh sign-in: reauth_required has a fixed message", async () => {
  const t = await signedInKeys({
    "POST /ui/v1/me/keys": { status: 403, body: { error: { message: "sign in again to continue", type: "reauth_required" } } },
  });
  t.$("key-name").value = "x";
  t.$("key-create").click();
  await t.idle();
  assert.equal(t.$("error").textContent, "Your last sign-in is too old for this action. Sign out and sign in again.");
});

test("key names longer than 64 bytes are refused locally", async () => {
  const t = await signedInKeys({});
  t.$("key-name").value = "\u00e9".repeat(33); // 66 bytes, 33 characters
  t.$("key-create").click();
  await t.idle();
  assert.equal(t.calls.filter((c) => c.method === "POST").length, 0);
  assert.equal(t.$("error").textContent, "Key names can be at most 64 bytes.");
});

test("key creation failures use fixed messages, including known error types", async () => {
  const t = await signedInKeys({
    "POST /ui/v1/me/keys": { status: 409, body: { error: { message: "user u_self has 10 keys (max 10)", type: "key_limit" } } },
  });
  t.$("key-name").value = "x";
  t.$("key-create").click();
  await t.idle();
  assert.equal(t.$("error").textContent, "You have reached the maximum number of API keys. Revoke one first.");
  assert.equal(t.$("new-key").hidden, true);
  assert.ok(!t.text().includes("max 10"));
});

async function withNewKey(routes = {}, expectToken = true) {
  const t = await signedInKeys({ "POST /ui/v1/me/keys": created("laptop"), ...routes });
  t.$("key-name").value = "laptop";
  t.$("key-create").click();
  await t.idle();
  if (expectToken) assert.equal(t.$("new-key-token").textContent, TOKEN);
  return t;
}

test("Done discards the token from the DOM", async () => {
  const t = await withNewKey();
  t.$("new-key-done").click();
  await t.idle();
  assert.equal(t.$("new-key").hidden, true);
  assert.ok(!t.text().includes(TOKEN));
  assert.equal(t.doc.activeElement, t.$("key-name"));
});

test("leaving the keys view or the page discards the token", async () => {
  const t = await withNewKey();
  t.$("nav-profile").click();
  await t.idle();
  assert.ok(!t.text().includes(TOKEN));
  const t2 = await withNewKey();
  t2.fireWindow("pagehide");
  assert.ok(!t2.text().includes(TOKEN));
});

test("copy writes the token to the clipboard only", async () => {
  const t = await withNewKey();
  t.$("new-key-copy").click();
  await t.idle();
  assert.deepEqual(t.clipboard, [TOKEN]);
  assert.equal(t.$("notice").textContent, "Copied to clipboard.");
});

test("a 401 clears the token, the CSRF token and the profile", async () => {
  let n = 0;
  const t = await withNewKey({
    "GET /ui/v1/me/keys": () => (++n > 1 ? unauth : { status: 200, body: { keys: [] } }),
  }, false);
  // withNewKey's refresh was the 2nd list call and returned 401.
  assert.equal(visibleView(t.doc), "view-login");
  assert.ok(!t.text().includes(TOKEN));
  assert.ok(!t.text().includes("ada@example.test"));
  assert.ok(!t.text().includes("Ada Example"));
  assert.equal(t.$("nav").hidden, true);
  // A submit from the hidden form cannot send anything: CSRF state is gone.
  const before = t.calls.length;
  t.$("key-name").value = "again";
  t.$("key-create").click();
  await t.idle();
  assert.equal(t.calls.length, before);
});

test("a token arriving after sign-out is never rendered", async () => {
  let release;
  const wait = new Promise((r) => (release = r));
  let n = 0;
  const t = await signedInKeys({
    "POST /ui/v1/me/keys": { ...created("slow"), wait },
    // Only the second list call is unauthenticated, so nothing after it
    // would hide a token that had been rendered.
    "GET /ui/v1/me/keys": () => (++n === 2 ? unauth : { status: 200, body: { keys: [] } }),
  });
  t.$("key-name").value = "slow";
  t.$("key-create").click();
  await new Promise((r) => setImmediate(r));
  // Another request signs the page out while the POST is in flight.
  t.$("nav-keys").click();
  await new Promise((r) => setImmediate(r));
  release();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-login");
  assert.ok(!t.text().includes(TOKEN));
});

test("revoking an own key asks for confirmation and encodes the id", async () => {
  const odd = { id: "k/../x?y", name: "odd", created_at: "2026-10-01T12:00:00Z", expires_at: FUTURE, revoked_at: null };
  let n = 0;
  const t = await signedInKeys({
    "GET /ui/v1/me/keys": () => (n++, { status: 200, body: { keys: [odd] } }),
    "DELETE /ui/v1/me/keys/k%2F..%2Fx%3Fy": { status: 204 },
  });
  const revoke = () => descendants(t.$("keys"), "button").find((b) => b.textContent === "Revoke");
  revoke().click();
  await new Promise((r) => setImmediate(r));
  assert.equal(t.$("confirm").open, true);
  assert.ok(t.$("confirm-text").textContent.includes("odd"));
  assert.equal(t.doc.activeElement, t.$("confirm-cancel"), "safe choice focused");
  t.$("confirm-cancel").click();
  await t.idle();
  assert.equal(t.$("confirm").open, false);
  assert.equal(t.calls.filter((c) => c.method === "DELETE").length, 0);

  const listCalls = n;
  revoke().click();
  await new Promise((r) => setImmediate(r));
  t.$("confirm-ok").click();
  await t.idle();
  const del = t.calls.filter((c) => c.method === "DELETE");
  assert.equal(del.length, 1);
  assert.equal(del[0].init.headers["X-LocalRouter-CSRF"], CSRF);
  assert.equal(del[0].init.body, undefined);
  assert.equal(n, listCalls + 1, "list refreshed");
  assert.equal(t.$("notice").textContent, "Key revoked.");
  assert.deepEqual(t.unknown, []);
});

test("sign out posts to /auth/logout with CSRF and forgets the session", async () => {
  const t = setup({ "GET /ui/v1/me": me(), "POST /auth/logout": { status: 204 } });
  await t.idle();
  t.$("logout").click();
  await t.idle();
  const post = t.calls.find((c) => c.key === "POST /auth/logout");
  assert.ok(post);
  assert.equal(post.init.headers["X-LocalRouter-CSRF"], CSRF);
  assert.equal(post.init.credentials, "same-origin");
  assert.equal(post.init.body, undefined);
  assert.equal(visibleView(t.doc), "view-login");
  assert.equal(t.$("notice").textContent, "Signed out.");
  assert.ok(!t.text().includes("ada@example.test"));
  assert.ok(!t.text().includes("u_self"));
});

test("failed sign out keeps the session and shows a fixed error", async () => {
  const t = setup({
    "GET /ui/v1/me": me(),
    "POST /auth/logout": { status: 403, body: { error: { message: "csrf mismatch for session lrs_abc" } } },
  });
  await t.idle();
  t.$("logout").click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-profile");
  assert.equal(t.$("error").textContent, "Sign out failed. Reload the page and try again.");
  assert.equal(t.$("logout").disabled, false);
});

const usageDoc = (model) => ({
  schema_version: 1,
  since: "2026-10-03T00:00:00Z",
  group: "model",
  rows: [{ key: model, requests: 12, input_tokens: 3400, cost_usd: 0.123456789 }],
});

test("usage view loads own usage, analytics, dimensions and budget", async () => {
  const t = setup({
    "GET /ui/v1/me": me(),
    "GET /ui/v1/me/usage?since=7d&group=model": { status: 200, body: usageDoc(HOSTILE) },
    "GET /ui/v1/me/analytics?range=7d": { status: 200, body: { totals: { requests: 12, cost_usd: 0.5 }, series: [] } },
    "GET /ui/v1/me/dimensions?range=7d": { status: 200, body: { dimensions: { model: [{ value: "m1", requests: 3 }] } } },
    "GET /ui/v1/me/budget": { status: 200, body: { enabled: true, day: { limit_usd: 5, spent_usd: 1.25 } } },
    "GET /ui/v1/me/usage?since=30d&group=class": { status: 200, body: usageDoc("interactive") },
    "GET /ui/v1/me/analytics?range=30d": { status: 200, body: { totals: { requests: 1 } } },
    "GET /ui/v1/me/dimensions?range=30d": { status: 503, body: { error: { message: "ledger: database is locked" } } },
  });
  await t.idle();
  t.$("nav-usage").click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-usage");
  const rows = rowsOf(t.$("usage-summary"));
  assert.equal(rows.length, 1);
  assert.equal(rows[0][0].textContent, HOSTILE);
  assert.equal(rows[0][1].textContent, "12");
  assert.equal(rows[0][3].textContent, "$0.1235");
  assert.ok(t.$("usage-analytics").textContent.includes("$0.5000"));
  assert.ok(t.$("usage-dimensions").textContent.includes("m1"));
  assert.ok(t.$("usage-budget").textContent.includes("$1.2500"));
  assert.ok(!t.$("usage-summary").textContent.includes("schema"));

  t.$("usage-range").value = "30d";
  t.$("usage-group").value = "class";
  t.$("usage-form").requestSubmit();
  await t.idle();
  assert.equal(rowsOf(t.$("usage-summary"))[0][0].textContent, "interactive");
  assert.equal(t.$("usage-dimensions").textContent, "Could not load.");
  assert.equal(t.$("error").textContent, "Service temporarily unavailable. Try again shortly.");
  assert.ok(!t.text().includes("locked"));
  assert.equal(t.calls.filter((c) => c.key === "GET /ui/v1/me/budget").length, 2);
  assert.deepEqual(t.unknown, []);
});

const userRow = (id, status, extra = {}) => ({
  id, role: "user", status, display_name: "Name " + id, email: id + "@example.test",
  created_at: "2026-09-01T00:00:00Z", last_login_at: "2026-10-09T10:00:00Z", ...extra,
});

async function adminUsers(routes) {
  const t = setup({
    "GET /ui/v1/me": me("admin"),
    "GET /ui/v1/admin/users": {
      status: 200,
      body: { users: [userRow("u_self", "active", { role: "admin" }), userRow("u_a", "active", { display_name: HOSTILE }), userRow("u_d", "disabled")], next_cursor: "c/2" },
    },
    ...routes,
  });
  await t.idle();
  t.$("nav-users").click();
  await t.idle();
  return t;
}

function buttonsIn(node) {
  return descendants(node, "button").map((b) => b.textContent);
}

test("admin users list shows lifecycle actions, never on the admin's own row", async () => {
  const t = await adminUsers({
    "GET /ui/v1/admin/users?after=c%2F2": { status: 200, body: { users: [userRow("u_z", "deleted")] } },
  });
  assert.equal(visibleView(t.doc), "view-users");
  const rows = rowsOf(t.$("users"));
  assert.equal(rows.length, 3);
  assert.equal(rows[1][0].textContent, HOSTILE);
  assert.equal(rows[1][1].textContent, "u_a@example.test");
  assert.equal(rows[1][5].textContent, "2026-10-09 10:00 UTC");
  assert.deepEqual(buttonsIn(rows[0][6]), ["Keys"]);
  assert.ok(rows[0][6].textContent.includes("You"));
  assert.deepEqual(buttonsIn(rows[1][6]), ["Keys", "Disable", "Delete"]);
  assert.deepEqual(buttonsIn(rows[2][6]), ["Keys", "Enable", "Delete"]);
  assert.equal(t.$("users-more").hidden, false);
  t.$("users-more").click();
  await t.idle();
  const all = rowsOf(t.$("users"));
  assert.equal(all.length, 4);
  assert.deepEqual(buttonsIn(all[3][6]), ["Keys"]);
  assert.equal(t.$("users-more").hidden, true);
  assert.deepEqual(t.unknown, []);
});

test("non-admins cannot open admin views", async () => {
  const t = setup({ "GET /ui/v1/me": me("user") });
  await t.idle();
  for (const id of ["nav-users", "nav-audit", "nav-global", "nav-system"]) t.$(id).click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-profile");
  assert.equal(t.calls.filter((c) => c.url.startsWith("/ui/v1/admin/")).length, 0);
});

function rowButton(t, rowIndex, label) {
  return descendants(rowsOf(t.$("users"))[rowIndex][6], "button").find((b) => b.textContent === label);
}

test("admin disable and delete confirm first; enable does not", async () => {
  const t = await adminUsers({
    "POST /ui/v1/admin/users/u_a/disable": { status: 204 },
    "POST /ui/v1/admin/users/u_a/delete": { status: 204 },
    "POST /ui/v1/admin/users/u_d/enable": { status: 204 },
  });
  rowButton(t, 1, "Disable").click();
  await new Promise((r) => setImmediate(r));
  assert.equal(t.$("confirm").open, true);
  assert.ok(t.$("confirm-text").textContent.includes(HOSTILE));
  assert.equal(t.$("confirm-ok").textContent, "Disable user");
  t.$("confirm-cancel").click();
  await t.idle();
  assert.equal(t.calls.filter((c) => c.method === "POST").length, 0);

  rowButton(t, 1, "Disable").click();
  await new Promise((r) => setImmediate(r));
  t.$("confirm-ok").click();
  await t.idle();
  rowButton(t, 1, "Delete").click();
  await new Promise((r) => setImmediate(r));
  assert.ok(t.$("confirm-text").textContent.includes("cannot be undone"));
  assert.equal(t.$("confirm-ok").textContent, "Delete user");
  t.$("confirm-ok").click();
  await t.idle();
  rowButton(t, 2, "Enable").click();
  await t.idle();
  assert.equal(t.$("confirm").open, false);

  const posts = t.calls.filter((c) => c.method === "POST");
  assert.deepEqual(posts.map((c) => c.url), [
    "/ui/v1/admin/users/u_a/disable",
    "/ui/v1/admin/users/u_a/delete",
    "/ui/v1/admin/users/u_d/enable",
  ]);
  for (const p of posts) {
    assert.equal(p.init.headers["X-LocalRouter-CSRF"], CSRF);
    assert.equal(p.init.body, undefined);
  }
  assert.equal(t.$("notice").textContent, "User enabled.");
  assert.equal(t.calls.filter((c) => c.key === "GET /ui/v1/admin/users").length, 4, "list refreshed after each action");
  assert.deepEqual(t.unknown, []);
});

test("self-action denial from the backend shows a fixed message", async () => {
  const t = await adminUsers({
    "POST /ui/v1/admin/users/u_a/disable": { status: 409, body: { error: { message: "actor u_self == target", type: "self_action" } } },
  });
  rowButton(t, 1, "Disable").click();
  await new Promise((r) => setImmediate(r));
  t.$("confirm-ok").click();
  await t.idle();
  assert.equal(t.$("error").textContent, "You cannot disable or delete your own account.");
  assert.ok(!t.text().includes("actor"));
});

test("admin views and revokes another user's keys", async () => {
  let n = 0;
  const t = await adminUsers({
    "GET /ui/v1/admin/users/u_a/keys": () => (n++, { status: 200, body: keysDoc }),
    "DELETE /ui/v1/admin/users/u_a/keys/k_live": { status: 204 },
  });
  rowButton(t, 1, "Keys").click();
  await t.idle();
  assert.equal(t.$("user-keys").hidden, false);
  assert.equal(t.$("user-keys-title").textContent, "Keys of " + HOSTILE);
  const rows = rowsOf(t.$("user-keys-list"));
  assert.equal(rows.length, 3);
  assert.equal(rows[0][1].textContent, "k_live");
  assert.ok(!t.text().includes("lrk_"), "admins never see key material");
  descendants(t.$("user-keys-list"), "button").find((b) => b.textContent === "Revoke").click();
  await new Promise((r) => setImmediate(r));
  assert.equal(t.$("confirm").open, true);
  t.$("confirm-ok").click();
  await t.idle();
  const del = t.calls.filter((c) => c.method === "DELETE");
  assert.deepEqual(del.map((c) => c.url), ["/ui/v1/admin/users/u_a/keys/k_live"]);
  assert.equal(del[0].init.headers["X-LocalRouter-CSRF"], CSRF);
  assert.equal(n, 2, "keys reloaded");
  assert.equal(t.$("notice").textContent, "Key revoked.");
  assert.deepEqual(t.unknown, []);
});

async function admin(routes, nav) {
  const t = setup({ "GET /ui/v1/me": me("admin"), ...routes });
  await t.idle();
  t.$(nav).click();
  await t.idle();
  return t;
}

test("admin audit log renders events as text and pages by sequence", async () => {
  const ev = (seq, target) => ({
    seq, at: "2026-10-09T10:00:00Z", action: "user_disable", actor_kind: "admin", actor_user_id: "u_self",
    target_user_id: target, target_key_id: "", outcome: "ok", reason: "",
  });
  const t = await admin({
    "GET /ui/v1/admin/audit": { status: 200, body: { events: [ev(7, HOSTILE)], next_cursor: 7 } },
    "GET /ui/v1/admin/audit?after=7": { status: 200, body: { events: [ev(8, "u_b")] } },
  }, "nav-audit");
  assert.equal(visibleView(t.doc), "view-audit");
  let rows = rowsOf(t.$("audit"));
  assert.equal(rows.length, 1);
  assert.deepEqual(rows[0].map((c) => c.textContent), ["7", "2026-10-09 10:00 UTC", "user_disable", "admin", "u_self", HOSTILE, "—", "ok", "—"]);
  assert.equal(t.$("audit-more").hidden, false);
  t.$("audit-more").click();
  await t.idle();
  rows = rowsOf(t.$("audit"));
  assert.equal(rows.length, 2);
  assert.equal(rows[1][5].textContent, "u_b");
  assert.equal(t.$("audit-more").hidden, true);
  assert.deepEqual(t.unknown, []);
});

test("admin global usage and budget lookup", async () => {
  const t = await admin({
    "GET /ui/v1/admin/usage?since=7d&group=account": { status: 200, body: usageDoc("acct-main") },
    "GET /ui/v1/admin/analytics?range=7d": { status: 200, body: { totals: { requests: 99 } } },
    "GET /ui/v1/admin/dimensions?range=7d": { status: 200, body: { dimensions: { client: [{ value: "c1" }] } } },
    "GET /ui/v1/admin/budgets?scope=client&key=lap+top": { status: 200, body: { enabled: true, scope: "client", key: "lap top" } },
  }, "nav-global");
  assert.equal(visibleView(t.doc), "view-global");
  assert.equal(rowsOf(t.$("global-summary"))[0][0].textContent, "acct-main");
  assert.ok(t.$("global-analytics").textContent.includes("99"));
  assert.ok(t.$("global-dimensions").textContent.includes("c1"));
  t.$("budget-form").requestSubmit();
  await t.idle();
  assert.equal(t.calls.filter((c) => c.url.startsWith("/ui/v1/admin/budgets")).length, 0);
  assert.equal(t.$("error").textContent, "Enter a name or ID to look up.");
  t.$("budget-key").value = " lap top ";
  t.$("budget-form").requestSubmit();
  await t.idle();
  assert.ok(t.$("global-budgets").textContent.includes("lap top"));
  assert.deepEqual(t.unknown, []);
});

test("admin status and diagnostics load and refresh", async () => {
  const t = await admin({
    "GET /ui/v1/admin/status": { status: 200, body: { accounts: [{ id: "acct-1", healthy: true }] } },
    "GET /ui/v1/admin/diagnostics": { status: 200, body: { ready: false, checks: [{ name: "identity", ok: true }] } },
  }, "nav-system");
  assert.equal(visibleView(t.doc), "view-system");
  assert.ok(t.$("system-status").textContent.includes("acct-1"));
  assert.ok(t.$("system-diagnostics").textContent.includes("identity"));
  t.$("system-refresh").click();
  await t.idle();
  assert.equal(t.calls.filter((c) => c.key === "GET /ui/v1/admin/status").length, 2);
  assert.deepEqual(t.unknown, []);
});

test("a failed session load offers a retry instead of loading forever", async () => {
  let n = 0;
  const t = setup({ "GET /ui/v1/me": () => (++n === 1 ? { status: 503 } : me()) });
  await t.idle();
  assert.equal(visibleView(t.doc), "view-loading");
  assert.ok(!t.$("view-loading").textContent.includes("Loading"));
  const retry = descendants(t.$("view-loading"), "button").find((b) => b.textContent === "Retry");
  assert.ok(retry);
  retry.click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-profile");
  assert.equal(t.$("error").hidden, true);
});

test("a 401 during an admin session leaves no user data in the page", async () => {
  const t = await adminUsers({ "GET /ui/v1/admin/users/u_a/keys": unauth });
  assert.ok(t.text().includes("u_a@example.test"));
  rowButton(t, 1, "Keys").click();
  await t.idle();
  assert.equal(visibleView(t.doc), "view-login");
  for (const pii of ["u_a@example.test", "Name u_d", HOSTILE, "u_self", "Keys of"]) assert.ok(!t.text().includes(pii), pii);
  assert.equal(t.$("users-more").hidden, true);
  assert.equal(t.$("user-keys").hidden, true);
});

test("notices do not carry over to another view", async () => {
  const t = await withNewKey();
  t.$("new-key-copy").click();
  await t.idle();
  assert.equal(t.$("notice").textContent, "Copied to clipboard.");
  t.$("nav-profile").click();
  await t.idle();
  assert.equal(t.$("notice").textContent, "");
});

test("generic tables group thousands and label USD columns", async () => {
  const t = setup({
    "GET /ui/v1/me": me(),
    "GET /ui/v1/me/usage?since=7d&group=model": { status: 200, body: { rows: [{ key: "m", input_tokens: 1234567, cost_usd: 2 }] } },
    "GET /ui/v1/me/analytics?range=7d": { status: 200, body: {} },
    "GET /ui/v1/me/dimensions?range=7d": { status: 200, body: {} },
    "GET /ui/v1/me/budget": { status: 200, body: {} },
  });
  await t.idle();
  t.$("nav-usage").click();
  await t.idle();
  const table = descendants(t.$("usage-summary"), "th").map((th) => th.textContent);
  assert.deepEqual(table, ["Key", "Input tokens", "Cost (USD)"]);
  assert.deepEqual(rowsOf(t.$("usage-summary"))[0].map((c) => c.textContent), ["m", "1,234,567", "$2.0000"]);
});
