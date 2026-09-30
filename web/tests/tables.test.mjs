import assert from "node:assert/strict";
import { test } from "node:test";
import { loadTablePage, renderWorkspace, retryTableCount } from "../features/workspace.js";
import { canEditRemoteCell, cellEditor, formatCellValue, getCellMenuActions, handleGridAction, initializeGrid, mutateRemoteRow, renderData, selectedRemoteRow } from "../features/grid.js";
import { state } from "../state.js";

function setup(context) {
  const tab = { id: "table", connectionId: "pg", db: "other database", schemaName: 'quoted"schema', table: "a.b", page: 1, pageSize: 20, rows: [], schema: [] };
  state.currentUser = { id: "user" };
  state.tabs = [tab];
  state.activeTabId = null;
  context.after(() => { state.currentUser = null; state.tabs = []; });
  return tab;
}

test("welcome connection cards identify their database engine", context => {
  const workspace = { innerHTML: "" };
  state.currentUser = null;
  state.tabs = [];
  state.activeTabId = null;
  state.connections = [];
  globalThis.document = { querySelector: selector => selector === "#workspace" ? workspace : null };
  context.after(() => { delete globalThis.document; });
  renderWorkspace();
  for (const engine of ["PostgreSQL", "MySQL", "MariaDB", "SQLite"]) {
    assert.match(workspace.innerHTML, new RegExp(`data-engine="${engine}"`));
  }
});

test("cell menu offers typed empty values and nullable-only NULL", () => {
  assert.deepEqual(getCellMenuActions({ type: "integer", nullable: false }, true).map(action => action.action), ["cell-empty", "cell-copy"]);
  assert.equal(getCellMenuActions({ type: "integer", nullable: false }, true)[0].value, "0");
  assert.deepEqual(getCellMenuActions({ type: "text", nullable: true }, true).map(action => action.action), ["cell-empty", "cell-null", "cell-copy"]);
  assert.deepEqual(getCellMenuActions({ type: "text", nullable: true }, false).map(action => action.action), ["cell-copy"]);
});

test("query result replaces the open remote table grid", context => {
  const tab = setup(context);
  tab.remote = true;
  tab.view = "Data";
  tab.result = { error: "syntax error" };
  tab.search = "";
  tab.sorts = [];
  state.activeTabId = tab.id;
  const area = { innerHTML: "" };
  const search = {};
  globalThis.document = { querySelector: selector => selector === "#data-area" ? area : selector === "#table-search" ? search : null };
  context.after(() => { delete globalThis.document; });
  renderData();
  assert.match(area.innerHTML, /syntax error/);
  assert.match(area.innerHTML, /Back to Table/);
});

test("table pages preserve exact strings and NULL, and encode identifiers as parameters", async context => {
  const tab = setup(context);
  context.mock.method(globalThis, "fetch", async path => {
    const query = new URL(path, "http://localhost").searchParams;
    assert.equal(query.get("database"), tab.db);
    assert.equal(query.get("schema"), tab.schemaName);
    assert.equal(query.get("table"), "a.b");
    if (path.includes("/rows/count?")) return { ok: true, status: 200, json: async () => ({ totalRows: 45 }) };
    assert.equal(query.get("page"), "2");
    return { ok: true, status: 200, json: async () => ({ columns: [{ name: "number" }, { name: "optional" }], rows: [["9007199254740993.1234", null]], versions: ["42"], primaryKey: ["number"], editable: true, indexes: [{ name: "idx" }], constraints: [{ name: "unique" }], hasMore: false, totalRows: 45, duration: 12 }) };
  });
  await loadTablePage(tab, 2);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(tab.page, 2);
  assert.equal(tab.loading, false);
  assert.deepEqual(tab.rows, [["9007199254740993.1234", null]]);
  assert.equal(tab.hasMore, false);
  assert.equal(tab.totalRows, 45);
  assert.deepEqual(selectedRemoteRow(tab, 0), { values: ["9007199254740993.1234"], version: "42" });
  assert.equal(tab.editable, true);
  assert.deepEqual(tab.constraints, [{ name: "unique" }]);
});

