import assert from "node:assert/strict";
import { test } from "node:test";
import { state } from "../state.js";
import { formatTableSize, renderOverview, sortOverviewTables } from "../features/overview.js";
import { handleWorkspaceAction, loadDatabaseOverview } from "../features/workspace.js";
import { handleConnectionsAction, initializeConnections } from "../features/connections.js";

function setup(context, engine = "PostgreSQL") {
  const area = { innerHTML: "" };
  globalThis.document = { querySelector: selector => selector === "#data-area" ? area : null };
  state.currentUser = { id: "user" };
  state.connections = [{ id: "connection", engine }];
  const tab = { id: "overview", connectionId: "connection", db: "shop & sales", schemaName: 'my"schema', overview: true };
  state.tabs = [tab];
  state.activeTabId = tab.id;
  context.after(() => {
    state.tabs = []; state.connections = []; state.activeTabId = null; state.currentUser = null;
    delete globalThis.document;
  });
  return { tab, area };
}

test("overview escapes names, distinguishes unknown and empty counts, and exposes table actions", context => {
  const { tab, area } = setup(context);
  tab.tables = [
    { name: '<img src=x onerror="alert(1)">', rows: 12, estimated: true, sizeBytes: 2048 },
    { name: "empty", rows: 0, estimated: false, sizeBytes: 0 },
    { name: "unknown", rows: null, sizeBytes: null }
  ];
  renderOverview(tab);
  assert.doesNotMatch(area.innerHTML, /<img src=x/);
  assert.match(area.innerHTML, /&lt;img/);
  assert.match(area.innerHTML, /data-schema="my&quot;schema"/);
  assert.match(area.innerHTML, /≈ 12/);
  assert.match(area.innerHTML, /<td>0<\/td><td>0 B/);
  assert.match(area.innerHTML, /<td>—<\/td><td>—/);
  for (const action of ["open-table", "edit-overview-table", "delete-table", "create-table", "refresh-overview"]) {
    assert.ok(area.innerHTML.includes(`data-action="${action}"`));
  }
});

test("overview protects system databases and virtual tables", context => {
  const { tab, area } = setup(context, "MySQL");
  tab.db = "mysql";
  tab.tables = [{ name: "users", rows: 0, sizeBytes: null }];
  renderOverview(tab);
  assert.doesNotMatch(area.innerHTML, /data-action="(edit-overview-table|delete-table|create-table)"/);
  state.connections[0].engine = "SQLite";
  tab.tables[0].virtual = true;
  renderOverview(tab);
  assert.doesNotMatch(area.innerHTML, /data-action="(edit-overview-table|delete-table)"/);
  assert.match(area.innerHTML, /data-action="create-table"/);
});

test("overview loads the selected database/schema and ignores responses from a closed tab", async context => {
  const { tab, area } = setup(context);
  let resolve;
  context.mock.method(globalThis, "fetch", async path => {
    const params = new URL(path, "http://localhost").searchParams;
    assert.equal(params.get("database"), tab.db);
    assert.equal(params.get("schema"), tab.schemaName);
    return { ok: true, status: 200, json: () => new Promise(done => { resolve = done; }) };
  });
  const loading = loadDatabaseOverview(tab);
  assert.match(area.innerHTML, /Loading tables/);
  await new Promise(done => setImmediate(done));
  state.tabs = [];
  resolve([{ name: "late", rows: 12, sizeBytes: 1024 }]);
  await loading;
  assert.equal(tab.tables, undefined);
  assert.doesNotMatch(area.innerHTML, /late/);
});

test("overview displays loaded statistics and handles empty databases", async context => {
  const { tab, area } = setup(context);
  context.mock.method(globalThis, "fetch", async () => ({ ok: true, status: 200, json: async () => [] }));
  await loadDatabaseOverview(tab);
  assert.equal(tab.loading, false);
  assert.equal(tab.overviewLoaded, true);
  assert.match(area.innerHTML, /No tables yet/);
});

test("clicking a PostgreSQL schema opens its overview with the selected scope", context => {
  setup(context);
  let destination;
  initializeConnections({ onOpenOverview: (...args) => { destination = args; } });
  context.after(() => initializeConnections({}));
  assert.equal(handleConnectionsAction("database-schema", {
    dataset: { id: "connection", db: "shop", schema: "private" }, closest: () => ({ open: false })
  }), true);
  assert.deepEqual(destination, ["connection", "shop", "private"]);
});

test("table sizes distinguish unavailable statistics and scale to large tables", () => {
  assert.equal(formatTableSize(null), "—");
  assert.equal(formatTableSize(0), "0 B");
  assert.equal(formatTableSize(1024), "1 KB");
  assert.equal(formatTableSize(1024 ** 4), "1 TB");
});

test("overview sorts numeric counts and bytes while keeping unknown values last", context => {
  const { tab, area } = setup(context);
  tab.tables = [
    { name: "large", rows: 100, sizeBytes: 1024 ** 2 },
    { name: "unknown", rows: null, sizeBytes: null },
    { name: "small", rows: 2, sizeBytes: 2048 },
    { name: "empty", rows: 0, sizeBytes: 0 }
  ];
  const original = [...tab.tables];
  for (const field of ["rows", "sizeBytes"]) {
    const element = { dataset: { field } };
    handleWorkspaceAction("sort-overview", element);
    assert.deepEqual(sortOverviewTables(tab.tables, tab.overviewSort).map(table => table.name), ["empty", "small", "large", "unknown"]);
    assert.match(area.innerHTML, /aria-sort="ascending"/);
    handleWorkspaceAction("sort-overview", element);
    assert.deepEqual(sortOverviewTables(tab.tables, tab.overviewSort).map(table => table.name), ["large", "small", "empty", "unknown"]);
    assert.match(area.innerHTML, /aria-sort="descending"/);
    handleWorkspaceAction("sort-overview", element);
    assert.equal(tab.overviewSort, null);
  }
  assert.deepEqual(tab.tables, original);
});
