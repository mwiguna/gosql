import assert from "node:assert/strict";
import { test } from "node:test";
import { openQueryTab, openTable, readTableLocation, tableLocation } from "../features/workspace.js";
import { state } from "../state.js";
import { bindConnectionSearch, getConnectionSearch, resetConnectionTree, objectSQLTemplate } from "../features/connections.js";

test("connection search is cleared when a workspace starts", context => {
  const input = {};
  globalThis.document = { querySelector: selector => selector === "#connection-search" ? input : null };
  context.after(() => { delete globalThis.document; resetConnectionTree(); });
  bindConnectionSearch();
  assert.equal(input.readOnly, undefined);
  input.onfocus();
  assert.equal(input.readOnly, false);
  input.onblur();
  assert.equal(input.readOnly, true);
  input.oninput({ target: { value: "previous user" } });
  assert.equal(getConnectionSearch(), "previous user");
  resetConnectionTree();
  assert.equal(getConnectionSearch(), "");
});

test("table and database links round-trip all workspace views and escaped names", () => {
  for (const table of [undefined, "customers", "customer_summary"]) {
    for (const view of ["Data", "Structure", "Indexes", "Constraints"]) {
      const destination = { connectionId: "pg", db: "shop & sales", schemaName: "public", table, view };
      assert.deepEqual(readTableLocation(tableLocation(destination)), { ...destination, table: table || null });
    }
  }
  const sqlite = { connectionId: "sqlite", db: "workspace", schemaName: "", table: "customers", view: "Data" };
  assert.deepEqual(readTableLocation(tableLocation(sqlite)), sqlite);
});

test("empty links show the welcome screen and incomplete links are rejected", () => {
  assert.equal(tableLocation(null), "");
  assert.equal(readTableLocation(""), null);
  assert.equal(readTableLocation("#"), null);
  assert.throws(() => readTableLocation("#connection=pg"), /incomplete or invalid/);
  assert.throws(() => readTableLocation("#database=shop"), /incomplete or invalid/);
  assert.throws(() => readTableLocation("#connection=pg&database=shop&view=Unknown"), /incomplete or invalid/);
});

test("object templates use the selected schema or database and a single SQL statement", () => {
  assert.equal(objectSQLTemplate("PostgreSQL", "shop", 'my"schema', "view"), 'CREATE VIEW "my""schema"."new_view" AS\nSELECT 1 AS id;');
  assert.match(objectSQLTemplate("PostgreSQL", "shop", 'my"schema', "function"), /^CREATE FUNCTION "my""schema"\."new_function"\(\)\nRETURNS integer/);
  assert.match(objectSQLTemplate("PostgreSQL", "shop", "public", "procedure"), /^CREATE PROCEDURE "public"\."new_procedure"\(\)\nLANGUAGE sql/);
  for (const engine of ["MySQL", "MariaDB"]) {
    assert.equal(objectSQLTemplate(engine, "shop`db", "", "view"), 'CREATE VIEW `shop``db`.`new_view` AS\nSELECT 1 AS id;');
    assert.match(objectSQLTemplate(engine, "shop`db", "", "function"), /^CREATE FUNCTION `shop``db`\.`new_function`\(\)\nRETURNS INT/);
    assert.match(objectSQLTemplate(engine, "shop", "", "procedure"), /^CREATE PROCEDURE `shop`\.`new_procedure`\(\)\nSELECT 1;/);
  }
  assert.equal(objectSQLTemplate("SQLite", "local", "", "view"), 'CREATE VIEW main."new_view" AS\nSELECT 1 AS id;');
});

test("opening a query at the tab limit leaves the active table unchanged", context => {
  const toast = { classList: { add() {}, remove() {} } };
  globalThis.document = { querySelector: () => toast };
  context.mock.method(globalThis, "setTimeout", () => 1);
  context.after(() => { delete globalThis.document; state.tabs = []; state.activeTabId = null; });
  state.tabs = Array.from({ length: 30 }, (_, index) => ({ id: String(index), table: "customers", sql: "SELECT 1" }));
  state.activeTabId = "0";
  const before = structuredClone(state.tabs);
  openQueryTab("SELECT 2");
  openTable("pg", "shop", "orders");
  assert.deepEqual(state.tabs, before);
  assert.equal(state.activeTabId, "0");
  assert.match(toast.textContent, /30 tabs/);
});