test("table page keeps the database read-only reason", async context => {
  const tab = setup(context);
  context.mock.method(globalThis, "fetch", async path => {
    if (path.includes("/rows/count?")) return { ok: true, status: 200, json: async () => ({ totalRows: 1 }) };
    return { ok: true, status: 200, json: async () => ({ columns: [{ name: "id" }], rows: [["1"]], versions: [""], primaryKey: ["id"], editable: false, readOnlyReason: "Editing supports InnoDB, MyISAM, and Aria; this table uses MEMORY.", indexes: [], constraints: [], hasMore: false }) };
  });
  await loadTablePage(tab, 1);
  assert.equal(tab.readOnlyReason, "Editing supports InnoDB, MyISAM, and Aria; this table uses MEMORY.");
});

test("row count is reused across pages and refreshed on demand", async context => {
  const tab = setup(context);
  const requests = [];
  context.mock.method(globalThis, "fetch", async path => {
    const query = new URL(path, "http://localhost").searchParams;
    if (path.includes("/rows/count?")) {
      requests.push("count");
      return { ok: true, status: 200, json: async () => ({ totalRows: 45 }) };
    }
    requests.push(query.get("page"));
    return { ok: true, status: 200, json: async () => ({ columns: [], rows: [[query.get("page")]], versions: [], primaryKey: [], indexes: [], constraints: [], hasMore: true }) };
  });
  await loadTablePage(tab, 1);
  await new Promise(resolve => setImmediate(resolve));
  await loadTablePage(tab, 2);
  await loadTablePage(tab, 2, true);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(requests, ["1", "count", "2", "2", "count"]);
  assert.equal(tab.totalRows, 45);
});

test("primary-key pages use saved cursors for Next and Previous", async context => {
  const tab = setup(context);
  const requests = [];
  context.mock.method(globalThis, "fetch", async path => {
    const query = new URL(path, "http://localhost").searchParams;
    if (path.includes("/rows/count?")) return { ok: true, status: 200, json: async () => ({ totalRows: 45 }) };
    requests.push([query.get("page"), query.get("cursor")]);
    const page = Number(query.get("page"));
    return { ok: true, status: 200, json: async () => ({
      columns: [{ name: "id" }], rows: [[String(page * 20)]], versions: ["1"],
      primaryKey: ["id"], indexes: [], constraints: [], cursorPaging: true,
      hasMore: page < 3, nextCursor: page < 3 ? JSON.stringify([String(page * 20)]) : ""
    }) };
  });
  await loadTablePage(tab, 1);
  await new Promise(resolve => setImmediate(resolve));
  await loadTablePage(tab, 2);
  await loadTablePage(tab, 3);
  await loadTablePage(tab, 2);
  assert.deepEqual(requests, [["1", null], ["2", '["20"]'], ["3", '["40"]'], ["2", '["20"]']]);
  assert.equal(tab.page, 2);
  assert.equal(tab.cursorPaging, true);
});

test("failed row counts remain visible and can be retried", async context => {
  const tab = setup(context);
  tab.totalRows = null;
  let counts = 0;
  context.mock.method(globalThis, "fetch", async path => {
    if (path.includes("/rows/count?")) {
      counts++;
      if (counts === 1) return { ok: false, status: 429, json: async () => ({ error: { code: "count_busy", message: "Another row count is running. Retry shortly." } }) };
      return { ok: true, status: 200, json: async () => ({ totalRows: 45 }) };
    }
    return { ok: true, status: 200, json: async () => ({ columns: [], rows: [["row"]], versions: [], primaryKey: [], indexes: [], constraints: [], hasMore: true }) };
  });
  await loadTablePage(tab, 1);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(tab.countError, "Another row count is running. Retry shortly.");
  assert.equal(tab.totalRows, null);
  retryTableCount(tab);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(tab.countError, "");
  assert.equal(tab.totalRows, 45);
  assert.equal(counts, 2);
});

