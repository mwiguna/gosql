import assert from "node:assert/strict";
import { beforeEach, test } from "node:test";
import { initializeSession, refreshSession, setConnected } from "../features/session.js";
import { connectToDatabase } from "../features/connections.js";
import { state } from "../state.js";
import {
  MAX_HISTORY_ENTRIES, MAX_SQL_LENGTH, appendQueryHistory, saveQuery,
  loadWorkspaceData, clearQueryHistory, deleteHistoryEntry, deleteSavedQuery,
  migrateLegacyWorkspace
} from "../storage.js";

let storedValues;

let storageWrites;

beforeEach(() => {
  storedValues = new Map();
  storageWrites = 0;
  globalThis.localStorage = {
    getItem: key => storedValues.get(key) ?? null,
    setItem: (key, value) => { storedValues.set(key, value); storageWrites++; },
    removeItem: key => storedValues.delete(key)
  };
  state.currentUser = null;
  state.connections = [];
  state.savedQueries = [];
  state.queryHistory = [];
  state.connectedConnectionIds = new Set();
});

test("server duplicate response leaves SQL history unchanged", async context => {
  state.currentUser = { id: "user" };
  state.queryHistory = [{ id: "previous", connectionId: "pg", db: "app", sql: "SELECT 1" }];
  context.mock.method(globalThis, "fetch", async (path) => {
    assert.equal(path, "/api/history?dedupeLatest=1");
    return { ok: true, status: 200, json: async () => ({ stored: false }) };
  });
  const stored = await appendQueryHistory({ id: "duplicate", connectionId: "pg", db: "app", sql: "SELECT 1" }, { dedupeLatest: true });
  assert.equal(stored, false);
  assert.deepEqual(state.queryHistory.map(item => item.id), ["previous"]);
});

test("server session and SQLite workspace restore without browser storage writes", async context => {
  let heartbeat;
  let channel;
  let loginCount = 0;
  let restoreCount = 0;
  let logoutCount = 0;
  const events = new Map();
  const nodes = new Map();
  const server = {
    user: { id: "user", username: "admin", enabled: true, role: "Super Admin" },
    remember: true, connections: ["pg"], expiresAt: new Date(Date.now() + 100000).toISOString()
  };
  context.mock.method(globalThis, "fetch", async (path, options) => {
    if (path === "/api/session/connections") {
      const input = JSON.parse(options.body);
      server.connections = input.enabled ? [input.id] : [];
      return { ok: true, status: 204 };
    }
    return { ok: true, status: 200, json: async () => path === "/api/session" ? { ...server }
      : path === "/api/workspace" ? { saved: [{ id: "saved" }], history: [{ id: "history" }] }
      : [{ id: "pg", database: "company", engine: "PostgreSQL" }] };
  });
  context.mock.method(globalThis, "setInterval", callback => { heartbeat = callback; return 1; });
  context.mock.method(globalThis, "BroadcastChannel", function () { channel = {}; return channel; });
  globalThis.window = { addEventListener: (name, callback) => events.set(name, callback) };
  globalThis.document = { querySelector: selector => {
    if (!nodes.has(selector)) nodes.set(selector, { close() {} });
    return nodes.get(selector);
  } };
  context.after(() => { delete globalThis.window; delete globalThis.document; });
  storedValues.set("gosql-session", JSON.stringify({ userId: "forged" }));
  await initializeSession({
    onWorkspaceReady: async () => {}, onLogin: () => loginCount++,
    onRestoreWorkspace: () => restoreCount++, onLogout: () => logoutCount++
  });
  assert.equal(loginCount, 1);
  assert.equal(state.currentUser.id, "user");
  assert.equal(state.connectedConnectionIds.has("pg"), true);
  assert.equal(state.savedQueries[0].id, "saved");
  assert.equal(state.queryHistory[0].id, "history");
  server.connections = [];
  await events.get("pageshow")({ persisted: true });
  assert.equal(state.connectedConnectionIds.size, 0);
  assert.equal(restoreCount, 1);
  await setConnected("pg", true);
  assert.deepEqual(server.connections, ["pg"]);
  await heartbeat();
  assert.equal(state.connections[0].database, "company");
  server.user = null;
  await channel.onmessage({ data: { type: "logout" } });
  assert.equal(state.currentUser, null);
  assert.equal(logoutCount, 1);
  assert.equal(storageWrites, 0);
});

test("browser account and session cannot bypass server authentication", async context => {
  const nodes = new Map();
  globalThis.document = { querySelector: selector => {
    if (!nodes.has(selector)) nodes.set(selector, { close() {} });
    return nodes.get(selector);
  } };
  context.after(() => { delete globalThis.document; });
  context.mock.method(globalThis, "fetch", async () => ({ ok: true, status: 200, json: async () => ({ user: null, setupRequired: true }) }));
  storedValues.set("gosql-users", JSON.stringify([{ id: "user", role: "Super Admin", enabled: true }]));
  storedValues.set("gosql-session", JSON.stringify({ userId: "user", remember: true, expiresAt: Date.now() + 100000 }));
  await refreshSession();
  assert.equal(state.currentUser, null);
  assert.match(nodes.get("#app").innerHTML, /Create Admin Account/);
  assert.equal(storageWrites, 0);
});

test("refresh reuses the server database session without asking for a password", async context => {
  const id = "restored-pg";
  state.currentUser = { id: "user" };
  state.connections = [{ id, name: "PG", engine: "PostgreSQL", database: "shop", databases: ["shop"] }];
  state.connectedConnectionIds.add(id);
  let opened = 0;
  globalThis.document = { querySelector: () => null };
  context.after(() => { delete globalThis.document; });
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, `/api/connections/${id}/catalog?database=shop`);
    assert.equal(options.method, "GET");
    return { ok: true, status: 200, json: async () => ({ databases: ["shop", "analytics"], schemas: [] }) };
  });
  await connectToDatabase(id, () => opened++);
  assert.equal(opened, 1);
  assert.deepEqual(state.connections[0].databases, ["shop", "analytics"]);
});

