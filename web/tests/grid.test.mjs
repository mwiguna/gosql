import assert from "node:assert/strict";
import { test } from "node:test";
import { applyRemoteSort, columnSortTitle, getFilteredRows, initializeGrid, matchesGroup, pageNumbers, renderData, toggleColumnSort } from "../features/grid.js";
import { state } from "../state.js";

const rows = [
  { id: "10", name: "Charlie", status: "active", note: null },
  { id: "2", name: "Alice", status: "active", note: "Hello" },
  { id: "1", name: "Bob", status: "inactive", note: "" }
];

test("unsorted data keeps its original order and sorting does not mutate source rows", () => {
  const tab = { rows, search: "", sorts: [], filter: null, result: null };
  assert.equal(getFilteredRows(tab), rows);
  tab.sorts = [{ field: "id", dir: "asc" }];
  assert.deepEqual(getFilteredRows(tab).map(row => row.id), ["1", "2", "10"]);
  assert.deepEqual(rows.map(row => row.id), ["10", "2", "1"]);
  tab.sorts = [{ field: "status", dir: "asc" }, { field: "id", dir: "desc" }];
  assert.deepEqual(getFilteredRows(tab).map(row => row.id), ["10", "2", "1"]);
});

test("search and nested filters combine while NULL retains its special meaning", () => {
  const filter = { mode: "AND", children: [
    { field: "status", op: "=", value: "active" },
    { mode: "OR", children: [
      { field: "note", op: "IS NULL" },
      { field: "name", op: "contains", value: "ali" }
    ] }
  ] };
  assert.deepEqual(rows.filter(row => matchesGroup(row, filter)).map(row => row.id), ["10", "2"]);
  assert.deepEqual(getFilteredRows({ rows, filter, search: "HELLO", sorts: [] }).map(row => row.id), ["2"]);
  assert.equal(matchesGroup(rows[0], { mode: "AND", children: [{ field: "note", op: "=", value: "null" }] }), false);
  assert.equal(getFilteredRows({ rows, result: { rows: [] }, search: "", sorts: [] }).length, 0);
});

test("column sorting cycles ascending, descending, removed without disturbing other rules", () => {
  const original = [{ field: "name", dir: "asc" }];
  const ascending = toggleColumnSort(original, "id");
  const descending = toggleColumnSort(ascending, "id");
  assert.deepEqual(descending, [{ field: "name", dir: "asc" }, { field: "id", dir: "desc" }]);
  assert.deepEqual(toggleColumnSort(descending, "id"), original);
  assert.equal(original.length, 1);
});

test("sorting a database column resets pagination and keeps row identities intact", context => {
  const tab = { page: 4, cursorPaging: true, pageCursors: { 1: "", 4: "old" }, rows: [["1", "two"], ["2", "one"]], versions: ["a", "b"] };
  const before = structuredClone(tab.rows);
  let loaded;
  initializeGrid({ onLoadTablePage: (current, page) => { loaded = [current, page]; } });
  context.after(() => initializeGrid({}));
  applyRemoteSort(tab, [{ field: "name", dir: "desc" }]);
  assert.deepEqual(loaded, [tab, 1]);
  assert.equal(tab.cursorPaging, false);
  assert.deepEqual(tab.pageCursors, { 1: "" });
  assert.deepEqual(tab.rows, before);
  assert.deepEqual(tab.versions, ["a", "b"]);
  assert.equal(columnSortTitle(tab.sorts, "name"), 'name<span class="sort-priority">1 ↓</span>');
  assert.equal(columnSortTitle(tab.sorts, "<other>"), "&lt;other&gt;");
});

test("pagination keeps first and last page visible", () => {
  assert.deepEqual(pageNumbers(1, 1), [1]);
  assert.deepEqual(pageNumbers(5, 10), [1, "…", 3, 4, 5, 6, 7, "…", 10]);
  assert.deepEqual(pageNumbers(10, 10), [1, "…", 6, 7, 8, 9, 10]);
});

test("cursor pagination renders the current page at button size", context => {
  const pagination = { innerHTML: "" };
  const elements = {
    "#data-area": { querySelector: selector => elements[selector] ?? null },
    "#remote-status": { classList: { toggle() {} } },
    "#reconnect-table": {}, "#delete-selected": {}, "#remote-add-row": {},
    '[data-action="refresh-table"]': {}, "#pagination": pagination, "#page-size": {}
  };
  globalThis.document = { querySelector: selector => elements[selector] ?? null };
  state.tabs = [{ id: "cursor", remote: true, view: "Data", page: 2, pageSize: 20,
    cursorPaging: true, loading: true, rows: [], schema: [], hasMore: true }];
  state.activeTabId = "cursor";
  context.after(() => { delete globalThis.document; state.tabs = []; state.activeTabId = null; });
  renderData();
  assert.match(pagination.innerHTML, /class="page-current" aria-current="page">2<\/span>/);
  assert.doesNotMatch(pagination.innerHTML, /class="pill">2<\/span>/);
  assert.match(pagination.innerHTML, /Table size: …/);
  state.tabs[0].sizeBytes = 2048;
  state.tabs[0].sizeLoaded = true;
  renderData();
  assert.match(pagination.innerHTML, /Table size: 2 KB/);
  state.tabs[0].sizeBytes = null;
  renderData();
  assert.match(pagination.innerHTML, /Table size: —/);
});