test("a recount after deletion returns to the last existing page", async context => {
  const tab = setup(context);
  tab.page = 3;
  tab.countLoaded = true;
  tab.totalRows = 45;
  const requestedPages = [];
  context.mock.method(globalThis, "fetch", async path => {
    const query = new URL(path, "http://localhost").searchParams;
    if (path.includes("/rows/count?")) return { ok: true, status: 200, json: async () => ({ totalRows: 39 }) };
    requestedPages.push(Number(query.get("page")));
    return { ok: true, status: 200, json: async () => ({ columns: [], rows: [["row"]], versions: [], primaryKey: [], indexes: [], constraints: [], hasMore: false }) };
  });
  await loadTablePage(tab, 3, true);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(requestedPages, [3, 2]);
  assert.equal(tab.page, 2);
  assert.equal(tab.totalRows, 39);
});

test("row identity preserves composite key order and exact text values", () => {
  const tab = { schema: [{ name: "second" }, { name: "first" }], primaryKey: ["first", "second"], rows: [["0002", "9007199254740993"]], versions: ["7"] };
  assert.deepEqual(selectedRemoteRow(tab, 0), { values: ["9007199254740993", "0002"], version: "7" });
});

test("MySQL and MariaDB primary key cells can open the editor", context => {
  const tab = { connectionId: "db", editable: true, schema: [{ name: "id", editable: true }], primaryKey: ["id"] };
  context.after(() => { state.connections = []; });
  for (const engine of ["MySQL", "MariaDB"]) {
    state.connections = [{ id: "db", engine }];
    assert.equal(canEditRemoteCell(tab, 0), true);
  }
  state.connections = [{ id: "db", engine: "PostgreSQL" }];
  assert.equal(canEditRemoteCell(tab, 0), false);
});

test("editing sends a JSON object with original key and version", async context => {
  const tab = { connectionId: "pg", db: "shop", schemaName: "public", table: "orders", page: 1, primaryKey: ["id"] };
  state.currentUser = { id: "user" };
  state.queryHistory = [];
  context.after(() => { state.currentUser = null; state.queryHistory = []; });
  let reloaded = 0;
  initializeGrid({ onLoadTablePage: () => { reloaded++; } });
  const toast = { classList: { add() {}, remove() {} } };
  const dialog = { open: false, close() {} };
  globalThis.document = { querySelector: selector => selector === "#dialog" ? dialog : toast };
  context.after(() => { delete globalThis.document; });
  context.mock.method(globalThis, "setTimeout", () => 1);
  context.mock.method(globalThis, "fetch", async (path, options) => {
    if (path === "/api/history") {
      const entry = JSON.parse(options.body);
      assert.match(entry.sql, /^UPDATE "public"\."orders" SET "status" = E'done' WHERE "id" = E'9007199254740993' AND xmin::text = E'42' RETURNING 1;$/);
      assert.equal(entry.rowCount, 1);
      return { ok: true, status: 204 };
    }
    assert.equal(path, "/api/connections/pg/rows/edit");
    assert.deepEqual(JSON.parse(options.body), {
      database: "shop", schema: "public", table: "orders", column: "status", value: "done",
      row: { values: ["9007199254740993"], version: "42" }
    });
    return { ok: true, status: 200, json: async () => ({ affectedRows: 1 }) };
  });
  assert.equal(await mutateRemoteRow(tab, "edit", { column: "status", value: "done", row: { values: ["9007199254740993"], version: "42" } }), true);
  assert.equal(reloaded, 1);
});