test("MySQL refresh restores database names from the existing server session", async context => {
  const id = "restored-mysql";
  state.currentUser = { id: "user" };
  state.connections = [{ id, name: "MySQL", engine: "MySQL", databases: [] }];
  state.connectedConnectionIds.add(id);
  let opened = 0;
  globalThis.document = { querySelector: () => null };
  context.after(() => { delete globalThis.document; });
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, `/api/connections/${id}/catalog`);
    assert.equal(options.method, "GET");
    return { ok: true, status: 200, json: async () => ({ databases: ["shop", "analytics"] }) };
  });
  await connectToDatabase(id, () => opened++);
  assert.equal(opened, 1);
  assert.deepEqual(state.connections[0].databases, ["shop", "analytics"]);
});

test("MariaDB refresh restores database names from the existing server session", async context => {
  const id = "restored-mariadb";
  state.currentUser = { id: "user" };
  state.connections = [{ id, name: "MariaDB", engine: "MariaDB", databases: [] }];
  state.connectedConnectionIds.add(id);
  let opened = 0;
  globalThis.document = { querySelector: () => null };
  context.after(() => { delete globalThis.document; });
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, `/api/connections/${id}/catalog`);
    assert.equal(options.method, "GET");
    return { ok: true, status: 200, json: async () => ({ databases: ["shop"] }) };
  });
  await connectToDatabase(id, () => opened++);
  assert.equal(opened, 1);
  assert.deepEqual(state.connections[0].databases, ["shop"]);
});

test("history is persisted through API before state changes and retains bounds", async context => {
  state.currentUser = { id: "user" };
  let lastBody;
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/history");
    lastBody = JSON.parse(options.body);
    return { ok: true, status: 204 };
  });
  for (let index = 0; index < MAX_HISTORY_ENTRIES + 4; index++) {
    await appendQueryHistory({ id: String(index), sql: "SELECT 1", status: ["Success", "Error", "Cancelled"][index % 3] });
  }
  assert.equal(state.queryHistory.length, MAX_HISTORY_ENTRIES);
  assert.equal(state.queryHistory[0].id, String(MAX_HISTORY_ENTRIES + 3));
  await appendQueryHistory({ id: "schema", sql: "x".repeat(MAX_SQL_LENGTH + 1), status: "Success" });
  assert.equal(lastBody.sql.length, MAX_SQL_LENGTH);
  assert.equal(lastBody.rowCount, 0);
  assert.equal(storageWrites, 0);
  context.mock.method(globalThis, "fetch", async () => ({ ok: false, status: 500, json: async () => ({ error: { message: "Disk full" } }) }));
  await assert.rejects(appendQueryHistory({ id: "failed", sql: "SELECT 1" }), /Disk full/);
  assert.equal(state.queryHistory.some(item => item.id === "failed"), false);
});

test("saved query and history deletes persist without a full workspace overwrite", async context => {
  state.currentUser = { id: "user" };
  const requests = [];
  context.mock.method(globalThis, "fetch", async (path, options) => {
    requests.push([options.method, path]);
    if (path === "/api/workspace") return { ok: true, status: 200, json: async () => ({ saved: [], history: [{ id: "from-server" }] }) };
    return { ok: true, status: 204 };
  });
  await saveQuery({ id: "saved", name: "Example", sql: "SELECT 1" });
  assert.equal(state.savedQueries.length, 1);
  await deleteSavedQuery("saved");
  await appendQueryHistory({ id: "history", sql: "SELECT 1" });
  await deleteHistoryEntry("history");
  await clearQueryHistory();
  assert.deepEqual(state.savedQueries, []);
  assert.deepEqual(state.queryHistory, []);
  await loadWorkspaceData();
  assert.equal(state.queryHistory[0].id, "from-server");
  assert.deepEqual(requests.slice(0, 2), [["POST", "/api/saved-queries"], ["DELETE", "/api/saved-queries/saved"]]);
  assert.equal(storageWrites, 0);
});

test("late history response cannot modify another user's workspace", async context => {
  state.currentUser = { id: "first" };
  let complete;
  context.mock.method(globalThis, "fetch", () => new Promise(resolve => { complete = resolve; }));
  const pending = appendQueryHistory({ id: "late", sql: "SELECT 1" });
  state.currentUser = { id: "second" };
  complete({ ok: true, status: 204 });
  await pending;
  assert.deepEqual(state.queryHistory, []);
});

test("browser migration removes legacy data only after successful server import", async context => {
  const key = "gosql-data-user";
  const raw = JSON.stringify({ connections: [{ id: "pg" }], saved: [{ id: "q", name: "Example", sql: "SELECT 1", connectionId: "pg", db: "app" }], history: [] });
  storedValues.set(key, raw);
  context.mock.method(globalThis, "fetch", async () => ({ ok: false, status: 500, json: async () => ({ error: { message: "Import failed" } }) }));
  await assert.rejects(migrateLegacyWorkspace("user"), /Import failed/);
  assert.equal(storedValues.get(key), raw);
  context.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/workspace/import");
    const body = JSON.parse(options.body);
    assert.match(body.source, /^[a-f0-9]{64}$/);
    assert.equal(body.saved[0].schemaName, "");
    assert.equal(body.connections, undefined);
    return { ok: true, status: 204 };
  });
  await migrateLegacyWorkspace("user");
  assert.equal(storedValues.has(key), false);
  assert.equal(storageWrites, 0);
});