test("Add row inserts defaults directly and shows the stored row without a modal", async context => {
  const tab = setup(context);
  tab.remote = true;
  tab.table = "items";
  tab.schema = [{ name: "id" }];
  tab.versions = [];
  tab.page = 2;
  state.activeTabId = tab.id;
  state.queryHistory = [];
  context.after(() => { state.queryHistory = []; delete globalThis.document; });
  const toast = { classList: { add() {}, remove() {} } };
  globalThis.document = { querySelector: selector => selector === "#toast" ? toast : null };
  context.mock.method(globalThis, "setTimeout", () => 1);
  let reloaded = 0;
  initializeGrid({ onLoadTablePage: async (current, page, recount) => {
    assert.equal(current, tab);
    assert.equal(page, 1);
    assert.equal(recount, undefined);
    tab.rows = [["7"]];
    tab.versions = ["42"];
    reloaded++;
  } });
  context.mock.method(globalThis, "fetch", async (path, options) => {
    if (path === "/api/history") return { ok: true, status: 204 };
    assert.equal(path, "/api/connections/pg/rows/insert");
    assert.deepEqual(JSON.parse(options.body), { database: tab.db, schema: tab.schemaName, table: "items", values: {} });
    return { ok: true, status: 200, json: async () => ({ affectedRows: 1, row: ["7"], version: "42", sql: `INSERT INTO "${tab.schemaName}"."items" ("id") VALUES (7);` }) };
  });
  assert.equal(await handleGridAction("add-row", {}), true);
  assert.equal(reloaded, 1);
  assert.equal(state.queryHistory[0].sql, `INSERT INTO "${tab.schemaName}"."items" ("id") VALUES (7);`);
});

test("text cell editor inserts a newline with Alt+Enter and saves with Enter", context => {
  const events = new Map();
  const input = {
    value: "", selectionStart: 4, selectionEnd: 4,
    setAttribute() {}, addEventListener(name, callback) { events.set(name, callback); },
    focus() {}, select() {},
    setRangeText(text, start, end) { this.value = this.value.slice(0, start) + text + this.value.slice(end); this.selectionStart = this.selectionEnd = start + text.length; }
  };
  globalThis.document = { createElement: name => { assert.equal(name, "textarea"); return input; } };
  context.after(() => { delete globalThis.document; state.tabs = []; state.activeTabId = null; });
  state.tabs = [{ id: "table", remote: true, schema: [{ name: "description", type: "text" }] }];
  state.activeTabId = "table";
  let saved;
  cellEditor({ getField: () => "c0", getValue: () => "line", getRow: () => ({ getData: () => ({ _row: 0 }) }) }, callback => callback(), value => { saved = value; }, () => {});
  const key = (altKey) => events.get("keydown")({ key: "Enter", altKey, shiftKey: false, preventDefault() {}, stopPropagation() {} });
  key(true);
  assert.equal(input.value, "line\n");
  key(false);
  assert.equal(saved, "line\n");
});

test("long values are shortened only in the grid preview", context => {
  const value = "<blob>" + "x".repeat(150);
  assert.equal(formatCellValue(value), "&lt;blob&gt;" + "x".repeat(114) + "...");
  assert.equal(formatCellValue(null), '<span class="null-value">NULL</span>');
  const input = { value: "", setAttribute() {}, addEventListener() {}, focus() {}, select() {} };
  globalThis.document = { createElement: name => { assert.equal(name, "textarea"); return input; } };
  context.after(() => { delete globalThis.document; state.tabs = []; state.activeTabId = null; });
  state.tabs = [{ id: "table", remote: true, schema: [{ name: "payload", type: "bytea" }] }];
  state.activeTabId = "table";
  cellEditor({ getField: () => "c0", getValue: () => value, getRow: () => ({ getData: () => ({ _row: 0 }) }) }, callback => callback(), () => {}, () => {});
  assert.equal(input.value, value);
});

test("a late page response cannot restore a closed tab or another user's data", async context => {
  const tab = setup(context);
  let release;
  context.mock.method(globalThis, "fetch", () => new Promise(resolve => { release = resolve; }));
  const pending = loadTablePage(tab, 2);
  state.tabs = [];
  state.currentUser = { id: "other" };
  release({ ok: true, status: 200, json: async () => ({ columns: [], rows: [["private"]], hasMore: false }) });
  await pending;
  assert.deepEqual(tab.rows, []);
  assert.equal(tab.page, 1);
});

test("failed page loads preserve the page number and show the server error", async context => {
  const tab = setup(context);
  context.mock.method(globalThis, "fetch", async () => ({ ok: false, status: 403, json: async () => ({ error: { code: "database_permission_denied", message: "Access denied." } }) }));
  await loadTablePage(tab, 2);
  assert.equal(tab.page, 1);
  assert.equal(tab.loadError, "Access denied.");
  assert.equal(tab.loading, false);
});
